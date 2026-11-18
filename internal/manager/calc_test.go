package manager

import (
	"testing"
	"time"

	"agentrm/internal/model"
	"agentrm/internal/sys"
)

// A build is charged for what it started, by footprint: not for a simulator or a Gradle daemon
// the session already had, and not for shared pages counted once per process.
func TestBuildIsMeasuredByWhatItStarted(t *testing.T) {
	m, c := newTest(t, 8)
	now := c.Now()
	fp := map[int]float64{101: 1500, 102: 300, 103: 400}
	m.footprint = func(pid int) (float64, bool) { v, ok := fp[pid]; return v, ok }
	procs := map[int]*sys.Proc{
		100: {PID: 100, PPID: 1, Exe: "claude", Args: "claude", Start: now.Add(-time.Hour)},
		101: {PID: 101, PPID: 100, Exe: "launchd_sim", Args: "launchd_sim", RSSMB: 1800, Start: now.Add(-30 * time.Minute)},
		102: {PID: 102, PPID: 100, Exe: "xcodebuild", Args: "xcodebuild build", RSSMB: 900, Start: now.Add(time.Second)},
		103: {PID: 103, PPID: 102, Exe: "swift-frontend", Args: "swift-frontend", RSSMB: 1100, Start: now.Add(2 * time.Second)},
	}
	m.learnSession(100, "s", "claude")
	m.st.Leases["L"] = &model.Lease{ID: "L", Session: "s", Kind: model.KXcodeBuild, MemGB: 3, Created: now,
		Expires: now.Add(time.Hour), LastUsed: now}
	c.Add(5 * time.Second)
	m.updateFootprints(procs, c.Now())
	l := m.st.Leases["L"]
	if l.UsedMB != 700 || l.PeakMB != 700 {
		t.Fatalf("build charged %.0f MB (peak %.0f), want 700: the simulator started before it, and footprint not RSS", l.UsedMB, l.PeakMB)
	}
	if got := m.materialisedMB(l); got != 700 {
		t.Fatalf("materialised %.0f", got)
	}
}

// One odd run must not double every later estimate. TinyDays' learned xcode-build peak was
// 1793 MB with a spread of 1272 MB: charged 4.2 GB before, now mean plus at most half again.
func TestEstimateSpreadIsCapped(t *testing.T) {
	m, _ := newTest(t, 8)
	m.st.Learned["xcode-build@TinyDays"], m.st.LearnedDev["xcode-build@TinyDays"] = 1793, 1272
	if gb := m.estimateGB("xcode-build", "/Volumes/Dev/Projects/apps/TinyDays"); gb > 2.7 || gb < 2.5 {
		t.Fatalf("charged %.2f GB, want ~2.6", gb)
	}
	m.st.Learned["cargo-build"], m.st.LearnedDev["cargo-build"] = 2000, 100
	if gb := m.estimateGB("cargo-build", ""); gb < 2.24 || gb > 2.25 {
		t.Fatalf("a steady kind: %.3f GB, want 2000*1.15", gb)
	}
}

// Releasing a granted build that has not started yet frees its whole promise; a stopped
// container gives back only part of what Docker counted.
func TestReclaimCountsWhatReallyComesBack(t *testing.T) {
	m, c := newTest(t, 1)
	m.cfg.DryRun = false
	now := c.Now()
	since := now.Add(-time.Hour)
	ctr := addRes(m, "compose:big", model.RKContainers, 2048, "")
	ctr.OrphanSince = &since
	m.st.Leases["fresh"] = &model.Lease{ID: "fresh", Session: "s2", Kind: model.KCargoBuild, MemGB: 2, TTL: 30,
		Verified: false, LastUsed: now.Add(-10 * time.Minute), Expires: now.Add(time.Hour), Resources: []string{"x"}}
	m.st.Leases["other"] = &model.Lease{ID: "other", Session: "s3", Kind: model.KCargoBuild, MemGB: 2, TTL: 30,
		Verified: false, LastUsed: now.Add(-9 * time.Minute), Expires: now.Add(time.Hour), Resources: []string{"y"}}
	// 1 GB available - 0.75 reserve: a 2.25 GB request needs ~2 GB. The container counts 1 GB
	// (half of 2 GB, nothing learned yet), the first lease its 2 GB promise: enough.
	if r := acquire(t, m, agent("waiting"), "cargo-build", 2.25); r.Granted {
		t.Fatal("granted without room")
	}
	if !ctr.Urgent {
		t.Fatal("the container should be reclaimed")
	}
	pressed := 0
	for _, id := range []string{"fresh", "other"} {
		if m.st.Leases[id].IdleAsked != nil {
			pressed++
		}
	}
	if pressed != 1 {
		t.Fatalf("pressed %d holders; one lease's promise plus half the container covers it", pressed)
	}
}

func TestReturnRatioIsLearnedFromStops(t *testing.T) {
	m, c := newTest(t, 2)
	if r := m.returnRatio("docker"); r != returnDefault {
		t.Fatalf("default %.2f", r)
	}
	if m.returnRatio("procs") != 1 {
		t.Fatal("a process gives back what it held")
	}
	// Yesterday: two kind clusters, 2.8 GB counted, ~0.8 GB came back.
	for i := 0; i < 3; i++ {
		m.mem.AvailableMB = 2500
		m.noteStopped(model.Resource{ID: "kind:a", Source: "docker", MemMB: 1400}, c.Now())
		m.noteStopped(model.Resource{ID: "kind:b", Source: "docker", MemMB: 1400}, c.Now())
		c.Add(returnSettle)
		m.takeMem(sys.MemStats{AvailableMB: 2500 + 800}, c.Now())
	}
	if r := m.returnRatio("docker"); r < 0.27 || r > 0.30 {
		t.Fatalf("learned %.2f, want ~0.29 (0.8 of 2.8 GB)", r)
	}
}

// A Gradle build holds its daemon as a resource. Its growth was also recorded as inferred
// heavy work, charging the same memory twice (2026-11-18 08:10).
func TestGrowthDuringALeaseIsNotChargedTwice(t *testing.T) {
	m, c := newTest(t, 8)
	now := c.Now()
	procs := map[int]*sys.Proc{100: {PID: 100, PPID: 1, Exe: "claude", Args: "claude", Start: now.Add(-time.Hour)}}
	m.learnSession(100, "s", "claude")
	m.updateFootprints(procs, now) // the idle baseline
	m.st.Leases["G"] = &model.Lease{ID: "G", Session: "s", Kind: model.KGradle, MemGB: 2.7, Created: now,
		Expires: now.Add(time.Hour), LastUsed: now, Resources: []string{"gradle:1"}}
	procs[101] = &sys.Proc{PID: 101, PPID: 100, Exe: "java", Args: "GradleDaemon", RSSMB: 2048, Start: now.Add(time.Second)}
	c.Add(30 * time.Second)
	m.updateFootprints(procs, c.Now())
	for _, l := range m.st.Leases {
		if inferred(l) {
			t.Fatalf("the Gradle build's growth was charged again as %s", l.ID)
		}
	}
}
