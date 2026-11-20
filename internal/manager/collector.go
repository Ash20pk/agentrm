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
	"strings"
	"sync"
	"time"

	"agentrm/internal/api"
	"agentrm/internal/discover"
	"agentrm/internal/model"
	"agentrm/internal/sys"
	"agentrm/internal/trace"
)

// maxTree bounds how many processes are remembered per session.
const maxTree = 4000

// ---- who started a process ----

// procKey identifies one process over time: a PID is reused, a PID and its start are not.
type procKey struct {
	pid   int
	start int64
}

func keyOf(p *sys.Proc) procKey { return procKey{p.PID, p.Start.Unix()} }

// infoCache remembers what each process was started with (argv, environment, which agent
// session), read once per process from the kernel. It has its own lock so it can be filled
// outside mu.
type infoCache struct {
	mu   sync.Mutex
	m    map[procKey]sys.ProcInfo
	read func(pid int) (sys.ProcInfo, bool)
	// direct: no caching, every lookup reads (replay, where the trace is the cache and an
	// owner may be recorded a frame after the process first appears).
	direct bool
}

func newInfoCache(read func(int) (sys.ProcInfo, bool)) *infoCache {
	return &infoCache{m: map[procKey]sys.ProcInfo{}, read: read}
}

// systemBinary says whether a process is one of Apple's, whose environment the kernel withholds
// and which no agent leaves behind worth collecting.
func systemBinary(exe string) bool {
	for _, p := range []string{"/System/", "/usr/bin/", "/usr/sbin/", "/usr/libexec/", "/bin/", "/sbin/", "/Library/Apple/"} {
		if strings.HasPrefix(exe, p) {
			return true
		}
	}
	return false
}

// refresh reads every process not seen before and forgets those that are gone.
func (c *infoCache) refresh(procs map[int]*sys.Proc) {
	if procs == nil || c.read == nil || c.direct {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	seen := make(map[procKey]bool, len(procs))
	for _, p := range procs {
		k := keyOf(p)
		seen[k] = true
		if _, ok := c.m[k]; ok || p.PID <= 1 || systemBinary(p.Exe) {
			continue
		}
		info, _ := c.read(p.PID)
		c.m[k] = info
	}
	for k := range c.m {
		if !seen[k] {
			delete(c.m, k)
		}
	}
}

// getStarted is the entry for the process that has pid and started at start: never another
// process that has the PID since. Empty if that process was never read, or start is unknown.
func (c *infoCache) getStarted(pid int, start time.Time) sys.ProcInfo {
	if c.direct || start.IsZero() {
		return sys.ProcInfo{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[procKey{pid, start.Unix()}]
}

func (c *infoCache) get(p *sys.Proc) sys.ProcInfo {
	if p == nil {
		return sys.ProcInfo{}
	}
	if c.direct {
		info, _ := c.read(p.PID)
		return info
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[keyOf(p)]
}

// envOwner is the agent session an agent's environment says started p, and whether that
// agent is still running. ok is false when nothing says.
func (m *Manager) envOwner(p *sys.Proc, procs map[int]*sys.Proc) (session string, agentPID int, alive, ok bool) {
	info := m.infos.get(p)
	if !info.Owned() {
		return "", 0, false, false
	}
	session = info.Session
	switch {
	case session == "" && info.AgentPID > 0:
		session = m.sessionOf(info.AgentPID, info.Agent)
	case session == "":
		// Only the agent's kind is known (an MCP server inherits the agent's own environment,
		// which names no session). Its agent is certainly gone if no agent of that kind that
		// was already running when it started is running now.
		session = info.Agent + "-unknown"
		for _, ap := range procs {
			if sys.AgentKind(ap) == info.Agent && (ap.Start.IsZero() || p.Start.IsZero() || !ap.Start.After(p.Start.Add(3*time.Second))) {
				alive = true
				break
			}
		}
		return session, 0, alive, true
	}
	if info.AgentPID > 0 {
		// The agent is alive if that PID is still an agent, and was already running when p
		// started (a reused PID is younger than the processes it is named in).
		ap := procs[info.AgentPID]
		alive = ap != nil && sys.AgentKind(ap) != "" &&
			(ap.Start.IsZero() || p.Start.IsZero() || !ap.Start.After(p.Start.Add(3*time.Second)))
	} else if s := m.st.Sessions[session]; s != nil {
		alive = s.EndedAt == nil && s.AgentPID > 0 && procs[s.AgentPID] != nil
	}
	return session, info.AgentPID, alive, true
}

// ---- liveness ----

// checkLiveness finds sessions whose agent process has exited, ends them, and remembers each
// live agent's process tree for when it does. Must hold mu; procs must be a full table.
func (m *Manager) checkLiveness(procs map[int]*sys.Proc, now time.Time) {
	m.infos.refresh(procs)
	children := map[int][]int{}
	for _, p := range procs {
		children[p.PPID] = append(children[p.PPID], p.PID)
	}
	// What each agent started that has detached, by the agent PID in its environment.
	byAgent := map[int][]*sys.Proc{}
	for _, p := range procs {
		if _, apid, alive, ok := m.envOwner(p, procs); ok && alive && apid > 0 {
			byAgent[apid] = append(byAgent[apid], p)
			// The environment names the agent's real session, which is what the hooks would
			// say: learn it, so a session is one session however it was first seen.
			// An environment can be set by hand: a session another live agent holds is not learned.
			if info := m.infos.get(p); info.Session != "" && m.agentSessions[apid] == "" && sys.AgentKind(procs[apid]) == "claude" &&
				m.sessionHolder(info.Session, apid, procs) == 0 {
				m.learnSession(apid, info.Session)
			}
		}
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
			m.watchAgent(s.AgentPID)
			if o := owner[s.AgentPID]; o != "" && o != id {
				delete(m.st.Trees, id) // its process now belongs to a newer session
				continue
			}
			m.recordTree(id, s.AgentPID, procs, children, byAgent[s.AgentPID])
			continue
		}
		if _, tracked := m.st.Trees[id]; !tracked && s.EndedAt != nil {
			continue
		}
		// Gone from one table could be a ps hiccup; gone from two is gone. (The kernel's exit
		// notice, AgentExited, does not wait for a second table.)
		m.missing[id]++
		if m.missing[id] < 2 {
			continue
		}
		m.sessionGone(s, procs, now, fmt.Sprintf("its %s process (pid %d) exited", s.Agent, s.AgentPID))
	}
	m.findEnvLeftovers(procs, now)
	m.findStaleAgents(procs, now)
}

// sessionGone ends a session whose agent process is gone and collects what it left. Must hold mu.
func (m *Manager) sessionGone(s *model.Session, procs map[int]*sys.Proc, now time.Time, why string) {
	delete(m.missing, s.ID)
	if s.EndedAt == nil {
		m.endSession(s, why, now)
	}
	m.findLeftovers(s, procs, now)
	delete(m.st.Trees, s.ID)
	if m.agentSessions[s.AgentPID] == s.ID {
		delete(m.agentSessions, s.AgentPID)
		delete(m.st.AgentSessions, strconv.Itoa(s.AgentPID))
	}
}

// AgentExited is the kernel telling us an agent process exited: its sessions end now, not on
// the next sweep. procs is a table read after the exit, outside mu.
func (m *Manager) AgentExited(pid int, procs map[int]*sys.Proc) {
	if procs == nil {
		return // no table: the sweep will see it
	}
	m.infos.refresh(procs)
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	for _, s := range m.st.Sessions {
		if s.AgentPID != pid || (procs[pid] != nil && sys.SameProcess(procs[pid], s.AgentStart)) {
			continue
		}
		if s.EndedAt == nil || m.st.Trees[s.ID] != nil {
			m.sessionGone(s, procs, now, fmt.Sprintf("its %s process (pid %d) exited", s.Agent, pid))
		}
	}
	m.dirty = true
}

// watchAgent asks the kernel to say when an agent process exits.
func (m *Manager) watchAgent(pid int) {
	if m.exits != nil {
		m.exits.Watch(pid)
	}
}

// recordTree adds the agent's current descendants to what its session started, and what its
// environment says it started that has already detached; earlier ones still running are kept.
func (m *Manager) recordTree(id string, agentPID int, procs map[int]*sys.Proc, children map[int][]int, detached []*sys.Proc) {
	tree := map[int]model.TreeProc{}
	for pid, tp := range m.st.Trees[id] {
		if p := procs[pid]; p != nil && sys.SameProcess(p, tp.Start) {
			tp.PeakMB = max(tp.PeakMB, m.procMB(p))
			tree[pid] = tp
		}
	}
	stack := append([]int(nil), children[agentPID]...)
	for _, p := range detached {
		stack = append(stack, p.PID)
	}
	for len(stack) > 0 && len(tree) < maxTree {
		pid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if _, seen := tree[pid]; seen {
			continue
		}
		if p := procs[pid]; p != nil {
			tree[pid] = model.TreeProc{Start: p.Start, PeakMB: m.procMB(p)}
			stack = append(stack, children[pid]...)
		}
	}
	m.st.Trees[id] = tree
}

// reviveSession is a session that ended talking again (resumed, or its end was a false
// alarm). It is a root once more: what it started or held is no longer collected as left
// behind. Its leases were released; it claims back what it still uses when asked, and the
// claim is verified because it started or used them. Must hold mu.
func (m *Manager) reviveSession(s *model.Session) {
	s.EndedAt, s.EndReason = nil, ""
	n := 0
	for _, r := range m.st.Resources {
		if r.Owner != s.ID {
			continue
		}
		r.Owner, r.OwnerGoneAt = "", nil
		if r.PausedAt == nil {
			r.OrphanSince = nil // its question starts again, at the normal pace
			delete(m.st.Questions, r.ID)
		}
		n++
	}
	m.event("SESSION %s is active again; %d thing(s) it started or held are no longer collected as left behind", short(s.ID), n)
}

// handOver moves what one session has to its successor in the same agent process (Claude
// Code's /clear starts a new session there). Must hold mu.
func (m *Manager) handOver(from, to string) {
	if from == to || from == "" {
		return
	}
	n := 0
	for _, l := range m.st.Leases {
		if l.Session == from {
			l.Session = to
			n++
		}
	}
	for _, r := range m.st.Resources {
		if r.StartedBy == from {
			r.StartedBy = to
		}
		if r.Owner == from {
			r.Owner, r.OwnerGoneAt = "", nil
		}
	}
	for _, w := range m.waiters {
		if w.Session == from {
			w.Session = to
		}
	}
	for _, q := range m.st.Questions {
		if q.For == from {
			q.For = to
		}
	}
	if t := m.st.Trees[from]; t != nil {
		m.st.Trees[to] = t
		delete(m.st.Trees, from)
	}
	if nw := m.st.Sessions[to]; nw != nil && nw.EndedAt != nil {
		// Handed back to a session that was cleared before (it is the one talking): live again.
		nw.EndedAt, nw.EndReason = nil, ""
	}
	// A cleared conversation's vault grants end with it: the new one asks for its own.
	m.vaultEndSession(from, "its session was cleared", m.now())
	if old := m.st.Sessions[from]; old != nil {
		now := m.now()
		old.EndedAt, old.EndReason = &now, "cleared: continued as "+short(to)
		if nw := m.st.Sessions[to]; nw != nil {
			nw.AgentStart, nw.Priority = old.AgentStart, max(nw.Priority, old.Priority)
		}
	}
	m.event("SESSION %s continues as %s in the same agent process; %d lease(s) handed over", short(from), short(to), n)
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
	// A session that ended waits for nothing, and holds no access.
	m.dropWaitersOf(s.ID)
	m.vaultEndSession(s.ID, "its session ended: "+why, now)
	m.event("ENDED session %s (%s): %s. Released %d lease(s); %d thing(s) it started or held (%.1f GB) are now collected.",
		short(s.ID), s.Agent, why, n, marked, mb/1024)
	if marked > 0 {
		m.notify(agentTitle(s.Agent)+" finished", fmt.Sprintf("It left %d thing(s) running (%.1f GB). Collecting them unless someone claims them.",
			marked, mb/1024))
	}
	if n > 0 {
		m.grantWaiters(now, false) // what it held can go to the queue at once
	}
}

// dropWaitersOf takes every queued request of a session out of the queue.
func (m *Manager) dropWaitersOf(session string) {
	for _, w := range append([]*model.Waiter(nil), m.waiters...) {
		if w.Session == session {
			m.dropWaiter(w.Session, w.Kind)
		}
	}
}

// treeSize is what a process and everything under it hold.
func (m *Manager) treeSize(root *sys.Proc, children map[int][]int, procs map[int]*sys.Proc, size func(*sys.Proc) float64) float64 {
	total, stack, seen := 0.0, []int{root.PID}, map[int]bool{}
	for len(stack) > 0 && len(seen) < maxTree {
		pid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		if p := procs[pid]; p != nil {
			total += size(p)
			stack = append(stack, children[pid]...)
		}
	}
	return total
}

func childrenOf(procs map[int]*sys.Proc) map[int][]int {
	children := map[int][]int{}
	for _, p := range procs {
		children[p.PPID] = append(children[p.PPID], p.PID)
	}
	return children
}

// track records one leftover, with what it needs to be started again.
func (m *Manager) track(p *sys.Proc, session, agent, via string, mb float64, now time.Time) {
	if m.st.Leftovers == nil {
		m.st.Leftovers = map[string]*model.Leftover{}
	}
	key := strconv.Itoa(p.PID)
	if lo := m.st.Leftovers[key]; lo != nil && sys.SameProcess(p, lo.Start) {
		return
	}
	info := m.infos.get(p)
	lo := &model.Leftover{PID: p.PID, Start: p.Start, Session: session, Agent: agent, Found: now, Via: via,
		AskOnly: strings.Contains(p.Exe, ".app/Contents/"), Argv: info.Argv, Env: info.EnvList()}
	m.st.Leftovers[key] = lo
	what := "kept running after it ended"
	if via == "env" {
		what = "is still running, and that session's agent has exited"
	}
	m.event("LEFTOVER %s (pid %d, %.1f GB) was started by session %s (%s) and %s",
		filepath.Base(p.Exe), p.PID, mb/1024, short(session), agent, what)
}

// findLeftovers records processes from an ended session's tree that are still running and no
// longer under any agent. Only the top of each leftover tree is recorded, sized with what is
// under it. Must hold mu.
func (m *Manager) findLeftovers(s *model.Session, procs map[int]*sys.Proc, now time.Time) {
	tree := m.st.Trees[s.ID]
	cands := map[int]*sys.Proc{}
	for pid, tp := range tree {
		p := procs[pid]
		if p == nil || pid == s.AgentPID || !sys.SameProcess(p, tp.Start) {
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
	children := childrenOf(procs)
	size := func(p *sys.Proc) float64 { return max(m.procMB(p), tree[p.PID].PeakMB) }
	found := 0
	for _, p := range cands {
		if cands[p.PPID] != nil {
			continue
		}
		mb := m.treeSize(p, children, procs, size)
		if mb < m.cfg.LeftoverMinMB && p.CPU < 2 {
			continue
		}
		m.track(p, s.ID, s.Agent, "tree", mb, now)
		found++
	}
	if found > 0 {
		m.kickSweep()
	}
}

// findEnvLeftovers finds what agents left that the process tree cannot show: processes whose
// environment names an agent session that is no longer running. That covers what detached
// before a sample saw it, and what sessions left before AgentRM was running at all. Must hold mu.
func (m *Manager) findEnvLeftovers(procs map[int]*sys.Proc, now time.Time) {
	type cand struct {
		p       *sys.Proc
		session string
		agent   string
	}
	cands := map[int]cand{}
	for _, p := range procs {
		if p.PID == m.selfPID || sys.AgentKind(p) != "" {
			continue
		}
		session, _, alive, ok := m.envOwner(p, procs)
		if !ok || alive {
			continue
		}
		if apid, _ := sys.AgentAncestor(procs, p.PID); apid > 0 {
			continue // under a live agent: its tree, not a leftover
		}
		if gui, _, _ := sys.GUIAncestor(procs, p.PPID); gui {
			continue // under your terminal or an app now
		}
		cands[p.PID] = cand{p, session, m.infos.get(p).Agent}
	}
	if len(cands) == 0 {
		return
	}
	children := childrenOf(procs)
	for pid, c := range cands {
		if _, parentToo := cands[c.p.PPID]; parentToo {
			continue
		}
		if lo := m.st.Leftovers[strconv.Itoa(pid)]; lo != nil && sys.SameProcess(c.p, lo.Start) {
			continue
		}
		mb := m.treeSize(c.p, children, procs, m.procMB)
		if mb < m.cfg.LeftoverMinMB && c.p.CPU < 2 {
			continue
		}
		m.track(c.p, c.session, c.agent, "env", mb, now)
	}
}

// staleAgentHours is how long an agent with no terminal above it must have been silent before
// it is mentioned.
const staleAgentHours = 2

// findStaleAgents notes agent processes with no terminal or app above them that have not been
// heard from in hours: a closed window's agent that kept running. They are listed and asked
// about, never paused or stopped. Must hold mu.
func (m *Manager) findStaleAgents(procs map[int]*sys.Proc, now time.Time) {
	m.stale = map[int]bool{}
	for _, p := range procs {
		kind := sys.AgentKind(p)
		if kind == "" || p.CPU >= 1 {
			continue
		}
		if parent := procs[p.PPID]; parent != nil && sys.AgentKind(parent) != "" {
			continue // a subagent
		}
		if gui, _, _ := sys.GUIAncestor(procs, p.PPID); gui {
			continue
		}
		s := m.st.Sessions[m.sessionOf(p.PID, kind)]
		if s != nil && now.Sub(s.LastSeen) < staleAgentHours*time.Hour {
			continue
		}
		if !p.Start.IsZero() && now.Sub(p.Start) < staleAgentHours*time.Hour {
			continue
		}
		m.stale[p.PID] = true
	}
}

// withLeftovers adds the tracked leftovers that are still running, and stale agents, to a
// discovery result. A leftover discovery already knows (a Gradle daemon, a dev server) is not
// added again; it is credited to the session that started it instead. Must hold mu.
func (m *Manager) withLeftovers(found []*model.Resource, procs map[int]*sys.Proc) []*model.Resource {
	byPID := map[int]*model.Resource{}
	for _, r := range found {
		if r.PID > 0 {
			byPID[r.PID] = r
		}
	}
	var children map[int][]int
	if len(m.st.Leftovers) > 0 || len(m.stale) > 0 {
		children = childrenOf(procs)
	}
	for key, lo := range m.st.Leftovers {
		p := procs[lo.PID]
		if p == nil || !sys.SameProcess(p, lo.Start) {
			delete(m.st.Leftovers, key)
			continue
		}
		// Its session may be running again (resumed): then it is started-by, not left behind.
		var gone *time.Time
		if s := m.st.Sessions[lo.Session]; s == nil || s.EndedAt != nil || s.AgentPID <= 0 || procs[s.AgentPID] == nil {
			g := lo.Found
			if s != nil && s.EndedAt != nil {
				g = *s.EndedAt
			}
			gone = &g
		}
		if r := byPID[lo.PID]; r != nil {
			if r.StartedBy == "" {
				r.StartedBy = lo.Session
			}
			if r.Owner == "" && gone != nil {
				r.Owner, r.OwnerGoneAt = lo.Session, gone
			}
			if r.Cwd == "" {
				r.Cwd = lo.Cwd
			}
			if len(r.RestartArgv) == 0 && len(lo.Argv) > 0 {
				r.RestartArgv, r.RestartEnv = lo.Argv, lo.Env
			}
			r.AskOnly = r.AskOnly || lo.AskOnly
			continue
		}
		r := &model.Resource{ID: "left:" + key, Kind: model.RKLeftover, Source: "procs",
			Name: filepath.Base(p.Exe), Detail: trunc(p.Args, 90), PID: p.PID, ProcStart: p.Start, MemMB: m.treeSize(p, children, procs, m.procMB), CPU: p.CPU,
			Cwd: lo.Cwd, StartedBy: lo.Session, AskOnly: lo.AskOnly, RestartArgv: lo.Argv, RestartEnv: lo.Env,
			Restart: "agentrm restore left:" + key}
		if gone != nil {
			r.Owner, r.OwnerGoneAt = lo.Session, gone
		}
		if len(lo.Argv) == 0 {
			r.Restart = "re-run the command that started it"
		}
		found = append(found, r)
	}
	for pid := range m.stale {
		p := procs[pid]
		if p == nil {
			continue
		}
		kind := sys.AgentKind(p)
		found = append(found, &model.Resource{ID: "agent:" + strconv.Itoa(pid), Kind: model.RKStaleAgent, Source: "procs",
			Name: agentTitle(kind), Detail: fmt.Sprintf("no terminal, not heard from in %dh+: %s", staleAgentHours, trunc(p.Args, 60)),
			PID: pid, ProcStart: p.Start, MemMB: m.treeSize(p, children, procs, m.procMB), CPU: p.CPU, AskOnly: true, Restart: "start the agent again"})
	}
	m.keepRestartInfo(found, procs)
	return found
}

// keepRestartInfo notes, on each process-backed resource, the start of its process and, for
// those AgentRM may start again, the command and environment it was started with. They are
// read now, while the process runs and its PID is still its own, so that a stop decided later
// carries them on the resource and never has to look a PID up after the process is gone.
// Must hold mu.
func (m *Manager) keepRestartInfo(found []*model.Resource, procs map[int]*sys.Proc) {
	for _, r := range found {
		p := procs[r.PID]
		if r.PID <= 0 || p == nil {
			continue
		}
		if r.ProcStart.IsZero() {
			r.ProcStart = p.Start
		}
		if len(r.RestartArgv) > 0 || !discover.Restartable(r) || m.infos == nil || !sys.SameProcess(p, r.ProcStart) {
			continue
		}
		if info := m.infos.get(p); len(info.Argv) > 0 {
			r.RestartArgv, r.RestartEnv = info.Argv, info.EnvList()
		}
	}
}

// LeftoverCwds lists leftovers whose working directory is not known yet. Must hold mu.
func (m *Manager) leftoverCwdsNeeded() []int {
	var pids []int
	for _, lo := range m.st.Leftovers {
		if lo.Cwd == "" {
			pids = append(pids, lo.PID)
		}
	}
	return pids
}

// setLeftoverCwds records working directories read outside mu. Must hold mu.
func (m *Manager) setLeftoverCwds(cwds map[int]string) {
	for _, lo := range m.st.Leftovers {
		if c := cwds[lo.PID]; c != "" && lo.Cwd == "" {
			lo.Cwd = c
		}
	}
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
	pressed, _ := m.thrashing()
	pressed = pressed || blocked
	if g, ok := m.learnedGrace(r, map[bool]float64{true: 3, false: 1}[pressed]); ok && !old {
		return g, hold, fmt.Sprintf("unclaimed for %s; for a %s that is when holding it starts to cost more than restarting it would (learned from %d idle periods)",
			g.Round(time.Minute), r.Kind, len(m.timingState()[r.Kind])), false
	}
	switch {
	case old:
		grace *= 3
		reason = fmt.Sprintf("unclaimed, and it has been running for %s", now.Sub(r.FirstSeen).Round(time.Hour))
	default:
		if pressed {
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
	if r.Reason == "clear" {
		// /clear: the agent goes on in a new session, which takes over this one's leases and
		// what it started as soon as it is heard from (handOver). If the agent exits first,
		// the session ends then.
		m.event("SESSION %s was cleared; what it holds passes to the next session of the same agent", short(s.ID))
		return &api.CountResp{}, nil, nil
	}
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

// traceOwner is what a trace records about who started a process.
func (m *Manager) traceOwner(p *sys.Proc) (trace.Owner, bool) {
	info := m.infos.get(p)
	if !info.Owned() {
		return trace.Owner{}, false
	}
	return trace.Owner{Agent: info.Agent, Session: info.Session, AgentPID: info.AgentPID}, true
}
