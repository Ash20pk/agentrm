package manager

import (
	"math"
	"regexp"
	"strconv"
	"testing"
	"time"

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
	vm := &model.Resource{ID: "docker-vm", Kind: model.RKDockerVM, Source: "docker", MemMB: 500}
	m.st.Resources[vm.ID] = vm
	m.learnSession(100, "s", "claude")
	procs := map[int]*sys.Proc{100: {PID: 100, PPID: 1, Exe: "claude", Args: "claude", Start: t0.Add(-time.Hour)}}
	m.updateFootprints(procs, t0)
	if r := acquire(t, m, agent("s"), model.KDockerBuild, 3); !r.Granted {
		t.Fatal(r.Reason)
	}
	vm.MemMB = 2500
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
		{1.36, 2.14, 0.01, 0.75}, {0.3, 0.2, 0, 0.5},
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
