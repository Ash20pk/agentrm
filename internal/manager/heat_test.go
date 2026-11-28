package manager

import (
	"strings"
	"testing"
	"time"

	"agentrm/internal/api"
	"agentrm/internal/sys"
)

func inboxOf(t *testing.T, m *Manager, session string) []string {
	t.Helper()
	out, _, err := m.inbox(agent(session), api.Req{})
	if err != nil {
		t.Fatal(err)
	}
	return out.(*api.InboxResp).Questions
}

// When the Mac runs hot, builds wait and the agents keeping the CPU busy are asked to ease off;
// once it has stayed cool, those who were asked or refused are told they can carry on.
func TestHeatAsksBusyAgentsToEaseOff(t *testing.T) {
	m, c := newTest(t, 8)
	var notes []string
	m.notify = func(title, body string) { notes = append(notes, title+": "+body) }
	now := c.Now()
	procs := map[int]*sys.Proc{
		100: {PID: 100, PPID: 1, Exe: "claude", Args: "claude", Start: now.Add(-time.Hour), CPU: 40},
		101: {PID: 101, PPID: 100, Exe: "/Users/x/.cargo/bin/cargo", Args: "cargo test", Start: now, CPU: 30},
		102: {PID: 102, PPID: 101, Exe: "/usr/bin/rustc", Args: "rustc", Start: now, CPU: 280},
		200: {PID: 200, PPID: 1, Exe: "claude", Args: "claude", Start: now.Add(-time.Hour), CPU: 60},
		201: {PID: 201, PPID: 200, Exe: "/bin/zsh", Args: "zsh", Start: now, CPU: 1},
	}
	m.learnSession(100, "busy", "claude")
	m.learnSession(200, "quiet", "claude")
	tick := func(level int) {
		mem := m.mem
		mem.Thermal = level
		m.fastWith(procs, mem)
	}

	tick(1)
	if q := inboxOf(t, m, "busy"); len(q) != 0 || m.hot() {
		t.Fatalf("moderate heat acted: hot=%v %q", m.hot(), q)
	}
	if s := m.st.Sessions["busy"]; s.CPUPercent != 310 || len(s.BusyProcs) != 2 || !strings.HasPrefix(s.BusyProcs[0], "rustc pid 102") {
		t.Fatalf("busy session measured %.0f%% %q: want 310%% (its tree, not the agent), rustc first", s.CPUPercent, s.BusyProcs)
	}

	tick(2)
	got := inboxOf(t, m, "busy")
	if len(got) != 1 || !strings.Contains(got[0], "running hot") || !strings.Contains(got[0], "310% CPU") || !strings.Contains(got[0], "rustc pid 102") {
		t.Fatalf("busy agent was not asked to ease off: %q", got)
	}
	if q := inboxOf(t, m, "quiet"); len(q) != 0 {
		t.Fatalf("an agent using no CPU was asked: %q", q)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "Claude Code") {
		t.Fatalf("want one notification naming who was asked, got %q", notes)
	}
	if r := acquire(t, m, agent("quiet"), "cargo-build", 1); r.Granted || !strings.Contains(r.Reason, "running hot") {
		t.Fatalf("a build was granted while hot: %+v", r)
	}
	if r := acquire(t, m, agent("quiet"), "server", 0.2); !r.Granted {
		t.Fatalf("heat refused work that is not CPU-heavy: %+v", r)
	}

	// Still hot: not asked again while it has just read it, asked again after heatReask.
	c.Add(5 * time.Minute)
	tick(2)
	if q := inboxOf(t, m, "busy"); len(q) != 0 {
		t.Fatalf("asked again after 5 min: %q", q)
	}
	c.Add(heatReask)
	tick(3)
	if q := inboxOf(t, m, "busy"); len(q) != 1 || !strings.Contains(q[0], "critical") {
		t.Fatalf("not asked again after %s still hot: %q", heatReask, q)
	}
	if len(notes) != 1 {
		t.Fatalf("notified again within one episode: %q", notes)
	}

	// Cooling: builds still wait until it has stayed cool for coolFor.
	tick(1)
	if r := acquire(t, m, agent("quiet"), "cargo-build", 1); r.Granted {
		t.Fatal("a build was granted the moment the level dipped")
	}
	c.Add(coolFor)
	tick(0)
	if m.hot() {
		t.Fatal("still hot after staying cool")
	}
	for _, s := range []string{"busy", "quiet"} { // asked, and refused
		if q := inboxOf(t, m, s); len(q) != 1 || !strings.Contains(q[0], "cooled down") {
			t.Fatalf("%s was not told the Mac cooled: %q", s, q)
		}
	}
	if r := acquire(t, m, agent("quiet"), "cargo-build", 1); !r.Granted {
		t.Fatalf("a build was refused after cooling: %+v", r)
	}
	tick(0)
	for key := range m.st.Questions {
		if strings.HasPrefix(key, "hot:") || strings.HasPrefix(key, "cool:") {
			t.Fatalf("%s left after the episode", key)
		}
	}
}

// An unread request to ease off is withdrawn when the Mac cools, with nothing to say after.
func TestHeatUnreadRequestIsWithdrawn(t *testing.T) {
	m, c := newTest(t, 8)
	now := c.Now()
	procs := map[int]*sys.Proc{
		100: {PID: 100, PPID: 1, Exe: "claude", Args: "claude", Start: now.Add(-time.Hour)},
		101: {PID: 101, PPID: 100, Exe: "node", Args: "node vite", Start: now, CPU: 120},
	}
	m.learnSession(100, "s", "claude")
	mem := m.mem
	mem.Thermal = 2
	m.fastWith(procs, mem)
	if m.st.Questions["hot:s"] == nil {
		t.Fatal("not asked")
	}
	mem.Thermal = 0
	m.fastWith(procs, mem)
	c.Add(coolFor)
	m.fastWith(procs, mem)
	if q := inboxOf(t, m, "s"); len(q) != 0 {
		t.Fatalf("a session that never read the request was told: %q", q)
	}
}

// Off, or unknown (Linux): heat refuses nothing and asks nobody.
func TestHeatOffOrUnknown(t *testing.T) {
	for _, tc := range []struct {
		name           string
		level, thermal int
	}{{"off", 0, 4}, {"unknown", 2, -1}} {
		m, c := newTest(t, 8)
		m.cfg.ThermalLevel = tc.level
		now := c.Now()
		procs := map[int]*sys.Proc{
			100: {PID: 100, PPID: 1, Exe: "claude", Args: "claude", Start: now.Add(-time.Hour)},
			101: {PID: 101, PPID: 100, Exe: "rustc", Args: "rustc", Start: now, CPU: 400},
		}
		m.learnSession(100, "s", "claude")
		mem := m.mem
		mem.Thermal = tc.thermal
		m.fastWith(procs, mem)
		if len(m.st.Questions) != 0 {
			t.Fatalf("%s: asked %v", tc.name, m.st.Questions)
		}
		if r := acquire(t, m, agent("s"), "cargo-build", 1); !r.Granted {
			t.Fatalf("%s: refused %+v", tc.name, r)
		}
	}
}
