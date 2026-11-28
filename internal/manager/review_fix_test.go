package manager

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agentrm/internal/api"
	"agentrm/internal/discover"
	"agentrm/internal/model"
	"agentrm/internal/store"
	"agentrm/internal/sys"
)

// With dry_run off every level stops; a stop marked wrong must still turn its level off.
func TestWrongMarkLocksTheLevelWithDryRunOff(t *testing.T) {
	m, _ := newTest(t, 8)
	m.cfg.DryRun = false
	r := addRes(m, "srv:1", model.RKServer, 500, "")
	m.relock(&model.Decision{Verdict: "wrong", Category: "unclaimed", Action: "stop", Resource: r.ID})
	if !m.dryFor(r) {
		t.Fatalf("stops of the level go on after one was marked wrong: %+v dry_run=%v", m.cfg.AutoStop, m.cfg.DryRun)
	}
	if !m.cfg.AutoStop["owner_gone"] || m.cfg.AutoStop["idle_lease"] {
		t.Fatalf("levels before it stay on, after it go off: %+v", m.cfg.AutoStop)
	}
}

// A refusal for heat opens the hot spell at once, so the session is told when it has cooled;
// and while you let everything through, heat refuses nothing.
func TestHeatRefusalOpensTheSpellAndBypassLetsThrough(t *testing.T) {
	m, c := newTest(t, 8)
	m.readFast = func() sys.MemStats { s := m.mem; s.Thermal = 2; return s }
	m.touchSession("s", "claude", "")
	if r := acquire(t, m, agent("s"), "cargo-build", 1); r.Granted || !strings.Contains(r.Reason, "running hot") {
		t.Fatalf("a build was not refused for heat: %+v", r)
	}
	if m.heat.since == nil || !m.heat.refused["s"] {
		t.Fatalf("no hot spell behind the refusal: %+v", m.heat)
	}
	m.mem.Thermal = 0
	m.watchHeat(c.Now())
	c.Add(coolFor + time.Second)
	m.watchHeat(c.Now())
	if q := inboxOf(t, m, "s"); len(q) != 1 || !strings.Contains(q[0], "cooled down") {
		t.Fatalf("the refused session was not told it cooled: %q", q)
	}
	until := c.Now().Add(time.Hour)
	m.st.BypassUntil = &until
	if r := acquire(t, m, agent("s"), "cargo-build", 1); !r.Granted {
		t.Fatalf("refused for heat while everything is let through: %s", r.Reason)
	}
}

// What was said about heat survives a restart; other questions and their clocks do not.
func TestHeatSurvivesARestart(t *testing.T) {
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	st := model.NewState()
	since, asked := now.Add(-time.Minute), now.Add(-time.Minute)
	st.HotSince, st.HotRefused = &since, []string{"s"}
	st.Questions["hot:s"] = &model.Question{For: "s", Text: "hot"}
	st.Questions["idle:l"] = &model.Question{For: "s", Text: "idle"}
	st.Leases["l"] = &model.Lease{ID: "l", Session: "s", IdleAsked: &asked}
	b, _ := json.Marshal(st)
	var back model.State
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	store.Migrate(&back, now)
	if back.Questions["hot:s"] == nil || back.Questions["idle:l"] != nil || back.Leases["l"].IdleAsked != nil {
		t.Fatalf("questions %v, idle asked %v", back.Questions, back.Leases["l"].IdleAsked)
	}
	if back.HotSince == nil || len(back.HotRefused) != 1 {
		t.Fatalf("the hot spell was lost: %+v", back)
	}
}

// A PID reused by another process is a new thing: nobody's leftover, not paused, not pinned.
func TestReusedPIDInheritsNothing(t *testing.T) {
	m, c := newTest(t, 8)
	gone := c.Now().Add(-time.Hour)
	old := addRes(m, "srv:4321", model.RKServer, 300, "")
	old.PID, old.ProcStart, old.StartedBy, old.Owner, old.OwnerGoneAt, old.PausedAt = 4321, gone, "s", "s", &gone, &gone
	m.st.Pinned["srv:4321"] = true
	fresh := &model.Resource{ID: "srv:4321", Kind: model.RKServer, Name: "node", PID: 4321, ProcStart: c.Now(), MemMB: 300}
	m.merge(discover.Result{Resources: []*model.Resource{fresh}}, map[int]*sys.Proc{}, nil, c.Now())
	got := m.st.Resources["srv:4321"]
	if got.Owner != "" || got.OwnerGoneAt != nil || got.PausedAt != nil || got.Pinned || got.StartedBy == "s" {
		t.Fatalf("another process inherited the first one's record: %+v", got)
	}
}

func TestNoProcessTableIsNotQuiet(t *testing.T) {
	m, c := newTest(t, 8)
	if m.agentsQuiet(nil, c.Now()) {
		t.Fatal("with no process table read, something new was taken for yours")
	}
}

// A stop you asked for is logged as done, in dry run too.
func TestStopByHandIsNotLoggedAsDryRun(t *testing.T) {
	m, _ := newTest(t, 8)
	r := addRes(m, "srv:9", model.RKServer, 300, "")
	if _, _, err := m.stopNow(api.Caller{Agent: "you", Session: "you", Human: true, ViaApp: true}, api.Req{ResourceID: r.ID}); err != nil {
		t.Fatal(err)
	}
	if d := m.st.Decisions[len(m.st.Decisions)-1]; d.DryRun {
		t.Fatalf("logged as a dry run: %+v", d)
	}
	for _, e := range m.events {
		if strings.HasPrefix(e.Text, "WOULD") {
			t.Fatalf("said nothing was done: %s", e.Text)
		}
	}
}
