package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type Config struct {
	ReserveGB         float64            `json:"reserve_gb"`          // kept back for macOS and your own apps
	MinFreePercent    int                `json:"min_free_percent"`    // refuse new leases below this memory_pressure free %
	GraceMinutes      int                `json:"grace_minutes"`       // how long an orphan may run unclaimed
	DefaultTTLMinutes int                `json:"default_ttl_minutes"` // lease lifetime, renewed while the session is active
	BuildTTLMinutes   int                `json:"build_ttl_minutes"`   // builds are not renewed
	SweepSeconds      int                `json:"sweep_seconds"`
	DryRun            bool               `json:"dry_run"` // ask and notify, but never stop anything
	KindMemGB         map[string]float64 `json:"kind_mem_gb"`
	ServerNames       []string           `json:"server_names"`   // listening processes treated as dev servers
	ServerExclude     []string           `json:"server_exclude"` // substrings of args that are never dev servers
	NeverStop         []string           `json:"never_stop"`     // resource IDs or name substrings left alone
	// ExtraPatterns adds command regexes per lease kind, e.g. {"compose": ["my-stack\\.sh\\s+start"]}.
	ExtraPatterns map[string][]string `json:"extra_patterns"`

	IdleMinutes          int     `json:"idle_minutes"`           // a held resource unused this long gets its holder asked
	ClaimWindowMinutes   int     `json:"claim_window_minutes"`   // a claim is verified if the session used the resource this recently
	UnverifiedTTLMinutes int     `json:"unverified_ttl_minutes"` // lifetime of a claim with no evidence behind it
	MinDiskGB            float64 `json:"min_disk_gb"`            // refuse disk-heavy leases below this free space on /
	MaxLoadPerCore       float64 `json:"max_load_per_core"`      // refuse builds above this 1-minute load per core
}

func defaultConfig() Config {
	return Config{
		ReserveGB: 4, MinFreePercent: 10, GraceMinutes: 10, DefaultTTLMinutes: 120,
		BuildTTLMinutes: 30, SweepSeconds: 30, DryRun: true, ExtraPatterns: map[string][]string{},
		IdleMinutes: 30, ClaimWindowMinutes: 60, UnverifiedTTLMinutes: 30, MinDiskGB: 10, MaxLoadPerCore: 1.5,
		KindMemGB: map[string]float64{
			"cluster": 3, "compose": 2, "docker-build": 3, "cargo-build": 2.5, "gradle": 3,
			"xcode-build": 3, "simulator": 1.5, "loadtest": 2, "server": 0.5,
		},
		ServerNames:   []string{"node", "python", "python3*", "ruby", "deno", "bun", "php", "uvicorn", "gunicorn", "java", "air", "main"},
		ServerExclude: []string{"mcp", "claude", "codex", "copilot", "language-server", "langserver", "tsserver", "gopls", "rust-analyzer"},
	}
}

// buildKinds are leases for a single command: released when the tool call returns.
var buildKinds = map[string]bool{"docker-build": true, "cargo-build": true, "gradle": true, "xcode-build": true, "loadtest": true}

// diskKinds write images, layers or build output; they need free disk.
var diskKinds = map[string]bool{"cluster": true, "compose": true, "docker-build": true, "cargo-build": true, "gradle": true, "xcode-build": true}

// cpuKinds are refused when the machine is already saturated.
var cpuKinds = map[string]bool{"docker-build": true, "cargo-build": true, "gradle": true, "xcode-build": true, "loadtest": true}

// resourceToLease is the lease kind for an unleased resource attributed to a session.
var resourceToLease = map[string]string{
	RKCluster: "cluster", RKContainers: "compose", RKGradle: "gradle", RKSimulator: "simulator", RKServer: "server",
}

// leaseToResource says which discovered resource kind a lease kind produces.
var leaseToResource = map[string]string{
	"cluster": RKCluster, "compose": RKContainers, "gradle": RKGradle, "simulator": RKSimulator, "server": RKServer,
}

type Lease struct {
	ID        string    `json:"id"`
	Session   string    `json:"session"`
	Agent     string    `json:"agent"`
	Kind      string    `json:"kind"`
	MemGB     float64   `json:"mem_gb"`
	Purpose   string    `json:"purpose"`
	Cmd       string    `json:"cmd,omitempty"`
	TTL       int       `json:"ttl_minutes"`
	Created   time.Time `json:"created"`
	Expires   time.Time `json:"expires"`
	Resources []string  `json:"resources,omitempty"`

	Verified  bool       `json:"verified"`
	Evidence  string     `json:"evidence,omitempty"`
	LastUsed  time.Time  `json:"last_used"`
	IdleAsked *time.Time `json:"idle_asked,omitempty"`
	PeakMB    float64    `json:"peak_mb,omitempty"`
}

type Session struct {
	ID       string    `json:"id"`
	Agent    string    `json:"agent"`
	Cwd      string    `json:"cwd"`
	LastSeen time.Time `json:"last_seen"`
}

type Question struct {
	ResourceID string            `json:"resource_id"`
	Text       string            `json:"text"`
	Created    time.Time         `json:"created"`
	Delivered  map[string]bool   `json:"delivered"`
	Answers    map[string]string `json:"answers"`       // session -> "not mine" etc.
	For        string            `json:"for,omitempty"` // only this session is asked
}

type State struct {
	Leases    map[string]*Lease    `json:"leases"`
	Sessions  map[string]*Session  `json:"sessions"`
	Resources map[string]*Resource `json:"resources"`
	Questions map[string]*Question `json:"questions"`
	Pinned    map[string]bool      `json:"pinned"`
	// Touches records, per resource, when each session last ran a command that used it.
	Touches map[string]map[string]time.Time `json:"touches"`
	// Learned is the observed peak memory per lease kind, in MB (moving average).
	Learned map[string]float64 `json:"learned_mb"`
}

type Daemon struct {
	mu        sync.Mutex
	cfg       Config
	st        State
	mem       MemStats
	dir       string
	lastSweep time.Time
	firstDone bool
	events    []Event
	eventSeq  int

	agentSessions map[int]string         // agent PID -> Claude session id, learned from hooks
	recent        map[string][]cmdRecord // session -> recent commands
}

type Event struct {
	Seq  int       `json:"seq"`
	Time time.Time `json:"time"`
	Kind string    `json:"kind"` // first word: GRANT, ORPHAN, STOPPED, ...
	Text string    `json:"text"`
}

func homeDir() string {
	if d := os.Getenv("AGENTRM_HOME"); d != "" {
		return d
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".agentrm")
}

func sockPath() string {
	if s := os.Getenv("AGENTRM_SOCK"); s != "" {
		return s
	}
	return filepath.Join(homeDir(), "agentrm.sock")
}

func newID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func loadConfig(dir string) Config {
	cfg := defaultConfig()
	p := filepath.Join(dir, "config.json")
	if b, err := os.ReadFile(p); err == nil {
		if err := json.Unmarshal(b, &cfg); err != nil {
			log.Printf("config.json invalid, using defaults: %v", err)
			cfg = defaultConfig()
		}
	} else {
		b, _ := json.MarshalIndent(cfg, "", "  ")
		_ = os.WriteFile(p, b, 0o644)
	}
	return cfg
}

func (d *Daemon) save() {
	b, _ := json.MarshalIndent(d.st, "", "  ")
	tmp := filepath.Join(d.dir, "state.json.tmp")
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, filepath.Join(d.dir, "state.json"))
	}
}

func (d *Daemon) event(format string, a ...any) {
	text := fmt.Sprintf(format, a...)
	d.eventSeq++
	kind, _, _ := strings.Cut(text, " ")
	d.events = append(d.events, Event{Seq: d.eventSeq, Time: time.Now(), Kind: kind, Text: text})
	if len(d.events) > 100 {
		d.events = d.events[len(d.events)-100:]
	}
	line := time.Now().Format("2006-01-02 15:04:05 ") + text
	log.Print(line)
	f, err := os.OpenFile(filepath.Join(d.dir, "events.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err == nil {
		fmt.Fprintln(f, line)
		f.Close()
	}
}

// notify shows a macOS notification. When the menu-bar app runs the daemon it sets
// AGENTRM_NOTIFY=app and posts notifications itself from the event feed.
func notify(title, msg string) {
	if os.Getenv("AGENTRM_NOTIFY") == "app" {
		return
	}
	esc := func(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) }
	_ = exec.Command("osascript", "-e",
		fmt.Sprintf(`display notification "%s" with title "agentrm" subtitle "%s"`, esc(msg), esc(title))).Start()
}

func runDaemon() error {
	dir := homeDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	d := &Daemon{dir: dir, cfg: loadConfig(dir), agentSessions: map[int]string{}, recent: map[string][]cmdRecord{}}
	d.st = State{Leases: map[string]*Lease{}, Sessions: map[string]*Session{}, Resources: map[string]*Resource{},
		Questions: map[string]*Question{}, Pinned: map[string]bool{}}
	if b, err := os.ReadFile(filepath.Join(dir, "state.json")); err == nil {
		_ = json.Unmarshal(b, &d.st)
		// Orphan clocks restart with the daemon; discovery rebuilds the resource list.
		for _, r := range d.st.Resources {
			r.OrphanSince = nil
		}
		d.st.Questions = map[string]*Question{}
	}
	for _, m := range []*map[string]*Lease{&d.st.Leases} {
		if *m == nil {
			*m = map[string]*Lease{}
		}
	}
	if d.st.Sessions == nil {
		d.st.Sessions = map[string]*Session{}
	}
	if d.st.Resources == nil {
		d.st.Resources = map[string]*Resource{}
	}
	if d.st.Pinned == nil {
		d.st.Pinned = map[string]bool{}
	}
	if d.st.Touches == nil {
		d.st.Touches = map[string]map[string]time.Time{}
	}
	if d.st.Learned == nil {
		d.st.Learned = map[string]float64{}
	}
	d.mem = readMem()

	_ = os.Remove(sockPath())
	ln, err := net.Listen("unix", sockPath())
	if err != nil {
		return err
	}
	d.event("daemon started (grace %dm, reserve %.0f GB, dry_run=%v)", d.cfg.GraceMinutes, d.cfg.ReserveGB, d.cfg.DryRun)

	go func() {
		for {
			d.sweep()
			time.Sleep(time.Duration(d.cfg.SweepSeconds) * time.Second)
		}
	}()

	mux := http.NewServeMux()
	handle := func(path string, fn func(req map[string]any) (any, error)) {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			req := map[string]any{}
			_ = json.NewDecoder(r.Body).Decode(&req)
			pid, _ := r.Context().Value(ctxKey{}).(int)
			d.mu.Lock()
			d.identify(pid, req)
			resp, err := fn(req)
			d.save()
			d.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
			_ = json.NewEncoder(w).Encode(resp)
		})
	}
	handle("/acquire", d.acquire)
	handle("/release", d.release)
	handle("/renew", d.renew)
	handle("/claim", d.claim)
	handle("/disown", d.disown)
	handle("/pin", d.pin)
	handle("/inbox", d.inbox)
	handle("/session", func(req map[string]any) (any, error) { d.touch(req); return map[string]bool{"ok": true}, nil })
	handle("/status", d.status)
	handle("/stop", d.stopNow)
	handle("/config", d.setConfig)
	handle("/touch", d.touchCmd)
	handle("/whoami", func(req map[string]any) (any, error) {
		return map[string]any{"session": str(req, "session"), "agent": str(req, "agent"), "human": req["_human"]}, nil
	})
	srv := &http.Server{Handler: mux, ConnContext: withPeer}
	return srv.Serve(ln)
}

func str(req map[string]any, k string) string {
	if v, ok := req[k].(string); ok {
		return v
	}
	return ""
}

func num(req map[string]any, k string) float64 {
	if v, ok := req[k].(float64); ok {
		return v
	}
	return 0
}

func (d *Daemon) touch(req map[string]any) *Session {
	id := str(req, "session")
	if id == "" {
		return nil
	}
	s := d.st.Sessions[id]
	if s == nil {
		s = &Session{ID: id}
		d.st.Sessions[id] = s
	}
	if a := str(req, "agent"); a != "" {
		s.Agent = a
	}
	if c := str(req, "cwd"); c != "" {
		s.Cwd = c
	}
	s.LastSeen = time.Now()
	return s
}

// committedMB is what is already spoken for: each lease counts the larger of its
// estimate and what its resources really use; unleased resources count as measured.
func (d *Daemon) committedMB() float64 {
	total := 0.0
	for _, l := range d.st.Leases {
		used := 0.0
		for _, rid := range l.Resources {
			if r := d.st.Resources[rid]; r != nil {
				used += r.MemMB
			}
		}
		total += max(l.MemGB*1024, used)
	}
	for _, r := range d.st.Resources {
		if r.LeaseID == "" {
			total += r.MemMB
		}
	}
	return total
}

func (d *Daemon) budgetMB() float64 { return d.mem.TotalMB - d.cfg.ReserveGB*1024 }

func (d *Daemon) holders() string {
	var parts []string
	for _, l := range d.st.Leases {
		parts = append(parts, fmt.Sprintf("%s %s %.1f GB (%s: %s)", l.ID, l.Kind, l.MemGB, short(l.Session), trunc(l.Purpose, 50)))
	}
	var orphans []string
	for _, r := range d.st.Resources {
		if r.LeaseID == "" && r.MemMB > 200 {
			orphans = append(orphans, fmt.Sprintf("%s %.1f GB", r.ID, r.MemMB/1024))
		}
	}
	sort.Strings(parts)
	sort.Strings(orphans)
	s := "Leases: " + orDash(strings.Join(parts, "; "))
	if len(orphans) > 0 {
		s += ". Unclaimed: " + strings.Join(orphans, "; ")
	}
	return s
}

func orDash(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func short(s string) string { return trunc(s, 12) }

func (d *Daemon) acquire(req map[string]any) (any, error) {
	d.touch(req)
	kind := str(req, "kind")
	if kind == "" {
		return nil, fmt.Errorf("kind is required")
	}
	session := str(req, "session")
	// A session that already holds a lease of this kind reuses it.
	for _, l := range d.st.Leases {
		if l.Session == session && l.Kind == kind {
			l.Expires = time.Now().Add(time.Duration(l.TTL) * time.Minute)
			l.LastUsed = time.Now()
			return map[string]any{"granted": true, "lease": l, "reused": true}, nil
		}
	}
	memGB := num(req, "mem_gb")
	if memGB == 0 {
		memGB = d.estimateGB(kind)
	}
	d.mem = readMem()
	if diskKinds[kind] && d.mem.DiskFreeGB >= 0 && d.mem.DiskFreeGB < d.cfg.MinDiskGB {
		return map[string]any{"granted": false, "reason": fmt.Sprintf(
			"the disk is nearly full (%.1f GB free on /, minimum %.0f GB). Prune Docker images or build caches first",
			d.mem.DiskFreeGB, d.cfg.MinDiskGB)}, nil
	}
	if cpuKinds[kind] && d.mem.Cores > 0 && d.mem.Load1 > float64(d.mem.Cores)*d.cfg.MaxLoadPerCore {
		return map[string]any{"granted": false, "reason": fmt.Sprintf(
			"the CPU is saturated (load %.1f on %d cores, limit %.1f per core). %s",
			d.mem.Load1, d.mem.Cores, d.cfg.MaxLoadPerCore, d.holders())}, nil
	}
	committed, budget := d.committedMB(), d.budgetMB()
	if committed+memGB*1024 > budget {
		return map[string]any{"granted": false, "reason": fmt.Sprintf(
			"not enough memory: %.1f GB committed + %.1f GB requested > %.1f GB budget (%.0f GB total, %.0f GB reserved). %s",
			committed/1024, memGB, budget/1024, d.mem.TotalMB/1024, d.cfg.ReserveGB, d.holders())}, nil
	}
	if d.mem.FreePercent >= 0 && d.mem.FreePercent < d.cfg.MinFreePercent {
		return map[string]any{"granted": false, "reason": fmt.Sprintf(
			"the Mac is under memory pressure (%d%% free, minimum %d%%). %s",
			d.mem.FreePercent, d.cfg.MinFreePercent, d.holders())}, nil
	}
	ttl := int(num(req, "ttl_minutes"))
	if ttl == 0 {
		ttl = d.cfg.DefaultTTLMinutes
		if buildKinds[kind] {
			ttl = d.cfg.BuildTTLMinutes
		}
	}
	l := &Lease{ID: newID(), Session: session, Agent: str(req, "agent"), Kind: kind, MemGB: memGB,
		Purpose: str(req, "purpose"), Cmd: trunc(str(req, "cmd"), 200), TTL: ttl, Created: time.Now(),
		Expires:  time.Now().Add(time.Duration(ttl) * time.Minute),
		Verified: true, Evidence: "requested before starting", LastUsed: time.Now()}
	d.st.Leases[l.ID] = l
	d.event("GRANT %s %s %.1f GB to %s (%s): %s", l.ID, kind, memGB, session, l.Agent, l.Purpose)
	return map[string]any{"granted": true, "lease": l}, nil
}

// estimateGB is the memory a new lease of kind is expected to need: what past leases of
// that kind really used (plus 15%), or the configured default until there is history.
func (d *Daemon) estimateGB(kind string) float64 {
	if mb := d.st.Learned[kind]; mb > 0 {
		return max(mb*1.15/1024, 0.25)
	}
	if gb := d.cfg.KindMemGB[kind]; gb > 0 {
		return gb
	}
	return 1
}

func (d *Daemon) learn(l *Lease) {
	kind := l.Kind
	if rk, ok := strings.CutPrefix(kind, "claim:"); ok {
		kind = resourceToLease[rk]
	}
	if kind == "" || l.PeakMB <= 0 {
		return
	}
	if old := d.st.Learned[kind]; old > 0 {
		d.st.Learned[kind] = 0.7*old + 0.3*l.PeakMB
	} else {
		d.st.Learned[kind] = l.PeakMB
	}
}

func (d *Daemon) dropLease(l *Lease, why string) {
	d.learn(l)
	delete(d.st.Questions, "idle:"+l.ID)
	for _, rid := range l.Resources {
		if r := d.st.Resources[rid]; r != nil && r.LeaseID == l.ID {
			r.LeaseID = ""
		}
	}
	delete(d.st.Leases, l.ID)
	d.event("RELEASE %s %s (%s): %s", l.ID, l.Kind, short(l.Session), why)
}

func (d *Daemon) release(req map[string]any) (any, error) {
	id, session, kind := str(req, "lease_id"), str(req, "session"), str(req, "kind")
	if l := d.st.Leases[id]; id != "" && l != nil && l.Session != session && req["_human"] != true {
		return nil, fmt.Errorf("lease %s belongs to %s; an agent can release only its own leases", id, l.Session)
	}
	n := 0
	for _, l := range d.st.Leases {
		if (id != "" && l.ID == id) || (id == "" && session != "" && l.Session == session && (kind == "" || l.Kind == kind)) {
			d.dropLease(l, "released by "+orDash(session))
			n++
		}
	}
	return map[string]int{"released": n}, nil
}

func (d *Daemon) renew(req map[string]any) (any, error) {
	d.touch(req)
	session := str(req, "session")
	n := 0
	for _, l := range d.st.Leases {
		// Leases that hold resources renew on use (see sweep), not on any tool call.
		if l.Session == session && !buildKinds[l.Kind] && len(l.Resources) == 0 {
			l.Expires = time.Now().Add(time.Duration(l.TTL) * time.Minute)
			n++
		}
	}
	return map[string]int{"renewed": n}, nil
}

func (d *Daemon) claim(req map[string]any) (any, error) {
	d.touch(req)
	rid := str(req, "resource_id")
	r := d.st.Resources[rid]
	if r == nil {
		return nil, fmt.Errorf("no running resource %q (see agentrm status)", rid)
	}
	session, now := str(req, "session"), time.Now()
	if r.LeaseID != "" {
		if l := d.st.Leases[r.LeaseID]; l != nil {
			if l.Session == session {
				// The holder confirming it still needs it.
				l.LastUsed, l.IdleAsked = now, nil
				l.Expires = now.Add(time.Duration(l.TTL) * time.Minute)
				delete(d.st.Questions, "idle:"+l.ID)
				d.event("CONFIRM %s by %s (lease %s)", rid, session, l.ID)
				return map[string]any{"lease": l, "verified": l.Verified, "evidence": l.Evidence}, nil
			}
			return nil, fmt.Errorf("%s is already held by lease %s (%s: %s)", rid, l.ID, l.Session, l.Purpose)
		}
	}
	purpose := str(req, "purpose")
	if purpose == "" {
		return nil, fmt.Errorf("say why you need it: --purpose")
	}
	ttl := int(num(req, "ttl_minutes"))
	if ttl == 0 {
		ttl = d.cfg.DefaultTTLMinutes
	}
	verified, evidence := d.claimEvidence(r, session, req["_human"] == true)
	if !verified {
		ttl = min(ttl, d.cfg.UnverifiedTTLMinutes)
	}
	l := &Lease{ID: newID(), Session: session, Agent: str(req, "agent"), Kind: "claim:" + r.Kind,
		MemGB: r.MemMB / 1024, Purpose: purpose, TTL: ttl, Created: now,
		Expires: now.Add(time.Duration(ttl) * time.Minute), Resources: []string{rid},
		Verified: verified, Evidence: evidence, LastUsed: now}
	d.st.Leases[l.ID] = l
	r.LeaseID, r.OrphanSince = l.ID, nil
	delete(d.st.Questions, rid)
	tag := "verified"
	if !verified {
		tag = "UNVERIFIED"
	}
	d.event("CLAIM %s by %s as lease %s (%s: %s): %s", rid, session, l.ID, tag, evidence, purpose)
	return map[string]any{"lease": l, "verified": verified, "evidence": evidence}, nil
}

// claimEvidence decides whether a claim is backed by what the machine has seen.
func (d *Daemon) claimEvidence(r *Resource, session string, human bool) (bool, string) {
	switch {
	case human:
		return true, "claimed by a person, not an agent"
	case r.StartedBy != "" && r.StartedBy == session:
		return true, "this session started it"
	}
	if t, ok := d.st.Touches[r.ID][session]; ok && time.Since(t) < time.Duration(d.cfg.ClaimWindowMinutes)*time.Minute {
		return true, "this session used it at " + t.Format("15:04")
	}
	return false, fmt.Sprintf("no sign this session has used it in the last %d min; "+
		"held for %d min unless a command of this session uses it", d.cfg.ClaimWindowMinutes, d.cfg.UnverifiedTTLMinutes)
}

func (d *Daemon) disown(req map[string]any) (any, error) {
	d.touch(req)
	rid, session := str(req, "resource_id"), str(req, "session")
	if q := d.st.Questions[rid]; q != nil {
		q.Answers[session] = "not mine"
	}
	d.event("DISOWN %s by %s", rid, session)
	return map[string]bool{"ok": true}, nil
}

func (d *Daemon) pin(req map[string]any) (any, error) {
	if req["_human"] != true {
		return nil, fmt.Errorf("only a person can pin or unpin; an agent should claim instead")
	}
	rid := str(req, "resource_id")
	on := str(req, "pinned") != "false"
	if on {
		d.st.Pinned[rid] = true
	} else {
		delete(d.st.Pinned, rid)
	}
	if r := d.st.Resources[rid]; r != nil {
		r.Pinned, r.OrphanSince = on, nil
	}
	d.event("PIN %s = %v", rid, on)
	return map[string]bool{"pinned": on}, nil
}

// inbox returns the questions this session has not seen yet.
func (d *Daemon) inbox(req map[string]any) (any, error) {
	d.touch(req)
	session := str(req, "session")
	all := str(req, "all") == "true"
	var out []string
	for _, q := range d.st.Questions {
		if q.Answers[session] != "" || (!all && q.Delivered[session]) || (q.For != "" && q.For != session) {
			continue
		}
		q.Delivered[session] = true
		out = append(out, q.Text)
	}
	sort.Strings(out)
	return map[string]any{"questions": out}, nil
}

func (d *Daemon) status(req map[string]any) (any, error) {
	return map[string]any{
		"mem": d.mem, "committed_mb": d.committedMB(), "budget_mb": d.budgetMB(), "config": d.cfg,
		"leases": d.st.Leases, "resources": d.st.Resources, "sessions": d.st.Sessions,
		"questions": d.st.Questions, "last_sweep": d.lastSweep, "events": d.events,
		"learned_mb": d.st.Learned, "touches": d.st.Touches,
	}, nil
}

// stopNow stops a resource immediately, at the user's request.
func (d *Daemon) stopNow(req map[string]any) (any, error) {
	rid := str(req, "resource_id")
	r := d.st.Resources[rid]
	if r == nil {
		return nil, fmt.Errorf("no running resource %q", rid)
	}
	if l := d.st.Leases[r.LeaseID]; l != nil && l.Session != str(req, "session") && req["_human"] != true {
		return nil, fmt.Errorf("%s is held by %s (lease %s); an agent can stop only what it holds or what nobody holds",
			rid, l.Session, l.ID)
	}
	if r.Pinned && req["_human"] != true {
		return nil, fmt.Errorf("%s is pinned by the user", rid)
	}
	if err := stop(r); err != nil {
		return nil, err
	}
	if l := d.st.Leases[r.LeaseID]; l != nil {
		l.Resources = remove(l.Resources, rid)
	}
	delete(d.st.Resources, rid)
	delete(d.st.Questions, rid)
	d.event("STOPPED %s (%s, %.1f GB) by hand. Restart: %s", rid, r.Name, r.MemMB/1024, howToRestart(r))
	return map[string]string{"restart": howToRestart(r)}, nil
}

// setConfig changes settings live and writes them to config.json.
func (d *Daemon) setConfig(req map[string]any) (any, error) {
	if req["_human"] != true {
		return nil, fmt.Errorf("only a person can change agentrm's settings")
	}
	if v, ok := req["dry_run"].(bool); ok {
		d.cfg.DryRun = v
	}
	if v := num(req, "grace_minutes"); v >= 1 {
		d.cfg.GraceMinutes = int(v)
	}
	if v := num(req, "reserve_gb"); v >= 1 {
		d.cfg.ReserveGB = v
	}
	if v := num(req, "min_free_percent"); v >= 1 {
		d.cfg.MinFreePercent = int(v)
	}
	if v, ok := req["never_stop"].([]any); ok {
		d.cfg.NeverStop = nil
		for _, x := range v {
			if s, ok := x.(string); ok && s != "" {
				d.cfg.NeverStop = append(d.cfg.NeverStop, s)
			}
		}
	}
	b, _ := json.MarshalIndent(d.cfg, "", "  ")
	if err := os.WriteFile(filepath.Join(d.dir, "config.json"), b, 0o644); err != nil {
		return nil, err
	}
	d.event("CONFIG dry_run=%v grace=%dm reserve=%.0fGB min_free=%d%%", d.cfg.DryRun, d.cfg.GraceMinutes, d.cfg.ReserveGB, d.cfg.MinFreePercent)
	return d.cfg, nil
}

func (d *Daemon) neverStop(r *Resource) bool {
	for _, x := range d.cfg.NeverStop {
		if r.ID == x || strings.Contains(r.Name, x) {
			return true
		}
	}
	return false
}

func (d *Daemon) sweep() {
	found := discover(d.cfg) // slow; done without the lock
	mem := readMem()
	est := establishedPorts()
	procs := procCache.get()

	d.mu.Lock()
	defer d.mu.Unlock()
	defer d.save()
	now := time.Now()
	d.mem, d.lastSweep = mem, now

	// Expire leases.
	for _, l := range d.st.Leases {
		if now.After(l.Expires) {
			d.dropLease(l, "expired (not renewed or released)")
		}
	}

	// Merge discovery into state.
	seen := map[string]bool{}
	for _, r := range found {
		seen[r.ID] = true
		old := d.st.Resources[r.ID]
		if old != nil {
			r.FirstSeen, r.LeaseID, r.OrphanSince = old.FirstSeen, old.LeaseID, old.OrphanSince
			r.LastActive, r.StartedBy = old.LastActive, old.StartedBy
		} else {
			r.FirstSeen = now
			if d.firstDone {
				d.attribute(r, procs, now)
			}
		}
		if isActive(r, est) {
			r.LastActive = now
		}
		if r.LeaseID != "" && d.st.Leases[r.LeaseID] == nil {
			r.LeaseID = ""
		}
		r.Pinned = d.st.Pinned[r.ID]
		d.st.Resources[r.ID] = r
	}
	for id := range d.st.Resources {
		if !seen[id] {
			delete(d.st.Resources, id)
			delete(d.st.Questions, id)
			delete(d.st.Touches, id)
			for _, l := range d.st.Leases {
				l.Resources = remove(l.Resources, id)
			}
		}
	}
	d.firstDone = true

	d.checkLeases(now)
	grace := time.Duration(d.cfg.GraceMinutes) * time.Minute

	// Orphans: ask, then stop after the grace period.
	for _, r := range d.st.Resources {
		if r.LeaseID != "" || r.Pinned || d.neverStop(r) {
			r.OrphanSince = nil
			delete(d.st.Questions, r.ID)
			continue
		}
		if r.OrphanSince == nil {
			t := now
			r.OrphanSince = &t
			d.st.Questions[r.ID] = &Question{ResourceID: r.ID, Created: now,
				Delivered: map[string]bool{}, Answers: map[string]string{},
				Text: fmt.Sprintf("[agentrm] %s (%s, %s, %.1f GB) is running with no lease. If you are using it, run: "+
					"agentrm claim %s --purpose \"<why>\"   — if not, run: agentrm disown %s. "+
					"Unclaimed, it is stopped at %s.", r.ID, r.Name, r.Detail, r.MemMB/1024, r.ID, r.ID,
					now.Add(grace).Format("15:04"))}
			d.event("ORPHAN %s (%s, %.1f GB): asking active sessions; stop at %s",
				r.ID, r.Name, r.MemMB/1024, now.Add(grace).Format("15:04"))
			notify("Unclaimed: "+r.Name, fmt.Sprintf("%.1f GB. Stopping at %s unless an agent claims it.",
				r.MemMB/1024, now.Add(grace).Format("15:04")))
			continue
		}
		if now.Sub(*r.OrphanSince) < grace {
			continue
		}
		if d.cfg.DryRun {
			continue
		}
		if err := stop(r); err != nil {
			d.event("STOP FAILED %s: %v", r.ID, err)
			continue
		}
		d.event("STOPPED %s (%s, %.1f GB), unclaimed for %s. Restart: %s",
			r.ID, r.Name, r.MemMB/1024, grace, howToRestart(r))
		notify("Stopped "+r.Name, fmt.Sprintf("Freed ~%.1f GB. Restart: %s", r.MemMB/1024, howToRestart(r)))
		delete(d.st.Resources, r.ID)
		delete(d.st.Questions, r.ID)
	}

	// Forget sessions idle for a day.
	for id, s := range d.st.Sessions {
		if now.Sub(s.LastSeen) > 24*time.Hour {
			delete(d.st.Sessions, id)
		}
	}
}

// checkLeases keeps resource-holding leases alive on use and asks holders about idle ones.
func (d *Daemon) checkLeases(now time.Time) {
	// Leases that hold resources stay alive on use: a command of the holder session that
	// touched one of them, or live activity on them. Idle ones get their holder asked.
	grace := time.Duration(d.cfg.GraceMinutes) * time.Minute
	idle := time.Duration(d.cfg.IdleMinutes) * time.Minute
	for _, l := range d.st.Leases {
		if len(l.Resources) == 0 {
			continue
		}
		used, mem := l.LastUsed, 0.0
		for _, rid := range l.Resources {
			r := d.st.Resources[rid]
			if r == nil {
				continue
			}
			mem += r.MemMB
			if r.LastActive.After(used) {
				used = r.LastActive
			}
			if t := d.st.Touches[rid][l.Session]; t.After(used) {
				used = t
			}
		}
		l.PeakMB = max(l.PeakMB, mem)
		if used.After(l.LastUsed) {
			l.LastUsed, l.IdleAsked = used, nil
			delete(d.st.Questions, "idle:"+l.ID)
		}
		if exp := l.LastUsed.Add(time.Duration(l.TTL) * time.Minute); exp.After(l.Expires) {
			l.Expires = exp
		}
		if now.Sub(l.LastUsed) < idle {
			continue
		}
		if l.IdleAsked == nil {
			t := now
			l.IdleAsked = &t
			d.st.Questions["idle:"+l.ID] = &Question{ResourceID: strings.Join(l.Resources, ","), Created: now, For: l.Session,
				Delivered: map[string]bool{}, Answers: map[string]string{},
				Text: fmt.Sprintf("[agentrm] Your lease %s on %s has not been used for %s. Still need it? Run "+
					"agentrm claim %s --purpose \"<why>\" to keep it, or agentrm release %s. "+
					"No answer by %s releases it.", l.ID, strings.Join(l.Resources, ", "), now.Sub(l.LastUsed).Round(time.Minute),
					l.Resources[0], l.ID, now.Add(grace).Format("15:04"))}
			d.event("IDLE lease %s (%s) unused for %s: asking %s", l.ID, strings.Join(l.Resources, ","),
				now.Sub(l.LastUsed).Round(time.Minute), short(l.Session))
		} else if now.Sub(*l.IdleAsked) > grace {
			d.dropLease(l, fmt.Sprintf("idle for %s and no reply", now.Sub(l.LastUsed).Round(time.Minute)))
		}
	}
}

// touchCmd records a command an agent is about to run, and which resources it uses.
func (d *Daemon) touchCmd(req map[string]any) (any, error) {
	d.touch(req)
	session, cmd, cwd, now := str(req, "session"), str(req, "cmd"), str(req, "cwd"), time.Now()
	if session == "" || cmd == "" {
		return map[string]any{"touched": []string{}}, nil
	}
	rec := append(d.recent[session], cmdRecord{Time: now, Cmd: trunc(cmd, 500), Cwd: cwd})
	if len(rec) > 50 {
		rec = rec[len(rec)-50:]
	}
	d.recent[session] = rec
	ids := touchedBy(cmd, cwd, d.st.Resources, currentKubeContext())
	for _, id := range ids {
		if d.st.Touches[id] == nil {
			d.st.Touches[id] = map[string]time.Time{}
		}
		d.st.Touches[id][session] = now
	}
	return map[string]any{"touched": ids}, nil
}

// attribute binds a newly seen resource to the session that started it.
func (d *Daemon) attribute(r *Resource, procs map[int]*proc, now time.Time) {
	if s := d.startedBy(r, procs); s != "" {
		r.StartedBy = s
		var best *Lease
		for _, l := range d.st.Leases {
			if l.Session == s && leaseToResource[l.Kind] == r.Kind && (best == nil || l.Created.After(best.Created)) {
				best = l
			}
		}
		if best == nil {
			// Started without asking: give it a lease so its holder is known and it is idle-checked.
			agent := "claude"
			if strings.HasPrefix(s, "codex-") {
				agent = "codex"
			}
			best = &Lease{ID: newID(), Session: s, Agent: agent, Kind: resourceToLease[r.Kind], MemGB: r.MemMB / 1024,
				Purpose: "started without a lease", TTL: d.cfg.DefaultTTLMinutes, Created: now,
				Expires: now.Add(time.Duration(d.cfg.DefaultTTLMinutes) * time.Minute), Verified: true,
				Evidence: "this session started it", LastUsed: now}
			d.st.Leases[best.ID] = best
			d.event("UNLEASED %s was started by %s without a lease; recorded as lease %s", r.ID, short(s), best.ID)
		}
		r.LeaseID = best.ID
		best.Resources = append(best.Resources, r.ID)
		d.event("BIND %s to lease %s (%s started it)", r.ID, best.ID, short(s))
		return
	}
	// No evidence: bind only if exactly one recent lease of the right kind could own it.
	var cands []*Lease
	for _, l := range d.st.Leases {
		if leaseToResource[l.Kind] == r.Kind && now.Sub(l.Created) < 15*time.Minute {
			cands = append(cands, l)
		}
	}
	if len(cands) == 1 {
		r.LeaseID = cands[0].ID
		cands[0].Resources = append(cands[0].Resources, r.ID)
		d.event("BIND %s to lease %s (%s; only candidate)", r.ID, cands[0].ID, short(cands[0].Session))
	}
}

func remove(xs []string, x string) []string {
	out := xs[:0]
	for _, y := range xs {
		if y != x {
			out = append(out, y)
		}
	}
	return out
}
