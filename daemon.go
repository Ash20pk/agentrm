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
}

func defaultConfig() Config {
	return Config{
		ReserveGB: 4, MinFreePercent: 10, GraceMinutes: 10, DefaultTTLMinutes: 120,
		BuildTTLMinutes: 30, SweepSeconds: 30,
		KindMemGB: map[string]float64{
			"cluster": 3, "compose": 2, "docker-build": 3, "cargo-build": 2.5, "gradle": 3,
			"xcode-build": 3, "simulator": 1.5, "loadtest": 2, "server": 0.5,
		},
		ServerNames:   []string{"node", "python", "python3*", "ruby", "deno", "bun", "php", "uvicorn", "gunicorn", "java", "air", "main", "core"},
		ServerExclude: []string{"mcp", "claude", "codex", "copilot", "language-server", "langserver", "tsserver", "gopls", "rust-analyzer"},
	}
}

// buildKinds are leases for a single command: released when the tool call returns.
var buildKinds = map[string]bool{"docker-build": true, "cargo-build": true, "gradle": true, "xcode-build": true, "loadtest": true}

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
	Answers    map[string]string `json:"answers"` // session -> "not mine" etc.
}

type State struct {
	Leases    map[string]*Lease    `json:"leases"`
	Sessions  map[string]*Session  `json:"sessions"`
	Resources map[string]*Resource `json:"resources"`
	Questions map[string]*Question `json:"questions"`
	Pinned    map[string]bool      `json:"pinned"`
}

type Daemon struct {
	mu        sync.Mutex
	cfg       Config
	st        State
	mem       MemStats
	dir       string
	lastSweep time.Time
	firstDone bool
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
	line := time.Now().Format("2006-01-02 15:04:05 ") + fmt.Sprintf(format, a...)
	log.Print(line)
	f, err := os.OpenFile(filepath.Join(d.dir, "events.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err == nil {
		fmt.Fprintln(f, line)
		f.Close()
	}
}

func notify(title, msg string) {
	esc := func(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) }
	_ = exec.Command("osascript", "-e",
		fmt.Sprintf(`display notification "%s" with title "agentrm" subtitle "%s"`, esc(msg), esc(title))).Start()
}

func runDaemon() error {
	dir := homeDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	d := &Daemon{dir: dir, cfg: loadConfig(dir)}
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
			d.mu.Lock()
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
	return http.Serve(ln, mux)
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
			return map[string]any{"granted": true, "lease": l, "reused": true}, nil
		}
	}
	memGB := num(req, "mem_gb")
	if memGB == 0 {
		memGB = d.cfg.KindMemGB[kind]
		if memGB == 0 {
			memGB = 1
		}
	}
	d.mem = readMem()
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
		Expires: time.Now().Add(time.Duration(ttl) * time.Minute)}
	d.st.Leases[l.ID] = l
	d.event("GRANT %s %s %.1f GB to %s (%s): %s", l.ID, kind, memGB, session, l.Agent, l.Purpose)
	return map[string]any{"granted": true, "lease": l}, nil
}

func (d *Daemon) dropLease(l *Lease, why string) {
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
		if l.Session == session && !buildKinds[l.Kind] {
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
	if r.LeaseID != "" {
		if l := d.st.Leases[r.LeaseID]; l != nil {
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
	l := &Lease{ID: newID(), Session: str(req, "session"), Agent: str(req, "agent"), Kind: "claim:" + r.Kind,
		MemGB: r.MemMB / 1024, Purpose: purpose, TTL: ttl, Created: time.Now(),
		Expires: time.Now().Add(time.Duration(ttl) * time.Minute), Resources: []string{rid}}
	d.st.Leases[l.ID] = l
	r.LeaseID, r.OrphanSince = l.ID, nil
	delete(d.st.Questions, rid)
	d.event("CLAIM %s by %s as lease %s: %s", rid, l.Session, l.ID, purpose)
	return map[string]any{"lease": l}, nil
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
		if q.Answers[session] != "" || (!all && q.Delivered[session]) {
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
		"questions": d.st.Questions, "last_sweep": d.lastSweep,
	}, nil
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
		} else {
			r.FirstSeen = now
			// New resource: bind it to the newest lease of the matching kind made in the last 15 min.
			if d.firstDone {
				var best *Lease
				for _, l := range d.st.Leases {
					if leaseToResource[l.Kind] == r.Kind && now.Sub(l.Created) < 15*time.Minute &&
						(best == nil || l.Created.After(best.Created)) {
						best = l
					}
				}
				if best != nil {
					r.LeaseID = best.ID
					best.Resources = append(best.Resources, r.ID)
					d.event("BIND %s to lease %s (%s)", r.ID, best.ID, short(best.Session))
				}
			}
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
			for _, l := range d.st.Leases {
				l.Resources = remove(l.Resources, id)
			}
		}
	}
	d.firstDone = true

	// Orphans: ask, then stop after the grace period.
	grace := time.Duration(d.cfg.GraceMinutes) * time.Minute
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

func remove(xs []string, x string) []string {
	out := xs[:0]
	for _, y := range xs {
		if y != x {
			out = append(out, y)
		}
	}
	return out
}
