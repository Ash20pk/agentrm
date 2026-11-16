// Package manager is AgentRM's policy: who may run what, and what happens to things nobody
// claims. All state lives here behind one mutex; nothing that can block runs under it (D1).
package manager

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"agentrm/internal/api"
	"agentrm/internal/evidence"
	"agentrm/internal/model"
	"agentrm/internal/shell"
	"agentrm/internal/store"
	"agentrm/internal/sys"
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
	hookSeen        time.Time
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
	self            sys.SelfUsage
	tamper          tamperState
	dockerEventsErr string
	cfgDirty        bool

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
		kick: make(chan struct{}, 1), readFast: sys.ReadFast,
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

// Close writes the state and marks a clean shutdown.
func (m *Manager) Close() {
	m.persist()
	if m.saver != nil {
		m.saver.Close()
	}
	m.tamperCleanShutdown()
	if c, ok := m.evlog.(io.Closer); ok {
		_ = c.Close()
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

// errApproval asks the dispatcher to hold a privileged request for a click in the app.
type errApproval struct{ what string }

func (e *errApproval) Error() string { return e.what }

// ErrNeedsApproval is returned by Handle with the approval to wait for.
type ErrNeedsApproval struct{ Approval *model.Approval }

func (e *ErrNeedsApproval) Error() string {
	return fmt.Sprintf("%s needs your approval in the AgentRM app (approval %s); click Approve there",
		e.Approval.What, e.Approval.ID)
}

func (m *Manager) routes() map[string]handler {
	return map[string]handler{
		"/acquire":  m.acquire,
		"/release":  m.release,
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
		"/approval": m.approvalStatus,
		"/whoami": func(c api.Caller, r api.Req) (any, []Effect, error) {
			return c, nil, nil
		},
	}
}

// Handle serves one request: it finds out who is calling (outside the lock), decides (under
// it), runs any effects (outside it) and records their results (under it again).
func (m *Manager) Handle(path string, peerPID int, req api.Req) (any, error) {
	h, ok := m.routes()[path]
	if !ok {
		return nil, fmt.Errorf("unknown request %s", path)
	}
	facts := gatherFacts(peerPID)

	m.mu.Lock()
	caller := m.resolve(facts, &req)
	resp, effects, err := h(caller, req)
	var ea *errApproval
	if errors.As(err, &ea) {
		a := m.newApproval(path, req, caller, ea.what)
		m.mu.Unlock()
		return nil, &ErrNeedsApproval{Approval: a}
	}
	m.dirty = true
	m.mu.Unlock()

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
		Self: m.self, StartedAt: m.started, Notes: m.notes,
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
