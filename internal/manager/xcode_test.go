package manager

import (
	"strings"
	"testing"
	"time"

	"agentrm/internal/api"
	"agentrm/internal/discover"
	"agentrm/internal/model"
)

// xcodeHelper puts an idle Xcode helper in the state, first seen long enough ago to be idle.
func xcodeHelper(m *Manager, id string, mb float64) *model.Resource {
	r := &model.Resource{ID: id, Kind: model.RKXcodeHelper, Source: discover.SrcProcs, Name: "SourceKitService",
		MemMB: mb, PID: 11, ProcStart: m.now().Add(-2 * time.Hour), FirstSeen: m.now().Add(-time.Hour), Cwd: "/",
		Restart: "Xcode starts it again when it needs it"}
	m.st.Resources[id] = r
	return r
}

func TestXcodeHelperAskedAboutUntilTrusted(t *testing.T) {
	m, c := newTest(t, 8)
	m.cfg.DryRun = false
	got := fakeEffects(t, nil)
	var notes []string
	m.notify = func(title, msg string) { notes = append(notes, title) }
	r := xcodeHelper(m, "xcode:11", 2500)
	for i := 0; i < 3; i++ {
		sweepOrphans(m, c.Now())
		c.Add(minutes(m.cfg.GraceMinutes))
	}
	if len(*got) != 0 || len(notes) != 1 || !strings.Contains(notes[0], "SourceKitService") {
		t.Fatalf("with its level off it should only ask, once: effects %+v, notes %v", *got, notes)
	}
	d := m.st.Decisions[len(m.st.Decisions)-1]
	if d.Category != catXcode || !d.DryRun {
		t.Fatalf("decision %+v", d)
	}

	// Earned and turned on: ended, by AgentRM, and not listed as stopped (Xcode starts it).
	for i := 0; i < 5; i++ {
		m.decide("stop", r, "test")
		d := m.st.Decisions[len(m.st.Decisions)-1]
		d.Verdict, d.Time = "right", c.Now().Add(-8*24*time.Hour)
	}
	if _, _, err := m.setConfig(you, api.Req{AutoStop: map[string]bool{catXcode: true}}); err != nil {
		t.Fatal(err)
	}
	if m.cfg.AutoStop[catEngine] {
		t.Fatal("turning on one opt-in level turned on another")
	}
	sweepOrphans(m, c.Now())
	if len(*got) != 1 || (*got)[0].By != "agentrm" || (*got)[0].Res.ID != r.ID {
		t.Fatalf("not ended once trusted: %+v", *got)
	}
	if m.st.Resources[r.ID] != nil || len(m.st.Stopped) != 0 {
		t.Fatal("an ended helper is still listed, or listed for restoring")
	}
}

func TestXcodeHelperLeftAloneWhileXcodeWorks(t *testing.T) {
	cases := map[string]func(m *Manager, r *model.Resource){
		"an Xcode build lease": func(m *Manager, r *model.Resource) {
			m.st.Leases["b"] = &model.Lease{ID: "b", Session: "s1", Kind: model.KXcodeBuild, MemGB: 4}
		},
		"an Xcode build waiting": func(m *Manager, r *model.Resource) {
			m.waiters = append(m.waiters, &model.Waiter{Session: "s2", Kind: model.KXcodeBuild, NeedMB: 4096, Since: m.now(), LastSeen: m.now()})
		},
		"busy": func(m *Manager, r *model.Resource) { r.CPU = 40 },
		"used lately": func(m *Manager, r *model.Resource) {
			r.LastActive = m.now().Add(-5 * time.Minute)
		},
		"a live session in its project": func(m *Manager, r *model.Resource) {
			r.Cwd = "/Users/x/src/app"
			m.st.Sessions["s3"] = &model.Session{ID: "s3", Agent: "claude", Cwd: "/Users/x/src/app/Sources", LastSeen: m.now()}
		},
		"too small": func(m *Manager, r *model.Resource) { r.MemMB = 600 },
	}
	for name, setup := range cases {
		m, c := newTest(t, 8)
		got := fakeEffects(t, nil)
		r := xcodeHelper(m, "xcode:11", 2500)
		m.cfg.AutoStop = map[string]bool{catXcode: true}
		for i := 0; i < 3; i++ {
			setup(m, r) // still the case at every sweep
			sweepOrphans(m, c.Now())
			c.Add(minutes(m.cfg.GraceMinutes))
		}
		if len(*got) != 0 || r.OrphanSince != nil {
			t.Errorf("%s: ended or counting down: %+v", name, *got)
		}
	}

	// A session that has ended does not keep it.
	m, c := newTest(t, 8)
	got := fakeEffects(t, nil)
	r := xcodeHelper(m, "xcode:11", 2500)
	r.Cwd = "/Users/x/src/app"
	ended := c.Now().Add(-time.Hour)
	m.st.Sessions["s3"] = &model.Session{ID: "s3", Cwd: "/Users/x/src/app", EndedAt: &ended}
	m.st.Sessions["s4"] = &model.Session{ID: "s4", Cwd: "/Users/x/src/application"} // a sibling, not the project
	m.cfg.AutoStop = map[string]bool{catXcode: true}
	sweepOrphans(m, c.Now())
	c.Add(minutes(m.cfg.GraceMinutes) + time.Minute)
	sweepOrphans(m, c.Now())
	if len(*got) != 1 {
		t.Fatalf("kept by a session that ended, or by another project: %+v", *got)
	}
}

func TestOnlyYouEndXcodeHelpers(t *testing.T) {
	m, _ := newTest(t, 8)
	r := xcodeHelper(m, "xcode:11", 2500)
	if _, _, err := m.stopNow(agent("a1"), api.Req{ResourceID: r.ID}); err == nil {
		t.Fatal("an agent ended an Xcode helper")
	}
	_, effects, err := m.stopNow(you, api.Req{ResourceID: r.ID})
	if err != nil || len(effects) != 1 || effects[0].By != "you" {
		t.Fatalf("%+v %v", effects, err)
	}
}

// An Xcode helper's CPU is a sign of use: it moves its idle clock.
func TestXcodeHelperCPUIsUse(t *testing.T) {
	m, _ := newTest(t, 8)
	r := xcodeHelper(m, "xcode:11", 2500)
	r.CPU = 30
	if !m.activity.Active(r, nil) {
		t.Fatal("a busy SourceKitService was not counted as in use")
	}
}
