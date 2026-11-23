package manager

import (
	"sort"
	"strings"
	"testing"
	"time"

	"agentrm/internal/api"
	"agentrm/internal/model"
)

// prepWorld: session "a" in project "app" runs compose, which is followed by a cargo build nine
// times in ten; a build there peaks at 4 GB; 2 GB can be handed out; an unclaimed server has
// sat idle for 20 minutes.
func prepWorld(t *testing.T) (*Manager, *clock, *[]Effect) {
	t.Helper()
	m, c := newTest(t, 3)
	m.cfg.DryRun = false
	got := fakeEffects(t, nil)
	m.predictState().Next["app"] = map[string]map[string]int{"compose": {"cargo-build": 9, "server": 1}}
	m.st.Learned["cargo-build"] = 4096
	m.st.Accuracy = map[string]*model.Accuracy{"predict:cargo-build": {N: 10, Actual: 9}}
	idle(m, c, "srv:old", 3000)
	return m, c, got
}

// idle adds an unclaimed server, asked about 3 minutes ago and unused for 20.
func idle(m *Manager, c *clock, id string, mb float64) *model.Resource {
	since := c.At(-3 * time.Minute)
	r := &model.Resource{ID: id, Kind: model.RKServer, Source: "procs", Name: id, MemMB: mb, PID: 4242,
		FirstSeen: c.At(-time.Hour), LastActive: c.At(-20 * time.Minute), OrphanSince: &since}
	m.st.Resources[id] = r
	return r
}

func events(m *Manager, kind string) []string {
	var out []string
	for _, e := range m.events {
		if e.Kind == kind {
			out = append(out, e.Text)
		}
	}
	return out
}

func run(m *Manager, effects []Effect) {
	m.applyEffects(RunEffects(m.gateAuto(effects)))
}

// A queued request starts reclaim in the pass that queues it: the plan is made and the clocks
// are moved before acquire returns, and the very next orphan check stops what it counted on.
func TestQueuedRequestReclaimsInTheSamePass(t *testing.T) {
	m, c := newTest(t, 3) // 2 GB can be handed out
	m.cfg.DryRun = false
	got := fakeEffects(t, nil)
	r := idle(m, c, "srv:old", 3000)
	before := *r.OrphanSince
	if out := acquire(t, m, agent("a"), "cluster", 3); out.Granted || !strings.Contains(out.Reason, "Reclaiming unclaimed srv:old") {
		t.Fatalf("not queued with a reclaim plan: %+v", out)
	}
	if !r.Urgent || !r.OrphanSince.Before(before) || len(events(m, "RECLAIM")) != 1 {
		t.Fatalf("reclaim did not run when the request was queued: urgent %v, orphan since %v (was %v)", r.Urgent, r.OrphanSince, before)
	}
	run(m, m.checkOrphans(c.Now())) // no time passes
	if len(*got) != 1 || (*got)[0].Op != "stop" || (*got)[0].Res.ID != "srv:old" {
		t.Fatalf("the next orphan check did not stop what reclaim counted on: %+v", *got)
	}
}

// A confident prediction pauses the cheapest unclaimed thing before the request; the request
// then fits and is granted at once; when its work is done what was paused is resumed, with its
// unclaimed clock where it was.
func TestPreparedAheadOfAConfidentPrediction(t *testing.T) {
	m, c, got := prepWorld(t)
	r := m.st.Resources["srv:old"]
	since := *r.OrphanSince
	m.observeKind("a", "/src/app", "compose", c.Now())
	run(m, m.tendPrepared(c.Now()))
	if len(*got) != 1 || (*got)[0].Op != "pause" || (*got)[0].Res.ID != "srv:old" {
		t.Fatalf("expected one pause ahead of the build: %+v", *got)
	}
	if r.PausedAt == nil || r.PreparedFor == "" || r.PreparedUntil == nil || len(events(m, "PREPARED")) != 1 {
		t.Fatalf("pause not recorded as prepared: %+v %v", r, m.events)
	}
	*got = nil

	// Paused for a while, it is not taken further while nobody waits.
	m.cfg.PauseMinutes = 1
	c.Add(3 * time.Minute)
	if effs := m.checkOrphans(c.Now()); len(effs) != 0 {
		t.Fatalf("a pause made on a prediction led to %+v", effs)
	}

	// The build comes, and fits in what was made ready.
	m.mem.AvailableMB += 3000
	m.observeKind("a", "/src/app", "cargo-build", c.Now())
	if out, _, _ := m.acquire(agent("a"), api.Req{Kind: "cargo-build", Cwd: "/src/app", Purpose: "test"}); !out.(*api.AcquireResp).Granted {
		t.Fatalf("the predicted request was not granted at once: %+v", out)
	}
	if a := m.st.Accuracy["predict:cargo-build"]; a.N != 11 || a.Actual != 10 {
		t.Fatalf("the hit was not scored: %+v", a)
	}
	// Still building: it stays paused.
	run(m, m.tendPrepared(c.Now()))
	if len(*got) != 0 || r.PausedAt == nil {
		t.Fatalf("resumed while the build it made room for runs: %+v", *got)
	}
	// The build is done: resumed, and its unclaimed clock goes on.
	_, _, _ = m.release(agent("a"), api.Req{Kind: "cargo-build"})
	run(m, m.tendPrepared(c.Now()))
	if len(*got) != 1 || (*got)[0].Op != "resume" || r.PausedAt != nil || r.PreparedFor != "" || r.PreparedUntil != nil {
		t.Fatalf("not resumed once the build was done: %+v %+v", *got, r)
	}
	if r.OrphanSince == nil || !r.OrphanSince.Equal(since) || len(events(m, "UNPREPARED")) != 1 {
		t.Fatalf("undoing the pause restarted its clock or was not reported: %v %v", r.OrphanSince, events(m, "UNPREPARED"))
	}
}

// A prediction that does not come true is undone after the window and scored as a miss.
func TestPreparedMissIsUndoneAndScored(t *testing.T) {
	m, c, got := prepWorld(t)
	r := m.st.Resources["srv:old"]
	m.observeKind("a", "/src/app", "compose", c.Now())
	run(m, m.tendPrepared(c.Now()))
	if r.PausedAt == nil {
		t.Fatal("not paused ahead")
	}
	*got = nil
	c.Add(prepareWindow - time.Second)
	run(m, m.tendPrepared(c.Now()))
	if len(*got) != 0 {
		t.Fatalf("undone before the window ended: %+v", *got)
	}
	c.Add(time.Second)
	run(m, m.tendPrepared(c.Now()))
	if len(*got) != 1 || (*got)[0].Op != "resume" || r.PausedAt != nil || r.PreparedFor != "" {
		t.Fatalf("not resumed after a miss: %+v", *got)
	}
	if a := m.st.Accuracy["predict:cargo-build"]; a.N != 11 || a.Actual != 9 {
		t.Fatalf("the miss was not scored: %+v", a)
	}
	if len(m.preps) != 0 {
		t.Fatalf("settled prediction kept: %+v", m.preps)
	}
	if line := AccuracyLine(m.st.Accuracy); !strings.Contains(line, "cargo-build predicted ahead came 9/11 (82%)") {
		t.Fatalf("ACCURACY line: %q", line)
	}
}

// Below the hit rate (or with too few tries) nothing is done; what would have been is logged,
// and the prediction is still scored, so the record can earn the right to act.
func TestPredictionsBelowTheBarOnlyRecord(t *testing.T) {
	for name, acc := range map[string]*model.Accuracy{
		"low hit rate": {N: 10, Actual: 6},
		"too few":      {N: 4, Actual: 4},
		"none":         nil,
	} {
		t.Run(name, func(t *testing.T) {
			m, c, got := prepWorld(t)
			if acc == nil {
				delete(m.st.Accuracy, "predict:cargo-build")
			} else {
				m.st.Accuracy["predict:cargo-build"] = acc
			}
			m.observeKind("a", "/src/app", "compose", c.Now())
			run(m, m.tendPrepared(c.Now()))
			if len(*got) != 0 || m.st.Resources["srv:old"].PausedAt != nil {
				t.Fatalf("acted on an untrusted prediction: %+v", *got)
			}
			if ev := events(m, "PREDICT"); len(ev) == 0 || !strings.Contains(ev[len(ev)-1], "would pause srv:old") {
				t.Fatalf("what it would have done was not logged: %v", ev)
			}
			want := 1
			if acc != nil {
				want = acc.N + 1
			}
			m.observeKind("a", "/src/app", "cargo-build", c.Now())
			if a := m.st.Accuracy["predict:cargo-build"]; a == nil || a.N != want {
				t.Fatalf("not scored: %+v", a)
			}
		})
	}
}

// In dry run nothing changes: no effect, no field of the resource, only the decision logged.
func TestPrepareDryRunChangesNothing(t *testing.T) {
	m, c, got := prepWorld(t)
	m.cfg.DryRun = true
	r := m.st.Resources["srv:old"]
	before := *r
	m.observeKind("a", "/src/app", "compose", c.Now())
	run(m, m.tendPrepared(c.Now()))
	c.Add(prepareWindow)
	run(m, m.tendPrepared(c.Now()))
	if len(*got) != 0 || r.PausedAt != nil || r.PreparedFor != "" || r.Urgent || !r.OrphanSince.Equal(*before.OrphanSince) {
		t.Fatalf("dry run changed something: %+v %+v", *got, r)
	}
	if len(m.st.Decisions) != 1 || m.st.Decisions[0].Action != "prepare" || !m.st.Decisions[0].DryRun || len(events(m, "WOULD")) != 1 {
		t.Fatalf("the would-be pause was not logged: %+v", m.st.Decisions)
	}
	for _, lv := range m.trustLevels(c.Now()) {
		if lv.Decided != 0 {
			t.Fatalf("a prepare decision counted as auto-stop evidence: %+v", lv)
		}
	}
}

// Only unclaimed, idle, pausable things are paused ahead: never what a live lease holds, what
// its source no longer shows, what is pinned or exempt, what you started, what was used just
// now, or what the predicting session itself started. Nor anything while AgentRM lets
// everything through.
func TestPrepareTouchesOnlyFairGame(t *testing.T) {
	m, c, got := prepWorld(t)
	m.st.Learned["cargo-build"] = 40000 // short of everything: every candidate is taken
	m.mem.TotalMB = 96 * 1024
	m.st.Leases["L"] = &model.Lease{ID: "L", Session: "b", Kind: "server", MemGB: 1, Created: c.Now(), LastUsed: c.At(-time.Hour)}
	held := idle(m, c, "srv:held", 900)
	held.LeaseID = "L"
	m.st.Leases["L"].Resources = []string{"srv:held"}
	unseen := idle(m, c, "srv:unseen", 900)
	t0 := c.Now()
	unseen.Unseen = &t0
	idle(m, c, "srv:pinned", 900).Pinned = true
	idle(m, c, "srv:yours", 900).StartedBy = "you"
	idle(m, c, "srv:exempt", 900)
	m.cfg.NeverStop = []string{"srv:exempt"}
	idle(m, c, "srv:busy", 900).LastActive = c.At(-time.Minute)
	idle(m, c, "srv:own", 900).StartedBy = "a"
	idle(m, c, "srv:fresh", 900).OrphanSince = nil
	idle(m, c, "sim:x", 900).Kind = model.RKSimulator // cannot be paused
	idle(m, c, "srv:gone", 500).Owner = "z"
	gone := c.At(-10 * time.Minute)
	m.st.Resources["srv:gone"].OwnerGoneAt = &gone

	until := c.At(time.Hour)
	m.st.BypassUntil = &until
	m.observeKind("a", "/src/app", "compose", c.Now())
	run(m, m.tendPrepared(c.Now()))
	if len(*got) != 0 {
		t.Fatalf("paused while everything is let through: %+v", *got)
	}
	m.st.BypassUntil = nil
	m.preps = map[string]*prep{}
	m.observeKind("a", "/src/app", "compose", c.Now())
	run(m, m.tendPrepared(c.Now()))
	var ids []string
	for _, e := range *got {
		ids = append(ids, e.Res.ID)
	}
	sort.Strings(ids)
	if strings.Join(ids, " ") != "srv:gone srv:old" {
		t.Fatalf("paused %v, want only srv:gone and srv:old", ids)
	}
	// Cheapest first: what an ended session left, then the longest idle, then the biggest.
	idle(m, c, "srv:older", 100).LastActive = c.At(-time.Hour)
	idle(m, c, "srv:big", 9000)
	var order []string
	for _, r := range m.prepareCandidates("a", c.Now()) {
		order = append(order, r.ID)
	}
	if strings.Join(order, " ") != "srv:older srv:big" {
		t.Fatalf("candidates in order %v", order)
	}
}
