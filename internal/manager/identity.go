package manager

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"agentrm/internal/api"
	"agentrm/internal/sys"
)

// facts is what the process table says about a caller, gathered before taking the lock
// because reading the table may run ps.
type facts struct {
	pid      int
	agentPID int
	agent    string
	gui      bool
	app      string
	appPID   int // the GUI app's process, when gui
	viaApp   bool
	// procs is the table the facts were read from: it says which agent processes are alive.
	// nil when unknown (a replayed request).
	procs map[int]*sys.Proc
}

func gatherFacts(pid int) facts {
	f := facts{pid: pid}
	if pid <= 0 {
		return f
	}
	procs := sys.Procs.Lookup(pid)
	f.procs = procs
	f.agentPID, f.agent = sys.AgentAncestor(procs, pid)
	g := sys.CallerGUI(procs, pid)
	f.gui, f.app, f.appPID, f.viaApp = g.Found, g.App, g.AppPID, g.ViaApp
	return f
}

// sessionOf names an agent process's session: Claude's own session id once a hook has told
// us, otherwise agent-pid.
func (m *Manager) sessionOf(agentPID int, kind string) string {
	if s := m.agentSessions[agentPID]; s != "" {
		return s
	}
	return fmt.Sprintf("%s-%d", kind, agentPID)
}

// resolve turns facts into a Caller and makes req's session and agent match it. Must be
// called with mu held.
//
//   - Under an agent process: that agent's session, whatever the request says.
//   - Under a terminal, an IDE or the AgentRM app, with no agent above: a person.
//   - Detached (hangs off launchd) or untraceable: nobody. It gets no session's identity and
//     none of a person's rights.
func (m *Manager) resolve(f facts, req *api.Req) api.Caller {
	c := api.Caller{PID: f.pid, AgentPID: f.agentPID, Agent: f.agent, App: f.app}
	switch {
	case f.agent != "":
		// A hook's session ID names the agent it runs under, unless the hook says it is another
		// agent's (run by hand, or from inside a different agent): then it renames nothing.
		// A session that is another live agent's is not taken over: the caller keeps its own.
		if req.Via == "hook" && (req.Agent == "" || req.Agent == f.agent) {
			m.hookSeen[f.agent] = m.now()
			if real := req.Session; real != "" && m.agentSessions[f.agentPID] != real {
				if holder := m.sessionHolder(real, f.agentPID, f.procs); holder != 0 {
					m.refuseClaim(f, real, holder)
				} else {
					m.learnSession(f.agentPID, real, f.agent)
				}
			}
		}
		c.Session = m.sessionOf(f.agentPID, f.agent)
	case f.gui && req.Via == "hook" && hookAgents[req.Agent]:
		// An agent that runs inside an app (Cursor's): its ancestry shows the app, so it would
		// pass for you. It says it is an agent, and is taken at its word, because that can
		// only lower its rights: an agent may do less than a person.
		//
		// Its session ID is its own word too, so it is kept inside a name of its own
		// (agent:app-pid:id): it can never name a session of another agent, or of an agent in
		// another app. (Agents inside the same app process cannot be told apart by ancestry.)
		c.Agent, c.App = req.Agent, f.app
		c.Session = req.Agent + ":" + strconv.Itoa(f.appPID)
		if req.Session != "" {
			c.Session += ":" + req.Session
		}
		m.hookSeen[req.Agent] = m.now()
	case f.gui:
		c.Agent, c.Human, c.ViaApp = "you", true, f.viaApp
		c.Session = "you"
	default:
		c.Agent, c.Session = "detached", "detached"
	}
	req.Session, req.Agent = c.Session, c.Agent
	if c.Session != "detached" {
		s := m.touchSession(c.Session, c.Agent, req.Cwd)
		if s != nil && req.Branch != "" {
			s.Repo, s.Branch = req.Repo, req.Branch
		}
	}
	return c
}

// hookAgents are the agents whose hooks AgentRM installs besides Claude Code's.
var hookAgents = map[string]bool{"codex": true, "gemini": true, "cursor": true}

// sessionHolder returns the live agent process, other than pid, that session id belongs to, or
// 0 when none does. A session whose agent exited may be taken up by a new process (a resumed
// session); one whose agent is alive may not. With no process table (procs nil) a bound agent
// counts as alive. Must be called with mu held.
func (m *Manager) sessionHolder(id string, pid int, procs map[int]*sys.Proc) int {
	alive := func(q int, start time.Time) bool {
		if procs == nil {
			return true
		}
		p := procs[q]
		return p != nil && sys.AgentKind(p) != "" && sys.SameProcess(p, start)
	}
	if s := m.st.Sessions[id]; s != nil && s.EndedAt == nil && s.AgentPID > 0 && s.AgentPID != pid && alive(s.AgentPID, s.AgentStart) {
		return s.AgentPID
	}
	for q, sid := range m.agentSessions {
		if sid != id || q == pid {
			continue
		}
		var start time.Time
		if s := m.st.Sessions[id]; s != nil && s.AgentPID == q {
			start = s.AgentStart
		}
		if alive(q, start) {
			return q
		}
	}
	return 0
}

// refuseClaim reports, once per agent process and session, a hook that named another live
// agent's session. Must be called with mu held.
func (m *Manager) refuseClaim(f facts, id string, holder int) {
	key := strconv.Itoa(f.agentPID) + " " + id
	if m.claimRefused[key] {
		return
	}
	m.claimRefused[key] = true
	m.event("TAMPER a hook under %s (pid %d) named session %s, which belongs to the live agent pid %d: ignored, it keeps its own session.",
		agentTitle(f.agent), f.agentPID, short(id), holder)
}

// learnSession records that agent process pid is the agent's session real (as its hooks or its
// commands' environment say), and moves anything recorded under the placeholder name to it.
func (m *Manager) learnSession(pid int, real string, kind ...string) {
	if prev := m.agentSessions[pid]; prev != "" && prev != real {
		m.handOver(prev, real) // the same agent started a new session (/clear)
	}
	m.agentSessions[pid] = real
	m.st.AgentSessions[strconv.Itoa(pid)] = real
	k := "claude"
	if len(kind) > 0 && kind[0] != "" {
		k = kind[0]
	}
	placeholder := fmt.Sprintf("%s-%d", k, pid)
	if s := m.st.Sessions[placeholder]; s != nil {
		delete(m.st.Sessions, placeholder)
		if m.st.Sessions[real] == nil {
			s.ID = real
			m.st.Sessions[real] = s
		}
	}
	for _, l := range m.st.Leases {
		if l.Session == placeholder {
			l.Session = real
		}
	}
	for _, r := range m.st.Resources {
		if r.StartedBy == placeholder {
			r.StartedBy = real
		}
	}
	for _, w := range m.waiters {
		if w.Session == placeholder {
			w.Session = real
		}
	}
	if b, ok := m.footBase[placeholder]; ok {
		m.footBase[real] = b
		delete(m.footBase, placeholder)
	}
}

func agentOfSession(s string) string {
	if s == "you" {
		return "you"
	}
	if k, _, ok := strings.Cut(s, ":"); ok && hookAgents[k] {
		return k // an agent inside an app: agent:app-pid:id
	}
	if i := strings.LastIndex(s, "-"); i > 0 {
		if _, err := strconv.Atoi(s[i+1:]); err == nil {
			return s[:i]
		}
	}
	return "claude"
}
