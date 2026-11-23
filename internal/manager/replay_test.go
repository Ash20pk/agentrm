package manager

import (
	"context"
	"testing"
	"time"

	"agentrm/internal/api"
	"agentrm/internal/discover"
	"agentrm/internal/model"
	"agentrm/internal/sys"
	"agentrm/internal/trace"
)

// recordScenario runs a live manager over an hour, recording: a Claude session starts a dev
// server (detached, as `npm run dev &` is), takes a lease for it, and exits five minutes in.
// With usedAt > 0 someone connects to the server that long after the start.
func recordScenario(t *testing.T, usedAt time.Duration) string {
	t.Helper()
	dir := t.TempDir()
	m, c := newTest(t, 8)
	fakeEffects(t, nil)
	// Kept for good: the writer prunes by the wall clock, and this clock is fixed in the past.
	m.recP.Store(trace.NewWriter(dir, 36500))
	t0 := c.Now()
	fakeInfos(m, map[int]sys.ProcInfo{700: {Agent: "claude", Session: "s", AgentPID: 500, Argv: []string{"node", "server.js"}}})
	agentUp := func() map[int]*sys.Proc {
		return map[int]*sys.Proc{
			1:   {PID: 1, Exe: "/sbin/launchd"},
			400: {PID: 400, PPID: 1, Exe: "/Applications/Warp.app/Contents/MacOS/stable", Path: "/Applications/Warp.app/Contents/MacOS/stable"},
			500: {PID: 500, PPID: 400, Exe: "/usr/local/bin/claude", Args: "claude", Start: t0.Add(-time.Hour), RSSMB: 300},
			700: {PID: 700, PPID: 1, Exe: "node", Args: "node server.js", Start: t0, RSSMB: 500},
		}
	}
	srv := func() discover.Result {
		return discover.Result{Resources: []*model.Resource{{ID: "srv:3000", Kind: model.RKServer, Source: "procs", Name: "node",
			PID: 700, MemMB: 500, Ports: []string{"3000"}}}, Failed: map[string]error{}}
	}
	mem := sys.MemStats{TotalMB: 16384, WiredMB: 3000, AvailableMB: 8 * 1024, PressureLevel: 1, Cores: 10, DiskFreeGB: 100}
	step := func(procs map[int]*sys.Proc, est map[string]bool) {
		m.recordSweep(procs, srv(), mem, est, nil)
		m.sweepWith(procs, srv(), mem, est, nil)
	}
	step(agentUp(), nil)
	h := m.routes()["/acquire"]
	if _, err := m.handleFacts(context.Background(), "/acquire", h, facts{pid: 900, agentPID: 500, agent: "claude"},
		api.Req{Session: "s", Via: "hook", Kind: "server", Purpose: "dev server"}); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 120; i++ { // an hour of 30-second sweeps
		c.Add(30 * time.Second)
		procs := agentUp()
		if c.Now().Sub(t0) >= 5*time.Minute {
			delete(procs, 500)
			if c.Now().Sub(t0) == 5*time.Minute {
				m.recorder().Write(trace.Frame{T: c.Now(), Exit: 500, ExitTab: m.recorder().Procs(c.Now(), procs, m.traceOwner, nil)})
				m.AgentExited(500, procs)
			}
		}
		var est map[string]bool
		if usedAt > 0 && c.Now().Sub(t0) == usedAt {
			est = map[string]bool{"3000": true}
		}
		step(procs, est)
	}
	m.recorder().Close()
	if len(m.st.Decisions) == 0 {
		t.Fatal("the live run (dry run) decided nothing: the scenario is wrong")
	}
	return dir
}

func TestReplayScoresStopsAgainstTheRecording(t *testing.T) {
	cfg := model.DefaultConfig()
	unused, err := Replay(recordScenario(t, 0), ReplayOptions{Config: &cfg})
	if err != nil {
		t.Fatal(err)
	}
	if unused.StopsN != 1 || unused.WrongStops != 0 || unused.FreedGBh <= 0 {
		t.Fatalf("an abandoned server should be stopped once, rightly, freeing memory:\n%s", unused)
	}
	if unused.Agreement != 1 {
		t.Fatalf("replay disagrees with the live daemon's decisions (%d): %.2f\n%s", unused.LiveDecisions, unused.Agreement, unused)
	}
	used, err := Replay(recordScenario(t, 40*time.Minute), ReplayOptions{Config: &cfg})
	if err != nil {
		t.Fatal(err)
	}
	if used.StopsN != 1 || used.WrongStops != 1 {
		t.Fatalf("a server used again after its stop should count as a wrong stop:\n%s", used)
	}
}

func TestReaperKillsALiveSessionsServer(t *testing.T) {
	// The same recording, through the reaper rule: the server hangs off launchd from the start
	// (it was started with &), so the reaper kills it while its agent is still running.
	rep, err := Replay(recordScenario(t, 40*time.Minute), ReplayOptions{Policy: "reaper"})
	if err != nil {
		t.Fatal(err)
	}
	if rep.StopsN != 1 || rep.LiveOwnerStops != 1 || rep.WrongStops != 1 {
		t.Fatalf("reaper: %s", rep)
	}
	if !rep.Stops[0].At.Equal(rep.From) {
		t.Fatalf("the reaper should have killed it on the first sweep: %s", rep)
	}
}
