package manager

import (
	"testing"
	"time"

	"agentrm/internal/api"
	"agentrm/internal/model"
	"agentrm/internal/sys"
)

// A Claude Code process (pid 500) that started npm (600), which started node (601).
func agentTree(start time.Time) map[int]*sys.Proc {
	return map[int]*sys.Proc{
		1:   {PID: 1, PPID: 0, Exe: "/sbin/launchd", Args: "/sbin/launchd"},
		400: {PID: 400, PPID: 1, Exe: "/Applications/Warp.app/Contents/MacOS/stable", Args: "stable"},
		500: {PID: 500, PPID: 400, Exe: "/usr/local/bin/claude", Args: "claude", Start: start, RSSMB: 300},
		600: {PID: 600, PPID: 500, Exe: "npm", Args: "npm run dev", Start: start.Add(time.Minute), RSSMB: 60},
		601: {PID: 601, PPID: 600, Exe: "node", Args: "node vite", Start: start.Add(time.Minute), RSSMB: 400},
	}
}

// The same machine after the agent exited: npm was re-parented to launchd.
func afterExit(procs map[int]*sys.Proc) map[int]*sys.Proc {
	out := map[int]*sys.Proc{}
	for pid, p := range procs {
		if pid == 500 {
			continue
		}
		q := *p
		if q.PPID == 500 {
			q.PPID = 1
		}
		out[pid] = &q
	}
	return out
}

func liveSession(m *Manager, id string, start time.Time) *model.Session {
	s := &model.Session{ID: id, Agent: "claude", AgentPID: 500, AgentStart: start, LastSeen: m.now()}
	m.st.Sessions[id] = s
	return s
}

func TestDeadAgentReleasesLeasesAndMarksWhatItLeft(t *testing.T) {
	m, c := newTest(t, 8)
	start := c.Now().Add(-time.Hour)
	s := liveSession(m, "s", start)
	l := acquire(t, m, agent("s"), "cluster", 2)
	r := addRes(m, "kind:dev", model.RKCluster, 2000, l.Lease.ID)
	procs := agentTree(start)
	m.checkLiveness(procs, c.Now())

	gone := afterExit(procs)
	m.checkLiveness(gone, c.Now())
	if s.EndedAt != nil {
		t.Fatal("ended after one missing sweep: a ps hiccup would end sessions")
	}
	m.checkLiveness(gone, c.Now())
	if s.EndedAt == nil {
		t.Fatal("the agent exited but its session was not ended")
	}
	if m.st.Leases[l.Lease.ID] != nil || r.LeaseID != "" {
		t.Fatal("a dead session's lease was kept")
	}
	if r.Owner != "s" || r.OwnerGoneAt == nil {
		t.Fatalf("what it held was not marked owner-gone: %+v", r)
	}
}

func TestReusedPIDIsNotTheAgent(t *testing.T) {
	m, c := newTest(t, 8)
	start := c.Now().Add(-time.Hour)
	s := liveSession(m, "s", start)
	procs := agentTree(start)
	m.checkLiveness(procs, c.Now())
	reused := agentTree(start)
	reused[500] = &sys.Proc{PID: 500, PPID: 1, Exe: "/usr/bin/python3", Args: "python3 x.py", Start: c.Now()}
	m.checkLiveness(reused, c.Now())
	m.checkLiveness(reused, c.Now())
	if s.EndedAt == nil {
		t.Fatal("a different process with the agent's old PID kept the session alive")
	}
}

func TestLeftoverProcessesAreFoundAndCollected(t *testing.T) {
	m, c := newTest(t, 8)
	start := c.Now().Add(-time.Hour)
	liveSession(m, "s", start)
	procs := agentTree(start)
	m.checkLiveness(procs, c.Now())
	gone := afterExit(procs)
	m.checkLiveness(gone, c.Now())
	m.checkLiveness(gone, c.Now())
	if len(m.st.Leftovers) != 1 || m.st.Leftovers["600"] == nil {
		t.Fatalf("expected npm (the top of what was left) as the one leftover: %+v", m.st.Leftovers)
	}
	res := m.withLeftovers(nil, gone)
	if len(res) != 1 || res[0].ID != "left:600" || res[0].Kind != model.RKLeftover {
		t.Fatalf("leftover not offered as a resource: %+v", res)
	}
	if res[0].MemMB < 450 || res[0].Owner != "s" || res[0].OwnerGoneAt == nil {
		t.Fatalf("leftover should carry what is under it and its owner: %+v", res[0])
	}
	// Once it exits, it is forgotten.
	delete(gone, 600)
	delete(gone, 601)
	if res := m.withLeftovers(nil, gone); len(res) != 0 || len(m.st.Leftovers) != 0 {
		t.Fatal("a leftover that exited was kept")
	}
}

func TestLeftoverAlreadyDiscoveredIsCreditedNotDuplicated(t *testing.T) {
	m, c := newTest(t, 8)
	start := c.Now().Add(-time.Hour)
	liveSession(m, "s", start)
	procs := agentTree(start)
	m.checkLiveness(procs, c.Now())
	gone := afterExit(procs)
	m.checkLiveness(gone, c.Now())
	m.checkLiveness(gone, c.Now())
	srv := &model.Resource{ID: "srv:600", Kind: model.RKServer, PID: 600, MemMB: 400}
	res := m.withLeftovers([]*model.Resource{srv}, gone)
	if len(res) != 1 || srv.StartedBy != "s" || srv.Owner != "s" {
		t.Fatalf("discovered leftover should be credited to its session, once: %+v", res)
	}
}

func TestOwnerGoneIsCollectedFastAndUseStopsIt(t *testing.T) {
	m, c := newTest(t, 8)
	got := fakeEffects(t, nil)
	m.cfg.DryRun = false
	m.st.Sessions["s"] = &model.Session{ID: "s", Agent: "claude"}
	gone := c.Now()
	fast := addRes(m, "srv:1", model.RKServer, 300, "")
	fast.Owner, fast.OwnerGoneAt, fast.FirstSeen = "s", &gone, c.Now()
	used := addRes(m, "srv:2", model.RKServer, 300, "")
	used.Owner, used.OwnerGoneAt, used.FirstSeen = "s", &gone, c.Now()
	unknown := addRes(m, "srv:3", model.RKServer, 300, "")
	unknown.FirstSeen = c.Now()
	m.applyEffects(RunEffects(m.checkOrphans(c.Now())))

	c.Add(2 * time.Minute)
	used.LastActive = c.Now() // someone is still talking to it
	c.Add(time.Second)
	m.applyEffects(RunEffects(m.checkOrphans(c.Now())))
	ops := map[string]string{}
	for _, e := range *got {
		ops[e.Res.ID] = e.Op
	}
	if ops["srv:1"] != "pause" {
		t.Fatalf("owner gone and unused: should be paused after %d min: %v", m.cfg.OwnerGoneGraceMinutes, ops)
	}
	if ops["srv:2"] != "" || ops["srv:3"] != "" {
		t.Fatalf("used after its owner left, or no known owner: should get the normal grace: %v", ops)
	}
	// Its pause is shorter too.
	c.Add(minutes(m.cfg.OwnerGonePauseMinutes) + time.Second)
	m.applyEffects(RunEffects(m.checkOrphans(c.Now())))
	stopped := false
	for _, e := range *got {
		stopped = stopped || (e.Res.ID == "srv:1" && e.Op == "stop")
	}
	if !stopped {
		t.Fatalf("owner-gone resource not stopped after its shorter pause: %+v", *got)
	}
}

func TestOldGenerationWaitsLonger(t *testing.T) {
	m, c := newTest(t, 8)
	got := fakeEffects(t, nil)
	m.cfg.DryRun = false
	db := addRes(m, "srv:5", model.RKServer, 300, "")
	db.FirstSeen = c.Now().Add(-48 * time.Hour)
	m.applyEffects(RunEffects(m.checkOrphans(c.Now())))
	c.Add(minutes(m.cfg.GraceMinutes) + time.Second)
	m.applyEffects(RunEffects(m.checkOrphans(c.Now())))
	if len(*got) != 0 {
		t.Fatal("something running for two days was paused on the normal grace")
	}
	c.Add(2 * minutes(m.cfg.GraceMinutes))
	m.applyEffects(RunEffects(m.checkOrphans(c.Now())))
	if len(*got) != 1 || (*got)[0].Op != "pause" {
		t.Fatalf("old generation not paused after three graces: %+v", *got)
	}
}

func TestEndedSessionDoesNotKeepWhatItStarted(t *testing.T) {
	m, c := newTest(t, 8)
	ended := c.Now()
	m.st.Sessions["s"] = &model.Session{ID: "s", Agent: "claude", EndedAt: &ended}
	m.dockerOrigin["compose:web"] = origin{session: "s", at: c.Now()}
	r := &model.Resource{ID: "compose:web", Kind: model.RKContainers, MemMB: 500}
	m.st.Resources[r.ID] = r
	m.attribute(r, nil, c.Now(), true)
	if r.LeaseID != "" || len(m.st.Leases) != 0 {
		t.Fatal("an ended session was given a lease on what it started")
	}
	if r.StartedBy != "s" || r.Owner != "s" || r.OwnerGoneAt == nil {
		t.Fatalf("not credited to its ended starter: %+v", r)
	}
}

func TestSessionEndHookEndsTheSession(t *testing.T) {
	m, _ := newTest(t, 8)
	l := acquire(t, m, agent("s"), "cluster", 2)
	r := addRes(m, "kind:dev", model.RKCluster, 2000, l.Lease.ID)
	if _, _, err := m.end(agent("s"), api.Req{}); err != nil {
		t.Fatal(err)
	}
	if s := m.st.Sessions["s"]; s == nil || s.EndedAt == nil {
		t.Fatal("SessionEnd did not end the session")
	}
	if len(m.st.Leases) != 0 || r.Owner != "s" {
		t.Fatalf("SessionEnd did not release and mark: leases=%d owner=%q", len(m.st.Leases), r.Owner)
	}
	// Talking again revives it.
	m.touchSession("s", "claude", "")
	if m.st.Sessions["s"].EndedAt != nil {
		t.Fatal("a session that talks again is still treated as ended")
	}
}
