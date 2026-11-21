package manager

import (
	"errors"
	"strings"
	"testing"
	"time"

	"agentrm/internal/api"
	"agentrm/internal/discover"
	"agentrm/internal/model"
	"agentrm/internal/sys"
)

// dockerVM puts Docker Desktop's VM in the state as a sweep would: found by the process table,
// credited with what it holds above its idle size.
func dockerVM(m *Manager, fullMB float64) *model.Resource {
	r := &model.Resource{ID: discover.DockerVMID, Kind: model.RKDockerVM, Source: discover.SrcProcs, Name: "Docker Desktop VM",
		MemMB: fullMB, PID: 120, ProcStart: m.now().Add(-time.Hour), FirstSeen: m.now()}
	m.creditDockerVM(r, 0, m.now())
	m.st.Resources[r.ID] = r
	return r
}

// sweepOrphans is the decision half of a sweep, with effects carried out (faked) and applied.
func sweepOrphans(m *Manager, now time.Time) {
	m.applyEffects(RunEffects(m.gateAuto(m.checkOrphans(now))))
}

// earnEngineTrust gives the engine_restart level the reviewed record it needs.
func earnEngineTrust(m *Manager, r *model.Resource, now time.Time) {
	for i := 0; i < 5; i++ {
		m.decide("stop", r, "test")
		d := m.st.Decisions[len(m.st.Decisions)-1]
		d.Verdict, d.Time = "right", now.Add(-8*24*time.Hour)
	}
}

func TestDockerVMCreditIsWhatARestartGivesBack(t *testing.T) {
	m, _ := newTest(t, 8)
	r := dockerVM(m, 6*1024)
	if r.MemMB != 6*1024-dockerVMIdleMB || !strings.Contains(r.Detail, "holds 6.0 GB") {
		t.Fatalf("credit %v (%s), want the footprint less the idle size", r.MemMB, r.Detail)
	}
	if m.returnRatio(r.Source) != 1 {
		t.Fatal("what a restart gives back was discounted again as if containers were stopped")
	}
}

// Off by default, even with dry run off: AgentRM asks, once, and records what it would do.
func TestDockerVMAskedAboutNotRestarted(t *testing.T) {
	m, c := newTest(t, 8)
	m.cfg.DryRun = false
	got := fakeEffects(t, nil)
	var notes []string
	m.notify = func(title, msg string) { notes = append(notes, title) }
	r := dockerVM(m, 6*1024)
	for i := 0; i < 4; i++ {
		sweepOrphans(m, c.Now())
		c.Add(minutes(m.cfg.GraceMinutes))
	}
	if len(*got) != 0 {
		t.Fatalf("Docker was restarted with its level off: %+v", *got)
	}
	if r.OrphanSince == nil || len(notes) != 1 {
		t.Fatalf("should have asked you once: %v", notes)
	}
	n := 0
	for _, d := range m.st.Decisions {
		if d.Resource == r.ID {
			n++
			if d.Category != catEngine || !d.DryRun || d.Action != "stop" {
				t.Fatalf("decision %+v", d)
			}
		}
	}
	if n != 1 {
		t.Fatalf("%d decisions recorded for one episode", n)
	}
	if lv := m.trustLevels(c.Now()); !strings.Contains(lv[len(lv)-1].Blocked, "review") || lv[len(lv)-1].On {
		t.Fatalf("the level should be off and say how it is earned: %+v", lv[len(lv)-1])
	}
}

// Nothing that uses Docker, or might, lets its VM be restarted, by AgentRM or by you.
func TestDockerVMNeverRestartedWhileDockerIsUsed(t *testing.T) {
	cases := map[string]func(m *Manager){
		"a container": func(m *Manager) { addRes(m, "compose:web", model.RKContainers, 800, "") },
		"a docker lease": func(m *Manager) {
			m.st.Leases["l1"] = &model.Lease{ID: "l1", Session: "s1", Kind: model.KDockerBuild, MemGB: 2}
		},
		"Docker not answering": func(m *Manager) { m.failed = map[string]error{discover.SrcDocker: discover.ErrEngineHung} },
		"a waiting compose request": func(m *Manager) {
			m.waiters = append(m.waiters, &model.Waiter{Session: "s2", Kind: model.KCompose, NeedMB: 4096, Since: m.now(), LastSeen: m.now()})
		},
		"a busy VM": func(m *Manager) { m.st.Resources[discover.DockerVMID].CPU = 80 },
	}
	for name, setup := range cases {
		m, c := newTest(t, 8)
		got := fakeEffects(t, nil)
		r := dockerVM(m, 6*1024)
		m.cfg.AutoStop = map[string]bool{catEngine: true}
		for i := 0; i < 3; i++ {
			setup(m) // still the case at every sweep
			sweepOrphans(m, c.Now())
			c.Add(minutes(m.cfg.GraceMinutes))
		}
		if len(*got) != 0 || r.OrphanSince != nil {
			t.Errorf("%s: restarted or counting down: %+v", name, *got)
		}
		setup(m)
		if _, _, err := m.stopNow(you, api.Req{ResourceID: r.ID}); err == nil {
			t.Errorf("%s: you could restart it", name)
		}
	}
}

// Only you restart Docker by hand; an agent never can, whoever holds what.
func TestOnlyYouRestartDocker(t *testing.T) {
	m, _ := newTest(t, 8)
	r := dockerVM(m, 6*1024)
	if _, _, err := m.stopNow(agent("a1"), api.Req{ResourceID: r.ID}); err == nil {
		t.Fatal("an agent restarted Docker")
	}
	if _, _, err := m.claim(agent("a1"), api.Req{ResourceID: r.ID, Purpose: "mine"}); err == nil {
		t.Fatal("an agent claimed Docker's VM")
	}
	m.cfg.RequireAppApproval = true
	if _, _, err := m.stopNow(terminal, api.Req{ResourceID: r.ID}); err == nil {
		t.Fatal("a terminal restarted Docker without the app's approval")
	}
	_, effects, err := m.stopNow(you, api.Req{ResourceID: r.ID})
	if err != nil || len(effects) != 1 || effects[0].Op != "stop" || effects[0].By != "you" {
		t.Fatalf("you could not restart it from the app: %+v %v", effects, err)
	}
	if d := m.st.Decisions[len(m.st.Decisions)-1]; d.DryRun || d.Category != catEngine {
		t.Fatalf("your restart was recorded as a dry run: %+v", d)
	}
}

// The level is turned on only once its record allows; then the VM is restarted after the grace
// period, through the same gate as every automatic action.
func TestDockerVMRestartedOnceTrusted(t *testing.T) {
	m, c := newTest(t, 8)
	got := fakeEffects(t, nil)
	r := dockerVM(m, 6*1024)
	if _, _, err := m.setConfig(you, api.Req{AutoStop: map[string]bool{catEngine: true}}); err == nil {
		t.Fatal("the level was turned on with no record")
	}
	earnEngineTrust(m, r, c.Now())
	if _, _, err := m.setConfig(you, api.Req{AutoStop: map[string]bool{catEngine: true}}); err != nil {
		t.Fatal(err)
	}
	if m.cfg.AutoStop["owner_gone"] || !m.cfg.DryRun {
		t.Fatal("turning it on changed other levels")
	}

	// You let everything through: nothing automatic happens, this included.
	until := c.Now().Add(time.Hour)
	m.st.BypassUntil = &until
	sweepOrphans(m, c.Now())
	c.Add(minutes(m.cfg.GraceMinutes) + time.Minute)
	sweepOrphans(m, c.Now())
	if len(*got) != 0 {
		t.Fatalf("restarted while everything is let through: %+v", *got)
	}
	m.st.BypassUntil = nil
	sweepOrphans(m, c.Now())
	if len(*got) != 1 || (*got)[0].Op != "stop" || (*got)[0].By != "agentrm" {
		t.Fatalf("not restarted once trusted: %+v", *got)
	}

	// Restarted: not listed as stopped (nothing to restore), and gone until it comes back.
	if m.st.Resources[r.ID] != nil || len(m.st.Stopped) != 0 {
		t.Fatalf("after a restart: resource %v, stopped %d", m.st.Resources[r.ID], len(m.st.Stopped))
	}
	// It comes back; it is left alone for a while, and its idle size is learned.
	c.Add(5 * time.Minute)
	back := dockerVM(m, 900)
	if m.st.DockerVMIdleMB != 900 || back.MemMB != 0 {
		t.Fatalf("idle size not learned after the restart: %v (credit %v)", m.st.DockerVMIdleMB, back.MemMB)
	}
	back.MemMB = 5000
	for i := 0; i < 3; i++ {
		c.Add(minutes(m.cfg.GraceMinutes))
		sweepOrphans(m, c.Now())
	}
	if len(*got) != 1 {
		t.Fatalf("restarted again within the hour: %+v", *got)
	}
}

// A restart that fails is not tried again on every sweep.
func TestFailedDockerRestartIsNotRetriedAtOnce(t *testing.T) {
	m, c := newTest(t, 8)
	got := fakeEffects(t, map[string]bool{"stop": true})
	r := dockerVM(m, 6*1024)
	m.cfg.AutoStop = map[string]bool{catEngine: true}
	for i := 0; i < 5; i++ {
		sweepOrphans(m, c.Now())
		c.Add(minutes(m.cfg.GraceMinutes))
	}
	if len(*got) != 1 || m.st.Resources[r.ID] == nil {
		t.Fatalf("a failed restart was retried: %d attempts", len(*got))
	}
}

// Docker's VM disappears while Docker restarts or is quit. That is not "never used again": the
// learned idle timing is left alone, and when it comes back it is found afresh.
func TestDockerVMGoneWhileDockerRestarts(t *testing.T) {
	m, c := newTest(t, 8)
	r := dockerVM(m, 6*1024)
	r.FirstSeen = c.Now().Add(-time.Hour)
	procs := map[int]*sys.Proc{1: {PID: 1, Exe: "launchd"}}
	m.merge(discover.Result{Failed: map[string]error{}}, procs, nil, c.Now())
	if m.st.Resources[r.ID] != nil {
		t.Fatal("still listed with its process gone")
	}
	if len(m.st.IdleGaps[model.RKDockerVM]) != 0 {
		t.Fatalf("recorded as never used again: %v", m.st.IdleGaps)
	}
	// While the process table cannot be read, it is kept as last seen, and nothing is decided.
	r = dockerVM(m, 6*1024)
	m.cfg.AutoStop = map[string]bool{catEngine: true}
	got := fakeEffects(t, nil)
	m.merge(discover.Result{Failed: map[string]error{discover.SrcProcs: errors.New("no table")}}, nil, nil, c.Now())
	if r.Unseen == nil {
		t.Fatal("not marked unseen")
	}
	for i := 0; i < 3; i++ {
		sweepOrphans(m, c.Now())
		c.Add(minutes(m.cfg.GraceMinutes))
	}
	if len(*got) != 0 {
		t.Fatal("restarted on a view that could not be checked")
	}
}

// A blocked request counts Docker's VM only where it would really be restarted.
func TestReclaimCountsDockerVMOnlyWhenTrusted(t *testing.T) {
	m, c := newTest(t, 1)
	r := dockerVM(m, 6*1024)
	since := c.Now().Add(-time.Minute)
	r.OrphanSince = &since
	if msg := m.reclaim(4096, "s1", c.Now()); strings.Contains(msg, r.ID) || r.Urgent {
		t.Fatalf("counted with its level off: %s", msg)
	}
	m.cfg.AutoStop = map[string]bool{catEngine: true}
	if msg := m.reclaim(4096, "s1", c.Now()); !strings.Contains(msg, r.ID) || !r.Urgent {
		t.Fatalf("not counted with its level on: %s", msg)
	}
}
