package manager

// The collector: mark and sweep over who owns what.
//
// Roots are what keeps something running: a live agent session (its process still there,
// checked by PID and start time, so a reused PID never counts), you (anything started from a
// GUI app, pinned or on the never-stop list), and leases with recent use. Edges are recorded
// as things start: session -> lease -> resource, and session -> every process in the agent's
// tree, including the ones that detach and re-parent to launchd (daemons, `nohup`, `&`).
//
// When a session stops being a root (it says it ended, or its agent process is gone) its
// leases are released at once, and everything it started or held is marked owner-gone. The
// processes from its tree that are still running are found and tracked as leftovers, even
// when they are of no kind AgentRM knows. Sweeping is then the usual staged and reversible
// path (asked, paused, stopped, restorable), with timing set by the evidence (collectTiming):
//
//   - owner gone, and nothing has used it since: the young generation, collected in minutes;
//   - no known owner: today's grace, halved under memory pressure;
//   - no known owner and running for a day or more: the old generation, three times the grace.
//
// Anything that shows use after its owner left (traffic, CPU, another session's command) loses
// the owner-gone fast path: someone may have taken it over.

import (
	"fmt"
	"path/filepath"
	"strconv"
	"time"

	"agentrm/internal/api"
	"agentrm/internal/model"
	"agentrm/internal/sys"
)

// maxTree bounds how many processes are remembered per session.
const maxTree = 4000

// checkLiveness finds sessions whose agent process has exited, ends them, and remembers each
// live agent's process tree for when it does. Must hold mu; procs must be a full table.
func (m *Manager) checkLiveness(procs map[int]*sys.Proc, now time.Time) {
	children := map[int][]int{}
	for _, p := range procs {
		children[p.PPID] = append(children[p.PPID], p.PID)
	}
	// Two sessions can share one agent process (Claude Code's /clear starts a new session):
	// only the newest one that has not ended owns the tree.
	owner := map[int]string{}
	for id, s := range m.st.Sessions {
		if s.AgentPID > 0 && s.EndedAt == nil && (owner[s.AgentPID] == "" || s.LastSeen.After(m.st.Sessions[owner[s.AgentPID]].LastSeen)) {
			owner[s.AgentPID] = id
		}
	}
	for id, s := range m.st.Sessions {
		if id == "you" || s.AgentPID <= 0 {
			continue
		}
		p := procs[s.AgentPID]
		alive := p != nil && sys.SameProcess(p, s.AgentStart)
		if alive {
			delete(m.missing, id)
			if o := owner[s.AgentPID]; o != "" && o != id {
				delete(m.trees, id) // its process now belongs to a newer session
				continue
			}
			m.recordTree(id, s.AgentPID, procs, children)
			continue
		}
		if _, tracked := m.trees[id]; !tracked && s.EndedAt != nil {
			continue
		}
		// Gone from one table could be a ps hiccup; gone from two is gone.
		m.missing[id]++
		if m.missing[id] < 2 {
			continue
		}
		delete(m.missing, id)
		if s.EndedAt == nil {
			m.endSession(s, fmt.Sprintf("its %s process (pid %d) exited", s.Agent, s.AgentPID), now)
		}
		m.findLeftovers(s, procs, now)
		delete(m.trees, id)
		if m.agentSessions[s.AgentPID] == id {
			delete(m.agentSessions, s.AgentPID)
			delete(m.st.AgentSessions, strconv.Itoa(s.AgentPID))
		}
	}
}

// recordTree adds the agent's current descendants to what its session started, keeping any
// earlier ones still running even if they have since detached from it.
func (m *Manager) recordTree(id string, agentPID int, procs map[int]*sys.Proc, children map[int][]int) {
	tree := map[int]time.Time{}
	for pid, start := range m.trees[id] {
		if p := procs[pid]; p != nil && sys.SameProcess(p, start) {
			tree[pid] = start
		}
	}
	stack := append([]int(nil), children[agentPID]...)
	for len(stack) > 0 && len(tree) < maxTree {
		pid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if _, seen := tree[pid]; seen {
			continue
		}
		if p := procs[pid]; p != nil {
			tree[pid] = p.Start
			stack = append(stack, children[pid]...)
		}
	}
	m.trees[id] = tree
}

// endSession stops treating a session as a root: its leases are released and what it started
// or held is marked owner-gone. Must hold mu.
func (m *Manager) endSession(s *model.Session, why string, now time.Time) {
	t := now
	s.EndedAt, s.EndReason = &t, why
	held := map[string]bool{}
	n := 0
	for _, l := range m.st.Leases {
		if l.Session != s.ID {
			continue
		}
		for _, rid := range l.Resources {
			held[rid] = true
		}
		m.dropLease(l, "its session ended: "+why)
		n++
	}
	marked, mb := 0, 0.0
	for _, r := range m.st.Resources {
		if r.LeaseID != "" || r.Owner != "" || !(held[r.ID] || r.StartedBy == s.ID) {
			continue
		}
		r.Owner, r.OwnerGoneAt = s.ID, &t
		marked++
		mb += r.MemMB
	}
	// A session that ended waits for nothing.
	kept := m.waiters[:0]
	for _, w := range m.waiters {
		if w.Session != s.ID {
			kept = append(kept, w)
		}
	}
	m.waiters = kept
	m.event("ENDED session %s (%s): %s. Released %d lease(s); %d thing(s) it started or held (%.1f GB) are now collected.",
		short(s.ID), s.Agent, why, n, marked, mb/1024)
	if marked > 0 {
		m.notify(agentTitle(s.Agent)+" finished", fmt.Sprintf("It left %d thing(s) running (%.1f GB). Collecting them unless someone claims them.",
			marked, mb/1024))
	}
}

// findLeftovers records processes from an ended session's tree that are still running and no
// longer under any agent. Only the top of each leftover tree is recorded, sized with what is
// under it. Must hold mu.
func (m *Manager) findLeftovers(s *model.Session, procs map[int]*sys.Proc, now time.Time) {
	cands := map[int]*sys.Proc{}
	for pid, start := range m.trees[s.ID] {
		p := procs[pid]
		if p == nil || pid == s.AgentPID || !sys.SameProcess(p, start) {
			continue
		}
		if apid, _ := sys.AgentAncestor(procs, pid); apid > 0 {
			continue // another agent has it
		}
		cands[pid] = p
	}
	if len(cands) == 0 {
		return
	}
	under := map[int]float64{}
	for _, p := range procs {
		for q, i := procs[p.PPID], 0; q != nil && q.PID > 1 && i < 64; q, i = procs[q.PPID], i+1 {
			under[q.PID] += p.RSSMB
		}
	}
	if m.st.Leftovers == nil {
		m.st.Leftovers = map[string]*model.Leftover{}
	}
	found := 0
	for pid, p := range cands {
		if cands[p.PPID] != nil {
			continue
		}
		mb := p.RSSMB + under[pid]
		if mb < m.cfg.LeftoverMinMB && p.CPU < 2 {
			continue
		}
		m.st.Leftovers[strconv.Itoa(pid)] = &model.Leftover{PID: pid, Start: p.Start, Session: s.ID, Agent: s.Agent, Found: now}
		m.event("LEFTOVER %s (pid %d, %.1f GB) was started by session %s (%s) and kept running after it ended",
			filepath.Base(p.Exe), pid, mb/1024, short(s.ID), s.Agent)
		found++
	}
	if found > 0 {
		m.kickSweep()
	}
}

// withLeftovers adds the tracked leftovers that are still running to a discovery result. A
// leftover discovery already knows (a Gradle daemon, a dev server) is not added again; it is
// credited to the session that started it instead. Must hold mu.
func (m *Manager) withLeftovers(found []*model.Resource, procs map[int]*sys.Proc) []*model.Resource {
	byPID := map[int]*model.Resource{}
	for _, r := range found {
		if r.PID > 0 {
			byPID[r.PID] = r
		}
	}
	under := map[int]float64{}
	if len(m.st.Leftovers) > 0 {
		for _, p := range procs {
			for q, i := procs[p.PPID], 0; q != nil && q.PID > 1 && i < 64; q, i = procs[q.PPID], i+1 {
				under[q.PID] += p.RSSMB
			}
		}
	}
	for key, lo := range m.st.Leftovers {
		p := procs[lo.PID]
		if p == nil || !sys.SameProcess(p, lo.Start) {
			delete(m.st.Leftovers, key)
			continue
		}
		gone := lo.Found
		if s := m.st.Sessions[lo.Session]; s != nil && s.EndedAt != nil {
			gone = *s.EndedAt
		}
		if r := byPID[lo.PID]; r != nil {
			if r.StartedBy == "" {
				r.StartedBy = lo.Session
			}
			if r.Owner == "" {
				g := gone
				r.Owner, r.OwnerGoneAt = lo.Session, &g
			}
			continue
		}
		g := gone
		found = append(found, &model.Resource{ID: "left:" + key, Kind: model.RKLeftover, Source: "procs",
			Name: filepath.Base(p.Exe), Detail: trunc(p.Args, 90), PID: p.PID, MemMB: p.RSSMB + under[p.PID], CPU: p.CPU,
			StartedBy: lo.Session, Owner: lo.Session, OwnerGoneAt: &g,
			Restart: "re-run the command that started it"})
	}
	return found
}

// ownerGone is when r's owner went, if it went and nothing has used r since.
func (m *Manager) ownerGone(r *model.Resource) *time.Time {
	if r.OwnerGoneAt == nil {
		return nil
	}
	gone := *r.OwnerGoneAt
	if r.LastActive.After(gone.Add(time.Minute)) {
		return nil // used after its owner left: someone may have taken it over
	}
	for session, t := range m.st.Touches[r.ID] {
		if session != r.Owner && t.After(gone) {
			return nil
		}
	}
	return &gone
}

// collectTiming is how long an unclaimed resource waits before it is paused (grace), and how
// long it stays paused before it is stopped (hold), from the evidence about who needs it.
func (m *Manager) collectTiming(r *model.Resource, now time.Time, blocked bool) (grace, hold time.Duration, reason string, ownerGone bool) {
	grace, hold = minutes(m.cfg.GraceMinutes), minutes(m.cfg.PauseMinutes)
	if gone := m.ownerGone(r); gone != nil {
		grace = minutes(max(1, m.cfg.OwnerGoneGraceMinutes))
		if m.cfg.PauseMinutes > 0 && m.cfg.OwnerGonePauseMinutes > 0 {
			hold = minutes(min(m.cfg.PauseMinutes, m.cfg.OwnerGonePauseMinutes))
		}
		who := agentTitle(agentOfSession(r.Owner))
		if s := m.st.Sessions[r.Owner]; s != nil && s.Agent != "" {
			who = agentTitle(s.Agent)
		}
		return grace, hold, fmt.Sprintf("%s, which started or held it, ended %s ago and nothing has used it since",
			who, now.Sub(*gone).Round(time.Minute)), true
	}
	old := m.cfg.OldGenerationHours > 0 && !r.FirstSeen.IsZero() &&
		now.Sub(r.FirstSeen) > time.Duration(m.cfg.OldGenerationHours)*time.Hour
	switch {
	case old:
		grace *= 3
		reason = fmt.Sprintf("unclaimed, and it has been running for %s", now.Sub(r.FirstSeen).Round(time.Hour))
	default:
		if t, _ := m.thrashing(); t || blocked {
			grace = max(time.Minute, grace/2)
		}
	}
	return grace, hold, reason, false
}

// end is the SessionEnd hook: the session says it is over. Its process may still be exiting,
// so its tree is kept until it is gone, and leftovers are found then.
func (m *Manager) end(c api.Caller, r api.Req) (any, []Effect, error) {
	if c.Session == "" || c.Session == "detached" || c.Session == "you" {
		return m.release(c, r)
	}
	s := m.touchSession(c.Session, c.Agent, r.Cwd)
	n := 0
	for _, l := range m.st.Leases {
		if l.Session == s.ID {
			n++
		}
	}
	m.endSession(s, "it ended", m.now())
	return &api.CountResp{N: n}, nil, nil
}

// sessionEnded reports whether id is a session that has ended.
func (m *Manager) sessionEnded(id string) *time.Time {
	if s := m.st.Sessions[id]; s != nil {
		return s.EndedAt
	}
	return nil
}

func agentTitle(agent string) string {
	switch agent {
	case "claude":
		return "Claude Code"
	case "codex":
		return "Codex"
	case "gemini":
		return "Gemini CLI"
	case "aider":
		return "Aider"
	case "opencode":
		return "OpenCode"
	case "cursor":
		return "Cursor"
	case "", "you":
		return "You"
	}
	return agent
}
