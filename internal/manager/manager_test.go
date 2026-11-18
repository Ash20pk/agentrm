package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"agentrm/internal/api"
	"agentrm/internal/discover"
	"agentrm/internal/model"
	"agentrm/internal/sys"
)

type clock struct{ t time.Time }

func (c *clock) Now() time.Time               { return c.t }
func (c *clock) Add(d time.Duration)          { c.t = c.t.Add(d) }
func (c *clock) At(d time.Duration) time.Time { return c.t.Add(d) }

func newTest(t *testing.T, availGB float64) (*Manager, *clock) {
	t.Helper()
	c := &clock{t: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)}
	m := New(Options{NoDisk: true, Now: c.Now})
	m.mem = sys.MemStats{TotalMB: 16384, WiredMB: 3000, AvailableMB: availGB * 1024, PressureLevel: 1, Cores: 10, DiskFreeGB: 100}
	return m, c
}

// fakeEffects records effects instead of acting on the machine.
func fakeEffects(t *testing.T, fail map[string]bool) *[]Effect {
	t.Helper()
	var mu sync.Mutex
	var got []Effect
	old := Executor
	Executor = func(e Effect) error {
		mu.Lock()
		got = append(got, e)
		mu.Unlock()
		if fail[e.Op] {
			return fmt.Errorf("%s failed", e.Op)
		}
		return nil
	}
	t.Cleanup(func() { Executor = old })
	return &got
}

func agent(s string) api.Caller { return api.Caller{Agent: "claude", Session: s} }

var you = api.Caller{Agent: "you", Session: "you", Human: true, ViaApp: true, App: "AgentRM.app"}
var terminal = api.Caller{Agent: "you", Session: "you", Human: true, App: "Warp.app"}

func acquire(t *testing.T, m *Manager, c api.Caller, kind string, gb float64) *api.AcquireResp {
	t.Helper()
	out, _, err := m.acquire(c, api.Req{Kind: kind, MemGB: gb, Purpose: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return out.(*api.AcquireResp)
}

func addRes(m *Manager, id, kind string, mb float64, lease string) *model.Resource {
	r := &model.Resource{ID: id, Kind: kind, Name: id, MemMB: mb, LeaseID: lease, Source: "docker",
		Containers: []string{strings.TrimPrefix(id, "kind:") + "-control-plane"}}
	m.st.Resources[id] = r
	if l := m.st.Leases[lease]; l != nil {
		l.Resources = append(l.Resources, id)
	}
	return r
}

// ---- admission ----

func TestAdmissionUsesMeasuredHeadroom(t *testing.T) {
	m, _ := newTest(t, 4) // 4 GB available, 1 GB kept free
	if r := acquire(t, m, agent("a"), "cluster", 2.5); !r.Granted {
		t.Fatalf("2.5 GB refused with 3 GB headroom: %s", r.Reason)
	}
	// The grant is a promise until the cluster shows up: it counts against others.
	if r := acquire(t, m, agent("b"), "cargo-build", 1); r.Granted {
		t.Fatal("granted 1 GB with 2.5 of 3 GB promised")
	}
	// Once the promise is real use it is in the measurement, not counted twice.
	l := m.findLease("a", "cluster")
	addRes(m, "kind:x", model.RKCluster, 2400, l.ID)
	m.mem.AvailableMB = 4*1024 - 2400
	m.waiters = nil
	if r := acquire(t, m, agent("b"), "server", 0.4); !r.Granted {
		t.Fatalf("0.4 GB refused with 0.5 GB headroom: %s", r.Reason)
	}
}

func TestAdmissionRefusesWhileThrashingOrFull(t *testing.T) {
	m, _ := newTest(t, 8)
	m.mem.PressureLevel = 2
	if r := acquire(t, m, agent("a"), "server", 0.1); !r.Granted {
		t.Fatalf("warning pressure with no swapping refused work: %+v", r)
	}
	m.st.Leases = map[string]*model.Lease{}
	m.swapRate = 150
	if r := acquire(t, m, agent("a"), "server", 0.1); r.Granted || !strings.Contains(r.Reason, "memory pressure") {
		t.Fatalf("granted under pressure while swapping: %+v", r)
	}
	m.mem.PressureLevel, m.swapRate = 4, 0
	if r := acquire(t, m, agent("a"), "server", 0.1); r.Granted || !strings.Contains(r.Reason, "critical") {
		t.Fatalf("granted under critical pressure: %+v", r)
	}
	m.mem.PressureLevel, m.swapRate = 1, 900
	if r := acquire(t, m, agent("a"), "server", 0.1); r.Granted || !strings.Contains(r.Reason, "swapping") {
		t.Fatalf("granted while swapping: %+v", r)
	}
	m.swapRate, m.mem.DiskFreeGB = 0, 3
	if r := acquire(t, m, agent("a"), "docker-build", 1); r.Granted || !strings.Contains(r.Reason, "disk") {
		t.Fatalf("granted a build on a full disk: %+v", r)
	}
	m.mem.DiskFreeGB, m.mem.Load1 = 100, 40
	if r := acquire(t, m, agent("a"), "cargo-build", 1); r.Granted || !strings.Contains(r.Reason, "CPU") {
		t.Fatalf("granted a build on a saturated CPU: %+v", r)
	}
	m.mem.Load1 = 1
	if r := acquire(t, m, agent("a"), "cluster", 40); r.Granted || !strings.Contains(r.Reason, "never fit") {
		t.Fatalf("an impossible request: %+v", r)
	}
	for _, w := range m.waiters {
		if w.NeedMB >= 40*1024 {
			t.Fatal("an impossible request was queued")
		}
	}
}

func TestQueueOrder(t *testing.T) {
	m, c := newTest(t, 3) // 2 GB headroom
	if r := acquire(t, m, agent("big"), "cluster", 3); r.Granted {
		t.Fatal("3 GB granted with 2 GB headroom")
	}
	c.Add(time.Second)
	if r := acquire(t, m, agent("small"), "server", 1); r.Granted || !strings.Contains(r.Reason, "earlier requests") {
		t.Fatalf("a later request jumped the queue: %+v", r)
	}
	// An urgent session goes ahead of normal waiters.
	m.touchSession("urgent", "claude", "").Priority = 1
	c.Add(time.Second)
	if r := acquire(t, m, agent("urgent"), "server", 1); !r.Granted {
		t.Fatalf("an urgent request waited behind normal ones: %s", r.Reason)
	}
	// Memory frees up: the first normal waiter gets it before the later one.
	m.mem.AvailableMB = 6 * 1024
	c.Add(time.Second)
	if r := acquire(t, m, agent("big"), "cluster", 3); !r.Granted {
		t.Fatalf("the head of the queue was refused: %s", r.Reason)
	}
	// A waiter whose session goes quiet loses its place.
	m.mem.AvailableMB = 3 * 1024
	acquire(t, m, agent("ghost"), "cluster", 3)
	c.Add(m.queueKeep() + time.Second)
	m.st.Leases = map[string]*model.Lease{}
	if r := acquire(t, m, agent("small"), "server", 1); !r.Granted {
		t.Fatalf("an abandoned request still blocks the queue: %s", r.Reason)
	}
}

func TestFairShare(t *testing.T) {
	m, c := newTest(t, 10)
	// "hog" holds a lot; while someone waits, it may not grow past its share.
	acquire(t, m, agent("hog"), "cluster", 6)
	acquire(t, m, agent("hog"), "compose", 1)
	m.mem.AvailableMB = 1.5 * 1024 // the rest is gone
	if r := acquire(t, m, agent("other"), "cluster", 2); r.Granted {
		t.Fatal("granted without room")
	}
	m.mem.AvailableMB = 10 * 1024
	c.Add(time.Second)
	if r := acquire(t, m, agent("hog"), "docker-build", 1); r.Granted || !strings.Contains(r.Reason, "may hold at most") {
		t.Fatalf("a session over its share grew while another waited: %+v", r)
	}
	if r := acquire(t, m, agent("other"), "cluster", 2); !r.Granted {
		t.Fatalf("the waiter was refused: %s", r.Reason)
	}
}

// ---- reclamation ----

func TestReclaimPressesTheLeastUsed(t *testing.T) {
	m, c := newTest(t, 1.5)
	m.cfg.DryRun = false
	now := c.Now()
	since := now.Add(-time.Minute)
	orphan := addRes(m, "compose:old", model.RKContainers, 800, "")
	orphan.OrphanSince = &since
	mk := func(id, sess string, mb float64, idle time.Duration, verified bool) {
		m.st.Leases[id] = &model.Lease{ID: id, Session: sess, Kind: "cluster", MemGB: mb / 1024, TTL: 120,
			Verified: verified, LastUsed: now.Add(-idle), Expires: now.Add(time.Hour)}
		addRes(m, "kind:"+id, model.RKCluster, mb, id)
	}
	mk("busy", "s1", 2000, time.Minute, true)
	mk("idle20", "s2", 2000, 20*time.Minute, true)
	mk("idle8", "s3", 2000, 8*time.Minute, true)
	mk("unver", "s4", 1000, 2*time.Minute, false)
	mk("vip", "s5", 4000, time.Hour, true)
	m.touchSession("s5", "claude", "").Priority = 1

	if r := acquire(t, m, agent("new"), "cluster", 3); r.Granted {
		t.Fatal("granted without room")
	}
	if m.st.Leases["busy"].IdleAsked != nil {
		t.Error("a lease used a minute ago was pressed")
	}
	if m.st.Leases["vip"].IdleAsked != nil {
		t.Error("an urgent session was pressed for a normal request")
	}
	if m.st.Leases["unver"].IdleAsked == nil || m.st.Leases["idle20"].IdleAsked == nil {
		t.Error("unverified and longest-idle leases were not pressed")
	}
	if m.st.Leases["idle8"].IdleAsked != nil {
		t.Error("pressed more holders than needed (0.8 + 1.0 + 2.0 GB covers 3 GB)")
	}
	if !orphan.Urgent {
		t.Error("the orphan was not marked for a direct stop")
	}
	// No answer by the deadline releases it; using it keeps it.
	deadline := now.Add(minutes(m.cfg.PressureGraceMinutes))
	m.st.Touches["kind:unver"] = map[string]time.Time{"s4": now.Add(time.Minute)}
	m.checkLeases(deadline.Add(time.Second))
	if m.st.Leases["idle20"] != nil {
		t.Error("a pressed lease with no answer was not released at the deadline")
	}
	if m.st.Leases["unver"] == nil || m.st.Leases["busy"] == nil {
		t.Error("a used or unpressed lease was released")
	}
}

// ---- claims and evidence ----

func TestClaimNeedsEvidence(t *testing.T) {
	m, c := newTest(t, 8)
	addRes(m, "kind:obx", model.RKCluster, 1600, "")
	claim := func(caller api.Caller) (*api.ClaimResp, error) {
		out, _, err := m.claim(caller, api.Req{ResourceID: "kind:obx", Purpose: "tests"})
		if err != nil {
			return nil, err
		}
		return out.(*api.ClaimResp), nil
	}
	r, _ := claim(agent("s1"))
	if r.Verified || r.Lease.TTL != m.cfg.UnverifiedTTLMinutes {
		t.Fatalf("claim without evidence: %+v", r)
	}
	if _, err := claim(agent("s2")); err == nil {
		t.Fatal("a second session took a held resource")
	}
	m.dropLease(r.Lease, "test")
	m.st.Touches["kind:obx"] = map[string]time.Time{"s2": c.Now().Add(-5 * time.Minute)}
	r, _ = claim(agent("s2"))
	if !r.Verified || r.Lease.TTL != m.cfg.DefaultTTLMinutes {
		t.Fatalf("claim after recent use: %+v", r)
	}
	m.dropLease(r.Lease, "test")
	m.st.Touches["kind:obx"]["s3"] = c.Now().Add(-3 * time.Hour)
	if r, _ = claim(agent("s3")); r.Verified {
		t.Fatal("stale use verified a claim")
	}
	m.dropLease(r.Lease, "test")
	if r, _ = claim(terminal); !r.Verified {
		t.Fatal("a person's claim was not verified")
	}
	m.dropLease(r.Lease, "test")
	if _, err := claim(api.Caller{Agent: "detached", Session: "detached"}); err == nil {
		t.Fatal("a detached process claimed something")
	}
}

func TestIdleLeaseIsAskedThenReleased(t *testing.T) {
	m, c := newTest(t, 8)
	now := c.Now()
	m.st.Leases["L1"] = &model.Lease{ID: "L1", Session: "s1", Kind: "cluster", TTL: 120, Created: now.Add(-2 * time.Hour),
		Expires: now.Add(time.Hour), LastUsed: now.Add(-45 * time.Minute)}
	addRes(m, "kind:obx", model.RKCluster, 1600, "L1")
	m.checkLeases(now)
	if q := m.st.Questions["idle:L1"]; q == nil || q.For != "s1" {
		t.Fatal("idle holder was not asked")
	}
	out, _, _ := m.inbox(agent("other"), api.Req{})
	if len(out.(*api.InboxResp).Questions) != 0 {
		t.Fatal("idle question leaked to another session")
	}
	m.st.Touches["kind:obx"] = map[string]time.Time{"s1": now}
	m.checkLeases(now)
	if m.st.Questions["idle:L1"] != nil {
		t.Fatal("use did not clear the idle question")
	}
	m.checkLeases(now.Add(45 * time.Minute))
	m.checkLeases(now.Add(45*time.Minute + minutes(m.cfg.GraceMinutes+1)))
	if m.st.Leases["L1"] != nil || m.st.Resources["kind:obx"].LeaseID != "" {
		t.Fatal("idle lease with no reply was not released")
	}
	if m.st.Learned["cluster"] != 1600 {
		t.Fatalf("peak not learned: %v", m.st.Learned)
	}
}

func TestActivityKeepsLeaseAlive(t *testing.T) {
	m, c := newTest(t, 8)
	now := c.Now()
	m.st.Leases["L1"] = &model.Lease{ID: "L1", Session: "s1", Kind: "server", TTL: 120, Expires: now.Add(time.Minute),
		LastUsed: now.Add(-2 * time.Hour)}
	r := addRes(m, "srv:42", model.RKServer, 100, "L1")
	r.LastActive = now.Add(-time.Minute)
	m.checkLeases(now)
	if m.st.Questions["idle:L1"] != nil || m.st.Leases["L1"].Expires.Before(now.Add(100*time.Minute)) {
		t.Fatal("activity did not keep the lease alive")
	}
}

func TestDeclaredEndTime(t *testing.T) {
	m, c := newTest(t, 8)
	out, _, _ := m.acquire(agent("s"), api.Req{Kind: "cluster", Purpose: "demo", UntilMinutes: 20})
	l := out.(*api.AcquireResp).Lease
	addRes(m, "kind:demo", model.RKCluster, 1000, l.ID)
	c.Add(10 * time.Minute)
	m.st.Touches["kind:demo"] = map[string]time.Time{"s": c.Now()}
	m.checkLeases(c.Now())
	if l.IdleAsked != nil {
		t.Fatal("asked before the declared end")
	}
	c.Add(11 * time.Minute)
	m.st.Touches["kind:demo"]["s"] = c.Now() // still in use: the end time is held anyway
	m.checkLeases(c.Now())
	if l.IdleAsked == nil || !strings.Contains(m.st.Questions["idle:"+l.ID].Text, "until") {
		t.Fatal("the holder was not asked at its declared end")
	}
	c.Add(minutes(m.cfg.GraceMinutes) + time.Second)
	m.checkLeases(c.Now())
	if m.st.Leases[l.ID] != nil {
		t.Fatal("the lease outlived its declared end without being extended")
	}
}

// ---- attribution ----

func TestAttribution(t *testing.T) {
	m, c := newTest(t, 8)
	m.firstDone = true
	m.agentSessions[100] = "sessA"
	procs := map[int]*sys.Proc{
		100: {PID: 100, PPID: 1, Exe: "claude", Args: "claude"},
		101: {PID: 101, PPID: 100, Exe: "/bin/zsh", Args: "zsh"},
		102: {PID: 102, PPID: 101, Exe: "node", Args: "node server.js"},
		10:  {PID: 10, PPID: 1, Exe: "/Applications/Warp.app/Contents/MacOS/stable", Path: "/Applications/Warp.app/Contents/MacOS/stable", Args: "stable"},
		11:  {PID: 11, PPID: 10, Exe: "-zsh", Args: "-zsh"},
		12:  {PID: 12, PPID: 11, Exe: "node", Args: "node dev.js"},
	}
	now := c.Now()
	srv := &model.Resource{ID: "srv:102", Kind: model.RKServer, PID: 102}
	m.attribute(srv, procs, now, true)
	if l := m.st.Leases[srv.LeaseID]; l == nil || l.Session != "sessA" {
		t.Fatal("a server was not attributed to the agent that started it")
	}
	mine := &model.Resource{ID: "srv:12", Kind: model.RKServer, PID: 12}
	m.attribute(mine, procs, now, false) // even when found after a restart
	if l := m.st.Leases[mine.LeaseID]; l == nil || l.Session != "you" {
		t.Fatal("a server you started in your terminal was not recorded as yours")
	}
	// Docker events win over everything for containers.
	m.dockerOrigin["compose:shop"] = origin{session: "sessB", at: now}
	shop := &model.Resource{ID: "compose:shop", Kind: model.RKContainers}
	m.attribute(shop, procs, now, true)
	if l := m.st.Leases[shop.LeaseID]; l == nil || l.Session != "sessB" {
		t.Fatal("a container was not attributed from the Docker event")
	}
	// Command text, for something just seen.
	m.recent["sessC"] = []cmdRecord{{Time: now, Cmd: "kind create cluster --name obx"}}
	cl := &model.Resource{ID: "kind:obx", Kind: model.RKCluster, Containers: []string{"obx-control-plane"}}
	m.attribute(cl, procs, now, true)
	if l := m.st.Leases[cl.LeaseID]; l == nil || l.Session != "sessC" {
		t.Fatal("a cluster was not attributed from the command that named it")
	}
	// No evidence and an agent recently active: leave it unbound rather than guess.
	m.hookSeen["claude"] = now
	unk := &model.Resource{ID: "compose:unknown", Kind: model.RKContainers, Containers: []string{"unknown-1"}}
	m.attribute(unk, procs, now, true)
	if unk.LeaseID != "" {
		t.Fatal("bound something with no evidence")
	}
}

func TestOriginFromProcs(t *testing.T) {
	procs := map[int]*sys.Proc{
		100: {PID: 100, PPID: 1, Exe: "claude", Args: "claude"},
		101: {PID: 101, PPID: 100, Exe: "/usr/local/bin/docker", Args: "docker compose -p shop up"},
		200: {PID: 200, PPID: 1, Exe: "codex", Args: "codex"},
		201: {PID: 201, PPID: 200, Exe: "/usr/local/bin/docker", Args: "docker ps"},
	}
	if got := originFromProcs(procs, []string{"shop"}); got != "pid:claude:100" {
		t.Fatalf("the client naming the project was not chosen: %q", got)
	}
	if got := originFromProcs(procs, []string{"other"}); got != "" {
		t.Fatalf("two sessions ran docker and neither named it: should be ambiguous, got %q", got)
	}
}

// ---- learning and declarations ----

func TestEstimatesLearnPerProjectFromRealRuns(t *testing.T) {
	m, _ := newTest(t, 8)
	m.dropLease(&model.Lease{ID: "a", Kind: "cluster", PeakMB: 0.4}, "unused")
	m.dropLease(&model.Lease{ID: "b", Kind: "cluster", PeakMB: 900}, "no resource ever bound")
	if _, ok := m.st.Learned["cluster"]; ok {
		t.Fatal("learned from leases that never ran a cluster")
	}
	m.dropLease(&model.Lease{ID: "c", Kind: "cargo-build", PeakMB: 6000, Cwd: "/code/bigrepo"}, "done")
	m.dropLease(&model.Lease{ID: "d", Kind: "cargo-build", PeakMB: 1000, Cwd: "/code/tiny"}, "done")
	if big, tiny := m.estimateGB("cargo-build", "/code/bigrepo"), m.estimateGB("cargo-build", "/code/tiny"); big < 6 || tiny > 2 {
		t.Fatalf("per-project estimates not used: big %.1f GB, tiny %.1f GB", big, tiny)
	}
	if other := m.estimateGB("cargo-build", "/code/new"); other < 1 {
		t.Fatalf("an unknown project should use the kind's estimate: %.1f", other)
	}
}

// What an agent declares is what it is charged, even below what AgentRM learned; only a
// session caught overrunning loses that for an hour.
func TestDeclaredFigureIsChargedUntilOverrun(t *testing.T) {
	m, c := newTest(t, 8)
	m.touchSession("s", "claude", "")
	if got := m.effectiveGB("cargo-build", "", 0.5, "s", c.Now()); got != 0.5 {
		t.Fatalf("no history: %.2f", got)
	}
	m.st.Learned["cargo-build"] = 2048
	if got := m.effectiveGB("cargo-build", "", 0.5, "s", c.Now()); got != 0.5 {
		t.Fatalf("declared below the learned mean was not charged as declared: %.2f", got)
	}
	delete(m.st.Learned, "cargo-build")
	l := &model.Lease{ID: "L", Session: "s", Kind: "cargo-build", MemGB: 0.5, Declared: true, PeakMB: 2500}
	m.checkOverrun(l, c.Now())
	if !l.Overran || m.effectiveGB("cargo-build", "", 0.5, "s", c.Now()) != 2.5 {
		t.Fatal("a 5x overrun did not cost the right to declare")
	}
}

func TestHeavyWorkIsInferredFromTheProcessTree(t *testing.T) {
	m, c := newTest(t, 8)
	m.agentSessions[100] = "s"
	tree := func(mb float64) map[int]*sys.Proc {
		return map[int]*sys.Proc{
			100: {PID: 100, PPID: 1, Exe: "claude", Args: "claude", RSSMB: 300},
			101: {PID: 101, PPID: 100, Exe: "make", Args: "make all", RSSMB: mb},
		}
	}
	m.updateFootprints(tree(50), c.Now()) // baseline
	m.recent["s"] = []cmdRecord{{Time: c.Now(), Cmd: "make all"}}
	c.Add(3 * time.Second)
	m.updateFootprints(tree(2500), c.Now())
	l := m.findLease("s", model.KHeavy)
	if l == nil || l.Cmd != "make all" {
		t.Fatalf("2.4 GB of unexplained growth did not become a lease: %+v", l)
	}
	if p := m.pendingMB(); p > 100 {
		t.Fatalf("the inferred lease double-counts its memory: %.0f MB pending", p)
	}
	c.Add(3 * time.Second)
	m.updateFootprints(tree(60), c.Now())
	if m.findLease("s", model.KHeavy) != nil {
		t.Fatal("the inferred lease outlived the work")
	}
	if m.st.Learned[model.KHeavy] < 2000 {
		t.Fatalf("the heavy run was not learned: %v", m.st.Learned)
	}
}

// ---- permissions and approvals ----

func TestPermissionsAndApprovals(t *testing.T) {
	m, c := newTest(t, 8)
	fakeEffects(t, nil)
	m.cfg.RequireAppApproval = true
	m.st.Leases["L1"] = &model.Lease{ID: "L1", Session: "owner", Kind: "cluster", Expires: c.At(time.Hour), LastUsed: c.Now()}
	addRes(m, "kind:x", model.RKCluster, 100, "L1")
	on := true
	if _, _, err := m.release(agent("intruder"), api.Req{LeaseID: "L1"}); err == nil {
		t.Error("an agent released another session's lease")
	}
	if _, _, err := m.stopNow(agent("intruder"), api.Req{ResourceID: "kind:x"}); err == nil {
		t.Error("an agent stopped another session's resource")
	}
	if _, _, err := m.pin(agent("intruder"), api.Req{ResourceID: "kind:x", Pinned: &on}); err == nil {
		t.Error("an agent pinned something")
	}
	f := false
	if _, _, err := m.setConfig(agent("intruder"), api.Req{DryRun: &f}); err == nil {
		t.Error("an agent changed the settings")
	}
	// From a terminal: held for approval in the app.
	var ea *errApproval
	if _, _, err := m.setConfig(terminal, api.Req{DryRun: &f}); !errors.As(err, &ea) {
		t.Fatalf("a terminal changed settings without approval: %v", err)
	}
	// Through Handle the approval is created; the app approves it; it runs.
	m.mu.Lock()
	a := m.newApproval("/config", api.Req{DryRun: &f}, terminal, ea.what)
	m.mu.Unlock()
	if _, _, err := m.approve(terminal, api.Req{ApprovalID: a.ID, Approve: &on}); err == nil {
		t.Error("a terminal answered an approval")
	}
	if _, _, err := m.approve(you, api.Req{ApprovalID: a.ID, Approve: &on}); err != nil || m.cfg.DryRun {
		t.Fatalf("the approved change did not run: %v, dry_run=%v", err, m.cfg.DryRun)
	}
	// Denied, and expired, requests do nothing.
	b := m.newApproval("/pin", api.Req{ResourceID: "kind:x", Pinned: &on}, terminal, "pin")
	m.approve(you, api.Req{ApprovalID: b.ID, Approve: &f})
	cc := m.newApproval("/pin", api.Req{ResourceID: "kind:x", Pinned: &on}, terminal, "pin")
	c.Add(approvalTTL + time.Second)
	m.approve(you, api.Req{ApprovalID: cc.ID, Approve: &on})
	if m.st.Pinned["kind:x"] || b.State != "denied" || cc.State != "expired" {
		t.Fatalf("denied/expired approvals acted: pinned=%v %s %s", m.st.Pinned["kind:x"], b.State, cc.State)
	}
	// The app acts at once.
	if _, _, err := m.pin(you, api.Req{ResourceID: "kind:x", Pinned: &on}); err != nil || !m.st.Pinned["kind:x"] {
		t.Fatal("the app could not pin")
	}
}

// ---- orphans, pausing and stopping ----

func TestOrphanAskPauseStop(t *testing.T) {
	m, c := newTest(t, 8)
	got := fakeEffects(t, nil)
	m.cfg.DryRun = false
	r := addRes(m, "srv:7", model.RKServer, 300, "")
	r.Ports = []string{"3000"}
	run := func() {
		effs := m.checkOrphans(c.Now())
		m.applyEffects(RunEffects(effs))
	}
	run()
	if m.st.Questions["srv:7"] == nil || !strings.Contains(m.st.Questions["srv:7"].Text, "paused") {
		t.Fatal("not asked, or not told it would be paused first")
	}
	c.Add(minutes(m.cfg.GraceMinutes) + time.Second)
	run()
	if r.PausedAt == nil || len(*got) != 1 || (*got)[0].Op != "pause" {
		t.Fatalf("not paused at the grace deadline: %+v", *got)
	}
	c.Add(minutes(m.cfg.PauseMinutes) / 2)
	run()
	if len(*got) != 1 {
		t.Fatal("stopped before the pause period was over")
	}
	c.Add(minutes(m.cfg.PauseMinutes))
	run()
	if len(*got) != 2 || (*got)[1].Op != "stop" || m.st.Resources["srv:7"] != nil {
		t.Fatalf("not stopped after the pause: %+v", *got)
	}
	if len(m.st.Stopped) != 1 || len(m.st.Decisions) != 2 {
		t.Fatalf("stop not recorded for restore/review: stopped=%d decisions=%d", len(m.st.Stopped), len(m.st.Decisions))
	}
}

func TestBlockedRequestSkipsThePauseAndVMsAreNeverStopped(t *testing.T) {
	m, c := newTest(t, 1.2)
	got := fakeEffects(t, nil)
	m.cfg.DryRun = false
	since := c.Now().Add(-time.Minute)
	r := addRes(m, "srv:9", model.RKServer, 900, "")
	r.OrphanSince = &since
	vm := addRes(m, "vm:lima-dev", model.RKVM, 2000, "")
	vm.OrphanSince = &since
	acquire(t, m, agent("waiting"), "server", 0.6)
	c.Add(minutes(m.cfg.PressureGraceMinutes) + time.Second)
	m.applyEffects(RunEffects(m.checkOrphans(c.Now())))
	ops := map[string]string{}
	for _, e := range *got {
		ops[e.Res.ID] = e.Op
	}
	if ops["srv:9"] != "stop" {
		t.Fatalf("with a request waiting the orphan should be stopped, not frozen: %v", ops)
	}
	if ops["vm:lima-dev"] == "stop" {
		t.Fatal("a VM was stopped automatically")
	}
}

func TestDryRunOnlyRecordsDecisions(t *testing.T) {
	m, c := newTest(t, 8)
	got := fakeEffects(t, nil)
	addRes(m, "srv:7", model.RKServer, 300, "")
	for i := 0; i < 5; i++ {
		m.checkOrphans(c.Now())
		c.Add(minutes(m.cfg.GraceMinutes))
	}
	if len(*got) != 0 {
		t.Fatal("dry run acted")
	}
	if len(m.st.Decisions) != 1 || !m.st.Decisions[0].DryRun {
		t.Fatalf("dry run should record the would-pause decision once: %d", len(m.st.Decisions))
	}
	out, _, _ := m.review(you, api.Req{DecisionID: m.st.Decisions[0].ID, Verdict: "wrong"})
	if r := out.(*api.ReviewResp); r.Reviewed != 1 || r.FalseRate != 1 {
		t.Fatalf("review not counted: %+v", r)
	}
	if _, _, err := m.review(agent("a"), api.Req{DecisionID: m.st.Decisions[0].ID, Verdict: "right"}); err == nil {
		t.Fatal("an agent marked a decision")
	}
}

func TestThawOnUseClaimAndPin(t *testing.T) {
	m, c := newTest(t, 8)
	got := fakeEffects(t, nil)
	paused := c.Now()
	r := addRes(m, "srv:7", model.RKServer, 300, "")
	r.Ports, r.PausedAt = []string{"3000"}, &paused
	out, err := m.Handle("/touch", 0, api.Req{Cmd: "curl -s localhost:3000/health"})
	_ = out
	if err == nil {
		t.Log("detached caller (pid 0) does not touch; checking with a resolved caller instead")
	}
	m.mu.Lock()
	resp, effs, _ := m.touchCmd(agent("s"), api.Req{Cmd: "curl -s localhost:3000/health"})
	m.mu.Unlock()
	m.applyEffects(RunEffects(effs))
	if len(resp.(*api.TouchResp).Thawed) != 1 || r.PausedAt != nil || (*got)[0].Op != "resume" {
		t.Fatalf("a command using a paused server did not thaw it: %+v", *got)
	}
}

func TestRestoreGoesThroughAdmission(t *testing.T) {
	m, c := newTest(t, 1.5)
	got := fakeEffects(t, nil)
	m.st.Stopped = []*model.Stopped{{Resource: model.Resource{ID: "kind:big", Kind: model.RKCluster, MemMB: 3000}, StoppedAt: c.Now()}}
	out, _, _ := m.restore(agent("s"), api.Req{ResourceID: "kind:big"})
	if r := out.(*api.AcquireResp); r.Granted || len(*got) != 0 {
		t.Fatal("restored 3 GB without room")
	}
	m.mem.AvailableMB = 8 * 1024
	m.waiters = nil
	out, effs, _ := m.restore(agent("s"), api.Req{ResourceID: "kind:big"})
	m.applyEffects(RunEffects(effs))
	if r := out.(*api.AcquireResp); !r.Granted || len(*got) != 1 || len(m.st.Stopped) != 0 {
		t.Fatal("restore did not run when there was room")
	}
	if m.dockerOrigin["kind:big"].session != "s" {
		t.Fatal("the restored cluster will not be bound to whoever restored it")
	}
}

// ---- sweep plumbing ----

func TestFailedSourceKeepsItsResources(t *testing.T) {
	m, c := newTest(t, 8)
	m.st.Leases["L"] = &model.Lease{ID: "L", Session: "s", Kind: "cluster", Expires: c.At(time.Hour), LastUsed: c.Now()}
	addRes(m, "kind:obx", model.RKCluster, 1600, "L")
	m.merge(discover.Result{Failed: map[string]error{"docker": errors.New("docker timed out")}}, nil, nil, c.Now())
	if m.st.Resources["kind:obx"] == nil || m.st.Resources["kind:obx"].LeaseID != "L" {
		t.Fatal("a docker timeout dropped the cluster and its lease")
	}
	m.merge(discover.Result{Failed: map[string]error{}}, nil, nil, c.Now())
	if m.st.Resources["kind:obx"] != nil {
		t.Fatal("a cluster docker says is gone was kept")
	}
}

// 2026-11-17: Docker hung for five hours. Every sweep kept its containers as last seen, reclaim
// counted on stopping them for a blocked request, each stop timed out, and the request waited
// on memory that never came. What a source cannot see is now neither acted on nor counted on.
func TestUnseenResourcesAreNotCountedOnOrActedOn(t *testing.T) {
	m, c := newTest(t, 1.0)
	got := fakeEffects(t, nil)
	m.cfg.DryRun = false
	now := c.Now()
	since := now.Add(-time.Hour)
	ghost := addRes(m, "compose:openbox-local", model.RKContainers, 1100, "")
	ghost.OrphanSince = &since
	m.st.Leases["idle"] = &model.Lease{ID: "idle", Session: "s2", Kind: "cluster", MemGB: 2, TTL: 120,
		Verified: true, LastUsed: now.Add(-30 * time.Minute), Expires: now.Add(time.Hour)}
	addRes(m, "kind:idle", model.RKCluster, 2048, "idle")
	live := &model.Resource{ID: "kind:idle", Kind: model.RKCluster, Name: "idle", MemMB: 2048, Source: "procs"}

	m.merge(discover.Result{Resources: []*model.Resource{live}, Failed: map[string]error{"docker": discover.ErrEngineHung}}, nil, nil, now)
	if ghost = m.st.Resources["compose:openbox-local"]; ghost == nil || ghost.Unseen == nil {
		t.Fatal("a resource on a hung source should be kept and marked unseen")
	}

	acquire(t, m, agent("waiting"), "heavy", 2)
	if ghost.Urgent {
		t.Error("reclaim counted on stopping something on a hung engine")
	}
	if m.st.Leases["idle"].IdleAsked == nil {
		t.Error("with nothing stoppable, the idle lease should have been pressed instead")
	}
	c.Add(time.Hour)
	m.applyEffects(RunEffects(m.checkOrphans(c.Now())))
	for _, e := range *got {
		if e.Res.ID == ghost.ID {
			t.Fatalf("acted on an unseen resource: %s", e.Op)
		}
	}

	// The engine answers again and the stack is still there: live once more.
	fresh := &model.Resource{ID: "compose:openbox-local", Kind: model.RKContainers, Name: "x", MemMB: 1100, Source: "docker"}
	m.merge(discover.Result{Resources: []*model.Resource{fresh}, Failed: map[string]error{}}, nil, nil, c.Now())
	if r := m.st.Resources["compose:openbox-local"]; r == nil || r.Unseen != nil {
		t.Fatal("a resource seen again should no longer be marked unseen")
	}
	// The engine is down: gone, at once.
	m.merge(discover.Result{Failed: map[string]error{}}, nil, nil, c.Now())
	if m.st.Resources["compose:openbox-local"] != nil {
		t.Fatal("a stack on a stopped engine was kept")
	}
}

func TestEffectsRunOutsideTheLock(t *testing.T) {
	m, c := newTest(t, 8)
	old := Executor
	defer func() { Executor = old }()
	locked := false
	Executor = func(e Effect) error {
		if m.mu.TryLock() {
			m.mu.Unlock()
		} else {
			locked = true
		}
		return nil
	}
	paused := c.Now()
	r := addRes(m, "srv:7", model.RKServer, 300, "")
	r.PausedAt = &paused
	if _, err := m.Handle("/resume", 0, api.Req{ResourceID: "srv:7"}); err != nil {
		t.Fatal(err)
	}
	if locked {
		t.Fatal("an effect ran while the manager lock was held (D1)")
	}
}

func TestResolveIdentity(t *testing.T) {
	m, _ := newTest(t, 8)
	req := api.Req{Session: "someone-else"}
	c := m.resolve(facts{pid: 5, agentPID: 100, agent: "claude"}, &req)
	if c.Session != "claude-100" || req.Session != "claude-100" {
		t.Fatalf("a claimed session was believed: %+v", c)
	}
	// A hook tells us the real session; what was recorded under the placeholder moves to it.
	m.st.Leases["L"] = &model.Lease{ID: "L", Session: "claude-100"}
	req = api.Req{Session: "real-id", Via: "hook"}
	if c = m.resolve(facts{pid: 6, agentPID: 100, agent: "claude"}, &req); c.Session != "real-id" || m.st.Leases["L"].Session != "real-id" {
		t.Fatalf("hook session not learned: %+v", c)
	}
	if c = m.resolve(facts{pid: 7, gui: true, app: "Warp.app"}, &api.Req{}); !c.Human || c.ViaApp {
		t.Fatalf("a terminal caller: %+v", c)
	}
	if c = m.resolve(facts{pid: 8, gui: true, viaApp: true}, &api.Req{}); !c.Human || !c.ViaApp {
		t.Fatalf("the app: %+v", c)
	}
	if c = m.resolve(facts{pid: 9}, &api.Req{Session: "you"}); c.Human || c.Session != "detached" {
		t.Fatalf("a detached caller got an identity: %+v", c)
	}
}

// T201: nohup bash -c 'sleep 2; exec -a /tmp/AgentRM.app/Contents/x agentrm config ...' &
// leaves a process hanging off launchd that names itself the app. It is detached: not you,
// and not the app.
func TestSpoofedAppCallerIsDetached(t *testing.T) {
	m, _ := newTest(t, 8)
	t.Cleanup(func() { sys.Procs.Set(nil) })
	const term = "/System/Applications/Utilities/Terminal.app/Contents/MacOS/Terminal"
	sys.Procs.Set(map[int]*sys.Proc{
		1:     {PID: 1, Path: "/sbin/launchd"},
		99901: {PID: 99901, PPID: 1, Exe: "/tmp/AgentRM.app/Contents/x", Args: "/tmp/AgentRM.app/Contents/x config --dry-run=false", Path: "/usr/local/bin/agentrm"},
		99902: {PID: 99902, PPID: 1, Exe: "/Applications/AgentRM.app/Contents/Helpers/agentrm", Path: "/tmp/AgentRM.app/Contents/Helpers/agentrm"},
		99903: {PID: 99903, PPID: 1, Exe: term, Path: term},
	})
	for _, pid := range []int{99901, 99902, 99903} {
		f := gatherFacts(pid)
		m.mu.Lock()
		c := m.resolve(f, &api.Req{Session: "you"})
		m.mu.Unlock()
		if f.gui || f.viaApp || c.Human || c.ViaApp || c.Session != "detached" {
			t.Errorf("pid %d: facts %+v, caller %+v", pid, f, c)
		}
	}
}

// T201: a hooked agent that names another live agent's session (they are in agentrm status)
// keeps its own; once that agent has exited, the session may be resumed.
func TestHookCannotTakeOverALiveSession(t *testing.T) {
	m, c := newTest(t, 8)
	start := c.Now().Add(-time.Hour)
	victim := liveSession(m, "victim", start) // agent pid 500
	m.agentSessions[500] = "victim"
	procs := map[int]*sys.Proc{
		1:   {PID: 1},
		500: {PID: 500, PPID: 1, Exe: "claude", Args: "claude", Start: start},
		600: {PID: 600, PPID: 1, Exe: "claude", Args: "claude", Start: start},
	}
	hook := func(procs map[int]*sys.Proc) api.Caller {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.resolve(facts{pid: 601, agentPID: 600, agent: "claude", procs: procs}, &api.Req{Via: "hook", Session: "victim"})
	}
	for _, table := range []map[int]*sys.Proc{procs, nil} {
		if got := hook(table); got.Session != "claude-600" || m.agentSessions[600] != "" || m.agentSessions[500] != "victim" || victim.EndedAt != nil {
			t.Fatalf("another agent's session was taken over: %+v, bindings %v", got, m.agentSessions)
		}
	}
	tamper := 0
	for _, e := range m.events {
		if e.Kind == "TAMPER" && strings.Contains(e.Text, "victim") {
			tamper++
		}
	}
	if tamper != 1 {
		t.Fatalf("the attempt was reported %d times, want once", tamper)
	}
	// A PID reused by another process is not the session's agent.
	reused := map[int]*sys.Proc{1: procs[1], 600: procs[600], 500: {PID: 500, PPID: 1, Exe: "claude", Args: "claude", Start: start.Add(time.Minute)}}
	if got := hook(reused); got.Session != "victim" || m.agentSessions[600] != "victim" {
		t.Fatalf("a session whose agent is gone was not resumed: %+v", got)
	}
}

func TestStatusEncodes(t *testing.T) {
	m, _ := newTest(t, 8)
	acquire(t, m, agent("a"), "cluster", 1)
	out, _, _ := m.status(you, api.Req{})
	b, err := json.Marshal(out)
	if err != nil || !strings.Contains(string(b), `"headroom_mb"`) || !strings.Contains(string(b), `"free_percent"`) {
		t.Fatalf("status does not encode as the app expects: %v", err)
	}
}

// The app polls /status every few seconds while sweeps change the state. The response used to
// point into the live maps and was encoded after the lock was released: "concurrent map
// iteration and map write", a fatal error that killed the daemon. Run with -race.
func TestStatusIsEncodedFromACopy(t *testing.T) {
	m, _ := newTest(t, 8)
	acquire(t, m, agent("a"), "cluster", 1)
	h := m.routes()["/status"]
	f := facts{pid: 1, gui: true, app: "AgentRM.app", viaApp: true}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 300; i++ {
			m.mu.Lock()
			id := fmt.Sprintf("srv:%d", i)
			m.st.Resources[id] = &model.Resource{ID: id, Kind: model.RKServer, Source: "procs"}
			delete(m.st.Resources, fmt.Sprintf("srv:%d", i-1))
			m.mu.Unlock()
		}
	}()
	for i := 0; i < 100; i++ {
		out, err := m.handleFacts(context.Background(), "/status", h, f, api.Req{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := json.Marshal(out); err != nil {
			t.Fatal(err)
		}
	}
	<-done
}

// One test against a real process tree, to check the effects do what they say: `sh -c` with a
// child, like `npm run dev` with vite under it. The child is paused and stopped with its parent.
func TestPauseAndStopARealProcess(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "sleep 60; :") // the `; :` keeps sh from exec'ing sleep
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	defer cmd.Process.Kill()
	pid := cmd.Process.Pid
	child := 0
	for i := 0; i < 100 && child == 0; i++ {
		for _, p := range sys.ListProcs() {
			if p.PPID == pid {
				child = p.PID
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if child == 0 {
		t.Fatal("sh started no child")
	}
	defer syscall.Kill(child, syscall.SIGKILL)
	state := func(pid int) string {
		out, _ := exec.Command("/bin/ps", "-o", "state=", "-p", fmt.Sprint(pid)).Output()
		return strings.TrimSpace(string(out))
	}
	frozen := func(pid int) bool { return strings.HasPrefix(state(pid), "T") }
	p := sys.QuickProc(pid)
	if p == nil || p.Start.IsZero() {
		t.Fatalf("no start time for a live process: %+v", p)
	}
	r := model.Resource{ID: "srv:test", Kind: model.RKServer, PID: pid, ProcStart: p.Start}

	// The PID is now someone else's (a start that does not match): nothing is signalled.
	other := r
	other.ProcStart = p.Start.Add(-time.Hour)
	if err := Executor(Effect{Op: "pause", Res: other}); err == nil || frozen(pid) {
		t.Fatalf("paused a process whose start does not match: %v %q", err, state(pid))
	}
	if err := Executor(Effect{Op: "stop", Res: other}); err == nil || state(pid) == "" {
		t.Fatalf("stopped a process whose start does not match: %v", err)
	}
	unknown := r
	unknown.ProcStart = time.Time{}
	if err := Executor(Effect{Op: "stop", Res: unknown}); err == nil {
		t.Fatal("signalled a PID with no recorded start")
	}

	if err := Executor(Effect{Op: "pause", Res: r}); err != nil || !frozen(pid) || !frozen(child) {
		t.Fatalf("not frozen: %v parent %q child %q", err, state(pid), state(child))
	}
	if err := Executor(Effect{Op: "resume", Res: r}); err != nil || frozen(pid) || frozen(child) {
		t.Fatalf("not thawed: %v parent %q child %q", err, state(pid), state(child))
	}
	_ = Executor(Effect{Op: "pause", Res: r})
	now := time.Now()
	r.PausedAt = &now
	if err := Executor(Effect{Op: "stop", Res: r}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(3 * time.Second):
		t.Fatal("a frozen process was not stopped (TERM must be followed by CONT)")
	}
	for i := 0; i < 150 && syscall.Kill(child, 0) == nil; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if syscall.Kill(child, 0) == nil {
		t.Fatalf("the child outlived its stopped parent: %q", state(child))
	}
	// Resuming what is gone is not an error: there is nothing left frozen.
	if err := Executor(Effect{Op: "resume", Res: r}); err != nil {
		t.Fatal(err)
	}
}

// A restore that fails keeps its record, so it can be tried again, and gives its lease back.
func TestFailedRestoreKeepsItsRecord(t *testing.T) {
	m, c := newTest(t, 8)
	fakeEffects(t, map[string]bool{"restore": true})
	st := &model.Stopped{Resource: model.Resource{ID: "left:9-1", Kind: model.RKLeftover, MemMB: 200}, StoppedAt: c.Now()}
	m.st.Stopped = []*model.Stopped{st}
	out, effs, err := m.restore(agent("s"), api.Req{ResourceID: "left:9-1"})
	if err != nil || !out.(*api.AcquireResp).Granted || len(m.st.Stopped) != 0 {
		t.Fatalf("restore was not started: %v %+v", err, out)
	}
	m.applyEffects(RunEffects(effs))
	if len(m.st.Stopped) != 1 || m.st.Stopped[0].Resource.ID != "left:9-1" {
		t.Fatalf("a failed restore lost the record of what was stopped: %+v", m.st.Stopped)
	}
	if len(m.st.Leases) != 0 {
		t.Fatalf("a failed restore kept its lease: %+v", m.st.Leases)
	}
}

// What a stop records for a restore comes with the resource, read while its process ran, and
// only for what may be started again.
func TestStopKeepsHowItWasStartedOnlyForRestartable(t *testing.T) {
	m, c := newTest(t, 8)
	start := c.Now().Add(-time.Hour)
	m.infos = newInfoCache(func(pid int) (sys.ProcInfo, bool) {
		return sys.ProcInfo{Argv: []string{"node", "server.js"}, Env: map[string]string{"PATH": "/x"}}, true
	})
	procs := map[int]*sys.Proc{
		1:   {PID: 1, Exe: "/sbin/launchd"},
		700: {PID: 700, PPID: 1, Exe: "/u/node", Args: "node server.js", Start: start},
		701: {PID: 701, PPID: 1, Exe: "/u/claude", Args: "claude -p fix", Start: start},
	}
	m.infos.refresh(procs)
	found := []*model.Resource{
		{ID: "srv:700", Kind: model.RKServer, PID: 700},
		{ID: "browser:701", Kind: model.RKBrowser, PID: 701},
	}
	m.keepRestartInfo(found, procs)
	if !found[0].ProcStart.Equal(start) || len(found[0].RestartArgv) != 2 || found[0].RestartEnv[0] != "PATH=/x" {
		t.Fatalf("a dev server did not keep how it was started: %+v", found[0])
	}
	if len(found[1].RestartArgv) != 0 || !found[1].ProcStart.Equal(start) {
		t.Fatalf("a headless browser kept a command to re-run: %+v", found[1])
	}
	// The process is gone and its PID reused by the time the stop is recorded: what the stop
	// recorded is what the resource carried, not what the new process at that PID runs.
	m.infos = newInfoCache(func(int) (sys.ProcInfo, bool) {
		return sys.ProcInfo{Argv: []string{"rm", "-rf", "x"}}, true
	})
	m.infos.refresh(map[int]*sys.Proc{700: {PID: 700, PPID: 1, Exe: "/bin/rm", Start: c.Now()}})
	m.recordStopped(*found[0], "test", "", c.Now())
	got := m.st.Stopped[0].Resource
	if strings.Join(got.RestartArgv, " ") != "node server.js" || got.Restart != "agentrm restore srv:700" {
		t.Fatalf("recorded %+v", got)
	}
	bare := model.Resource{ID: "srv:700", Kind: model.RKServer, PID: 700, ProcStart: start}
	m.recordStopped(bare, "test", "", c.Now())
	if got := m.st.Stopped[1].Resource; len(got.RestartArgv) != 0 {
		t.Fatalf("took the command of whatever has the PID now: %+v", got.RestartArgv)
	}
}

// On a clean shutdown nothing is left frozen: a daemon that is then disabled would never thaw it.
func TestCloseResumesWhatWasPaused(t *testing.T) {
	m, c := newTest(t, 8)
	got := fakeEffects(t, nil)
	r := addRes(m, "srv:5", model.RKServer, 300, "")
	t0 := c.Now()
	r.PausedAt = &t0
	addRes(m, "srv:6", model.RKServer, 300, "")
	m.Close()
	if len(*got) != 1 || (*got)[0].Op != "resume" || (*got)[0].Res.ID != "srv:5" || r.PausedAt != nil {
		t.Fatalf("shutdown left a paused process frozen: %+v", *got)
	}
}

// ---- the queue grants by itself ----

func TestQueuedRequestIsGrantedWhenItFits(t *testing.T) {
	m, c := newTest(t, 3) // 2 GB headroom
	m.touchSession("big", "claude", "")
	out, _, _ := m.acquire(agent("big"), api.Req{Kind: "cluster", MemGB: 3, Purpose: "e2e", Via: "hook"})
	if r := out.(*api.AcquireResp); r.Granted || r.Queue != 1 || !strings.Contains(r.Reason, "Do not retry in a loop") {
		t.Fatalf("not queued with instructions: %+v", r)
	}
	c.Add(time.Second)
	if r := acquire(t, m, agent("small"), "server", 1); r.Granted {
		t.Fatal("a later request jumped the queue")
	}
	// Nothing fits yet: nothing is granted.
	m.grantWaiters(c.Now(), false)
	if len(m.st.Leases) != 0 {
		t.Fatalf("granted without room: %+v", m.st.Leases)
	}
	// Memory frees up. Nobody asks again: both are granted, in order, and told.
	m.mem.AvailableMB = 6 * 1024
	c.Add(time.Minute)
	m.grantWaiters(c.Now(), false)
	l := m.findLease("big", "cluster")
	if l == nil || l.Offered == nil || l.Purpose != "e2e" || l.MemGB != 3 {
		t.Fatalf("the head of the queue was not granted what it asked for: %+v", l)
	}
	if m.findLease("small", "server") == nil || len(m.waiters) != 0 {
		t.Fatalf("the next in line was not granted: leases=%d waiters=%d", len(m.st.Leases), len(m.waiters))
	}
	in, _, _ := m.inbox(agent("big"), api.Req{})
	if q := in.(*api.InboxResp).Questions; len(q) != 1 || !strings.Contains(q[0], l.ID) {
		t.Fatalf("the session was not told about its lease: %v", q)
	}
	if in, _, _ := m.inbox(agent("small"), api.Req{}); len(in.(*api.InboxResp).Questions) != 1 {
		t.Fatal("a session is told only about its own lease, once")
	}
	// Running the command again takes the lease up; it is then an ordinary lease.
	r := acquire(t, m, agent("big"), "cluster", 3)
	if !r.Granted || !r.Reused || r.Lease.ID != l.ID || l.Offered != nil || m.st.Questions["grant:"+l.ID] != nil {
		t.Fatalf("the granted lease was not taken up: %+v", r)
	}
	c.Add(m.grantHold() + time.Minute)
	m.checkLeases(c.Now())
	if m.st.Leases[l.ID] == nil {
		t.Fatal("a lease that was taken up was dropped as unclaimed")
	}
	// The one nobody took up goes back.
	if m.findLease("small", "server") != nil {
		t.Fatal("a granted lease nobody took up was kept past its hold")
	}
}

func TestUnusedGrantGoesToTheNextInLine(t *testing.T) {
	m, c := newTest(t, 2) // 1 GB headroom
	acquire(t, m, agent("first"), "cluster", 3)
	c.Add(time.Second)
	acquire(t, m, agent("second"), "cluster", 3)
	m.mem.AvailableMB = 4.5 * 1024 // room for one of them
	m.grantWaiters(c.Now(), false)
	if m.findLease("first", "cluster") == nil || m.findLease("second", "cluster") != nil {
		t.Fatalf("exactly the first should hold a lease: %+v", m.st.Leases)
	}
	// "first" never shows up. Its hold runs out and the memory goes to "second".
	m.touchSession("second", "claude", "")
	c.Add(m.grantHold() + time.Second)
	m.st.Sessions["second"].LastSeen = c.Now()
	m.checkLeases(c.Now())
	m.grantWaiters(c.Now(), false)
	if m.findLease("first", "cluster") != nil || m.findLease("second", "cluster") == nil {
		t.Fatalf("the unused grant did not pass to the next in line: %+v", m.st.Leases)
	}
}

func TestStartingUnderAGrantTakesItUp(t *testing.T) {
	m, c := newTest(t, 2)
	acquire(t, m, agent("a"), "cluster", 3)
	m.mem.AvailableMB = 6 * 1024
	m.grantWaiters(c.Now(), false)
	l := m.findLease("a", "cluster")
	addRes(m, "kind:dev", model.RKCluster, 2000, l.ID)
	c.Add(m.grantHold() + time.Minute)
	m.checkLeases(c.Now())
	if m.st.Leases[l.ID] == nil || l.Offered != nil {
		t.Fatalf("a lease with something running under it was not taken up: %+v", l)
	}
}

func TestQueuePlaceFollowsTheSession(t *testing.T) {
	m, c := newTest(t, 2)
	m.touchSession("busy", "claude", "")
	acquire(t, m, agent("busy"), "cluster", 3)
	acquire(t, m, agent("quiet"), "cluster", 3)
	// "busy" keeps making tool calls and never asks again; "quiet" is not heard from.
	for i := 0; i < 3; i++ {
		c.Add(m.queueKeep() / 2)
		m.touchSession("busy", "claude", "")
	}
	ws := m.liveWaiters(c.Now())
	if len(ws) != 1 || ws[0].Session != "busy" {
		t.Fatalf("want only the session still talking in the queue: %+v", ws)
	}
	// A session that ends leaves the queue at once.
	m.endSession(m.st.Sessions["busy"], "it ended", c.Now())
	if len(m.liveWaiters(c.Now())) != 0 {
		t.Fatal("an ended session kept its place")
	}
}

func TestRestoreIsNotGrantedFromTheQueue(t *testing.T) {
	m, c := newTest(t, 2)
	m.admit(model.Waiter{Session: "a", Kind: "cluster", NeedMB: 3000, Retry: true}, c.Now())
	m.mem.AvailableMB = 8 * 1024
	m.grantWaiters(c.Now(), false)
	if len(m.st.Leases) != 0 || len(m.waiters) != 1 {
		t.Fatalf("a restore got a lease without being restored: leases=%d waiters=%d", len(m.st.Leases), len(m.waiters))
	}
}

func TestConnectedClientIsAnsweredOnGrant(t *testing.T) {
	m, _ := newTest(t, 2)
	c, req := agent("a"), api.Req{Kind: "cluster", MemGB: 3, Purpose: "e2e", WaitSeconds: 30}
	wait := func() (chan *api.AcquireResp, *api.AcquireResp) {
		m.mu.Lock()
		defer m.mu.Unlock()
		out, _, _ := m.acquire(c, req)
		return m.park(c.Session, req.Kind), out.(*api.AcquireResp)
	}
	ch, first := wait()
	if first.Granted || ch == nil {
		t.Fatalf("not queued: %+v", first)
	}
	type answer struct {
		out any
		err error
	}
	done := make(chan answer, 1)
	go func() {
		out, err := m.awaitGrant(context.Background(), ch, c, req)
		done <- answer{out, err}
	}()
	// A connected client keeps its place however long it waits, without the lock being held.
	m.mu.Lock()
	if ws := m.liveWaiters(m.now().Add(time.Hour)); len(ws) != 1 {
		t.Fatalf("a connected client lost its place: %+v", ws)
	}
	m.mem.AvailableMB = 6 * 1024
	m.grantWaiters(m.now(), false)
	m.mu.Unlock()
	select {
	case a := <-done:
		r, _ := a.out.(*api.AcquireResp)
		if a.err != nil || r == nil || !r.Granted || r.Lease == nil {
			t.Fatalf("the connected client was not given its lease: %+v %v", a.out, a.err)
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if l := m.st.Leases[r.Lease.ID]; l == nil || l.Offered != nil || len(m.st.Questions) != 0 || len(m.parked) != 0 {
			t.Fatalf("a delivered lease should be taken up, with nothing left over: %+v", l)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the connected client was never answered")
	}
}

func TestClientThatGoesAwayStopsWaiting(t *testing.T) {
	m, _ := newTest(t, 2)
	c, req := agent("a"), api.Req{Kind: "cluster", MemGB: 3, Purpose: "e2e", WaitSeconds: 30}
	m.mu.Lock()
	m.acquire(c, req)
	ch := m.park(c.Session, req.Kind)
	m.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.awaitGrant(ctx, ch, c, req); err == nil {
		t.Fatal("a client that went away was answered")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.parked) != 0 {
		t.Fatal("a client that went away is still counted as connected")
	}
	// Its place is then kept only as long as anyone's.
	if ws := m.liveWaiters(m.now().Add(m.queueKeep() + time.Second)); len(ws) != 0 {
		t.Fatalf("an abandoned request still holds a place: %+v", ws)
	}
}

func TestWaitTimesOutWithTheCurrentAnswer(t *testing.T) {
	m, _ := newTest(t, 2)
	c, req := agent("a"), api.Req{Kind: "cluster", MemGB: 3, Purpose: "e2e", WaitSeconds: 1}
	m.mu.Lock()
	m.acquire(c, req)
	ch := m.park(c.Session, req.Kind)
	m.mu.Unlock()
	out, err := m.awaitGrant(context.Background(), ch, c, req)
	if r, _ := out.(*api.AcquireResp); err != nil || r == nil || r.Granted || r.Queue != 1 {
		t.Fatalf("want the refusal as of now, still queued: %+v %v", out, err)
	}
}
