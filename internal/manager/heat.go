package manager

// Heat. A Mac can be slow with plenty of memory free: several agents each running a build, a
// test suite and a watcher heat it up, macOS slows the CPU down to cool it, and the person at it
// (on a call, say) feels everything lag. Memory leases do not see that, so the thermal pressure
// macOS reports is watched too (sys.ThermalLevel).
//
// While it is at or above thermal_level (2, "heavy", by default):
//
//   - builds and other CPU-heavy work are refused, as on a saturated CPU (busy);
//   - each live agent session whose work is using the CPU is asked, on its next tool call, to
//     ease off if it can: stop what it does not need now, run with fewer jobs, hold back new
//     heavy work. It is asked, not made to: only the agent knows what can wait. Asked again
//     every heatReask while it stays hot and busy;
//   - you get one notification naming who was asked.
//
// Once it has stayed below the level for coolFor, the episode ends: unread requests are
// withdrawn, and every session that read one or was refused is told it may go back to full speed.

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"agentrm/internal/model"
	"agentrm/internal/sys"
)

const (
	// hotAskCPU is how much CPU (percent of one core) a session's work must use to be asked.
	hotAskCPU = 50.0
	// coolFor is how long the Mac must stay below the level before it counts as cooled: the
	// thermal state flickers at the boundary, and agents should not be told to stop and start.
	coolFor = 2 * time.Minute
	// heatReask is how long after a session read the request it is asked again, if still busy.
	heatReask = 15 * time.Minute
	// busyShown is how many of a session's busiest processes are named.
	busyShown = 3
)

type heatState struct {
	since    *time.Time      // the episode began; nil while cool
	coolFrom *time.Time      // below the level since; nil while at or above it
	refused  map[string]bool // sessions refused work during the episode, told when it ends
}

func thermalName(level int) string {
	switch {
	case level < 0:
		return "unknown"
	case level == 0:
		return "nominal"
	case level == 1:
		return "moderate"
	case level == 2:
		return "heavy"
	}
	return "critical"
}

// hot says whether the Mac counts as running hot: at or above the level now, or still in an
// episode that has not yet stayed cool for coolFor.
func (m *Manager) hot() bool {
	lvl := m.cfg.ThermalLevel
	if lvl <= 0 {
		return false
	}
	return m.mem.Thermal >= lvl || m.heat.since != nil
}

// watchHeat follows the thermal state from one reading to the next. Must hold mu.
func (m *Manager) watchHeat(now time.Time) {
	lvl := m.cfg.ThermalLevel
	over := lvl > 0 && m.mem.Thermal >= lvl
	h := &m.heat
	switch {
	case over:
		h.coolFrom = nil
		if h.since == nil {
			t := now
			h.since = &t
			m.event("HOT macOS reports %s thermal pressure: the Mac is slowing itself down. Builds are refused and busy agents asked to ease off until it cools",
				thermalName(m.mem.Thermal))
			if asked := m.askToCool(now); len(asked) > 0 {
				m.notify("Your Mac is running hot", fmt.Sprintf("Asked %s to ease off. Builds wait until it cools.", agentsPhrase(asked)))
			}
			return
		}
		m.askToCool(now)
	case h.since != nil:
		if h.coolFrom == nil {
			t := now
			h.coolFrom = &t
		}
		if now.Sub(*h.coolFrom) >= coolFor {
			m.cooled(now)
		}
	default:
		m.dropCoolNotes(now)
	}
}

// askToCool asks each live agent session whose work is using the CPU, or that holds CPU-heavy
// leases, to ease off. It returns who was newly asked.
func (m *Manager) askToCool(now time.Time) []string {
	var asked []string
	ids := make([]string, 0, len(m.st.Sessions))
	for id := range m.st.Sessions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		s := m.st.Sessions[id]
		if id == "you" || s.EndedAt != nil || s.AgentPID == 0 {
			continue
		}
		leases := m.cpuLeases(id)
		if s.CPUPercent < hotAskCPU && len(leases) == 0 {
			continue
		}
		key := "hot:" + id
		if q := m.st.Questions[key]; q != nil && (!q.Delivered[id] || now.Sub(q.Created) < heatReask) {
			continue // not read yet, or read recently
		}
		var using []string
		if s.CPUPercent >= hotAskCPU {
			using = append(using, fmt.Sprintf("Your work is using %.0f%% CPU (%s).", s.CPUPercent, strings.Join(s.BusyProcs, ", ")))
		}
		if len(leases) > 0 {
			using = append(using, "You hold "+strings.Join(leases, ", ")+".")
		}
		delete(m.st.Questions, "cool:"+id)
		m.st.Questions[key] = &model.Question{Created: now, For: id, Delivered: map[string]bool{}, Answers: map[string]string{},
			Text: fmt.Sprintf("[agentrm] This Mac is running hot: macOS reports %s thermal pressure (since %s) and is slowing itself "+
				"down, so the person using it feels everything lag. %s If you can, ease off now: stop what you do not need this "+
				"minute (watchers, dev servers, a second build or test run), run builds and tests with fewer parallel jobs, and "+
				"hold back new heavy work. Keep what you really need running. New builds are refused until it cools; you are "+
				"told here when it has.", thermalName(m.mem.Thermal), m.heat.since.Format("15:04"), strings.Join(using, " "))}
		m.event("ASKHOT %s (%s): asked to ease off while the Mac is hot", short(id), strings.Join(using, " "))
		asked = append(asked, agentTitle(s.Agent))
	}
	return asked
}

// cpuLeases names the CPU-heavy leases a session holds: their work may run where its process
// tree does not show it (inside Docker's VM).
func (m *Manager) cpuLeases(session string) []string {
	var out []string
	for _, l := range m.st.Leases {
		if l.Session == session && model.CPUKinds[l.Kind] {
			out = append(out, fmt.Sprintf("lease %s (%s)", l.ID, l.Kind))
		}
	}
	sort.Strings(out)
	return out
}

// cooled ends the episode. Must hold mu.
func (m *Manager) cooled(now time.Time) {
	m.event("COOL thermal pressure is %s again after %s: builds are allowed", thermalName(m.mem.Thermal), now.Sub(*m.heat.since).Round(time.Minute))
	tell := map[string]bool{}
	for key, q := range m.st.Questions {
		id, ok := strings.CutPrefix(key, "hot:")
		if !ok {
			continue
		}
		if q.Delivered[id] {
			tell[id] = true
		}
		delete(m.st.Questions, key)
	}
	for id := range m.heat.refused {
		tell[id] = true
	}
	for id := range tell {
		if s := m.st.Sessions[id]; s == nil || s.EndedAt != nil {
			continue
		}
		m.st.Questions["cool:"+id] = &model.Question{Created: now, For: id, Delivered: map[string]bool{}, Answers: map[string]string{},
			Text: fmt.Sprintf("[agentrm] The Mac has cooled down (thermal pressure %s). You can go back to full speed, and start "+
				"again what you held back or stopped.", thermalName(m.mem.Thermal))}
	}
	m.heat = heatState{refused: map[string]bool{}}
}

// dropCoolNotes forgets "cooled down" notes once read or an hour old, and any request to ease
// off left with no hot spell to belong to. Must hold mu.
func (m *Manager) dropCoolNotes(now time.Time) {
	for key, q := range m.st.Questions {
		if id, ok := strings.CutPrefix(key, "cool:"); ok && (q.Delivered[id] || now.Sub(q.Created) > time.Hour) {
			delete(m.st.Questions, key)
		}
		if strings.HasPrefix(key, "hot:") {
			delete(m.st.Questions, key)
		}
	}
}

// noteHeatRefusal remembers a session refused work for heat, to tell it when the Mac cools.
func (m *Manager) noteHeatRefusal(session string) {
	if session != "you" && m.hot() {
		m.heat.refused[session] = true
	}
}

// busiest sums the CPU of an agent's process tree and names its busiest processes.
func busiest(tree []*sys.Proc) (float64, []string) {
	total := 0.0
	var top []*sys.Proc
	for _, p := range tree {
		total += p.CPU
		if p.CPU >= 5 {
			top = append(top, p)
		}
	}
	sort.Slice(top, func(i, j int) bool { return top[i].CPU > top[j].CPU })
	var names []string
	for i, p := range top {
		if i == busyShown {
			break
		}
		names = append(names, fmt.Sprintf("%s pid %d at %.0f%%", procName(p), p.PID, p.CPU))
	}
	return total, names
}

// agentsPhrase says who was asked: "Claude Code", "2 agents (Claude Code, Codex)".
func agentsPhrase(titles []string) string {
	if len(titles) == 1 {
		return titles[0]
	}
	seen, kinds := map[string]bool{}, []string{}
	for _, t := range titles {
		if !seen[t] {
			seen[t] = true
			kinds = append(kinds, t)
		}
	}
	return fmt.Sprintf("%d agents (%s)", len(titles), strings.Join(kinds, ", "))
}

func procName(p *sys.Proc) string {
	name := p.Exe
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	return name
}
