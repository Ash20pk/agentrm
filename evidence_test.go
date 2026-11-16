package main

import (
	"strings"
	"testing"
	"time"
)

func newTestDaemon() *Daemon {
	d := &Daemon{cfg: defaultConfig(), agentSessions: map[int]string{}, recent: map[string][]cmdRecord{}, dir: "/nonexistent"}
	d.st = State{Leases: map[string]*Lease{}, Sessions: map[string]*Session{}, Resources: map[string]*Resource{},
		Questions: map[string]*Question{}, Pinned: map[string]bool{}, Touches: map[string]map[string]time.Time{},
		Learned: map[string]float64{}, LearnedDev: map[string]float64{}, AgentSessions: map[string]string{}}
	return d
}

func TestAgentAncestor(t *testing.T) {
	procs := map[int]*proc{
		100: {pid: 100, ppid: 1, exe: "claude", args: "claude"},
		200: {pid: 200, ppid: 100, exe: "/bin/zsh", args: "/bin/zsh -c ..."},
		300: {pid: 300, ppid: 200, exe: "node", args: "node server.js"},
		400: {pid: 400, ppid: 1, exe: "node", args: "node /opt/homebrew/bin/codex"},
		500: {pid: 500, ppid: 400, exe: "cargo", args: "cargo build"},
		600: {pid: 600, ppid: 1, exe: "/bin/zsh", args: "-zsh"},
		700: {pid: 700, ppid: 600, exe: "node", args: "node dev.js"},
	}
	cases := []struct {
		pid      int
		agentPID int
		kind     string
	}{{300, 100, "claude"}, {500, 400, "codex"}, {700, 0, ""}, {999, 0, ""}}
	for _, c := range cases {
		apid, kind := agentAncestor(procs, c.pid)
		if apid != c.agentPID || kind != c.kind {
			t.Errorf("agentAncestor(%d) = %d %q, want %d %q", c.pid, apid, kind, c.agentPID, c.kind)
		}
	}
}

func TestTouchedBy(t *testing.T) {
	res := map[string]*Resource{
		"kind:obx":     {ID: "kind:obx", Kind: RKCluster, Containers: []string{"obx-control-plane"}, Ports: []string{"61234"}},
		"compose:shop": {ID: "compose:shop", Kind: RKContainers, Containers: []string{"shop-db-1"}, Ports: []string{"5432"}},
		"srv:42":       {ID: "srv:42", Kind: RKServer, PID: 42, Ports: []string{"3000"}},
		"sim:ABC-123":  {ID: "sim:ABC-123", Kind: RKSimulator, UDID: "ABC-123", Name: "iPhone 16"},
		"gradle:7":     {ID: "gradle:7", Kind: RKGradle, PID: 7},
	}
	cases := []struct {
		cmd, cwd, ctx string
		want          []string
	}{
		{"kubectl --context kind-obx get pods", "", "", []string{"kind:obx"}},
		{"kubectl get pods -A", "", "kind-obx", []string{"kind:obx"}},
		{"kubectl get pods -A", "", "prod-cluster", nil},
		{"kubectl --context prod get pods", "", "kind-obx", nil},
		{"helm --kube-context kind-obx list", "", "", []string{"kind:obx"}},
		{"kind export logs --name obx", "", "", []string{"kind:obx"}},
		{"docker exec -it shop-db-1 psql", "", "", []string{"compose:shop"}},
		{"docker compose -p shop logs", "", "", []string{"compose:shop"}},
		{"docker compose logs api", "/code/shop", "", []string{"compose:shop"}},
		{"psql postgres://u:p@localhost:5432/db", "", "", []string{"compose:shop"}},
		{"curl -s http://localhost:3000/health", "", "", []string{"srv:42"}},
		{"curl -s http://localhost:30001/health", "", "", nil},
		{"xcrun simctl install booted App.app", "", "", []string{"sim:ABC-123"}},
		{"xcrun simctl io ABC-123 screenshot x.png", "", "", []string{"sim:ABC-123"}},
		{"./gradlew assembleDebug", "", "", []string{"gradle:7"}},
		{"git status", "", "kind-obx", nil},
	}
	for _, c := range cases {
		got := touchedBy(c.cmd, c.cwd, res, c.ctx)
		if strings.Join(sorted(got), ",") != strings.Join(c.want, ",") {
			t.Errorf("touchedBy(%q) = %v, want %v", c.cmd, got, c.want)
		}
	}
}

func sorted(xs []string) []string {
	out := append([]string(nil), xs...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func TestClaimNeedsEvidence(t *testing.T) {
	d := newTestDaemon()
	d.st.Resources["kind:obx"] = &Resource{ID: "kind:obx", Kind: RKCluster, MemMB: 1600}

	// No evidence: allowed, but unverified and short.
	resp, err := d.claim(map[string]any{"resource_id": "kind:obx", "session": "s1", "agent": "claude", "purpose": "tests"})
	if err != nil {
		t.Fatal(err)
	}
	l := resp.(map[string]any)["lease"].(*Lease)
	if l.Verified || l.TTL != d.cfg.UnverifiedTTLMinutes {
		t.Fatalf("claim without evidence: verified=%v ttl=%d", l.Verified, l.TTL)
	}
	d.dropLease(l, "test")

	// A command of this session used it recently: verified, full TTL.
	d.st.Touches["kind:obx"] = map[string]time.Time{"s2": time.Now().Add(-5 * time.Minute)}
	resp, _ = d.claim(map[string]any{"resource_id": "kind:obx", "session": "s2", "agent": "claude", "purpose": "tests"})
	l = resp.(map[string]any)["lease"].(*Lease)
	if !l.Verified || l.TTL != d.cfg.DefaultTTLMinutes {
		t.Fatalf("claim with recent use: verified=%v ttl=%d", l.Verified, l.TTL)
	}

	// Another session cannot take it while held.
	if _, err := d.claim(map[string]any{"resource_id": "kind:obx", "session": "s3", "purpose": "mine"}); err == nil {
		t.Fatal("a second session took a held resource")
	}

	// Use from too long ago does not count.
	d.dropLease(l, "test")
	d.st.Touches["kind:obx"]["s4"] = time.Now().Add(-3 * time.Hour)
	resp, _ = d.claim(map[string]any{"resource_id": "kind:obx", "session": "s4", "purpose": "old"})
	if resp.(map[string]any)["verified"].(bool) {
		t.Fatal("stale use verified a claim")
	}

	// A person is trusted.
	d.dropLease(resp.(map[string]any)["lease"].(*Lease), "test")
	resp, _ = d.claim(map[string]any{"resource_id": "kind:obx", "session": "you", "purpose": "mine", "_human": true})
	if !resp.(map[string]any)["verified"].(bool) {
		t.Fatal("a person's claim was not verified")
	}
}

func TestIdleLeaseIsAskedThenReleased(t *testing.T) {
	d := newTestDaemon()
	now := time.Now()
	d.st.Resources["kind:obx"] = &Resource{ID: "kind:obx", Kind: RKCluster, MemMB: 1600, LeaseID: "L1"}
	d.st.Leases["L1"] = &Lease{ID: "L1", Session: "s1", Kind: "cluster", TTL: 120, Created: now.Add(-2 * time.Hour),
		Expires: now.Add(time.Hour), LastUsed: now.Add(-45 * time.Minute), Resources: []string{"kind:obx"}}

	d.checkLeases(now)
	q := d.st.Questions["idle:L1"]
	if q == nil || q.For != "s1" {
		t.Fatal("idle holder was not asked")
	}
	// Only the holder sees the question.
	out, _ := d.inbox(map[string]any{"session": "other"})
	if len(out.(map[string]any)["questions"].([]string)) != 0 {
		t.Fatal("idle question leaked to another session")
	}

	// Use resets it.
	d.st.Touches["kind:obx"] = map[string]time.Time{"s1": now}
	d.checkLeases(now)
	if d.st.Questions["idle:L1"] != nil || d.st.Leases["L1"].IdleAsked != nil {
		t.Fatal("use did not clear the idle question")
	}

	// Idle again and no answer within the grace period: released.
	later := now.Add(45 * time.Minute)
	d.checkLeases(later)
	d.checkLeases(later.Add(time.Duration(d.cfg.GraceMinutes+1) * time.Minute))
	if d.st.Leases["L1"] != nil {
		t.Fatal("idle lease with no reply was not released")
	}
	if d.st.Resources["kind:obx"].LeaseID != "" {
		t.Fatal("resource still bound to the released lease")
	}
	if d.st.Learned["cluster"] != 1600 {
		t.Fatalf("peak memory not learned: %v", d.st.Learned)
	}
}

func TestActivityKeepsLeaseAlive(t *testing.T) {
	d := newTestDaemon()
	now := time.Now()
	d.st.Resources["srv:42"] = &Resource{ID: "srv:42", Kind: RKServer, LeaseID: "L1", LastActive: now.Add(-time.Minute)}
	d.st.Leases["L1"] = &Lease{ID: "L1", Session: "s1", Kind: "server", TTL: 120, Expires: now.Add(time.Minute),
		LastUsed: now.Add(-2 * time.Hour), Resources: []string{"srv:42"}}
	d.checkLeases(now)
	if d.st.Questions["idle:L1"] != nil {
		t.Fatal("an active resource's lease was treated as idle")
	}
	if d.st.Leases["L1"].Expires.Before(now.Add(100 * time.Minute)) {
		t.Fatal("activity did not extend the lease")
	}
}

func TestAttributionByProcessAndCommand(t *testing.T) {
	d := newTestDaemon()
	d.firstDone = true
	d.agentSessions[100] = "sessA"
	procs := map[int]*proc{
		100: {pid: 100, ppid: 1, exe: "claude", args: "claude"},
		200: {pid: 200, ppid: 100, exe: "/bin/zsh", args: "zsh"},
		300: {pid: 300, ppid: 200, exe: "node", args: "node server.js"},
	}
	now := time.Now()

	// A dev server started by sessA without a lease gets a lease for sessA.
	r := &Resource{ID: "srv:300", Kind: RKServer, PID: 300, Ports: []string{"3000"}}
	d.attribute(r, procs, now, true)
	if r.StartedBy != "sessA" || r.LeaseID == "" || d.st.Leases[r.LeaseID].Session != "sessA" {
		t.Fatalf("server not attributed to its session: %+v", r)
	}

	// A cluster is attributed by the command that named it, not by guessing.
	d.st.Leases["Lb"] = &Lease{ID: "Lb", Session: "sessB", Kind: "cluster", Created: now}
	d.st.Leases["Lc"] = &Lease{ID: "Lc", Session: "sessC", Kind: "cluster", Created: now.Add(time.Second)}
	d.recent["sessB"] = []cmdRecord{{Time: now, Cmd: "kind create cluster --name obx"}}
	c := &Resource{ID: "kind:obx", Kind: RKCluster, Containers: []string{"obx-control-plane"}}
	d.attribute(c, procs, now, true)
	if c.LeaseID != "Lb" {
		t.Fatalf("cluster bound to %q, want the lease of the session that created it (Lb)", c.LeaseID)
	}

	// No evidence and two candidate leases: leave it unbound rather than guess.
	d.st.Leases["Ld"] = &Lease{ID: "Ld", Session: "sessD", Kind: "cluster", Created: now}
	u := &Resource{ID: "kind:other", Kind: RKCluster, Containers: []string{"other-control-plane"}}
	d.attribute(u, procs, now, true)
	if u.LeaseID != "" {
		t.Fatal("bound without evidence while two leases could own it")
	}
}

func TestLearnedEstimate(t *testing.T) {
	d := newTestDaemon()
	if d.estimateGB("cluster") != 3 {
		t.Fatal("default estimate changed")
	}
	d.st.Learned["cluster"] = 2048
	if got := d.estimateGB("cluster"); got < 2.29 || got > 2.31 {
		t.Fatalf("learned estimate = %.2f GB, want 2.3", got)
	}
}

func TestAgentsCannotOverrideOthers(t *testing.T) {
	d := newTestDaemon()
	d.st.Resources["kind:obx"] = &Resource{ID: "kind:obx", Kind: RKCluster, LeaseID: "L1"}
	d.st.Leases["L1"] = &Lease{ID: "L1", Session: "owner", Kind: "cluster", Resources: []string{"kind:obx"}}
	if _, err := d.release(map[string]any{"lease_id": "L1", "session": "intruder"}); err == nil {
		t.Error("an agent released another session's lease")
	}
	if _, err := d.stopNow(map[string]any{"resource_id": "kind:obx", "session": "intruder"}); err == nil {
		t.Error("an agent stopped another session's resource")
	}
	if _, err := d.pin(map[string]any{"resource_id": "kind:obx", "session": "intruder"}); err == nil {
		t.Error("an agent pinned a resource")
	}
	if _, err := d.setConfig(map[string]any{"dry_run": false, "session": "intruder"}); err == nil {
		t.Error("an agent changed the settings")
	}
	if _, err := d.release(map[string]any{"lease_id": "L1", "session": "you", "_human": true}); err != nil {
		t.Errorf("a person could not release a lease: %v", err)
	}
}
