package manager

import (
	"strings"
	"testing"
	"time"

	"agentrm/internal/api"
	"agentrm/internal/model"
)

// Two sessions hold a server each; a third needs memory.
func yieldSetup(t *testing.T) (*Manager, *clock, *model.Resource, *model.Resource) {
	m, c := newTest(t, 3) // 2 GB headroom
	m.cfg.DryRun = false
	for _, s := range []string{"a", "b", "c"} {
		m.touchSession(s, "claude", "")
	}
	la := acquire(t, m, agent("a"), "server", 0.5)
	lb := acquire(t, m, agent("b"), "server", 0.5)
	ra := addRes(m, "srv:3000", model.RKServer, 1500, la.Lease.ID)
	rb := addRes(m, "srv:3001", model.RKServer, 1500, lb.Lease.ID)
	ra.Ports, rb.Ports = []string{"3000"}, []string{"3001"}
	m.mem.AvailableMB = 1.5 * 1024
	// a's server has been idle for an hour; b used its own a minute ago.
	m.st.Leases[la.Lease.ID].LastUsed = c.Now().Add(-time.Hour)
	m.st.Leases[lb.Lease.ID].LastUsed = c.Now().Add(-time.Minute)
	return m, c, ra, rb
}

func TestBlockedRequestAsksHoldersWhatYieldingCosts(t *testing.T) {
	m, _, ra, rb := yieldSetup(t)
	if r := acquire(t, m, agent("c"), "cluster", 1.5); r.Granted {
		t.Fatal("granted without room")
	}
	for _, s := range []string{"a", "b"} {
		out, _, _ := m.inbox(agent(s), api.Req{})
		if q := out.(*api.InboxResp).Questions; len(q) != 1 || !strings.Contains(q[0], "agentrm yield") {
			t.Fatalf("holder %s was not asked what yielding costs: %v", s, q)
		}
	}
	_, _ = ra, rb
}

func TestCheapestOfferIsTakenFirst(t *testing.T) {
	m, _, ra, rb := yieldSetup(t)
	acquire(t, m, agent("c"), "cluster", 1.5)
	// b, which used its server a minute ago, says stopping it costs little; a does not answer.
	if _, _, err := m.yield(agent("b"), api.Req{LeaseID: rb.LeaseID, Cost: "low", RestartSeconds: 5}); err != nil {
		t.Fatal(err)
	}
	m.reclaim(1.5*1024, "c", m.now())
	if rb.LeaseID != "" || !rb.Urgent || rb.Owner != "b" {
		t.Fatalf("the low-cost offer was not taken at once: %+v", rb)
	}
	if ra.LeaseID == "" {
		t.Fatal("the silent holder's lease was taken although the offer covered the need")
	}
}

func TestNoneReleasesAtOnce(t *testing.T) {
	m, _, _, rb := yieldSetup(t)
	acquire(t, m, agent("c"), "cluster", 1.5)
	lease := rb.LeaseID
	out, _, err := m.yield(agent("b"), api.Req{LeaseID: lease, Cost: "none"})
	if err != nil || !strings.Contains(out.(*api.CountResp).Note, "released") {
		t.Fatalf("%v %v", out, err)
	}
	if m.st.Leases[lease] != nil || rb.Owner != "b" || !rb.Urgent {
		t.Fatalf("not released and marked for collection: %+v", rb)
	}
	if _, _, err := m.yield(agent("a"), api.Req{LeaseID: "nope", Cost: "none"}); err == nil {
		t.Fatal("yielded a lease that does not exist")
	}
}

func TestHighCostIsTakenLast(t *testing.T) {
	m, c, ra, rb := yieldSetup(t)
	m.st.Leases[rb.LeaseID].LastUsed = c.Now().Add(-2 * time.Hour) // b is even idler than a
	acquire(t, m, agent("c"), "cluster", 1.5)
	m.yield(agent("b"), api.Req{LeaseID: rb.LeaseID, Cost: "high"})
	out := m.reclaim(1.4*1024, "c", m.now())
	if !strings.Contains(out, ra.LeaseID) || strings.Contains(out, rb.LeaseID) {
		t.Fatalf("the holder in the middle of something was pressed before the silent one: %s", out)
	}
}

func TestMisreportCountsAgainstLaterOffers(t *testing.T) {
	m, c, _, rb := yieldSetup(t)
	acquire(t, m, agent("c"), "cluster", 1.5)
	m.yield(agent("b"), api.Req{LeaseID: rb.LeaseID, Cost: "none"})
	// A minute later b runs a command against the server it said it was done with.
	c.Add(time.Minute)
	m.touchCmd(agent("b"), api.Req{Cmd: "curl localhost:3001/health"})
	m.judgeYields(c.Now())
	s := m.st.Sessions["b"]
	if s == nil || s.YieldMisreports != 1 {
		t.Fatalf("misreport not recorded: %+v", s)
	}
	// Its next low-cost offer counts as no answer.
	m.mem.AvailableMB, m.waiters = 8*1024, nil
	l := acquire(t, m, agent("b"), "compose", 0.2)
	m.yield(agent("b"), api.Req{LeaseID: l.Lease.ID, Cost: "low"})
	if r := m.costRank(m.st.Leases[l.Lease.ID]); r != 2 {
		t.Fatalf("a session that misreported still has its offers believed: rank %d", r)
	}
	// An honest session's offers stand, and become true after the check window.
	m.yield(agent("a"), api.Req{LeaseID: m.findLease("a", "server").ID, Cost: "low"})
	c.Add(yieldCheck + time.Minute)
	m.judgeYields(c.Now())
	if a := m.st.Sessions["a"]; a == nil || a.YieldReports != 1 || a.YieldMisreports != 0 {
		t.Fatalf("an honest offer was not recorded as true: %+v", a)
	}
}

func TestRestartTimeIsMeasured(t *testing.T) {
	m, c, _, rb := yieldSetup(t)
	m.yield(agent("b"), api.Req{LeaseID: rb.LeaseID, Cost: "low", RestartSeconds: 10})
	m.yieldTaken(rb.ID, c.Now())
	c.Add(time.Minute)
	m.yieldsSeeUse("b", []string{rb.ID}, "", c.Now()) // b starts it again
	c.Add(2 * time.Minute)
	m.yieldsSeeReturn(&model.Resource{ID: rb.ID, Kind: model.RKServer}, c.Now())
	if got := m.st.RestartSeconds[model.RKServer]; got < 119 || got > 121 {
		t.Fatalf("restart time not learned: %v", got)
	}
	m.judgeYields(c.Now())
	if m.st.Sessions["b"].YieldMisreports != 1 {
		t.Fatal("a restart twelve times longer than promised was not a misreport")
	}
}
