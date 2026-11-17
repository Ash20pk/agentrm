package manager

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agentrm/internal/api"
	"agentrm/internal/model"
)

func TestPredictsTheNextHeavyKind(t *testing.T) {
	m, c := newTest(t, 8)
	for _, cmd := range []string{"docker compose up -d", "cargo build --release"} {
		if m.classifier.Classify(cmd) == "" {
			t.Fatalf("%q is not classified; the test needs heavy commands", cmd)
		}
	}
	// In this project a compose stack is always followed by a cargo build.
	for i := 0; i < 8; i++ {
		c.Add(time.Minute)
		m.touchCmd(agent("s"), api.Req{Cmd: "docker compose up -d", Cwd: "/src/app"})
		m.touchCmd(agent("s"), api.Req{Cmd: "cargo build --release", Cwd: "/src/app"})
	}
	compose, cargo := m.classifier.Classify("docker compose up -d"), m.classifier.Classify("cargo build --release")
	if next, p, n := m.predictNext("app", compose); next != cargo || p < 0.99 || n < 8 {
		t.Fatalf("predictNext = %s %.2f %d", next, p, n)
	}
	p := m.predictState()
	if p.Total == 0 || p.Hits <= p.BaseHits {
		t.Fatalf("the predictor should beat the most-frequent-kind baseline on an alternating pattern: %+v", *p)
	}
	if !strings.Contains(m.predictionScore(), "baseline") {
		t.Fatal(m.predictionScore())
	}
}

func TestPredictedNeedAsksHoldersEarly(t *testing.T) {
	m, c := newTest(t, 3)
	other := acquire(t, m, agent("o"), "server", 0.5)
	addRes(m, "srv:9", model.RKServer, 1800, other.Lease.ID)
	m.mem.AvailableMB = 1.2 * 1024
	for i := 0; i < 6; i++ {
		c.Add(time.Minute)
		m.touchCmd(agent("s"), api.Req{Cmd: "docker compose up -d", Cwd: "/src/app"})
		m.touchCmd(agent("s"), api.Req{Cmd: "kind create cluster", Cwd: "/src/app"})
	}
	out, _, _ := m.inbox(agent("o"), api.Req{})
	if q := out.(*api.InboxResp).Questions; len(q) == 0 || !strings.Contains(q[0], "agentrm yield") {
		t.Fatalf("a holder was not asked ahead of a likely request that will not fit: %v", q)
	}
	if len(m.st.Leases) != 1 {
		t.Fatal("a prediction took or granted something")
	}
}

func TestLifetimesGiveExpectedEndsAndLikelyDone(t *testing.T) {
	m, c := newTest(t, 8)
	for i := 0; i < 6; i++ {
		l := acquire(t, m, agent("s"), "server", 0.2).Lease
		l.HadResources, l.Cwd = true, "/src/app"
		c.Add(20 * time.Minute)
		m.dropLease(l, "done")
	}
	l := acquire(t, m, agent("s"), "server", 0.2).Lease
	l.Cwd = "/src/app"
	m.markExpected(c.Now())
	if l.ExpectedEnd == nil || !l.ExpectedEnd.Equal(l.Created.Add(20*time.Minute)) || l.LikelyDone {
		t.Fatalf("expected end: %+v", l)
	}
	c.Add(45 * time.Minute)
	m.markExpected(c.Now())
	if !l.LikelyDone {
		t.Fatal("a lease far past its kind's usual life, and idle, was not marked probably finished")
	}
}

func TestLearnedGraceWeighsHoldingAgainstRestarting(t *testing.T) {
	m, _ := newTest(t, 8)
	srv := &model.Resource{Kind: model.RKServer, MemMB: 400}
	if _, ok := m.learnedGrace(srv, 1); ok {
		t.Fatal("learned timing used with no observations")
	}
	// Servers here are nearly always used again within 6 minutes, and restarting one is slow.
	m.st.RestartSeconds = map[string]float64{model.RKServer: 120}
	for i := 0; i < 30; i++ {
		m.noteIdleGap(model.RKServer, float64(2+i%5))
	}
	g, ok := m.learnedGrace(srv, 1)
	if !ok || g < 6*time.Minute {
		t.Fatalf("with reuse common and restarts slow, it should wait past the usual gaps: %v %v", g, ok)
	}
	// Clusters here are never used again once idle: stop early.
	for i := 0; i < 30; i++ {
		m.noteIdleGap(model.RKCluster, never)
	}
	if g, _ := m.learnedGrace(&model.Resource{Kind: model.RKCluster, MemMB: 3000}, 1); g != time.Minute {
		t.Fatalf("never reused: want the lower bound, got %v", g)
	}
	// Bounded above by three times the grace, whatever is learned.
	m.st.RestartSeconds[model.RKServer] = 1e6
	if g, _ := m.learnedGrace(srv, 1); g > minutes(3*m.cfg.GraceMinutes) {
		t.Fatalf("past the bound: %v", g)
	}
	// And it survives being saved.
	if _, err := json.Marshal(m.st); err != nil {
		t.Fatalf("state with learned timing does not save: %v", err)
	}
}

func TestShortBuildBackfillsOnceAheadOfABigRequest(t *testing.T) {
	m, c := newTest(t, 4) // 3 GB headroom
	// Builds in this project take two minutes.
	for i := 0; i < 6; i++ {
		l := acquire(t, m, agent("b"), "cargo-build", 0.5).Lease
		l.Cwd = "/src/app"
		c.Add(2 * time.Minute)
		m.dropLease(l, "done")
	}
	if r := acquire(t, m, agent("big"), "cluster", 6); r.Granted {
		t.Fatal("6 GB granted with 3 GB headroom")
	}
	c.Add(time.Second)
	out, _, _ := m.acquire(agent("b"), api.Req{Kind: "cargo-build", MemGB: 1, Purpose: "build", Cwd: "/src/app"})
	r := out.(*api.AcquireResp)
	if !r.Granted || !r.Lease.Backfill {
		t.Fatalf("a short build that fits in free memory did not backfill: %+v", r)
	}
	m.dropLease(m.st.Leases[r.Lease.ID], "done")
	out, _, _ = m.acquire(agent("b"), api.Req{Kind: "cargo-build", MemGB: 1, Purpose: "build", Cwd: "/src/app"})
	if out.(*api.AcquireResp).Granted {
		t.Fatal("the same session went ahead of the same waiting request twice")
	}
	// A kind with no learned lifetime never backfills.
	out, _, _ = m.acquire(agent("x"), api.Req{Kind: "docker-build", MemGB: 1, Purpose: "build", Cwd: "/src/other"})
	if out.(*api.AcquireResp).Granted {
		t.Fatal("a build of unknown length backfilled")
	}
}
