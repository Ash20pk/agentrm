package manager

import (
	"errors"
	"math"
	"regexp"
	"strconv"
	"testing"
	"time"

	"agentrm/internal/discover"
	"agentrm/internal/model"
	"agentrm/internal/sys"
)

// The swap rate is over a minute. Measured over one 2-second gap, 20 MB read as 600 MB a minute,
// over the limit: every request was refused for a moment's burst.
func TestSwapRateIsOverAMinute(t *testing.T) {
	m, c := newTest(t, 8)
	m.takeMem(sys.MemStats{Swapouts: 1000}, c.Now())
	c.Add(2 * time.Second)
	m.takeMem(sys.MemStats{Swapouts: 1020}, c.Now())
	if m.swapRate != 0 {
		t.Fatalf("a rate from a 2 s gap: %.0f MB/min", m.swapRate)
	}
	for i := 0; i < 19; i++ { // 59 s: the burst is still inside the last minute
		c.Add(3 * time.Second)
		m.takeMem(sys.MemStats{Swapouts: 1020}, c.Now())
	}
	if m.swapRate < 15 || m.swapRate > 25 {
		t.Fatalf("20 MB in about a minute: %.1f MB/min", m.swapRate)
	}
	if thr, _ := m.thrashing(); thr {
		t.Fatal("20 MB a minute counted as thrashing")
	}
	// Sustained: 600 MB a minute for a minute is.
	for i := 0; i < 20; i++ {
		c.Add(3 * time.Second)
		m.takeMem(sys.MemStats{Swapouts: 1020 + float64(i+1)*30}, c.Now())
	}
	if thr, _ := m.thrashing(); !thr {
		t.Fatalf("600 MB a minute not thrashing (rate %.0f)", m.swapRate)
	}
	// The samples kept stay about a minute's worth.
	if n := len(m.swapSamples); n > 25 {
		t.Fatalf("%d samples kept", n)
	}
}

// A session with a server lease that then starts a build: the build's processes are charged to
// the build only. Charged to both, the server "used 2.4 GB" of its 1.2 GB, was flagged as an
// overrun and its session distrusted.
func TestOneSessionsLeasesDoNotShareProcesses(t *testing.T) {
	m, c := newTest(t, 8)
	t0 := c.Now()
	m.footprint = func(pid int) (float64, bool) { return map[int]float64{102: 2500}[pid], pid == 102 }
	m.learnSession(100, "s", "claude")
	m.st.Leases["srv"] = &model.Lease{ID: "srv", Session: "s", Kind: model.KServer, MemGB: 1.2, Declared: true,
		Created: t0, Expires: t0.Add(time.Hour), LastUsed: t0}
	m.st.Leases["bld"] = &model.Lease{ID: "bld", Session: "s", Kind: model.KXcodeBuild, MemGB: 3, Declared: true,
		Created: t0.Add(time.Minute), Expires: t0.Add(time.Hour), LastUsed: t0}
	procs := map[int]*sys.Proc{
		100: {PID: 100, PPID: 1, Exe: "claude", Args: "claude", Start: t0.Add(-time.Hour)},
		102: {PID: 102, PPID: 100, Exe: "xcodebuild", Args: "xcodebuild", Start: t0.Add(time.Minute + time.Second)},
	}
	c.Add(2 * time.Minute)
	m.updateFootprints(procs, c.Now())
	if u := m.st.Leases["srv"].UsedMB; u != 0 {
		t.Fatalf("the server lease was charged the build's %.0f MB", u)
	}
	if u := m.st.Leases["bld"].UsedMB; u != 2500 {
		t.Fatalf("the build was charged %.0f MB, want 2500", u)
	}
	if s := m.st.Sessions["s"]; c.Now().Before(s.DistrustUntil) {
		t.Fatal("the session was distrusted for its build")
	}
	if p := m.pendingMB(); math.Abs(p-(1.2*1024+572)) > 1 {
		t.Fatalf("pending %.0f MB, want the server's whole 1229 and the build's last 572", p)
	}
}

// Asking again for the same larger size is the same ask: 1, then 2, 2 and 2 GB hold 2 GB.
func TestAskingAgainDoesNotStackLeases(t *testing.T) {
	m, _ := newTest(t, 12)
	for _, gb := range []float64{1, 2, 2, 2} {
		if r := acquire(t, m, agent("s"), model.KServer, gb); !r.Granted {
			t.Fatalf("%.0f GB refused: %s", gb, r.Reason)
		}
	}
	held, n := 0.0, 0
	for _, l := range m.st.Leases {
		if l.Session == "s" && l.Kind == model.KServer {
			held += l.MemGB
			n++
		}
	}
	if held != 2 || n != 2 {
		t.Fatalf("%d leases holding %.1f GB, want 2 holding 2 GB", n, held)
	}
}

// A Docker build runs in Docker's VM, not in the agent's processes: the VM's growth is its use.
// Measured by the agent's tree alone (the docker CLI), a 3 GB build that took 2 GB of the VM
// held 2 GB less available and its whole 3 GB still promised.
func TestDockerBuildIsMeasuredInDockersVM(t *testing.T) {
	m, c := newTest(t, 8)
	t0 := c.Now()
	m.dockerFullMB, m.dockerMeasured = 1300, true // the VM's footprint; its containers hold nothing
	m.learnSession(100, "s", "claude")
	procs := map[int]*sys.Proc{100: {PID: 100, PPID: 1, Exe: "claude", Args: "claude", Start: t0.Add(-time.Hour)}}
	m.updateFootprints(procs, t0)
	if r := acquire(t, m, agent("s"), model.KDockerBuild, 3); !r.Granted {
		t.Fatal(r.Reason)
	}
	m.dockerFullMB = 3300
	c.Add(time.Minute)
	m.updateFootprints(procs, c.Now())
	for _, l := range m.st.Leases {
		if l.Kind == model.KDockerBuild && l.UsedMB != 2000 {
			t.Fatalf("docker build used %.0f MB, want the VM's 2000 MB of growth", l.UsedMB)
		}
	}
	if p := m.pendingMB(); p != 3*1024-2000 {
		t.Fatalf("pending %.0f MB, want %d", p, 3*1024-2000)
	}
	// Only what is held back: the VM's growth teaches nothing and flags nothing. The docker CLI
	// the session started is all it is known to have used.
	m.dockerFullMB = 9300
	procs[101] = &sys.Proc{PID: 101, PPID: 100, Exe: "docker", Args: "docker build .", RSSMB: 150, Start: t0.Add(time.Second)}
	c.Add(time.Minute)
	m.updateFootprints(procs, c.Now())
	l := m.findLease("s", model.KDockerBuild)
	if l.InsideMB != 3*1024 || l.PeakMB != 150 || l.Overran || c.Now().Before(m.st.Sessions["s"].DistrustUntil) {
		t.Fatalf("8 GB of VM growth on a 3 GB lease: inside %.0f, peak %.0f, overran %v", l.InsideMB, l.PeakMB, l.Overran)
	}
	learned := len(m.st.Learned)
	m.dropLease(l, "test")
	if len(m.st.Learned) != learned {
		t.Fatal("learned an estimate from the VM's growth")
	}
}

// Docker's VM holds its containers' memory; they are counted as resources of their own, so the
// VM is left with what is its own. A 4 GB compose stack was shown as 4 GB plus a 5.2 GB VM.
func TestDockerVMLeavesOutItsContainers(t *testing.T) {
	m, _ := newTest(t, 8)
	idle := m.dockerVMIdle()
	r := &model.Resource{ID: "docker-vm", Kind: model.RKDockerVM, MemMB: 6144}
	m.creditDockerVM(r, 4096, m.now())
	if want := 6144 - idle - 4096; r.MemMB != want {
		t.Fatalf("VM %.0f MB, want %.0f", r.MemMB, want)
	}
	r = &model.Resource{ID: "docker-vm", Kind: model.RKDockerVM, MemMB: 1000}
	m.creditDockerVM(r, 4096, m.now())
	if r.MemMB != 0 {
		t.Fatalf("VM below zero: %.0f", r.MemMB)
	}
}

// learnFullReturn makes stopped containers give back all they hold, for tests about something else.
func learnFullReturn(m *Manager) {
	if m.st.ReturnRatio == nil {
		m.st.ReturnRatio, m.st.ReturnN = map[string]float64{}, map[string]int{}
	}
	m.st.ReturnRatio["docker"], m.st.ReturnN["docker"] = 1, returnLearnAfter
}

// Releasing a lease on containers gives back part of what they hold, as stopping them does.
func TestReleasingADockerLeaseCountsItsReturn(t *testing.T) {
	m, _ := newTest(t, 8)
	m.st.Leases["L"] = &model.Lease{ID: "L", Session: "s", Kind: model.KCompose, MemGB: 2}
	addRes(m, "compose:app", model.RKContainers, 2048, "L")
	if got, want := m.releaseGivesMB(m.st.Leases["L"]), 2048*m.returnRatio("docker"); got != want {
		t.Fatalf("release gives %.0f MB, want %.0f", got, want)
	}
	m.st.Leases["L"].MemGB = 3 // a promise not all taken: the rest comes back whole
	if got, want := m.releaseGivesMB(m.st.Leases["L"]), 1024+2048*m.returnRatio("docker"); got != want {
		t.Fatalf("release gives %.0f MB, want %.0f", got, want)
	}
}

// A refusal's figures add up as printed, and never show need equal to what can be handed out.
func TestRefusalAddsUp(t *testing.T) {
	num := regexp.MustCompile(`[0-9]+\.[0-9]+`)
	for _, tc := range []struct{ need, avail, pending, keep float64 }{
		{2.4, 2.1, 0, 0.5}, {0.6, 2.1, 1.2, 0.5}, {0.62, 2.1, 1.02, 0.5}, {2.4, 2.1, 0, 0.75},
		{1.36, 2.14, 0.01, 0.75}, {0.3, 0.2, 0, 0.5}, {2052.0 / 1024, 4104.5 / 1024, 1029.0 / 1024, 1},
	} {
		s := shortfall(tc.need*1024, tc.avail*1024, tc.pending*1024, tc.keep*1024)
		f := num.FindAllString(s, -1)
		if len(f) != 5 {
			t.Fatalf("%q", s)
		}
		v := make([]float64, 5)
		for i := range f {
			v[i], _ = strconv.ParseFloat(f[i], 64)
		}
		need, head, avail, pending, keep := v[0], v[1], v[2], v[3], v[4]
		if math.Abs(max(0, avail-pending-keep)-head) > 0.001 {
			t.Errorf("%q: %.2f - %.2f - %.2f is not %.2f", s, avail, pending, keep, head)
		}
		if need <= head {
			t.Errorf("%q: need shown at or under what can be handed out", s)
		}
	}
}

// After using well under the ask, a receipt suggests less than the ask.
func TestReceiptSuggestsLessAfterAskingTooMuch(t *testing.T) {
	for _, tc := range []struct{ usedMB, asked float64 }{{100, 0.2}, {171, 0.3}, {184, 0.3}, {600, 1}} {
		if s := suggestGB(tc.usedMB, tc.asked); s >= tc.asked {
			t.Errorf("used %.0f MB of %.1f GB: suggests %.1f", tc.usedMB, tc.asked, s)
		}
	}
	if s := suggestGB(2500, 1.2); s != 3.0 {
		t.Errorf("over the ask: %.1f, want 3.0", s)
	}
}

// The 90th percentile of five lifetimes is the largest, not the fourth.
func TestLifetimePercentileIsNearestRank(t *testing.T) {
	m, _ := newTest(t, 8)
	m.predictState().Lifetimes["cargo-build"] = []float64{5, 1, 4, 2, 3}
	for q, want := range map[float64]float64{0.9: 5, 0.5: 3, 0.1: 1} {
		if d, ok := m.lifetime("cargo-build", "", q); !ok || d != time.Duration(want*float64(time.Minute)) {
			t.Errorf("q %.1f: %s, want %.0f min", q, d, want)
		}
	}
}

// What the app counts down to is what happens: nothing a second before NextAt, the step at it.
// And nothing is promised for what is never paused or stopped.
func TestNextStepIsWhatHappens(t *testing.T) {
	m, c := newTest(t, 8)
	m.cfg.DryRun = false
	mk := func(id, kind string) *model.Resource {
		r := &model.Resource{ID: id, Kind: kind, Name: id, MemMB: 500, Source: "procs", FirstSeen: c.Now()}
		m.st.Resources[id] = r
		return r
	}
	srv, sim := mk("srv:1", model.RKServer), mk("sim:1", model.RKSimulator)
	mine := mk("srv:2", model.RKServer)
	mine.StartedBy = "you"
	vm := mk("vm:lima-x", model.RKVM)
	m.checkOrphans(c.Now())
	if srv.NextStep != "pause" || sim.NextStep != "stop" {
		t.Fatalf("server %q, simulator %q: want pause, and stop (a simulator cannot be paused)", srv.NextStep, sim.NextStep)
	}
	if mine.NextStep != "" || vm.NextStep != "" {
		t.Fatalf("promised %q for what you started and %q for a VM that cannot be paused", mine.NextStep, vm.NextStep)
	}
	step := func(r *model.Resource, want string) {
		t.Helper()
		at := *r.NextAt
		c.t = at.Add(-time.Second)
		for _, e := range m.checkOrphans(c.Now()) {
			if e.Res.ID == r.ID {
				t.Fatalf("%s: %s a second before %s", r.ID, e.Op, at.Format("15:04:05"))
			}
		}
		c.t = at
		for _, e := range m.checkOrphans(c.Now()) {
			if e.Res.ID == r.ID && e.Op == want {
				delete(m.pending, r.ID)
				return
			}
		}
		t.Fatalf("%s: no %s at %s", r.ID, want, at.Format("15:04:05"))
	}
	step(srv, "pause")
	paused := c.Now()
	srv.PausedAt = &paused
	m.checkOrphans(c.Now())
	if srv.NextStep != "stop" || !srv.NextFrom.Equal(paused) {
		t.Fatalf("paused: next %q from %v", srv.NextStep, srv.NextFrom)
	}
	step(srv, "stop")
	// The simulator was stopped alongside; seen again from the start, its stop is on time too.
	delete(m.pending, sim.ID)
	sim.OrphanSince = nil
	m.checkOrphans(c.Now())
	step(sim, "stop")
}

// State written before the freed log existed: its stops still count in the totals, at what
// they gave back.
func TestFreedLogIsSeededFromPastStops(t *testing.T) {
	m, c := newTest(t, 8)
	m.st.Stopped = []*model.Stopped{
		{Resource: model.Resource{ID: "kind:a", Source: "docker", MemMB: 2048}, StoppedAt: c.Now().Add(-time.Hour)},
		{Resource: model.Resource{ID: "srv:1", Source: "procs", MemMB: 512}, StoppedAt: c.Now().Add(-10 * 24 * time.Hour)},
	}
	m.seedFreed()
	if mb, n := m.freedMB(c.Now().AddDate(0, 0, -7), c.Now()); n != 1 || mb != 2048*returnDefault {
		t.Fatalf("week: %d stops, %.0f MB", n, mb)
	}
	m.seedFreed() // once only
	if len(m.st.Freed) != 2 {
		t.Fatalf("seeded twice: %d", len(m.st.Freed))
	}
}

// A Docker lease's use is the VM's growth beyond its containers'. Containers stopping (the VM
// keeps what they held) or someone else's stack starting is no one's work.
func TestDockerLeaseIsNotChargedForContainers(t *testing.T) {
	m, c := newTest(t, 8)
	m.dockerFullMB, m.dockerContainersMB, m.dockerMeasured = 6144, 4096, true
	m.learnSession(100, "s", "claude")
	procs := map[int]*sys.Proc{100: {PID: 100, PPID: 1, Exe: "claude", Args: "claude", Start: c.Now().Add(-time.Hour)}}
	if r := acquire(t, m, agent("s"), model.KDockerBuild, 1); !r.Granted {
		t.Fatal(r.Reason)
	}
	l := m.findLease("s", model.KDockerBuild)
	measure := func(full, containers float64) float64 {
		m.dockerFullMB, m.dockerContainersMB = full, containers
		c.Add(time.Minute)
		m.updateFootprints(procs, c.Now())
		return l.UsedMB
	}
	if u := measure(7168, 5120); u != 0 {
		t.Fatalf("an unrelated stack started: charged %.0f MB", u)
	}
	if u := measure(7168, 0); u != 0 {
		t.Fatalf("the stacks stopped and the VM kept their memory: charged %.0f MB", u)
	}
	if u := measure(8168, 0); u != 1000 {
		t.Fatalf("the build grew the VM by 1000 MB: charged %.0f MB", u)
	}
	// Containers going up and down do not wear its use away.
	for i := 0; i < 5; i++ {
		measure(9168, 1000)
		measure(8168, 0)
	}
	if u := measure(8168, 0); u != 1000 {
		t.Fatalf("after containers came and went: %.0f MB, want 1000", u)
	}
}

// Docker not answering: its containers keep their last view, and the VM is credited with that,
// not with nothing (which made the VM jump by the containers' memory).
func TestDockerVMKeepsContainersWhenDockerFails(t *testing.T) {
	m, c := newTest(t, 8)
	m.st.Resources["compose:app"] = &model.Resource{ID: "compose:app", Kind: model.RKContainers, Source: "docker", MemMB: 4096}
	vm := &model.Resource{ID: "docker-vm", Kind: model.RKDockerVM, Source: "procs", MemMB: 6144}
	m.merge(discover.Result{Resources: []*model.Resource{vm}, Failed: map[string]error{"docker": errors.New("hung")}}, nil, nil, c.Now())
	if want := 6144 - m.dockerVMIdle() - 4096; vm.MemMB != want {
		t.Fatalf("VM %.0f MB, want %.0f", vm.MemMB, want)
	}
	if m.dockerContainersMB != 4096 {
		t.Fatalf("containers %.0f", m.dockerContainersMB)
	}
}

// A failed vm_stat is not a reading: taken as one, the next good reading counted every swap-out
// since boot as the last minute's, and refused everything for a minute.
func TestFailedSwapReadingIsIgnored(t *testing.T) {
	m, c := newTest(t, 8)
	for i := 0; i < 25; i++ {
		c.Add(3 * time.Second)
		m.takeMem(sys.MemStats{Swapouts: 500000}, c.Now())
	}
	c.Add(3 * time.Second)
	m.takeMem(sys.MemStats{SwapUnread: true}, c.Now())
	for i := 0; i < 25; i++ {
		c.Add(3 * time.Second)
		m.takeMem(sys.MemStats{Swapouts: 500000}, c.Now())
		if m.swapRate != 0 {
			t.Fatalf("rate %.0f MB/min after a failed reading", m.swapRate)
		}
	}
}

// An unused compose lease is not "started" by someone else growing Docker's VM.
func TestDockerVMGrowthIsNotTakeUp(t *testing.T) {
	m, c := newTest(t, 8)
	m.dockerFullMB, m.dockerMeasured = 2000, true
	m.learnSession(100, "s", "claude")
	procs := map[int]*sys.Proc{100: {PID: 100, PPID: 1, Exe: "claude", Args: "claude", Start: c.Now().Add(-time.Hour)}}
	m.updateFootprints(procs, c.Now())
	if r := acquire(t, m, agent("s"), model.KCompose, 2); !r.Granted {
		t.Fatal(r.Reason)
	}
	l := m.findLease("s", model.KCompose)
	off := c.Now()
	l.Offered = &off
	m.dockerFullMB = 2200 // someone's pull
	m.updateFootprints(procs, c.Now())
	if l.InsideMB != 200 || l.UsedMB != 200 {
		t.Fatalf("the VM's growth was not tracked: inside %.0f, used %.0f", l.InsideMB, l.UsedMB)
	}
	m.checkLeases(c.Now())
	if l.Offered == nil {
		t.Fatal("taken up on the VM's growth")
	}
	c.Add(16 * time.Minute)
	l.Offered = nil
	m.checkLeases(c.Now())
	if m.st.Leases[l.ID] != nil {
		t.Fatal("kept although nothing was started under it")
	}
}

// What a session starts after a later lease is that lease's, even once the later lease holds it
// as a resource or has been released: charged back to the earlier build, it overran.
func TestBuildIsNotChargedForALaterLeasesProcesses(t *testing.T) {
	m, c := newTest(t, 8)
	t0 := c.Now()
	m.footprint = func(pid int) (float64, bool) { return map[int]float64{102: 2500}[pid], pid == 102 }
	m.learnSession(100, "s", "claude")
	procs := map[int]*sys.Proc{
		100: {PID: 100, PPID: 1, Exe: "claude", Args: "claude", Start: t0.Add(-time.Hour)},
		102: {PID: 102, PPID: 100, Exe: "node", Args: "node server.js", Start: t0.Add(time.Minute + time.Second)},
	}
	m.updateFootprints(procs, t0)
	if r := acquire(t, m, agent("s"), model.KCargoBuild, 1); !r.Granted {
		t.Fatal(r.Reason)
	}
	bld := m.findLease("s", model.KCargoBuild)
	c.Add(time.Minute)
	if r := acquire(t, m, agent("s"), model.KServer, 2.5); !r.Granted {
		t.Fatal(r.Reason)
	}
	srv := m.findLease("s", model.KServer)
	// The server is bound as a resource of its lease.
	m.st.Resources["srv:3000"] = &model.Resource{ID: "srv:3000", Kind: model.RKServer, PID: 102, MemMB: 2500, LeaseID: srv.ID}
	srv.Resources = []string{"srv:3000"}
	c.Add(time.Minute)
	m.updateFootprints(procs, c.Now())
	if bld.UsedMB != 0 {
		t.Fatalf("the build was charged the bound server's %.0f MB", bld.UsedMB)
	}
	// Released while the server still runs.
	delete(m.st.Leases, srv.ID)
	delete(m.st.Resources, "srv:3000")
	c.Add(time.Minute)
	m.updateFootprints(procs, c.Now())
	if bld.UsedMB != 0 || bld.Overran {
		t.Fatalf("after the server's lease ended the build was charged %.0f MB", bld.UsedMB)
	}
}

// Holding 1 GB and asking for 6: the request waits for the difference, and is granted it.
func TestALargerAskWaitsInTheQueue(t *testing.T) {
	m, c := newTest(t, 3)
	if r := acquire(t, m, agent("s"), model.KServer, 1); !r.Granted {
		t.Fatal(r.Reason)
	}
	if r := acquire(t, m, agent("s"), model.KServer, 6); r.Granted || r.Queue != 1 {
		t.Fatalf("granted or not queued: %+v", r)
	}
	m.grantWaiters(c.Now(), false)
	if len(m.waiters) != 1 {
		t.Fatal("dropped from the queue because it holds a smaller lease")
	}
	m.mem.AvailableMB = 12 * 1024
	m.grantWaiters(c.Now(), false)
	if held := m.heldGB("s", model.KServer); held != 6 {
		t.Fatalf("holds %.1f GB, want 6", held)
	}
}

// A restored stop gave nothing back in the end.
func TestRestoreTakesTheStopOutOfTheTotals(t *testing.T) {
	m, c := newTest(t, 8)
	m.recordStopped(model.Resource{ID: "kind:a", Source: "procs", MemMB: 3000}, "test", "", c.Now())
	st := m.st.Stopped[len(m.st.Stopped)-1]
	m.applyEffects([]EffectResult{{Effect: Effect{Op: "restore", Res: st.Resource, Stopped: st}}})
	if mb, n := m.freedMB(c.Now().Add(-time.Hour), c.Now()); mb != 0 || n != 0 {
		t.Fatalf("still counted: %d stops, %.0f MB", n, mb)
	}
}

// While everything is let through, nothing is paused or stopped: the countdown says it only
// would. What is being paused or stopped right now says so.
func TestNextStepWhileHeldAndUnderWay(t *testing.T) {
	m, c := newTest(t, 8)
	m.cfg.DryRun = false
	r := &model.Resource{ID: "srv:1", Kind: model.RKServer, Name: "srv", MemMB: 500, Source: "procs", FirstSeen: c.Now()}
	m.st.Resources[r.ID] = r
	until := c.Now().Add(time.Hour)
	m.st.BypassUntil = &until
	m.checkOrphans(c.Now())
	if r.NextStep != "pause" || !r.NextDry {
		t.Fatalf("held: %q dry %v", r.NextStep, r.NextDry)
	}
	m.st.BypassUntil = nil
	m.pending[r.ID] = "stop"
	m.checkOrphans(c.Now())
	if r.NextStep != "stop" || r.NextAt == nil || !r.NextAt.Equal(c.Now()) {
		t.Fatalf("under way: %q at %v", r.NextStep, r.NextAt)
	}
}

// The VM not measured for a sweep (Docker restarting, the process table unread, the daemon just
// started): what it holds when measured again is a new base, not the lease's growth.
func TestDockerVMGapIsNotGrowth(t *testing.T) {
	m, c := newTest(t, 8)
	m.learnSession(100, "s", "claude")
	procs := map[int]*sys.Proc{100: {PID: 100, PPID: 1, Exe: "claude", Args: "claude", Start: c.Now().Add(-time.Hour)}}
	// Granted before the first sweep (the daemon just restarted): nothing measured yet.
	if r := acquire(t, m, agent("s"), model.KDockerBuild, 1); !r.Granted {
		t.Fatal(r.Reason)
	}
	l := m.findLease("s", model.KDockerBuild)
	sweep := func(full, cont float64, measured bool) float64 {
		m.dockerFullMB, m.dockerContainersMB, m.dockerMeasured = full, cont, measured
		c.Add(30 * time.Second)
		m.updateFootprints(procs, c.Now())
		return l.UsedMB
	}
	for i, st := range []struct {
		full, cont float64
		measured   bool
	}{{6144, 4096, true}, {0, 4096, false}, {6144, 4096, true}, {0, 0, false}, {2048, 0, true}, {6144, 4096, true}} {
		if u := sweep(st.full, st.cont, st.measured); u != 0 {
			t.Fatalf("step %d (%+v): charged %.0f MB", i, st, u)
		}
	}
	if u := sweep(7144, 4096, true); u != 1000 {
		t.Fatalf("real growth: %.0f MB, want 1000", u)
	}
	// One low reading, then back: the recovery is not growth.
	sweep(3000, 4096, true)
	if u := sweep(7144, 4096, true); u != 1000 {
		t.Fatalf("after a low reading: %.0f MB, want 1000", u)
	}
}

// A compose lease holding its containers: once they stop, what the VM keeps is not its use.
func TestComposeDownIsNotCharged(t *testing.T) {
	m, c := newTest(t, 8)
	m.dockerFullMB, m.dockerMeasured = 2000, true
	m.learnSession(100, "s", "claude")
	procs := map[int]*sys.Proc{100: {PID: 100, PPID: 1, Exe: "claude", Args: "claude", Start: c.Now().Add(-time.Hour)}}
	if r := acquire(t, m, agent("s"), model.KCompose, 3); !r.Granted {
		t.Fatal(r.Reason)
	}
	l := m.findLease("s", model.KCompose)
	addRes(m, "compose:app", model.RKContainers, 3000, l.ID)
	m.dockerFullMB, m.dockerContainersMB = 5000, 3000
	c.Add(time.Minute)
	m.updateFootprints(procs, c.Now())
	// compose down: the containers go, the VM keeps their memory.
	delete(m.st.Resources, "compose:app")
	l.Resources = nil
	m.dockerContainersMB = 0
	c.Add(time.Minute)
	m.updateFootprints(procs, c.Now())
	if l.UsedMB != 0 {
		t.Fatalf("charged %.0f MB the VM kept", l.UsedMB)
	}
}

// A queued Docker build is taken up when its command runs (the hook asks for it again), not by
// the VM growing: an 80 MB drift or someone's pull deleted its grant notice.
func TestDockerBuildIsTakenUpByItsCommand(t *testing.T) {
	m, c := newTest(t, 8)
	m.dockerFullMB, m.dockerMeasured = 2000, true
	m.learnSession(100, "s", "claude")
	procs := map[int]*sys.Proc{100: {PID: 100, PPID: 1, Exe: "claude", Args: "claude", Start: c.Now().Add(-time.Hour)}}
	m.updateFootprints(procs, c.Now())
	if r := acquire(t, m, agent("s"), model.KDockerBuild, 2); !r.Granted {
		t.Fatal(r.Reason)
	}
	l := m.findLease("s", model.KDockerBuild)
	off := c.Now()
	l.Offered = &off
	m.dockerFullMB = 3500
	c.Add(30 * time.Second)
	m.updateFootprints(procs, c.Now())
	m.checkLeases(c.Now())
	if l.Offered == nil {
		t.Fatal("taken up by the VM's growth")
	}
	if r := acquire(t, m, agent("s"), model.KDockerBuild, 0); !r.Granted || !r.Reused {
		t.Fatalf("its command's request: %+v", r)
	}
	if l.Offered != nil {
		t.Fatal("not taken up by its command")
	}
}

// Two Docker leases: the VM's growth is handed out once, oldest first, each up to its promise.
// Divided by the number of leases, a second lease halved the first's use (1.5 GB less headroom
// for nothing), and releasing it made the first's jump.
func TestDockerGrowthIsHandedOutOnce(t *testing.T) {
	m, c := newTest(t, 16)
	m.dockerFullMB, m.dockerMeasured = 2000, true
	m.learnSession(100, "a", "claude")
	m.learnSession(200, "b", "claude")
	procs := map[int]*sys.Proc{
		100: {PID: 100, PPID: 1, Exe: "claude", Args: "claude", Start: c.Now().Add(-time.Hour)},
		200: {PID: 200, PPID: 1, Exe: "claude", Args: "claude", Start: c.Now().Add(-time.Hour)},
	}
	sweep := func(full float64) {
		m.dockerFullMB = full
		c.Add(30 * time.Second)
		m.updateFootprints(procs, c.Now())
	}
	acquire(t, m, agent("a"), model.KDockerBuild, 2)
	a := m.findLease("a", model.KDockerBuild)
	sweep(5000)
	if a.InsideMB != 2048 {
		t.Fatalf("a: %.0f, want its 2048 promise", a.InsideMB)
	}
	sweep(3500)
	acquire(t, m, agent("b"), model.KDockerBuild, 2)
	b := m.findLease("b", model.KDockerBuild)
	sweep(3500)
	if a.InsideMB != 1500 || b.InsideMB != 0 {
		t.Fatalf("a second lease changed the first: a %.0f, b %.0f", a.InsideMB, b.InsideMB)
	}
	sweep(4500)
	if a.InsideMB != 2048 || b.InsideMB != 452 {
		t.Fatalf("2500 MB of growth: a %.0f (want 2048), b %.0f (want 452)", a.InsideMB, b.InsideMB)
	}
	m.dropLease(b, "test")
	sweep(4500)
	if a.InsideMB != 2048 {
		t.Fatalf("b released: a %.0f", a.InsideMB)
	}
}

// Asking for more of the same kind gets a second lease for the difference. The build's
// processes, started under the first, are measured with both: the first was charged all of
// them, flagged as an overrun and its session distrusted, and the second showed nothing used.
func TestMoreOfTheSameKindIsMeasuredAsOne(t *testing.T) {
	m, c := newTest(t, 12)
	t0 := c.Now()
	m.footprint = func(pid int) (float64, bool) { mb, ok := map[int]float64{102: 3072}[pid]; return mb, ok }
	m.learnSession(100, "s", "claude")
	if r := acquire(t, m, agent("s"), model.KCargoBuild, 1); !r.Granted {
		t.Fatal(r.Reason)
	}
	c.Add(time.Minute)
	if r := acquire(t, m, agent("s"), model.KCargoBuild, 3); !r.Granted || r.Reused {
		t.Fatalf("asking for 3: %+v", r)
	}
	var first, extra *model.Lease
	for _, l := range m.st.Leases {
		if l.Created.Equal(t0) {
			first = l
		} else {
			extra = l
		}
	}
	procs := map[int]*sys.Proc{
		100: {PID: 100, PPID: 1, Exe: "claude", Args: "claude", Start: t0.Add(-time.Hour)},
		102: {PID: 102, PPID: 100, Exe: "rustc", Args: "rustc", Start: t0.Add(10 * time.Second)},
	}
	c.Add(time.Minute)
	m.updateFootprints(procs, c.Now())
	if first.UsedMB != 1024 || extra.UsedMB != 2048 {
		t.Fatalf("first %.0f (want 1024), extra %.0f (want 2048)", first.UsedMB, extra.UsedMB)
	}
	if first.Overran || extra.Overran || c.Now().Before(m.st.Sessions["s"].DistrustUntil) {
		t.Fatal("flagged as an overrun for asking correctly")
	}
}

// Holding 1 GB and waiting for 6: if the 1 GB goes, the request waits for the whole 6.
func TestALargerAskNeedsWhatItNoLongerHolds(t *testing.T) {
	m, c := newTest(t, 3)
	acquire(t, m, agent("s"), model.KServer, 1)
	if r := acquire(t, m, agent("s"), model.KServer, 6); r.Granted {
		t.Fatal("granted without room")
	}
	delete(m.st.Leases, m.findLease("s", model.KServer).ID)
	m.mem.AvailableMB = 12 * 1024
	m.grantWaiters(c.Now(), false)
	if held := m.heldGB("s", model.KServer); held != 6 {
		t.Fatalf("holds %.1f GB, want 6", held)
	}
}

// A lease a sweep records for a server already running does not cut off a build's processes.
func TestRecordedLeaseDoesNotCutABuild(t *testing.T) {
	m, c := newTest(t, 8)
	t0 := c.Now()
	m.footprint = func(pid int) (float64, bool) { return map[int]float64{103: 900}[pid], pid == 103 }
	m.learnSession(100, "s", "claude")
	if r := acquire(t, m, agent("s"), model.KCargoBuild, 2); !r.Granted {
		t.Fatal(r.Reason)
	}
	bld := m.findLease("s", model.KCargoBuild)
	m.st.Leases["rec"] = &model.Lease{ID: "rec", Session: "s", Kind: model.KServer, MemGB: 0.1, Purpose: "started without a lease",
		Created: t0.Add(time.Minute), Expires: t0.Add(time.Hour), LastUsed: t0, Resources: []string{"srv:1"}}
	procs := map[int]*sys.Proc{
		100: {PID: 100, PPID: 1, Exe: "claude", Args: "claude", Start: t0.Add(-time.Hour)},
		103: {PID: 103, PPID: 100, Exe: "rustc", Args: "rustc", Start: t0.Add(2 * time.Minute)},
	}
	c.Add(3 * time.Minute)
	m.updateFootprints(procs, c.Now())
	if bld.UsedMB != 900 {
		t.Fatalf("build charged %.0f MB, want its rustc's 900", bld.UsedMB)
	}
}

// A memory reading taken for a request while a sweep runs counts for what is available, not as
// a swap sample (taken out of the ticks' order, it upset the rate), and only when the last
// reading is old.
func TestPeekedMemoryIsNotASwapSample(t *testing.T) {
	m, c := newTest(t, 8)
	m.takeMem(sys.MemStats{AvailableMB: 1000, Swapouts: 500}, c.Now())
	c.Add(2 * time.Second)
	m.peekWith(sys.MemStats{AvailableMB: 9000, Swapouts: 100})
	if m.mem.AvailableMB != 1000 {
		t.Fatal("a fresh reading was replaced")
	}
	c.Add(10 * time.Second)
	m.peekWith(sys.MemStats{AvailableMB: 6000, Swapouts: 900})
	if m.mem.AvailableMB != 6000 {
		t.Fatalf("available %.0f, want the peeked 6000", m.mem.AvailableMB)
	}
	if n := len(m.swapSamples); n != 1 {
		t.Fatalf("%d swap samples, want only the tick's", n)
	}
}

// Docker not answering: the containers' figure is its last view, so no Docker lease is credited
// with the VM's growth until it answers.
func TestNoVMCreditWhileDockerIsSilent(t *testing.T) {
	m, c := newTest(t, 8)
	m.learnSession(100, "s", "claude")
	procs := map[int]*sys.Proc{100: {PID: 100, PPID: 1, Exe: "claude", Args: "claude", Start: c.Now().Add(-time.Hour)}}
	sweep := func(full float64, fail bool) float64 {
		res := discover.Result{Resources: []*model.Resource{{ID: "docker-vm", Kind: model.RKDockerVM, Source: "procs", MemMB: full}}}
		if fail {
			res.Failed = map[string]error{discover.SrcDocker: errors.New("hung")}
		}
		c.Add(30 * time.Second)
		m.merge(res, procs, nil, c.Now())
		m.updateFootprints(procs, c.Now())
		if l := m.findLease("s", model.KDockerBuild); l != nil {
			return l.InsideMB
		}
		return -1
	}
	sweep(2000, false)
	acquire(t, m, agent("s"), model.KDockerBuild, 2)
	if u := sweep(3000, false); u != 1000 {
		t.Fatalf("growth %.0f, want 1000", u)
	}
	if u := sweep(3500, true); u != 0 {
		t.Fatalf("credited %.0f MB while Docker was not answering", u)
	}
	if u := sweep(3500, false); u != 1500 {
		t.Fatalf("Docker answered again: %.0f, want 1500 since the grant", u)
	}
}

// Restoring a stop made together with others, recorded before entries had IDs: the entry taken
// out is the one with what this stop gave back.
func TestRestoreTakesOutItsOwnLegacyEntry(t *testing.T) {
	m, c := newTest(t, 8)
	at := c.Now().Add(-time.Hour)
	m.st.Freed = []model.FreedMark{{At: at, MB: 3000}, {At: at, MB: 500}}
	m.unfree(&model.Stopped{Resource: model.Resource{ID: "b"}, StoppedAt: at, FreedMB: 500})
	if len(m.st.Freed) != 1 || m.st.Freed[0].MB != 3000 {
		t.Fatalf("left %+v, want the 3000 MB stop", m.st.Freed)
	}
}

// The app offers Restart exactly when the daemon would accept one: a VM restarted a minute ago
// is not restarted on AgentRM's own, but you may; one with a container running is refused.
func TestRestartIsOfferedWhenItWouldBeAccepted(t *testing.T) {
	m, c := newTest(t, 8)
	vm := &model.Resource{ID: "docker-vm", Kind: model.RKDockerVM, Name: "Docker", MemMB: 2000, Source: "procs", FirstSeen: c.Now()}
	m.st.Resources[vm.ID] = vm
	m.restartedAt[vm.ID] = &restartMark{at: c.Now().Add(-time.Minute), sampled: true}
	m.checkOrphans(c.Now())
	if vm.OrphanSince != nil || vm.RestartBlocked != "" {
		t.Fatalf("just restarted: orphan %v, blocked %q", vm.OrphanSince, vm.RestartBlocked)
	}
	addRes(m, "compose:app", model.RKContainers, 500, "")
	m.checkOrphans(c.Now())
	if vm.RestartBlocked == "" {
		t.Fatal("a container runs in it, but Restart is offered")
	}
}
