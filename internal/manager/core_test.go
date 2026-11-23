package manager

import (
	"strings"
	"testing"
	"time"

	"agentrm/internal/api"
	"agentrm/internal/model"
)

// Dry run decides nothing: a blocked request in dry run must leave no mark that would act the
// moment dry run is turned off.
func TestDryRunReclaimChangesNothing(t *testing.T) {
	m, c := newTest(t, 1)
	now := c.Now()
	since := now.Add(-time.Minute)
	r := addRes(m, "srv:1", model.RKServer, 2000, "")
	r.Source = "procs"
	r.OrphanSince = &since
	m.st.Leases["idle"] = &model.Lease{ID: "idle", Session: "s2", Kind: "cluster", MemGB: 2, TTL: 120,
		Verified: true, LastUsed: now.Add(-time.Hour), Expires: now.Add(time.Hour), Resources: []string{"x"}}
	acquire(t, m, agent("waiting"), "cluster", 3)
	if r.Urgent || !r.OrphanSince.Equal(since) {
		t.Fatal("dry run marked an orphan for a direct stop or moved its clock")
	}
	if m.st.Leases["idle"].IdleAsked != nil {
		t.Fatal("dry run pressed a lease, which then gets released for real")
	}
}

func TestUrgentClearsWhenNobodyWaits(t *testing.T) {
	m, c := newTest(t, 8)
	m.cfg.DryRun = false
	since := c.Now()
	r := addRes(m, "srv:2", model.RKServer, 500, "")
	r.Source, r.OrphanSince, r.Urgent = "procs", &since, true
	m.checkOrphans(c.Now())
	if r.Urgent {
		t.Fatal("still marked for a direct stop with nobody waiting")
	}
}

func TestDryRunGateCatchesWhatSlipsThrough(t *testing.T) {
	m, _ := newTest(t, 8)
	r := addRes(m, "srv:3", model.RKServer, 500, "")
	m.pending[r.ID] = "stop"
	out := m.gateAuto([]Effect{{Op: "stop", Res: *r, By: "agentrm"}, {Op: "stop", Res: *r, By: "s1"}})
	if len(out) != 1 || out[0].By != "s1" || m.pending[r.ID] != "" {
		t.Fatalf("the gate should drop only the automatic stop in dry run: %+v", out)
	}
}

// An agent may stop what it holds, what it started, or what AgentRM itself would stop.
func TestAgentStopRights(t *testing.T) {
	m, _ := newTest(t, 8)
	a := agent("a1")
	cases := []struct {
		id, kind, startedBy string
		askOnly             bool
		ok                  bool
	}{
		{"agent:9", model.RKStaleAgent, "", false, false},
		{"app:docker", model.RKServer, "", true, false},
		{"vm:lima-x", model.RKVM, "", false, false},
		{"srv:mine", model.RKServer, "you", false, false},
		{"srv:its", model.RKServer, "a1", false, true},
		{"srv:nobody", model.RKServer, "", false, true},
	}
	for _, tc := range cases {
		r := addRes(m, tc.id, tc.kind, 100, "")
		r.StartedBy, r.AskOnly = tc.startedBy, tc.askOnly
		_, _, err := m.stopNow(a, api.Req{ResourceID: tc.id})
		if (err == nil) != tc.ok {
			t.Errorf("%s: allowed=%v, want %v (%v)", tc.id, err == nil, tc.ok, err)
		}
		delete(m.pending, tc.id)
	}
}

// Holding 1 GB of a kind and asking for 8 GB used to be granted at once, at 1 GB.
func TestALargerRequestOfAHeldKindIsAdmitted(t *testing.T) {
	m, _ := newTest(t, 3)
	a := agent("a1")
	if r := acquire(t, m, a, "cluster", 1); !r.Granted {
		t.Fatal("the first lease should fit")
	}
	if r := acquire(t, m, a, "cluster", 8); r.Granted {
		t.Fatal("7 GB more than it holds was granted without admission")
	}
	if r := acquire(t, m, a, "cluster", 1); !r.Granted || !r.Reused {
		t.Fatal("asking again for what it holds should reuse the lease")
	}
}

// A refused restore that is never retried must not hold the queue while its agent works on.
func TestAnUnretriedRestoreLeavesTheQueue(t *testing.T) {
	m, c := newTest(t, 1)
	m.waiters = append(m.waiters, &model.Waiter{Session: "s1", Kind: "cluster", NeedMB: 4000, Retry: true, LastSeen: c.Now()})
	m.touchSession("s1", "claude", "")
	c.Add(minutes(m.cfg.QueueMinutes) + time.Minute)
	m.touchSession("s1", "claude", "").LastSeen = c.Now() // the agent is busy with other things
	for _, w := range m.liveWaiters(c.Now()) {
		if w.Retry {
			t.Fatal("a restore nobody retried still holds the queue")
		}
	}
}

func TestPruneDropsWhatIsGone(t *testing.T) {
	m, c := newTest(t, 8)
	now := c.Now()
	addRes(m, "srv:9094", model.RKServer, 10, "")
	m.st.Approvals["old"] = &model.Approval{ID: "old", Expires: now.Add(-48 * time.Hour)}
	m.st.Approvals["new"] = &model.Approval{ID: "new", Expires: now.Add(time.Hour)}
	since := "2026-11-18T08:20:05+05:30"
	m.wouldDecided["pause:srv:9094:"+since] = true
	m.wouldDecided["pause:srv:1:"+since] = true
	m.wouldDecided["ask:compose:gone"] = true
	m.prune(now)
	if m.st.Approvals["old"] != nil || m.st.Approvals["new"] == nil {
		t.Fatal("approvals")
	}
	for k := range m.wouldDecided {
		if !strings.Contains(k, "srv:9094") {
			t.Fatalf("kept %s", k)
		}
	}
	if !m.wouldDecided["pause:srv:9094:"+since] {
		t.Fatal("dropped the marker of a live resource (minutes :20 in the time)")
	}
}
