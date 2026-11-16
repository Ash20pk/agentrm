package main

import (
	"strings"
	"testing"
	"time"
)

func memDaemon(availGB float64) *Daemon {
	d := newTestDaemon()
	d.mem = MemStats{TotalMB: 16384, WiredMB: 3000, AvailableMB: availGB * 1024, PressureLevel: 1}
	return d
}

func TestActiveTextIgnoresMentions(t *testing.T) {
	cases := map[string]string{
		`echo "kind create cluster"`:                                     "",
		`git commit -m "docker build fix"`:                               "",
		`grep -r "cargo build" docs | head`:                              "",
		`cd svc && docker build -t x .`:                                  "docker-build",
		`FOO=1 sudo docker build .`:                                      "docker-build",
		`echo start; cargo test --all`:                                   "cargo-build",
		`agentrm acquire --kind cluster --purpose "kind create cluster"`: "",
		`X=$(docker build -q .)`:                                         "docker-build",
		`/opt/homebrew/bin/kind create cluster --name a`:                 "cluster",
	}
	for cmd, want := range cases {
		if got := classify(cmd); got != want {
			t.Errorf("classify(%q) = %q, want %q", cmd, got, want)
		}
	}
	res := map[string]*Resource{"kind:obx": {ID: "kind:obx", Kind: RKCluster, Containers: []string{"obx-control-plane"}}}
	for _, cmd := range []string{`echo "using kind-obx"`, `grep kind-obx ~/.kube/config`, `agentrm claim kind:obx --purpose "kubectl --context kind-obx"`} {
		if got := touchedBy(cmd, "", res, ""); len(got) != 0 {
			t.Errorf("a mention counted as use: %q -> %v", cmd, got)
		}
	}
	if got := touchedBy(`echo go; kubectl --context kind-obx get pods`, "", res, ""); len(got) != 1 {
		t.Errorf("real use after an echo was missed: %v", got)
	}
}

func TestHeredocBodiesAreData(t *testing.T) {
	write := "cat > notes.md <<'EOF'\nkind create cluster --name demo\ndocker build -t x .\nEOF\ngo test ./..."
	if got := classify(write); got != "" {
		t.Errorf("text written to a file was taken as a command: %q", got)
	}
	py := "python3 - <<EOF\nprint('cargo build')\nEOF"
	if got := classify(py); got != "" {
		t.Errorf("a script's text was taken as a command: %q", got)
	}
	sh := "bash <<EOF\ndocker build -t x .\nEOF"
	if got := classify(sh); got != "docker-build" {
		t.Errorf("a heredoc fed to a shell runs its commands: got %q", got)
	}
	after := "cat > f <<EOF\nhello\nEOF\ndocker build ."
	if got := classify(after); got != "docker-build" {
		t.Errorf("a real command after a heredoc was missed: got %q", got)
	}
}

func TestDetachedProcessIsNotAPerson(t *testing.T) {
	procs := map[int]*proc{
		10: {pid: 10, ppid: 1, exe: "/Applications/Warp.app/Contents/MacOS/stable", args: "stable"},
		11: {pid: 11, ppid: 10, exe: "-zsh", args: "-zsh"},
		12: {pid: 12, ppid: 11, exe: "agentrm", args: "agentrm pin x"},
		20: {pid: 20, ppid: 1, exe: "sh", args: "sh -c sleep 3; agentrm pin x"},
		21: {pid: 21, ppid: 20, exe: "agentrm", args: "agentrm pin x"},
		30: {pid: 30, ppid: 1, exe: "/Applications/AgentRM.app/Contents/MacOS/AgentRM", args: "AgentRM"},
		31: {pid: 31, ppid: 30, exe: "/Applications/AgentRM.app/Contents/Helpers/agentrm", args: "agentrm pin x"},
	}
	if gui, app := guiAncestor(procs, 12); !gui || app {
		t.Error("a command typed in a terminal should be a person, not the app")
	}
	if gui, _ := guiAncestor(procs, 21); gui {
		t.Error("a detached process was treated as a person")
	}
	if gui, app := guiAncestor(procs, 31); !gui || !app {
		t.Error("the app's own helper was not recognised")
	}
}

func TestAdmissionUsesMeasuredHeadroom(t *testing.T) {
	d := memDaemon(4) // 4 GB available, 1 GB kept free
	now := time.Now()
	if ok, why := d.admit("a", "cluster", 2.5*1024, now); !ok {
		t.Fatalf("2.5 GB refused with 3 GB of headroom: %s", why)
	}
	// A granted lease that has not taken its memory yet is a promise that counts.
	d.st.Leases["L1"] = &Lease{ID: "L1", Session: "a", Kind: "cluster", MemGB: 2.5, LastUsed: now}
	if ok, _ := d.admit("b", "cargo-build", 1024, now); ok {
		t.Fatal("granted 1 GB although 2.5 of the 3 GB headroom is promised")
	}
	// Once the promise is real use, it is in the measurement, not counted twice.
	d.st.Resources["kind:x"] = &Resource{ID: "kind:x", Kind: RKCluster, MemMB: 2400, LeaseID: "L1"}
	d.st.Leases["L1"].Resources = []string{"kind:x"}
	d.mem.AvailableMB = 4*1024 - 2400
	d.waiters = nil
	if ok, why := d.admit("b", "server", 400, now); !ok {
		t.Fatalf("refused 0.4 GB with 0.5 GB of headroom after the lease materialised: %s", why)
	}
}

func TestAdmissionRefusesWhileThrashing(t *testing.T) {
	d := memDaemon(8)
	d.mem.PressureLevel = 2
	if ok, why := d.admit("a", "server", 100, time.Now()); ok || !strings.Contains(why, "memory pressure") {
		t.Fatalf("granted under kernel memory pressure: %v %s", ok, why)
	}
	d.mem.PressureLevel, d.swapRate = 1, 900
	if ok, why := d.admit("a", "server", 100, time.Now()); ok || !strings.Contains(why, "swapping") {
		t.Fatalf("granted while swapping out 900 MB/min: %v %s", ok, why)
	}
	d.swapRate = 0
	if ok, _ := d.admit("a", "server", 100, time.Now()); !ok {
		t.Fatal("still refused after the pressure went away")
	}
}

func TestQueueIsFirstComeFirstServed(t *testing.T) {
	d := memDaemon(3) // 2 GB headroom
	now := time.Now()
	if ok, _ := d.admit("big", "cluster", 3*1024, now); ok {
		t.Fatal("3 GB granted with 2 GB headroom")
	}
	// A later, smaller request must not jump the queue and starve the first one.
	if ok, why := d.admit("small", "server", 1024, now.Add(time.Second)); ok || !strings.Contains(why, "earlier requests") {
		t.Fatalf("a later request jumped the queue: %v %s", ok, why)
	}
	// Memory frees up: the first waiter gets it.
	d.mem.AvailableMB = 4.5 * 1024
	if ok, why := d.admit("big", "cluster", 3*1024, now.Add(2*time.Second)); !ok {
		t.Fatalf("the head of the queue was refused: %s", why)
	}
	// A waiter that stops retrying loses its place, so it cannot block others forever.
	d.mem.AvailableMB = 3 * 1024
	d.admit("ghost", "cluster", 3*1024, now.Add(3*time.Second))
	if ok, why := d.admit("small", "server", 1024, now.Add(3*time.Second+2*waiterTimeout)); !ok {
		t.Fatalf("an abandoned request still blocks the queue: %s", why)
	}
	// Something that can never fit is refused outright, not queued.
	if ok, why := d.admit("huge", "cluster", 40*1024, now); ok || !strings.Contains(why, "never fit") {
		t.Fatalf("impossible request: %v %s", ok, why)
	}
	for _, w := range d.waiters {
		if w.Session == "huge" {
			t.Fatal("an impossible request was queued")
		}
	}
}

func TestReclaimPressesTheLeastUsed(t *testing.T) {
	d := memDaemon(1.5)
	d.cfg.DryRun = false
	now := time.Now()
	orphanSince := now.Add(-time.Minute)
	d.st.Resources["compose:old"] = &Resource{ID: "compose:old", Kind: RKContainers, MemMB: 800, OrphanSince: &orphanSince}
	mk := func(id, sess, rid string, mem float64, idle time.Duration, verified bool) {
		d.st.Resources[rid] = &Resource{ID: rid, Kind: RKCluster, MemMB: mem, LeaseID: id}
		d.st.Leases[id] = &Lease{ID: id, Session: sess, Kind: "cluster", MemGB: mem / 1024, TTL: 120, Verified: verified,
			LastUsed: now.Add(-idle), Expires: now.Add(time.Hour), Resources: []string{rid}}
	}
	mk("busy", "s1", "kind:busy", 2000, time.Minute, true)        // in use: must not be touched
	mk("idle20", "s2", "kind:idle20", 2000, 20*time.Minute, true) // idle
	mk("idle8", "s3", "kind:idle8", 2000, 8*time.Minute, true)    // less idle
	mk("unver", "s4", "kind:unver", 1000, 2*time.Minute, false)   // unverified claim

	ok, why := d.admit("new", "cluster", 3*1024, now)
	if ok {
		t.Fatal("granted without room")
	}
	deadline := now.Add(time.Duration(d.cfg.PressureGraceMinutes) * time.Minute)
	grace := time.Duration(d.cfg.GraceMinutes) * time.Minute
	if got := d.st.Resources["compose:old"].OrphanSince.Add(grace); got.After(deadline) {
		t.Errorf("the orphan's stop was not brought forward: %s", got.Sub(now))
	}
	if d.st.Leases["busy"].IdleAsked != nil {
		t.Error("a lease used a minute ago was pressed")
	}
	if d.st.Leases["unver"].IdleAsked == nil || d.st.Leases["idle20"].IdleAsked == nil {
		t.Errorf("unverified and longest-idle leases were not pressed: %s", why)
	}
	// 0.8 (orphan) + 1.0 (unverified) + 2.0 (idle 20 min) covers 3 GB: the 8-minute one is spared.
	if d.st.Leases["idle8"].IdleAsked != nil {
		t.Error("pressed more holders than needed")
	}
	if q := d.st.Questions["idle:idle20"]; q == nil || q.For != "s2" || !strings.Contains(q.Text, "waiting for memory") {
		t.Errorf("the idle holder was not told why: %+v", q)
	}
	// No answer by the deadline releases it; an answer (use) keeps it.
	d.st.Touches["kind:unver"] = map[string]time.Time{"s4": now.Add(time.Minute)}
	d.checkLeases(deadline.Add(time.Second))
	if d.st.Leases["idle20"] != nil {
		t.Error("a pressed lease with no answer was not released at the deadline")
	}
	if d.st.Leases["unver"] == nil {
		t.Error("a pressed lease that was then used was released anyway")
	}
	if d.st.Leases["busy"] == nil || d.st.Leases["idle8"] == nil {
		t.Error("an unpressed lease was released")
	}
}

func TestSubtreeRSSAndFootprint(t *testing.T) {
	procs := map[int]*proc{
		100: {pid: 100, ppid: 1, exe: "claude", args: "claude", rssMB: 300},
		101: {pid: 101, ppid: 100, exe: "zsh", args: "zsh -c ./build.sh", rssMB: 5},
		102: {pid: 102, ppid: 101, exe: "bash", args: "bash ./build.sh", rssMB: 5},
		103: {pid: 103, ppid: 102, exe: "rustc", args: "rustc", rssMB: 2500},
		200: {pid: 200, ppid: 1, exe: "claude", args: "claude", rssMB: 300},
		300: {pid: 300, ppid: 1, exe: "node", args: "node x", rssMB: 900},
	}
	got := subtreeRSS(procs)
	if got[100] != 2510 || got[200] != 0 || len(got) != 2 {
		t.Fatalf("subtreeRSS = %v", got)
	}
	// A wrapper script the patterns do not recognise is still measured, learned from and flagged.
	d := memDaemon(8)
	d.agentSessions[100] = "sA"
	now := time.Now()
	d.st.Sessions["sA"] = &Session{ID: "sA", Agent: "claude"}
	d.updateFootprints(procs, now)
	if d.st.Sessions["sA"].FootprintMB != 2510 || d.st.Sessions["sA"].FlaggedAt.IsZero() {
		t.Fatalf("heavy unleased use not measured or flagged: %+v", d.st.Sessions["sA"])
	}
	// A build lease learns its real peak from the process tree.
	d.st.Leases["B"] = &Lease{ID: "B", Session: "sA", Kind: "cargo-build", MemGB: 2.5, BaseFootprintMB: 10}
	d.updateFootprints(procs, now)
	if d.st.Leases["B"].PeakMB != 2500 {
		t.Fatalf("build peak = %v", d.st.Leases["B"].PeakMB)
	}
	if p := d.pendingMB(); p > 100 {
		t.Fatalf("a build that has taken its memory still counts as %v MB pending", p)
	}
	d.dropLease(d.st.Leases["B"], "done")
	if d.st.Learned["cargo-build"] != 2500 {
		t.Fatalf("learned = %v", d.st.Learned)
	}
}

func TestYourOwnProcessesAreYours(t *testing.T) {
	d := newTestDaemon()
	d.firstDone = true
	now := time.Now()
	procs := map[int]*proc{
		10: {pid: 10, ppid: 1, exe: "/Applications/Warp.app/Contents/MacOS/stable", args: "stable"},
		11: {pid: 11, ppid: 10, exe: "-zsh", args: "-zsh"},
		12: {pid: 12, ppid: 11, exe: "node", args: "node dev.js"},
	}
	r := &Resource{ID: "srv:12", Kind: RKServer, PID: 12, Ports: []string{"3000"}}
	d.attribute(r, procs, now, false) // even for something found after a restart
	if r.LeaseID == "" || d.st.Leases[r.LeaseID].Session != "you" {
		t.Fatalf("a server you started in your terminal was not recorded as yours: %+v", r)
	}
	// A container that appears while no agent is running can only be yours.
	c := &Resource{ID: "compose:mine", Kind: RKContainers, Containers: []string{"mine-db-1"}}
	d.attribute(c, procs, now, true)
	if c.LeaseID == "" || d.st.Leases[c.LeaseID].Session != "you" {
		t.Fatal("a container started with no agent running was not recorded as yours")
	}
	// With an agent that ran a command in the last 10 minutes, nothing is assumed.
	procs[50] = &proc{pid: 50, ppid: 1, exe: "claude", args: "claude"}
	d.hookSeen = now
	d.recent["s"] = []cmdRecord{{Time: now.Add(-time.Minute), Cmd: "./start-everything.sh"}}
	u := &Resource{ID: "compose:unknown", Kind: RKContainers, Containers: []string{"unknown-1"}}
	d.attribute(u, procs, now, true)
	if u.LeaseID != "" {
		t.Fatal("assumed yours although an agent was active")
	}
	// With agents running that have never reported (no hooks), nothing is assumed either.
	d2 := newTestDaemon()
	v := &Resource{ID: "compose:v", Kind: RKContainers, Containers: []string{"v-1"}}
	d2.attribute(v, procs, now, true)
	if v.LeaseID != "" {
		t.Fatal("assumed yours although an unhooked agent is running")
	}
}

func TestUnusedPromiseIsDropped(t *testing.T) {
	d := memDaemon(8)
	d.st.Leases["L"] = &Lease{ID: "L", Session: "s", Kind: "cluster", MemGB: 3, Created: time.Now().Add(-20 * time.Minute),
		Expires: time.Now().Add(time.Hour)}
	if d.pendingMB() != 3*1024 {
		t.Fatal("promise not counted")
	}
}

func TestUnderDeclaringDoesNotPay(t *testing.T) {
	d := memDaemon(8)
	now := time.Now()
	d.st.Sessions["s"] = &Session{ID: "s"}
	// No history: the declared figure is taken.
	if got := d.effectiveGB("cargo-build", 0.5, "s", now); got != 0.5 {
		t.Fatalf("declared 0.5 GB charged as %.2f", got)
	}
	// With history, a declaration cannot go below what this kind really used.
	d.st.Learned["cargo-build"] = 2048
	if got := d.effectiveGB("cargo-build", 0.5, "s", now); got != 2 {
		t.Fatalf("declared 0.5 GB against a learned 2 GB charged as %.2f", got)
	}
	// Declaring more than the estimate is always fine.
	if got := d.effectiveGB("cargo-build", 6, "s", now); got != 6 {
		t.Fatalf("declared 6 GB charged as %.2f", got)
	}
	// A lease that uses far more than declared is flagged, and the session loses the
	// right to declare.
	delete(d.st.Learned, "cargo-build")
	l := &Lease{ID: "L", Session: "s", Kind: "cargo-build", MemGB: 0.5, Declared: true, PeakMB: 2500}
	d.st.Leases["L"] = l
	d.checkOverrun(l, now)
	if !l.Overran || !d.st.Sessions["s"].DistrustUntil.After(now) {
		t.Fatal("a lease that used 5x its declared memory was not flagged")
	}
	if got := d.effectiveGB("cargo-build", 0.5, "s", now); got != 2.5 {
		t.Fatalf("a distrusted session's declaration was still taken: %.2f", got)
	}
	// An honest lease is not flagged.
	h := &Lease{ID: "H", Session: "s", Kind: "cargo-build", MemGB: 2, Declared: true, PeakMB: 2300}
	d.checkOverrun(h, now)
	if h.Overran {
		t.Fatal("a lease within 1.5x of its declaration was flagged")
	}
}

func TestEstimatesLearnOnlyFromRealRuns(t *testing.T) {
	d := memDaemon(8)
	// A cluster lease released before any cluster appeared teaches nothing.
	d.dropLease(&Lease{ID: "a", Session: "s", Kind: "cluster", MemGB: 3, PeakMB: 0.4}, "unused")
	d.dropLease(&Lease{ID: "b", Session: "s", Kind: "cluster", MemGB: 3, PeakMB: 900}, "no resource ever bound")
	if _, ok := d.st.Learned["cluster"]; ok || d.estimateGB("cluster") != 3 {
		t.Fatalf("learned from a lease that never ran a cluster: %v", d.st.Learned)
	}
	// One that held a real cluster does.
	d.dropLease(&Lease{ID: "c", Session: "s", Kind: "cluster", MemGB: 3, PeakMB: 1800, HadResources: true}, "done")
	if d.st.Learned["cluster"] != 1800 {
		t.Fatalf("did not learn from a real run: %v", d.st.Learned)
	}
	// A build that barely started teaches nothing.
	d.dropLease(&Lease{ID: "d", Session: "s", Kind: "cargo-build", MemGB: 2.5, PeakMB: 12}, "failed at once")
	if _, ok := d.st.Learned["cargo-build"]; ok {
		t.Fatal("learned from a build that never got going")
	}
}
