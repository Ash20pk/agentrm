// Package manager is AgentRM's policy: who may run what, and what happens to things nobody
// claims. All state lives here behind one mutex; nothing that can block runs under it (D1).
package manager

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"agentrm/internal/api"
	"agentrm/internal/discover"
	"agentrm/internal/evidence"
	"agentrm/internal/model"
	"agentrm/internal/shell"
	"agentrm/internal/store"
	"agentrm/internal/sys"
	"agentrm/internal/trace"
)

type cmdRecord struct {
	Time time.Time
	Cmd  string
	Cwd  string
}

// origin is who started a container, learned from the Docker event stream.
type origin struct {
	session string
	at      time.Time
}

type Manager struct {
	mu sync.Mutex

	cfg     model.Config
	st      model.State
	mem     sys.MemStats
	dir     string
	version string
	started time.Time
	notes   []string

	lastSweep time.Time
	firstDone bool
	events    []model.Event
	eventSeq  int
	failed    map[string]error // discovery sources that failed on the last sweep

	agentSessions   map[int]string
	recent          map[string][]cmdRecord
	hookSeen        map[string]time.Time // agent kind -> last hook call
	claimRefused    map[string]bool      // "pid session": a hook that named another agent's session, reported once
	waiters         []*model.Waiter
	swapRate        float64
	lastSwapouts    float64
	lastSwapAt      time.Time
	activity        *evidence.Activity
	classifier      *shell.Classifier
	dockerOrigin    map[string]origin          // resource ID -> who started it
	pending         map[string]string          // resource ID -> effect in flight (pause, stop, ...)
	connSeen        map[string]map[string]bool // paused resource -> connections already seen
	footBase        map[string]float64         // session -> idle footprint baseline (MB)
	wouldDecided    map[string]bool            // dry-run decisions already recorded
	missing         map[string]int             // session -> sweeps its agent process has been missing
	infos           *infoCache                 // what each process was started with, and by which session
	exits           *sys.ExitWatcher           // the kernel's word that an agent exited; nil in tests
	stale           map[int]bool               // agent processes with no terminal, silent for hours
	selfPID         int
	recP            atomic.Pointer[trace.Writer]              // what the daemon sees, for replay; nil unless recording (read without mu)
	onDecide        func(*model.Decision)                     // replay watches decisions as they are taken
	onGrant         func(needMB, headroomMB float64)          // tests check every grant fits
	backfilled      map[*model.Waiter]map[string]bool         // head of the queue -> sessions that went ahead of it
	lastBackfill    bool                                      // the admission just decided was a backfill
	footprint       func(pid int) (float64, bool)             // compressed memory included; nil in tests
	parked          map[*model.Waiter][]chan *api.AcquireResp // queued request -> clients connected and waiting for it
	self            sys.SelfUsage
	tamper          tamperState
	dockerEventsErr string
	returnProbes    []*returnProbe   // stops whose effect on available memory is being measured
	grantProbes     []*grantProbe    // recent grants, watched for the Mac starting to swap
	preps           map[string]*prep // open predictions of a session's next heavy request (prepare.go)
	cfgDirty        bool
	restartedAt     map[string]*restartMark   // what AgentRM restarted (it comes back by itself), and when
	leakTracks      map[procKey]*leakTrack    // large processes watched for leaks (leaks.go)
	leakProcs       map[int]*sys.Proc         // the process table the leak watch last saw
	procUID         func(pid int) (int, bool) // who a process runs as; nil in tests

	// The vault (vault.go): live credentials, in memory only, and the API-key proxy.
	vaultLive   map[string]*vaultLive // grant ID -> credential
	vaultTok    map[string]string     // proxy token -> grant ID
	vaultClient *http.Client          // nil: the default; tests point it at fakes
	vaultWG     sync.WaitGroup        // revocations in flight
	proxyOnce   sync.Once
	proxyBase   string
	proxyErr    error

	dirty    bool
	saver    *store.Saver
	evlog    io.Writer
	log      *slog.Logger
	notify   func(title, msg string)
	now      func() time.Time
	readFast func() sys.MemStats // live pressure, load and disk; tests replace it
	kick     chan struct{}       // ask for a full sweep now
}

// Options configures a Manager. Zero values give the real machine; tests override them.
type Options struct {
	Dir     string
	Version string
	Now     func() time.Time
	Notify  func(title, msg string)
	Log     io.Writer // daemon log; events go to events.log in Dir
	NoDisk  bool      // tests: keep state in memory only
}

func New(o Options) *Manager {
	now := o.Now
	if now == nil {
		now = time.Now
	}
	m := &Manager{
		dir: o.Dir, version: o.Version, now: now, started: now(),
		agentSessions: map[int]string{}, recent: map[string][]cmdRecord{}, failed: map[string]error{},
		activity: evidence.NewActivity(), dockerOrigin: map[string]origin{}, pending: map[string]string{},
		connSeen: map[string]map[string]bool{}, footBase: map[string]float64{}, wouldDecided: map[string]bool{},
		missing: map[string]int{}, hookSeen: map[string]time.Time{}, claimRefused: map[string]bool{}, infos: newInfoCache(nil), stale: map[int]bool{}, selfPID: os.Getpid(),
		parked: map[*model.Waiter][]chan *api.AcquireResp{}, backfilled: map[*model.Waiter]map[string]bool{},
		kick: make(chan struct{}, 1), readFast: sys.ReadFast, restartedAt: map[string]*restartMark{}, preps: map[string]*prep{},
		vaultLive: map[string]*vaultLive{}, vaultTok: map[string]string{},
	}
	m.notify = o.Notify
	if m.notify == nil {
		m.notify = func(string, string) {}
	}
	logw := o.Log
	if logw == nil {
		logw = io.Discard
	}
	m.log = slog.New(slog.NewTextHandler(logw, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if o.NoDisk {
		m.cfg, m.st, m.evlog = model.DefaultConfig(), model.NewState(), io.Discard
		m.readFast = func() sys.MemStats { return m.mem }
		m.classifier, _ = shell.NewClassifier(nil)
		return m
	}
	m.mem = sys.ReadMem()
	cfg, notes := store.LoadConfig(o.Dir, m.mem.TotalMB)
	st, snotes := store.LoadState(o.Dir, now())
	m.cfg, m.st = cfg, st
	m.notes = append(notes, snotes...)
	m.applyLogLevel()
	var bad []string
	m.classifier, bad = shell.NewClassifier(cfg.ExtraPatterns)
	for _, p := range bad {
		m.notes = append(m.notes, "extra_patterns: invalid pattern ignored: "+p)
	}
	m.infos = newInfoCache(sys.ReadProcInfo)
	m.footprint = sys.Footprint
	m.procUID = sys.ProcUID
	if w, err := sys.NewExitWatcher(); err == nil {
		m.exits = w
	} else {
		m.notes = append(m.notes, "could not watch agent exits (sweeps will notice instead): "+err.Error())
	}
	m.applyRecording()
	m.evlog = store.NewRotatingFile(filepath.Join(o.Dir, "events.log"), 5<<20, 3)
	m.saver = store.NewSaver(o.Dir, time.Second, func(err error) { m.log.Error("state not saved", "err", err) })
	// Keep only agent-process mappings whose process is still that agent.
	procs := sys.Procs.Get()
	kept := map[string]string{}
	for pidStr, session := range m.st.AgentSessions {
		var pid int
		if _, err := fmt.Sscan(pidStr, &pid); err == nil && procs[pid] != nil && sys.AgentKind(procs[pid]) == "claude" {
			m.agentSessions[pid] = session
			kept[pidStr] = session
		}
	}
	m.st.AgentSessions = kept
	m.vaultRestarted(now())
	m.tamperInit()
	for _, n := range m.notes {
		m.log.Warn(n)
	}
	return m
}

func (m *Manager) applyLogLevel() {
	lvl := slog.LevelInfo
	switch strings.ToLower(m.cfg.LogLevel) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	}
	slog.SetLogLoggerLevel(lvl)
}

func newID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// event records something the user may want to know. Must be called with mu held.
func (m *Manager) event(format string, a ...any) {
	text := fmt.Sprintf(format, a...)
	m.eventSeq++
	kind, _, _ := strings.Cut(text, " ")
	now := m.now()
	m.events = append(m.events, model.Event{Seq: m.eventSeq, Time: now, Kind: kind, Text: text})
	if len(m.events) > 200 {
		m.events = m.events[len(m.events)-200:]
	}
	fmt.Fprintln(m.evlog, now.Format("2006-01-02 15:04:05 ")+text)
	m.log.Info(text, "event", kind)
	m.dirty = true
}

// persist hands the state to the saver if it changed. Called on a timer and at shutdown.
func (m *Manager) persist() {
	if m.saver == nil {
		return
	}
	m.mu.Lock()
	if !m.dirty {
		m.mu.Unlock()
		return
	}
	b, err := json.MarshalIndent(m.st, "", "  ")
	m.dirty = false
	m.mu.Unlock()
	if err == nil {
		m.saver.Queue(b)
		m.tamperWrote(b)
	}
}

// applyRecording starts or stops the trace recorder to match the config. Must hold mu, or be
// in New.
func (m *Manager) applyRecording() {
	switch {
	case m.cfg.RecordTrace && m.recorder() == nil && m.dir != "":
		m.recP.Store(trace.NewWriter(trace.Dir(m.dir), max(1, m.cfg.TraceDays)))
	case !m.cfg.RecordTrace && m.recorder() != nil:
		m.recP.Swap(nil).Close()
	}
}

// recorder is the trace writer, or nil. Read it once into a local: recording can be turned off
// between two reads (by a config change, under mu, while a sweep runs outside it).
func (m *Manager) recorder() *trace.Writer { return m.recP.Load() }

// Close thaws what AgentRM froze, writes the state and marks a clean shutdown.
func (m *Manager) Close() {
	m.resumePaused()
	m.vaultClose()
	if rec := m.recP.Swap(nil); rec != nil {
		rec.Close()
	}
	m.persist()
	if m.saver != nil {
		m.saver.Close()
	}
	m.tamperCleanShutdown()
	if c, ok := m.evlog.(io.Closer); ok {
		_ = c.Close()
	}
}

// resumePaused thaws everything AgentRM paused. A frozen process stays frozen until something
// sends it SIGCONT, and once the daemon is gone (quit, disabled, uninstalled) nothing would:
// a dev server would hang every request on its port forever. What is still unclaimed is paused
// again by the next daemon, after a fresh grace period.
func (m *Manager) resumePaused() {
	m.mu.Lock()
	var effects []Effect
	for _, r := range m.st.Resources {
		if r.PausedAt != nil && m.pending[r.ID] == "" {
			effects = append(effects, m.thawEffect(r, "AgentRM is shutting down, and leaves nothing frozen behind"))
		}
	}
	m.mu.Unlock()
	if len(effects) > 0 {
		m.afterAsync(RunEffects(effects))
	}
}

// Notify shows a macOS notification unless the app is running the daemon (the app then posts
// its own from the event feed, with action buttons).
func OSANotify(title, msg string) {
	if os.Getenv("AGENTRM_NOTIFY") == "app" {
		return
	}
	esc := func(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) }
	_ = exec.Command("osascript", "-e",
		fmt.Sprintf(`display notification "%s" with title "AgentRM" subtitle "%s"`, esc(msg), esc(title))).Start()
}

// ---- requests ----

type handler func(c api.Caller, r api.Req) (any, []Effect, error)

// errApproval asks the dispatcher to hold a privileged request for a click in the app. a is set
// when the handler made the approval itself (a vault request).
type errApproval struct {
	what string
	a    *model.Approval
}

func (e *errApproval) Error() string { return e.what }

// ErrNeedsApproval is returned by Handle with the approval to wait for.
type ErrNeedsApproval struct{ Approval *model.Approval }

func (e *ErrNeedsApproval) Error() string {
	if v := e.Approval.Vault; v != nil {
		return fmt.Sprintf("asked the person for %s (vault request %s); they approve it with Touch ID in AgentRM", v.Account, e.Approval.ID)
	}
	return fmt.Sprintf("%s needs your approval in the AgentRM app (approval %s); click Approve there",
		e.Approval.What, e.Approval.ID)
}

func (m *Manager) routes() map[string]handler {
	return map[string]handler{
		"/acquire":  m.acquire,
		"/release":  m.release,
		"/end":      m.end,
		"/yield":    m.yield,
		"/collect":  m.collect,
		"/receipt":  m.receipt,
		"/renew":    m.renew,
		"/claim":    m.claim,
		"/disown":   m.disown,
		"/pin":      m.pin,
		"/inbox":    m.inbox,
		"/touch":    m.touchCmd,
		"/status":   m.status,
		"/stop":     m.stopNow,
		"/resume":   m.resumeNow,
		"/restore":  m.restore,
		"/config":   m.setConfig,
		"/priority": m.setPriority,
		"/review":   m.review,
		"/approve":  m.approve,
		"/bypass":   m.bypass,
		"/approval": m.approvalStatus,
		"/leak":     m.leakAct,
		// The vault. Its answer (with the secret) is served apart: see HandleCtx.
		"/vault/request":  m.vaultRequest,
		"/vault/list":     m.vaultList,
		"/vault/revoke":   m.vaultRevokeReq,
		"/vault/accounts": m.vaultSetAccounts,
		"/whoami": func(c api.Caller, r api.Req) (any, []Effect, error) {
			return c, nil, nil
		},
	}
}

// Handle serves one request: it finds out who is calling (outside the lock), decides (under
// it), runs any effects (outside it) and records their results (under it again).
func (m *Manager) Handle(path string, peerPID int, req api.Req) (any, error) {
	return m.HandleCtx(context.Background(), path, peerPID, req)
}

// HandleCtx is Handle for a caller that may go away: ctx ends when its connection does. Only a
// request that waits (acquire with WaitSeconds) looks at it.
func (m *Manager) HandleCtx(ctx context.Context, path string, peerPID int, req api.Req) (any, error) {
	facts := gatherFacts(peerPID)
	if sys.PeerSuspect(ctx) {
		facts.viaApp = false // accepted just after the daemon was frozen: see sys.PeerSuspect
	}
	if path == "/vault/answer" {
		// It carries a secret: never traced, and it mints outside the lock.
		return m.vaultAnswer(ctx, facts, req)
	}
	h, ok := m.routes()[path]
	if !ok {
		return nil, fmt.Errorf("unknown request %s", path)
	}
	req.Repo, req.Branch = "", ""
	if req.Cwd != "" && (path == "/touch" || path == "/acquire" || path == "/renew" || path == "/claim" || path == "/vault/request") {
		req.Repo, req.Branch = sys.GitHead(req.Cwd) // a file read: outside the lock
	}
	return m.handleFacts(ctx, path, h, facts, req)
}

// readOnly requests change nothing and are not recorded.
var readOnly = map[string]bool{"/status": true, "/whoami": true, "/approval": true, "/receipt": true, "/vault/list": true}

// handleFacts serves a request from a caller already traced (live, or from a trace).
func (m *Manager) handleFacts(ctx context.Context, path string, h handler, facts facts, req api.Req) (any, error) {
	if rec := m.recorder(); rec != nil && !readOnly[path] && !strings.HasPrefix(path, "/vault/") {
		rec.Write(trace.Frame{T: m.now(), Request: &trace.Request{Path: path, Req: req, Facts: trace.Facts{
			PID: facts.pid, AgentPID: facts.agentPID, Agent: facts.agent, GUI: facts.gui, App: facts.app, AppPID: facts.appPID, ViaApp: facts.viaApp}}})
	}
	m.mu.Lock()
	caller := m.resolve(facts, &req)
	resp, effects, err := h(caller, req)
	var ea *errApproval
	if errors.As(err, &ea) {
		a := ea.a
		if a == nil {
			a = m.newApproval(path, req, caller, ea.what)
		}
		m.mu.Unlock()
		return nil, &ErrNeedsApproval{Approval: a}
	}
	m.dirty = true
	// The response is encoded after the lock is released, while sweeps change the state it
	// points into: concurrent map iteration and write is a fatal error in Go, not a recoverable
	// one. So the caller gets a copy taken now.
	resp = snapshot(resp)
	var parked chan *api.AcquireResp
	if a, ok := resp.(*api.AcquireResp); ok && path == "/acquire" && err == nil && req.WaitSeconds > 0 && !a.Granted && a.Queue > 0 {
		parked = m.park(caller.Session, req.Kind)
	}
	m.mu.Unlock()
	if parked != nil {
		return m.awaitGrant(ctx, parked, caller, req)
	}

	if len(effects) > 0 {
		results := RunEffects(effects)
		m.mu.Lock()
		m.applyEffects(results)
		m.mu.Unlock()
		if r, ok := resp.(interface{ afterEffects([]EffectResult) }); ok {
			r.afterEffects(results)
		}
	}
	return resp, err
}

// ---- status ----

func (m *Manager) status(c api.Caller, r api.Req) (any, []Effect, error) {
	src := map[string]string{"docker": "", "simctl": "", "procs": ""}
	for s, err := range m.failed {
		src[s] = err.Error()
	}
	if m.dockerEventsErr != "" {
		src["docker-events"] = m.dockerEventsErr
	}
	return &api.Status{
		APIVersion: api.Version, DaemonVersion: m.version,
		Mem: m.mem, HeadroomMB: m.headroomMB(), PendingMB: m.pendingMB(), SwapRate: m.swapRate,
		Waiters: m.liveWaiters(m.now()), Config: m.cfg, Leases: m.st.Leases, Resources: m.st.Resources,
		Sessions: m.st.Sessions, Questions: m.st.Questions, LastSweep: m.lastSweep, Events: m.events,
		Learned: m.st.Learned, Stopped: m.st.Stopped, Approvals: m.st.Approvals, Sources: src,
		Self: m.self, StartedAt: m.started, Notes: m.notes, Prediction: m.predictionScore(),
		TrustLevels: m.trustLevels(m.now()), Accuracy: m.st.Accuracy, FailOpen: m.failOpen(m.now()),
		Leaks: m.leakList(), VaultAccounts: m.vaultAccountList(), VaultGrants: m.st.VaultGrants,
	}, nil, nil
}

// holders describes who holds what, for refusal messages.
func (m *Manager) holders() string {
	var parts []string
	for _, l := range m.st.Leases {
		parts = append(parts, fmt.Sprintf("%s %s %.1f GB (%s: %s)", l.ID, l.Kind, l.MemGB, short(l.Session), trunc(l.Purpose, 50)))
	}
	var orphans []string
	for _, r := range m.st.Resources {
		if r.LeaseID == "" && r.MemMB > 200 && !discover.SelfRestarting(r) {
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

func (m *Manager) neverStop(r *model.Resource) bool {
	for _, x := range m.cfg.NeverStop {
		if r.ID == x || strings.Contains(r.Name, x) {
			return true
		}
	}
	return false
}

// ---- small helpers ----

func orDash(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func short(s string) string { return trunc(s, 12) }

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
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

func minutes(n int) time.Duration { return time.Duration(n) * time.Minute }

// snapshot deep-copies a response through its JSON form, so nothing in it aliases the state.
// The JSON form is all a client ever sees, so nothing is lost.
func snapshot(v any) any {
	rv := reflect.ValueOf(v)
	if v == nil || rv.Kind() != reflect.Pointer || rv.IsNil() {
		return v
	}
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	out := reflect.New(rv.Elem().Type())
	if json.Unmarshal(b, out.Interface()) != nil {
		return v
	}
	return out.Interface()
}
