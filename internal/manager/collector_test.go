package manager

import (
	"strings"
	"testing"
	"time"

	"agentrm/internal/api"
	"agentrm/internal/discover"
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

func TestAgentStartIsRecordedForExistingSessions(t *testing.T) {
	m, c := newTest(t, 8)
	start := c.Now().Add(-time.Hour)
	// A session from before start times were recorded: PID known, start unknown.
	m.agentSessions[500] = "s"
	m.st.Sessions["s"] = &model.Session{ID: "s", Agent: "claude", AgentPID: 500, LastSeen: c.Now()}
	m.updateFootprints(agentTree(start), c.Now())
	if got := m.st.Sessions["s"].AgentStart; !got.Equal(start) {
		t.Fatalf("agent start not recorded: %v", got)
	}
}

func TestCompressedLeftoverIsStillFound(t *testing.T) {
	m, c := newTest(t, 8)
	start := c.Now().Add(-time.Hour)
	liveSession(m, "s", start)
	procs := agentTree(start)
	m.checkLiveness(procs, c.Now()) // seen at full size while the agent ran
	gone := afterExit(procs)
	gone[600].RSSMB, gone[601].RSSMB = 3, 4 // idle, and macOS compressed it
	m.checkLiveness(gone, c.Now())
	m.checkLiveness(gone, c.Now())
	if m.st.Leftovers["600"] == nil {
		t.Fatal("an idle leftover whose memory was compressed was missed")
	}
}

// fakeInfos makes the environment reader answer from a table.
func fakeInfos(m *Manager, infos map[int]sys.ProcInfo) {
	m.infos = newInfoCache(func(pid int) (sys.ProcInfo, bool) {
		i, ok := infos[pid]
		return i, ok
	})
}

func TestResumedSessionGetsItsThingsBack(t *testing.T) {
	m, c := newTest(t, 8)
	m.cfg.DryRun = false
	got := fakeEffects(t, nil)
	start := c.Now().Add(-time.Hour)
	liveSession(m, "s", start)
	l := acquire(t, m, agent("s"), "server", 0.5)
	r := addRes(m, "srv:3000", model.RKServer, 400, l.Lease.ID)
	r.StartedBy, r.FirstSeen = "s", c.Now()
	if _, _, err := m.end(agent("s"), api.Req{}); err != nil {
		t.Fatal(err)
	}
	if r.Owner != "s" {
		t.Fatal("not marked when the session ended")
	}
	m.applyEffects(RunEffects(m.checkOrphans(c.Now())))
	// It talks again before the owner-gone grace runs out.
	c.Add(time.Minute)
	m.touchSession("s", "claude", "")
	c.Add(minutes(m.cfg.OwnerGoneGraceMinutes) + time.Minute)
	m.applyEffects(RunEffects(m.checkOrphans(c.Now())))
	if r.Owner != "" || r.OwnerGoneAt != nil {
		t.Fatalf("still marked owner-gone after the session came back: %+v", r)
	}
	for _, e := range *got {
		if e.Res.ID == r.ID {
			t.Fatalf("%s a resumed session's server", e.Op)
		}
	}
	// And it claims it back, verified, because it started it.
	out, _, err := m.claim(agent("s"), api.Req{ResourceID: r.ID, Purpose: "still developing"})
	if err != nil || !out.(*api.ClaimResp).Verified {
		t.Fatalf("claim after resume: %+v %v", out, err)
	}
}

func TestClearHandsOverToTheNextSession(t *testing.T) {
	m, c := newTest(t, 8)
	start := c.Now().Add(-time.Hour)
	m.agentSessions[500] = "old"
	liveSession(m, "old", start)
	l := acquire(t, m, agent("old"), "server", 0.5)
	r := addRes(m, "srv:3000", model.RKServer, 400, l.Lease.ID)
	r.StartedBy = "old"
	if _, _, err := m.end(agent("old"), api.Req{Reason: "clear"}); err != nil {
		t.Fatal(err)
	}
	if m.st.Sessions["old"].EndedAt != nil || m.st.Leases[l.Lease.ID] == nil {
		t.Fatal("/clear ended the session and released its lease")
	}
	// The new session's first hook call arrives from the same agent process.
	m.mu.Lock()
	m.resolve(facts{pid: 900, agentPID: 500, agent: "claude"}, &api.Req{Session: "new", Via: "hook"})
	m.mu.Unlock()
	if got := m.st.Leases[l.Lease.ID]; got == nil || got.Session != "new" || r.StartedBy != "new" {
		t.Fatalf("not handed over: lease=%+v startedBy=%q", got, r.StartedBy)
	}
	if !strings.HasPrefix(m.st.Sessions["old"].EndReason, "cleared:") {
		t.Fatalf("old session: %+v", m.st.Sessions["old"])
	}
	// The old one does not come back to life by being mentioned.
	m.touchSession("old", "claude", "")
	if m.st.Sessions["old"].EndedAt == nil {
		t.Fatal("a cleared session was revived")
	}
}

func TestKernelExitEndsTheSessionAtOnce(t *testing.T) {
	m, c := newTest(t, 8)
	start := c.Now().Add(-time.Hour)
	s := liveSession(m, "s", start)
	l := acquire(t, m, agent("s"), "server", 0.5)
	procs := agentTree(start)
	m.checkLiveness(procs, c.Now())
	m.AgentExited(500, afterExit(procs))
	if s.EndedAt == nil || m.st.Leases[l.Lease.ID] != nil {
		t.Fatal("the exit notice did not end the session at once")
	}
	if m.st.Leftovers["600"] == nil {
		t.Fatal("what it left was not found on the exit notice")
	}
	// A notice for a PID that is still the agent (reused, or a stale notice) ends nothing.
	s2 := liveSession(m, "t", start)
	m.AgentExited(500, agentTree(start))
	if s2.EndedAt != nil {
		t.Fatal("an exit notice for a running agent ended its session")
	}
}

// A dev server started with & from a Bash command: re-parented to launchd before any sample
// saw it under the agent. Its environment still names the session.
func detachedServer(start time.Time) (map[int]*sys.Proc, map[int]sys.ProcInfo) {
	procs := agentTree(start)
	procs[700] = &sys.Proc{PID: 700, PPID: 1, Exe: "/opt/homebrew/bin/node", Args: "node server.js", Start: start.Add(2 * time.Minute), RSSMB: 500}
	infos := map[int]sys.ProcInfo{700: {Argv: []string{"node", "server.js"}, Env: map[string]string{"PATH": "/opt/homebrew/bin"},
		Agent: "claude", Session: "s", AgentPID: 500}}
	return procs, infos
}

func TestDetachedProcessIsOwnedByItsEnvironment(t *testing.T) {
	m, c := newTest(t, 8)
	start := c.Now().Add(-time.Hour)
	liveSession(m, "s", start)
	procs, infos := detachedServer(start)
	fakeInfos(m, infos)
	m.checkLiveness(procs, c.Now())
	if _, ok := m.st.Trees["s"][700]; !ok {
		t.Fatal("a process that detached at once was not recorded as the session's")
	}
	if len(m.st.Leftovers) != 0 {
		t.Fatal("a live session's detached server was taken for a leftover")
	}
	srv := &model.Resource{ID: "srv:3000", Kind: model.RKServer, PID: 700, MemMB: 500}
	if got := m.startedBy(srv, procs, c.Now(), false); got != "s" {
		t.Fatalf("detached server credited to %q, want s", got)
	}
	// The agent exits: the server is a leftover, with how to start it again.
	m.AgentExited(500, afterExit(procs))
	lo := m.st.Leftovers["700"]
	if lo == nil || lo.Session != "s" || len(lo.Argv) != 2 || len(lo.Env) != 1 {
		t.Fatalf("detached server not collected with its restart command: %+v", lo)
	}
	res := m.withLeftovers(nil, afterExit(procs))
	var r *model.Resource
	for _, x := range res {
		if x.ID == "left:700" {
			r = x
		}
	}
	if r == nil || r.Restart != "agentrm restore left:700" || len(r.RestartArgv) != 2 {
		t.Fatalf("leftover not restorable: %+v", r)
	}
}

func TestLeftoversFromBeforeAgentRMAreFound(t *testing.T) {
	m, c := newTest(t, 8)
	// An agent that exited yesterday left a database and launched Docker Desktop. AgentRM never
	// saw that agent; only the environment says who started them.
	procs := map[int]*sys.Proc{
		1:   {PID: 1, Exe: "/sbin/launchd"},
		800: {PID: 800, PPID: 1, Exe: "/opt/homebrew/bin/postgres", Args: "postgres -D /tmp/x", Start: c.Now().Add(-20 * time.Hour), RSSMB: 120},
		801: {PID: 801, PPID: 800, Exe: "/opt/homebrew/bin/postgres", Args: "postgres: checkpointer", Start: c.Now().Add(-20 * time.Hour), RSSMB: 20},
		900: {PID: 900, PPID: 1, Exe: "/Applications/Docker.app/Contents/MacOS/com.docker.backend", Start: c.Now().Add(-20 * time.Hour), RSSMB: 400},
		950: {PID: 950, PPID: 1, Exe: "/opt/homebrew/bin/redis-server", Start: c.Now().Add(-20 * time.Hour), RSSMB: 80}, // nobody's
	}
	gone := sys.ProcInfo{Agent: "claude", Session: "dead", AgentPID: 4242}
	fakeInfos(m, map[int]sys.ProcInfo{800: gone, 801: gone, 900: gone})
	m.checkLiveness(procs, c.Now())
	if m.st.Leftovers["800"] == nil || m.st.Leftovers["801"] != nil {
		t.Fatalf("want the top of the database's tree: %+v", m.st.Leftovers)
	}
	if lo := m.st.Leftovers["900"]; lo == nil || !lo.AskOnly {
		t.Fatalf("an app an agent launched must be ask-only: %+v", lo)
	}
	if m.st.Leftovers["950"] != nil {
		t.Fatal("a process with no agent in its environment was taken for a leftover")
	}
	for _, r := range m.withLeftovers(nil, procs) {
		if r.PID == 900 && (discover.CanAutoStop(r) || discover.CanPause(r)) {
			t.Fatal("an app an agent launched could be paused or stopped")
		}
	}
}

func TestStaleAgentIsListedAndOnlyAsked(t *testing.T) {
	m, c := newTest(t, 8)
	procs := map[int]*sys.Proc{
		1:   {PID: 1, Exe: "/sbin/launchd"},
		500: {PID: 500, PPID: 1, Exe: "/usr/local/bin/claude", Args: "claude", Start: c.Now().Add(-30 * time.Hour), RSSMB: 300},
		510: {PID: 510, PPID: 500, Exe: "node", Args: "node mcp-server", Start: c.Now().Add(-30 * time.Hour), RSSMB: 90},
	}
	m.checkLiveness(procs, c.Now())
	res := m.withLeftovers(nil, procs)
	if len(res) != 1 || res[0].Kind != model.RKStaleAgent || res[0].MemMB < 390 || discover.CanAutoStop(res[0]) {
		t.Fatalf("stale agent not listed as ask-only with its subtree: %+v", res)
	}
	// One under a terminal is not stale, however quiet.
	procs[400] = &sys.Proc{PID: 400, PPID: 1, Exe: "/Applications/Warp.app/Contents/MacOS/stable"}
	procs[500].PPID = 400
	m.checkLiveness(procs, c.Now())
	if len(m.withLeftovers(nil, procs)) != 0 {
		t.Fatal("an agent in a terminal was listed as stale")
	}
}

func TestMCPServerOfAGoneAgentIsCollected(t *testing.T) {
	m, c := newTest(t, 8)
	t0 := c.Now().Add(-10 * time.Hour)
	procs := map[int]*sys.Proc{
		1:   {PID: 1, Exe: "/sbin/launchd"},
		700: {PID: 700, PPID: 1, Exe: "node", Args: "node /x/mcp-server-github/index.js", Start: t0, RSSMB: 90},
	}
	fakeInfos(m, map[int]sys.ProcInfo{700: {Agent: "claude"}}) // only CLAUDECODE=1 in its environment
	// A Claude Code started later cannot be the one that spawned it.
	procs[500] = &sys.Proc{PID: 500, PPID: 1, Exe: "/usr/local/bin/claude", Args: "claude", Start: t0.Add(time.Hour)}
	m.checkLiveness(procs, c.Now())
	if lo := m.st.Leftovers["700"]; lo == nil || lo.Session != "claude-unknown" {
		t.Fatalf("an MCP server whose agent is gone was not collected: %+v", m.st.Leftovers)
	}
	// With an older Claude Code still running, it may be that one's: leave it.
	m.st.Leftovers = nil
	procs[500].Start = t0.Add(-time.Hour)
	m.checkLiveness(procs, c.Now())
	if len(m.st.Leftovers) != 0 {
		t.Fatal("an MCP server that a running agent may own was collected")
	}
}

func TestAgentInsideAnAppIsAnAgentNotYou(t *testing.T) {
	m, _ := newTest(t, 8)
	m.mu.Lock()
	c := m.resolve(facts{pid: 900, gui: true, app: "Cursor.app"}, &api.Req{Via: "hook", Agent: "cursor", Session: "conv-1"})
	m.mu.Unlock()
	if c.Human || c.Agent != "cursor" || c.Session != "conv-1" {
		t.Fatalf("Cursor's agent passed for a person: %+v", c)
	}
	// Without the hook's word it is you; claiming to be you from an agent gets nothing.
	m.mu.Lock()
	you := m.resolve(facts{pid: 901, gui: true, app: "Cursor.app"}, &api.Req{})
	under := m.resolve(facts{pid: 902, agentPID: 500, agent: "codex"}, &api.Req{Via: "hook", Agent: "you", Session: "you"})
	m.mu.Unlock()
	if !you.Human || under.Human || under.Agent != "codex" {
		t.Fatalf("you=%+v under-agent=%+v", you, under)
	}
}
