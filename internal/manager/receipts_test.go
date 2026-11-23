package manager

import (
	"os"
	"strings"
	"testing"
	"time"

	"agentrm/internal/api"
	"agentrm/internal/model"
	"agentrm/internal/sys"
)

// A finished lease writes one plain line to its project's receipts file, and an ask that was
// far off tells the agent what to ask next time.
func TestReceiptIsWrittenAndFarOffAskIsAnswered(t *testing.T) {
	m, c := newTest(t, 8)
	m.dir = t.TempDir()
	m.touchSession("s", "claude", "/w/TinyDays")
	l := &model.Lease{ID: "L1", Session: "s", Kind: "xcode-build", MemGB: 6, Declared: true, PeakMB: 2048,
		Cwd: "/w/TinyDays", Cmd: "xcodebuild -scheme TinyDays build"}
	m.learn(l)

	raw, err := os.ReadFile(m.receiptsPath("/w/TinyDays"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.HasPrefix(s, "# What TinyDays") || !strings.Contains(s, "xcode-build") ||
		!strings.Contains(s, "asked 6.0 GB  used 2.0 GB  next time ask 2.4 GB") {
		t.Fatalf("receipt:\n%s", s)
	}
	q := m.st.Questions["receipt:L1"]
	if q == nil || q.For != "s" || !strings.Contains(q.Text, "--mem 2.4") {
		t.Fatalf("far-off ask not answered: %+v", q)
	}
	out, _, _ := m.inbox(api.Caller{Session: "s"}, api.Req{})
	if got := out.(*api.InboxResp).Questions; len(got) != 1 {
		t.Fatalf("the agent did not get its receipt: %v", got)
	}

	// Close enough: a line in the file, nothing said.
	m.learn(&model.Lease{ID: "L2", Session: "s", Kind: "xcode-build", MemGB: 2.4, Declared: true, PeakMB: 2100, Cwd: "/w/TinyDays"})
	if m.st.Questions["receipt:L2"] != nil {
		t.Fatal("a good ask was answered")
	}
	if raw, _ := os.ReadFile(m.receiptsPath("/w/TinyDays")); strings.Count(string(raw), "xcode-build") != 2 {
		t.Fatalf("second receipt missing:\n%s", raw)
	}

	// A day later the note is gone; the file stays.
	c.Add(25 * 3600e9)
	m.prune(c.Now())
	if m.st.Questions["receipt:L1"] != nil {
		t.Fatal("old receipt note not pruned")
	}
}

// The hook's automatic request (no figure) reuses the lease the agent took with its own figure
// instead of asking for AgentRM's larger estimate on top.
func TestHookDoesNotSecondGuessADeclaredLease(t *testing.T) {
	m, _ := newTest(t, 8)
	m.st.Learned["xcode-build"] = 4096
	c := api.Caller{Session: "s", Agent: "claude"}
	out, _, err := m.acquire(c, api.Req{Kind: "xcode-build", MemGB: 2, Purpose: "build"})
	if err != nil || !out.(*api.AcquireResp).Granted {
		t.Fatalf("declared ask: %v %+v", err, out)
	}
	out, _, _ = m.acquire(c, api.Req{Kind: "xcode-build", Via: "hook", Purpose: "auto"})
	r := out.(*api.AcquireResp)
	if !r.Granted || !r.Reused || len(m.st.Leases) != 1 {
		t.Fatalf("hook asked again on top of the declared lease: %+v, %d leases", r, len(m.st.Leases))
	}
}

// A Gradle build runs inside a daemon launchd owns, outside the agent's tree. Its use is the
// daemons' growth since the grant (a daemon it started counts in full), not the small gradlew
// launcher alone: measuring only the launcher charged builds 26 times what they "used".
func TestGradleBuildIsMeasuredByDaemonGrowth(t *testing.T) {
	m, c := newTest(t, 8)
	now := c.Now()
	fp := map[int]float64{102: 150}
	m.footprint = func(pid int) (float64, bool) { v, ok := fp[pid]; return v, ok }
	m.st.Resources["gradle:50"] = &model.Resource{ID: "gradle:50", Kind: model.RKGradle, MemMB: 1000}
	m.learnSession(100, "s", "claude")
	out, _, err := m.acquire(api.Caller{Session: "s", Agent: "claude"}, api.Req{Kind: model.KGradle, MemGB: 2, Purpose: "build"})
	if err != nil || !out.(*api.AcquireResp).Granted {
		t.Fatalf("%v %+v", err, out)
	}
	l := out.(*api.AcquireResp).Lease
	procs := map[int]*sys.Proc{
		100: {PID: 100, PPID: 1, Exe: "claude", Args: "claude", Start: now.Add(-time.Hour)},
		102: {PID: 102, PPID: 100, Exe: "java", Args: "org.gradle.wrapper.GradleWrapperMain assembleDebug", RSSMB: 200, Start: now.Add(time.Second)},
	}
	// The warm daemon grows by 900 MB, and the build starts a Kotlin daemon of 600 MB.
	m.st.Resources["gradle:50"].MemMB = 1900
	m.st.Resources["gradle:60"] = &model.Resource{ID: "gradle:60", Kind: model.RKGradle, MemMB: 600}
	c.Add(5 * time.Second)
	m.updateFootprints(procs, c.Now())
	if got := m.st.Leases[l.ID].PeakMB; got != 150+900+600 {
		t.Fatalf("gradle build measured %.0f MB, want %d", got, 150+900+600)
	}
}
