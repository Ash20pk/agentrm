package manager

import (
	"strings"
	"testing"
	"time"

	"agentrm/internal/api"
	"agentrm/internal/model"
)

// The head of the queue is over its share because another waits; the other is behind memory
// kept for the head. With room for the second, it goes.
func TestHeldUpHeadReservesNothing(t *testing.T) {
	m, c := newTest(t, 10)
	acquire(t, m, agent("hog"), "cluster", 6)
	m.mem.AvailableMB = 1.5 * 1024
	if r := acquire(t, m, agent("hog"), "compose", 3); r.Granted {
		t.Fatal("granted without room")
	}
	c.Add(time.Second)
	if r := acquire(t, m, agent("other"), "cluster", 2); r.Granted {
		t.Fatal("granted without room")
	}
	m.mem.AvailableMB = 11 * 1024 // 4 GB of headroom
	c.Add(time.Second)
	if ok, why, _ := m.fits("hog", "compose", "", 3*1024, m.waiters[0], m.waiters); ok || !strings.Contains(why, "may hold at most") {
		t.Fatalf("the test needs the head to be over its share: %v %q", ok, why)
	}
	m.grantWaiters(c.Now(), false)
	if m.findLease("other", "cluster") == nil {
		t.Fatal("the second in the queue was held behind a request that could not be granted")
	}
}

// Requests that waited the longest a request waits are let through one a pass.
func TestOverdueWaitersGoOneAPass(t *testing.T) {
	m, c := newTest(t, 2)
	acquire(t, m, agent("a"), "cluster", 3)
	c.Add(time.Second)
	acquire(t, m, agent("b"), "cluster", 3)
	c.Add(m.maxWait() + time.Second)
	for _, w := range m.waiters {
		w.LastSeen = c.Now()
	}
	m.grantWaiters(c.Now(), false)
	if n := len(m.st.Leases); n != 1 {
		t.Fatalf("%d leases granted in one pass, want 1", n)
	}
	m.grantWaiters(c.Now(), false)
	if n := len(m.st.Leases); n != 2 {
		t.Fatalf("%d leases after the second pass, want 2", n)
	}
}

// A restore that does not fit keeps its own place, not the session's queued request's.
func TestRestoreDoesNotReplaceAQueuedRequest(t *testing.T) {
	m, c := newTest(t, 2)
	acquire(t, m, agent("s"), "compose", 3)
	c.Add(time.Second)
	m.admit(model.Waiter{Session: "s", Kind: "compose", NeedMB: 4 * 1024, Retry: true}, c.Now())
	plain := 0
	for _, w := range m.waiters {
		if !w.Retry && w.NeedMB == 3*1024 {
			plain++
		}
	}
	if plain != 1 || len(m.waiters) != 2 {
		t.Fatalf("the queued request was replaced: %+v", m.waiters)
	}
}

// A build in the background keeps its lease when another command of its kind returns.
func TestBackgroundBuildKeepsItsLease(t *testing.T) {
	m, _ := newTest(t, 10)
	m.touchSession("s", "claude", "")
	out, _, _ := m.acquire(agent("s"), api.Req{Kind: "cargo-build", Cmd: "cargo build", Via: "hook", RunInBackground: true})
	bg := out.(*api.AcquireResp).Lease
	if bg == nil {
		t.Fatal("not granted")
	}
	m.renew(agent("s"), api.Req{Via: "hook", Cmd: "cargo check"})
	if m.st.Leases[bg.ID] == nil {
		t.Fatal("the background build's lease was released by another command finishing")
	}
}

func TestLeaseLifetimeIsBounded(t *testing.T) {
	m, c := newTest(t, 10)
	for _, ttl := range []int{-5, 1 << 40} {
		l := m.grant(&model.Waiter{Session: "s", Kind: "server", NeedMB: 100, TTLMinutes: ttl}, c.Now(), "")
		if d := l.Expires.Sub(c.Now()); d <= 0 || d > 24*time.Hour {
			t.Fatalf("ttl %d gives a lease lasting %s", ttl, d)
		}
	}
}

func TestConfigSanitize(t *testing.T) {
	cfg := model.DefaultConfig()
	cfg.DefaultTTLMinutes, cfg.BuildTTLMinutes, cfg.HeadroomGB, cfg.MaxSessionShare = 0, -1, 40, 3
	if notes := cfg.Sanitize(16 * 1024); len(notes) != 4 {
		t.Fatalf("notes: %v", notes)
	}
	def := model.DefaultConfig()
	if cfg.DefaultTTLMinutes != def.DefaultTTLMinutes || cfg.BuildTTLMinutes != def.BuildTTLMinutes ||
		cfg.HeadroomGB != def.HeadroomGB || cfg.MaxSessionShare != def.MaxSessionShare {
		t.Fatalf("not reset: %+v", cfg)
	}
	if notes := def.Sanitize(16 * 1024); len(notes) != 0 {
		t.Fatalf("defaults changed: %v", notes)
	}
}
