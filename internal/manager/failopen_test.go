package manager

import (
	"testing"
	"time"

	"agentrm/internal/api"
	"agentrm/internal/model"
	"agentrm/internal/sys"
)

func TestBypassLetsEverythingThrough(t *testing.T) {
	m, c := newTest(t, 0.5) // no room at all
	got := fakeEffects(t, nil)
	m.cfg.DryRun = false
	if r := acquire(t, m, agent("a"), "cluster", 4); r.Granted {
		t.Fatal("granted with no room before the bypass")
	}
	if _, _, err := m.bypass(agent("a"), api.Req{UntilMinutes: 60}); err == nil {
		t.Fatal("an agent turned on the bypass")
	}
	if _, _, err := m.bypass(terminal, api.Req{UntilMinutes: 60}); err != nil {
		t.Fatal(err)
	}
	if m.findLease("a", "cluster") == nil {
		t.Fatal("the queued request was not let in when the bypass started")
	}
	if r := acquire(t, m, agent("b"), "cargo-build", 6); !r.Granted {
		t.Fatal("refused during the bypass")
	}
	since := c.Now().Add(-time.Hour)
	r := addRes(m, "srv:5", model.RKServer, 900, "")
	r.Source, r.OrphanSince = "procs", &since
	m.applyEffects(RunEffects(m.gateAuto(m.checkOrphans(c.Now()))))
	for _, e := range *got {
		if e.Op == "pause" || e.Op == "stop" {
			t.Fatalf("%s %s during the bypass", e.Op, e.Res.ID)
		}
	}
	c.Add(61 * time.Minute)
	if m.failOpen(c.Now()) != "" {
		t.Fatal("the bypass did not end")
	}
}

func TestBrokenReadingsFailOpen(t *testing.T) {
	m, c := newTest(t, 0.5)
	m.mem = sys.MemStats{TotalMB: 16384, Cores: 10, DiskFreeGB: 100, PressureLevel: 1} // vm_stat failed: nothing read
	if r := acquire(t, m, agent("a"), "cluster", 4); !r.Granted {
		t.Fatal("refused on memory figures that could not be read")
	}
	m2, c2 := newTest(t, 0.5)
	m2.firstDone, m2.lastSweep = true, c2.Now()
	c2.Add(10 * time.Minute) // the daemon is stuck
	if r := acquire(t, m2, agent("a"), "cluster", 4); !r.Granted {
		t.Fatal("refused on a measurement 10 minutes old")
	}
	_ = c
}

// Estimates decide the order; only the Mac's real pressure keeps a request out for long.
func TestAQueuedRequestIsGrantedAfterTheLongestWait(t *testing.T) {
	m, c := newTest(t, 1)
	if r := acquire(t, m, agent("a"), "cluster", 4); r.Granted {
		t.Fatal("granted with no room")
	}
	m.touchSession("a", "claude", "")
	c.Add(14 * time.Minute)
	m.touchSession("a", "claude", "").LastSeen = c.Now()
	m.grantWaiters(c.Now(), false)
	if m.findLease("a", "cluster") != nil {
		t.Fatal("granted before the longest wait")
	}
	c.Add(2 * time.Minute)
	m.touchSession("a", "claude", "").LastSeen = c.Now()
	m.mem.PressureLevel = 4 // the Mac really is short
	m.grantWaiters(c.Now(), false)
	if m.findLease("a", "cluster") != nil {
		t.Fatal("granted anyway while the Mac is under critical pressure")
	}
	m.mem.PressureLevel = 1
	m.grantWaiters(c.Now(), false)
	if m.findLease("a", "cluster") == nil {
		t.Fatal("not granted after the longest wait with the Mac not swapping")
	}
}
