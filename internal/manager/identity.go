package manager

import (
	"fmt"
	"strconv"
	"strings"

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
	viaApp   bool
}

func gatherFacts(pid int) facts {
	f := facts{pid: pid}
	if pid <= 0 {
		return f
	}
	procs := sys.Procs.Lookup(pid)
	f.agentPID, f.agent = sys.AgentAncestor(procs, pid)
	f.gui, f.app, f.viaApp = sys.GUIAncestor(procs, pid)
	return f
}

// sessionOf names an agent process's session: Claude's own session id once a hook has told
// us, otherwise agent-pid.
func (m *Manager) sessionOf(agentPID int, kind string) string {
	if kind == "claude" {
		if s := m.agentSessions[agentPID]; s != "" {
			return s
		}
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
	case f.agent == "claude":
		if req.Via == "hook" {
			m.hookSeen = m.now()
			if real := req.Session; real != "" && m.agentSessions[f.agentPID] != real {
				m.learnSession(f.agentPID, real)
			}
		}
		c.Session = m.sessionOf(f.agentPID, "claude")
	case f.agent != "":
		c.Session = m.sessionOf(f.agentPID, f.agent)
	case f.gui:
		c.Agent, c.Human, c.ViaApp = "you", true, f.viaApp
		c.Session = "you"
	default:
		c.Agent, c.Session = "detached", "detached"
	}
	req.Session, req.Agent = c.Session, c.Agent
	if c.Session != "detached" {
		m.touchSession(c.Session, c.Agent, req.Cwd)
	}
	return c
}

// learnSession records that agent process pid is Claude session real, and moves anything
// recorded under the placeholder name to it.
func (m *Manager) learnSession(pid int, real string) {
	m.agentSessions[pid] = real
	m.st.AgentSessions[strconv.Itoa(pid)] = real
	placeholder := fmt.Sprintf("claude-%d", pid)
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
	if i := strings.LastIndex(s, "-"); i > 0 {
		if _, err := strconv.Atoi(s[i+1:]); err == nil {
			return s[:i]
		}
	}
	return "claude"
}
