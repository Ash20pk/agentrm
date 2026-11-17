package sys

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ProcInfo is what a process carries from where it was started.
type ProcInfo struct {
	Argv []string
	// Env is the environment needed to start it again, without secrets and without the
	// agent's own variables.
	Env map[string]string
	// Who started it, from the variables an agent puts in the environment of every command it
	// runs. A process inherits them even after it detaches and hangs off launchd, so they say
	// who started it when the process tree no longer can.
	Agent    string // "claude", or "" when nothing says
	Session  string // the agent's session ID
	AgentPID int    // the agent process
}

// Owned reports whether an agent's environment marks the process: with its session or PID,
// or at least with the agent's kind.
func (p ProcInfo) Owned() bool { return p.AgentPID > 0 || p.Session != "" || p.Agent != "" }

// EnvList is Env as KEY=VALUE pairs, sorted, for exec.
func (p ProcInfo) EnvList() []string {
	out := make([]string, 0, len(p.Env))
	for k, v := range p.Env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

var secretEnv = regexp.MustCompile(`(?i)(TOKEN|SECRET|PASSW|PASSPHRASE|CREDENTIAL|PRIVATE|API_?KEY|ACCESS_?KEY|AUTH|COOKIE|SESSION)`)

// keepEnv says whether a variable is kept for a restart. Secrets are never stored: a process
// that needs one gets it from wherever it got it the first time (a .env file, the keychain).
func keepEnv(k string) bool {
	switch {
	case k == "" || secretEnv.MatchString(k):
		return false
	case strings.HasPrefix(k, "CLAUDE"), strings.HasPrefix(k, "CODEX"), strings.HasPrefix(k, "AI_AGENT"),
		strings.HasPrefix(k, "AGENTRM_"), k == "_", k == "OLDPWD", k == "SHLVL", strings.HasPrefix(k, "TERM_SESSION"):
		return false
	}
	return true
}

// addEnv files one environment variable: the agent markers that say who started the process,
// and the rest (minus secrets) for restarting it.
func (p *ProcInfo) addEnv(k, v string) {
	switch k {
	case "CLAUDE_CODE_SESSION_ID":
		p.Session = v
		p.Agent = "claude"
		return
	case "CLAUDE_PID":
		p.AgentPID, _ = strconv.Atoi(v)
		p.Agent = "claude"
		return
	case "CLAUDECODE":
		if p.Agent == "" {
			p.Agent = "claude"
		}
		return
	}
	if !keepEnv(k) {
		return
	}
	if p.Env == nil {
		p.Env = map[string]string{}
	}
	p.Env[k] = v
}
