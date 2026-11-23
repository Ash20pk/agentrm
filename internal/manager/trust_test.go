package manager

import (
	"strings"
	"testing"
	"time"

	"agentrm/internal/api"
	"agentrm/internal/model"
)

func TestAutoStopUnlocksLevelByLevel(t *testing.T) {
	m, c := newTest(t, 8)
	got := fakeEffects(t, nil)
	// Something an ended session left, and something nobody is known to own.
	gone := c.Now()
	left := addRes(m, "srv:1", model.RKServer, 500, "")
	left.Owner, left.OwnerGoneAt, left.FirstSeen = "s", &gone, c.Now()
	nobody := addRes(m, "srv:2", model.RKServer, 500, "")
	nobody.FirstSeen = c.Now()

	// Unclaimed cannot go on before owner-gone is on.
	if _, _, err := m.setConfig(you, api.Req{AutoStop: map[string]bool{"unclaimed": true}}); err == nil {
		t.Fatal("a later level was turned on before the first")
	}
	if _, _, err := m.setConfig(you, api.Req{AutoStop: map[string]bool{"owner_gone": true}}); err != nil {
		t.Fatal(err)
	}
	m.applyEffects(RunEffects(m.checkOrphans(c.Now())))
	c.Add(minutes(m.cfg.GraceMinutes) + time.Minute)
	m.applyEffects(RunEffects(m.checkOrphans(c.Now())))
	ops := map[string]string{}
	for _, e := range *got {
		ops[e.Res.ID] = e.Op
	}
	if ops["srv:1"] != "pause" || ops["srv:2"] != "" {
		t.Fatalf("with only owner_gone on, only what an ended session left should be paused: %v", ops)
	}
	levels := m.trustLevels(c.Now())
	if !levels[0].On || levels[1].On || levels[1].Decided == 0 {
		t.Fatalf("levels: %+v", levels)
	}
}

func TestWrongStopRelocksItsLevel(t *testing.T) {
	m, c := newTest(t, 8)
	m.setConfig(you, api.Req{AutoStop: map[string]bool{"owner_gone": true}})
	gone := c.Now()
	r := addRes(m, "srv:1", model.RKServer, 500, "")
	r.Owner, r.OwnerGoneAt = "s", &gone
	m.decide("stop", r, "test")
	d := m.st.Decisions[len(m.st.Decisions)-1]
	if d.DryRun || d.Category != "owner_gone" {
		t.Fatalf("decision under an enabled level: %+v", d)
	}
	if _, _, err := m.review(you, api.Req{DecisionID: d.ID, Verdict: "wrong"}); err != nil {
		t.Fatal(err)
	}
	if m.cfg.AutoStop["owner_gone"] {
		t.Fatal("a wrong stop did not turn its level off")
	}
	if !m.dryFor(r) {
		t.Fatal("after re-locking, decisions are still taken")
	}
}

func TestEvidenceMakesALevelEligible(t *testing.T) {
	m, c := newTest(t, 8)
	gone := c.Now()
	r := addRes(m, "srv:1", model.RKServer, 500, "")
	r.Owner, r.OwnerGoneAt = "s", &gone
	for i := 0; i < 6; i++ {
		m.decide("pause", r, "test")
		m.st.Decisions[len(m.st.Decisions)-1].Verdict = "right"
	}
	m.st.Decisions[0].Time = c.Now().Add(-8 * 24 * time.Hour)
	lv := m.trustLevels(c.Now())[0]
	if !lv.Eligible || lv.Reviewed != 6 || !strings.Contains(lv.Evidence, "6 reviewed, 0 wrong") {
		t.Fatalf("%+v", lv)
	}
	if b := m.trustLevels(c.Now())[1]; b.Blocked == "" {
		t.Fatal("a level after one that is off should say it is blocked")
	}
}

func TestCollectStopsWhatEndedSessionsLeft(t *testing.T) {
	m, c := newTest(t, 8)
	gone := c.Now()
	left := addRes(m, "srv:1", model.RKServer, 500, "")
	left.Owner, left.OwnerGoneAt = "s", &gone
	app := addRes(m, "left:9", model.RKLeftover, 600, "")
	app.Owner, app.OwnerGoneAt, app.AskOnly = "s", &gone, true
	mine := addRes(m, "srv:2", model.RKServer, 500, "")
	if _, _, err := m.collect(agent("x"), api.Req{}); err == nil {
		t.Fatal("an agent collected on its own")
	}
	out, effects, err := m.collect(you, api.Req{})
	if err != nil || out.(*api.CountResp).N != 1 || len(effects) != 1 || effects[0].Res.ID != "srv:1" || effects[0].By != "you" {
		t.Fatalf("%+v %+v %v", out, effects, err)
	}
	_ = mine
}

func TestReceiptSumsTheWeek(t *testing.T) {
	m, c := newTest(t, 8)
	// A cluster Docker counted at 2 GB gives back half (nothing learned yet); a process all 1 GB.
	m.recordStopped(model.Resource{ID: "kind:dev", Source: "docker", MemMB: 2048}, "test", "", c.Now())
	m.recordStopped(model.Resource{ID: "srv:1", Source: "procs", MemMB: 1024}, "test", "", c.Now())
	m.st.Stopped = m.st.Stopped[:0] // the totals do not depend on the list kept for restoring
	m.event("ENDED session s (claude): it ended.")
	m.event("LEFTOVER node (pid 7, 0.4 GB) was started by session s")
	m.event("GRANT ab cluster 3.0 GB to b (claude) (from the queue, after 1m30s): test")
	m.event("GRANT cd server 0.5 GB to c (claude): test")
	m.event("RESTORED kind:dev (kind) for s")
	out, _, _ := m.receipt(you, api.Req{Days: 7})
	r := out.(*api.Receipt)
	if r.Stopped != 2 || r.FreedGB != 2 || r.SessionsEnded != 1 || r.Leftovers != 1 || r.Queued != 1 ||
		r.WaitMean != 90*time.Second || r.Restored != 1 {
		t.Fatalf("%+v", r)
	}
	if !strings.Contains(r.Text, "freeing 2.0 GB") || !strings.Contains(r.Text, "1 request waited for memory, 1 min 30 s on average") {
		t.Fatal(r.Text)
	}
	// The weekly notification: not on the first sweep, then a week later.
	var sent []string
	m.notify = func(title, msg string) { sent = append(sent, msg) }
	m.weeklyReceipt(c.Now())
	c.Add(7*24*time.Hour + time.Minute)
	m.weeklyReceipt(c.Now())
	if len(sent) != 1 {
		t.Fatalf("weekly receipts sent: %v", sent)
	}
}
