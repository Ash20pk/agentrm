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

// A machine with a detached dev process (pid 700, node) whose footprint the test sets, and the
// agent tree of collector_test (Claude 500, npm 600, node 601).
func leakProcs(start time.Time) map[int]*sys.Proc {
	procs := agentTree(start)
	procs[700] = &sys.Proc{PID: 700, PPID: 1, Exe: "node", Args: "node /work/app/server.js", Start: start, RSSMB: 600}
	return procs
}

// sweepLeaks runs the leak watch once per sample interval, with footprint mb(i) for pid.
func sweepLeaks(m *Manager, c *clock, procs map[int]*sys.Proc, pid int, from, to int, mb func(i int) float64) {
	for i := from; i < to; i++ {
		procs[pid].RSSMB = mb(i)
		m.watchLeaks(procs, c.Now())
		c.Add(leakSampleEvery)
	}
}

func steady(i int) float64 { return 600 + 50*float64(i) } // +600 MB/h

func flagged(m *Manager) []*model.Leak { return m.leakList() }

func TestSteadyGrowthIsFlaggedAndAsked(t *testing.T) {
	m, c := newTest(t, 8)
	m.cfg.DryRun = false
	got := fakeEffects(t, nil)
	procs := leakProcs(c.Now().Add(-time.Hour))
	sweepLeaks(m, c, procs, 700, 0, 12, steady) // 55 minutes: not long enough to tell
	if len(flagged(m)) != 0 {
		t.Fatalf("flagged before an hour of history: %+v", flagged(m)[0])
	}
	// The rest through the full sweep, so the hook in sweepWith is what runs.
	for i := 12; i < 16; i++ {
		procs[700].RSSMB = steady(i)
		m.sweepWith(procs, discover.Result{}, m.mem, nil, nil)
		c.Add(leakSampleEvery)
	}
	ls := flagged(m)
	if len(ls) != 1 || ls[0].PID != 700 || ls[0].Action != "stop" || ls[0].RateMBh < 550 || ls[0].RateMBh > 650 {
		t.Fatalf("steady growth not flagged as expected: %+v", ls)
	}
	a := m.st.Approvals[ls[0].Approval]
	if a == nil || a.Path != "/leak" || a.State != "pending" || !strings.Contains(a.What, "node") {
		t.Fatalf("no approval for the leak: %+v", a)
	}
	if !hasEvent(m, "LEAK node (pid 700)") {
		t.Fatal("no LEAK event")
	}
	if len(*got) != 0 {
		t.Fatalf("acted without being asked: %+v", *got)
	}
	st, _, _ := m.status(you, api.Req{})
	if s := st.(*api.Status); len(s.Leaks) != 1 {
		t.Fatalf("status does not carry the leak: %+v", s.Leaks)
	}
	// Asked once, not on every sweep.
	m.watchLeaks(procs, c.Now())
	n := 0
	for _, a := range m.st.Approvals {
		if a.Path == "/leak" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("asked %d times", n)
	}
}

func hasEvent(m *Manager, prefix string) bool {
	for _, e := range m.events {
		if strings.HasPrefix(e.Text, prefix) {
			return true
		}
	}
	return false
}

func TestNoiseIsNotALeak(t *testing.T) {
	cases := map[string]func(i int) float64{
		// A garbage-collected heap: up and down, drifting slowly upward.
		"sawtooth": func(i int) float64 { return 800 + 15*float64(i) + 600*float64(i%2) },
		// A GC that runs every third sample.
		"gc": func(i int) float64 { return 900 + 250*float64(i%3) + 10*float64(i) },
		// Flat, then one jump at the end (a cache filled, a big file opened).
		"jump": func(i int) float64 {
			if i == 23 {
				return 2000
			}
			return 800
		},
		// Climbed once, flat since.
		"plateau": func(i int) float64 { return 600 + 100*float64(min(i, 5)) },
		// Growing, but by less than 500 MB in two hours.
		"slow": func(i int) float64 { return 900 + 15*float64(i) },
	}
	for name, mb := range cases {
		t.Run(name, func(t *testing.T) {
			m, c := newTest(t, 8)
			m.cfg.DryRun = false
			procs := leakProcs(c.Now().Add(-time.Hour))
			for i := 0; i < 24; i++ {
				procs[700].RSSMB = mb(i)
				m.watchLeaks(procs, c.Now())
				if ls := flagged(m); len(ls) > 0 {
					t.Fatalf("sample %d: flagged %+v", i, ls[0])
				}
				c.Add(leakSampleEvery)
			}
		})
	}
}

func TestGrowthUnderALeaseIsNotALeak(t *testing.T) {
	m, c := newTest(t, 8)
	m.cfg.DryRun = false
	start := c.Now().Add(-time.Hour)
	procs := leakProcs(start)
	liveSession(m, "s", start)
	m.agentSessions[500] = "s"
	if r := acquire(t, m, agent("s"), model.KDockerBuild, 1); !r.Granted {
		t.Fatal(r.Reason)
	}
	m.st.Leases[m.findLease("s", model.KDockerBuild).ID].Expires = c.At(10 * time.Hour)
	sweepLeaks(m, c, procs, 601, 0, 20, steady) // node under the agent, while it holds a build lease
	if len(flagged(m)) != 0 {
		t.Fatalf("growth inside a leased session flagged: %+v", flagged(m)[0])
	}
	// The lease ends; it keeps climbing: now nothing explains it.
	m.release(agent("s"), api.Req{})
	sweepLeaks(m, c, procs, 601, 20, 22, steady)
	if ls := flagged(m); len(ls) != 1 || ls[0].PID != 601 {
		t.Fatalf("growth after the lease ended was not flagged: %+v", ls)
	}
	// A build anywhere above it explains it too.
	procs[600].Args = "cargo build --release"
	m.watchLeaks(procs, c.Now())
	if len(flagged(m)) != 0 {
		t.Fatal("growth under a running build stayed flagged")
	}
}

func TestPIDReuseStartsAfresh(t *testing.T) {
	m, c := newTest(t, 8)
	m.cfg.DryRun = false
	procs := leakProcs(c.Now().Add(-time.Hour))
	sweepLeaks(m, c, procs, 700, 0, 16, steady)
	if len(flagged(m)) != 1 {
		t.Fatal("not flagged")
	}
	id := flagged(m)[0].Approval
	// pid 700 is now another process, already large.
	procs[700] = &sys.Proc{PID: 700, PPID: 1, Exe: "python3", Args: "python3 train.py", Start: c.Now(), RSSMB: 2000}
	m.watchLeaks(procs, c.Now())
	if len(flagged(m)) != 0 {
		t.Fatal("the new process inherited the old one's leak")
	}
	if len(m.leakTracks) != 1 {
		t.Fatalf("tracks: %d", len(m.leakTracks))
	}
	for _, tr := range m.leakTracks {
		if len(tr.samples) != 1 || tr.name != "python3" {
			t.Fatalf("history not reset: %+v", tr)
		}
	}
	if m.st.Approvals[id].State != "expired" {
		t.Fatalf("the question about the old process is still open: %s", m.st.Approvals[id].State)
	}
}

func TestDeadProcessIsPruned(t *testing.T) {
	m, c := newTest(t, 8)
	m.cfg.DryRun = false
	procs := leakProcs(c.Now().Add(-time.Hour))
	sweepLeaks(m, c, procs, 700, 0, 16, steady)
	id := flagged(m)[0].Approval
	delete(procs, 700)
	m.watchLeaks(procs, c.Now())
	if len(m.leakTracks) != 0 || len(flagged(m)) != 0 {
		t.Fatalf("a dead process is still tracked: %d tracks", len(m.leakTracks))
	}
	if m.st.Approvals[id].State != "expired" {
		t.Fatal("the question about a dead process is still open")
	}
	// After a daemon restart the tracks are gone, but approvals are kept: prune closes them.
	m2, _ := newTest(t, 8)
	m2.st.Approvals["x"] = &model.Approval{ID: "x", Path: "/leak", State: "pending", Expires: c.At(time.Hour),
		Request: map[string]any{"resource_id": "leak-gone"}}
	m2.prune(c.Now())
	if m2.st.Approvals["x"].State != "expired" {
		t.Fatal("a question about a leak nobody tracks was left open")
	}
}

func TestLeakIsActedOnOnlyAfterAPersonApproves(t *testing.T) {
	m, c := newTest(t, 8)
	m.cfg.DryRun = false
	m.cfg.RequireAppApproval = true
	got := fakeEffects(t, nil)
	procs := leakProcs(c.Now().Add(-time.Hour))
	sweepLeaks(m, c, procs, 700, 0, 16, steady)
	l := flagged(m)[0]
	yes := true
	// An agent cannot, either directly or by answering the approval.
	if _, _, err := m.leakAct(agent("a"), api.Req{ResourceID: l.ID, Approve: &yes}); err == nil {
		t.Fatal("an agent acted on a leak")
	}
	if _, _, err := m.approve(agent("a"), api.Req{ApprovalID: l.Approval, Approve: &yes}); err == nil {
		t.Fatal("an agent answered the leak's approval")
	}
	// From a terminal: held for a click in the app.
	var ea *errApproval
	if _, _, err := m.leakAct(terminal, api.Req{ResourceID: l.ID, Approve: &yes}); !errors.As(err, &ea) {
		t.Fatalf("a terminal acted without the app: %v", err)
	}
	if len(*got) != 0 || m.pending["proc:700"] != "" {
		t.Fatal("something was done before anyone approved")
	}
	// The app approves: SIGTERM to that process, start-time checked by the executor.
	out, effects, err := m.approve(you, api.Req{ApprovalID: l.Approval, Approve: &yes})
	if err != nil || out.(*api.ApprovalResp).Error != "" {
		t.Fatalf("approval failed: %v %+v", err, out)
	}
	if len(effects) != 1 || effects[0].Op != "stop" || effects[0].Res.PID != 700 || effects[0].Res.Kind != model.RKProcess ||
		effects[0].Res.ProcStart.IsZero() || effects[0].By != "you" {
		t.Fatalf("wrong effect: %+v", effects)
	}
	if m.st.Approvals[l.Approval].State != "approved" || len(flagged(m)) != 0 {
		t.Fatal("the leak was not settled")
	}
	m.applyEffects(RunEffects(effects))
	if len(*got) != 1 || !hasEvent(m, "STOPPED proc:700") {
		t.Fatalf("not stopped: %+v", *got)
	}
}

func TestLeakingServerWithAKnownRestartIsRestarted(t *testing.T) {
	m, c := newTest(t, 8)
	m.cfg.DryRun = false
	start := c.Now().Add(-time.Hour)
	procs := leakProcs(start)
	// npm (600) is a dev server AgentRM knows how to start again; node (601) under it leaks.
	delete(procs, 500)
	procs[600].PPID = 1
	m.st.Resources["srv:600"] = &model.Resource{ID: "srv:600", Kind: model.RKServer, Name: "vite", PID: 600,
		ProcStart: procs[600].Start, RestartArgv: []string{"npm", "run", "dev"}}
	sweepLeaks(m, c, procs, 601, 0, 16, steady)
	ls := flagged(m)
	if len(ls) != 1 || ls[0].Action != "restart" || ls[0].Resource != "srv:600" {
		t.Fatalf("expected a restart of the server: %+v", ls)
	}
	yes := true
	_, effects, err := m.leakAct(you, api.Req{ResourceID: "601", Approve: &yes}) // by PID from the CLI
	if err != nil || len(effects) != 1 || effects[0].Op != "restart" || effects[0].Res.ID != "srv:600" {
		t.Fatalf("expected a restart effect: %v %+v", err, effects)
	}
}

func TestDismissedLeakIsNotAskedAgain(t *testing.T) {
	m, c := newTest(t, 8)
	m.cfg.DryRun = false
	procs := leakProcs(c.Now().Add(-time.Hour))
	sweepLeaks(m, c, procs, 700, 0, 16, steady)
	l := flagged(m)[0]
	no := false
	m.approve(you, api.Req{ApprovalID: l.Approval, Approve: &no})
	sweepLeaks(m, c, procs, 700, 16, 20, steady)
	if !l.Dismissed || len(m.st.Approvals) != 1 {
		t.Fatalf("a denied leak was asked about again: dismissed=%v approvals=%d", l.Dismissed, len(m.st.Approvals))
	}
}

func TestDryRunOnlyReportsLeaks(t *testing.T) {
	m, c := newTest(t, 8) // dry run is the default
	got := fakeEffects(t, nil)
	procs := leakProcs(c.Now().Add(-time.Hour))
	sweepLeaks(m, c, procs, 700, 0, 20, steady)
	ls := flagged(m)
	if len(ls) != 1 || ls[0].Approval != "" || len(m.st.Approvals) != 0 {
		t.Fatalf("dry run asked to act: %+v approvals=%d", ls, len(m.st.Approvals))
	}
	n := 0
	for _, d := range m.st.Decisions {
		if d.Resource == "proc:700" {
			n++
			if !d.DryRun || d.Action != "stop" {
				t.Fatalf("decision not marked dry run: %+v", d)
			}
		}
	}
	if n != 1 {
		t.Fatalf("expected one would-stop decision, got %d", n)
	}
	yes := true
	if _, effects, err := m.leakAct(you, api.Req{ResourceID: ls[0].ID, Approve: &yes}); err == nil || len(effects) > 0 {
		t.Fatal("dry run acted on a leak")
	}
	if len(*got) != 0 || len(m.pending) != 0 {
		t.Fatal("dry run changed something")
	}
}

func TestFailOpenHoldsTheQuestion(t *testing.T) {
	m, c := newTest(t, 8)
	m.cfg.DryRun = false
	until := c.At(10 * time.Hour)
	m.st.BypassUntil = &until
	procs := leakProcs(c.Now().Add(-time.Hour))
	sweepLeaks(m, c, procs, 700, 0, 16, steady)
	if len(flagged(m)) != 1 || len(m.st.Approvals) != 0 {
		t.Fatal("asked while everything is let through")
	}
	m.st.BypassUntil = nil
	m.watchLeaks(procs, c.Now())
	if len(m.st.Approvals) != 1 {
		t.Fatal("not asked once the bypass ended")
	}
}

func TestNeverWatched(t *testing.T) {
	m, c := newTest(t, 8)
	m.cfg.DryRun = false
	m.cfg.NeverStop = []string{"keepme"}
	start := c.Now().Add(-time.Hour)
	procs := leakProcs(start)
	delete(procs, 700)
	big := func(pid int, exe, path, args string) {
		procs[pid] = &sys.Proc{PID: pid, PPID: 1, Exe: exe, Path: path, Args: args, Start: start, RSSMB: 600}
	}
	big(m.selfPID, "agentrm", "/usr/local/bin/agentrm", "agentrm daemon")
	big(801, "/System/Library/x/WindowServer", "/System/Library/x/WindowServer", "WindowServer")
	big(802, "/Applications/Slack.app/Contents/MacOS/Slack", "/Applications/Slack.app/Contents/MacOS/Slack", "Slack")
	big(803, "Google Chrome Helper", "/Applications/Google Chrome.app/Contents/Frameworks/x/Google Chrome Helper", "Google Chrome Helper --type=renderer")
	big(804, "com.apple.Virtualization.VirtualMachine", "", "com.apple.Virtualization.VirtualMachine")
	big(805, "ollama", "/opt/homebrew/bin/ollama", "ollama serve")
	big(806, "node", "", "node keepme.js")
	big(807, "qemu-system-aarch64", "", "qemu-system-aarch64 -avd Pixel")
	big(808, "node", "", "node vm-runner.js") // a VM resource owns it
	m.st.Resources["vm:x"] = &model.Resource{ID: "vm:x", Kind: model.RKVM, PID: 808, ProcStart: start}
	big(809, "rootd", "/opt/x/rootd", "rootd")
	m.procUID = func(pid int) (int, bool) { return map[bool]int{true: 0, false: 501}[pid == 809], true }
	// A language server inside an editor's bundle is still watched.
	big(810, "Code Helper (Plugin)", "/Applications/Visual Studio Code.app/Contents/Frameworks/Code Helper (Plugin).app/Contents/MacOS/Code Helper (Plugin)",
		"Code Helper (Plugin) /x/typescript/lib/tsserver.js")
	for i := 0; i < 20; i++ {
		for pid, p := range procs {
			if pid >= 800 || pid == m.selfPID {
				p.RSSMB = steady(i)
			}
		}
		m.watchLeaks(procs, c.Now())
		c.Add(leakSampleEvery)
	}
	ls := flagged(m)
	if len(ls) != 1 || ls[0].PID != 810 {
		for _, l := range ls {
			t.Errorf("flagged %d %s", l.PID, l.Name)
		}
		t.Fatal("expected only the language server to be flagged")
	}
}

func TestClimbing(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mk := func(mb ...float64) []leakSample {
		var s []leakSample
		for i, x := range mb {
			s = append(s, leakSample{at: at.Add(time.Duration(i) * leakSampleEvery), mb: x})
		}
		return s
	}
	// 13 samples, 60 minutes, +60 MB a step, one small dip (allocator jitter).
	ok, rate := climbing(mk(600, 660, 720, 780, 770, 840, 900, 960, 1020, 1080, 1140, 1200, 1260))
	if !ok || rate < 600 || rate > 760 {
		t.Fatalf("a steady climb with a dip: %v %.0f", ok, rate)
	}
	if ok, _ := climbing(mk(600, 660, 720, 780, 840, 900, 960, 1020, 1080, 1140, 1200, 1260)); ok {
		t.Fatal("55 minutes judged")
	}
	// Big process: +500 MB is less than half of 4 GB.
	if ok, _ := climbing(mk(4000, 4050, 4100, 4150, 4200, 4250, 4300, 4350, 4400, 4450, 4500, 4550, 4600)); ok {
		t.Fatal("a 4 GB process growing 15% flagged")
	}
}
