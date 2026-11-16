package main

// Evidence: what the machine shows, as opposed to what an agent says.
//
//   - Caller identity: the daemon reads the peer PID of every socket connection and walks
//     up the process tree to the agent (Claude Code or Codex) it belongs to. A CLI call made
//     from inside an agent's shell is attributed to that agent's session, whatever --session
//     it passes. A call with no agent above it comes from a person.
//   - Touches: every Bash command an agent runs is matched against the handles of running
//     resources (kube contexts, container and project names, ports, simulator IDs).
//   - Activity: established TCP connections on a resource's ports, and CPU for Gradle.
//   - Attribution: a new resource is bound to the session whose process started it, or
//     whose recent commands named it.

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ---- process table, cached briefly because every request may walk it ----

type procTable struct {
	mu    sync.Mutex
	at    time.Time
	procs map[int]*proc
}

var procCache = &procTable{}

func (t *procTable) get() map[int]*proc {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.procs == nil || time.Since(t.at) > 2*time.Second {
		if p := listProcs(); p != nil {
			t.procs, t.at = p, time.Now()
		}
	}
	return t.procs
}

// lookup returns the table, refreshed if pid is not in it (a caller that just started).
func (t *procTable) lookup(pid int) map[int]*proc {
	procs := t.get()
	if procs[pid] != nil {
		return procs
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if p := listProcs(); p != nil {
		t.procs, t.at = p, time.Now()
	}
	return t.procs
}

// agentKind says whether a process is a coding agent.
func agentKind(p *proc) string {
	base := filepath.Base(p.exe)
	switch {
	case p.args == "claude" || strings.HasPrefix(p.args, "claude ") || base == "claude" ||
		strings.Contains(p.exe, "/claude/versions/"):
		return "claude"
	case base == "codex" || strings.Contains(p.args, "/bin/codex") || strings.Contains(p.args, "@openai/codex"):
		return "codex"
	}
	return ""
}

// agentAncestor walks up from pid to the nearest agent process.
func agentAncestor(procs map[int]*proc, pid int) (int, string) {
	for i := 0; i < 64 && pid > 1; i++ {
		p := procs[pid]
		if p == nil {
			return 0, ""
		}
		if k := agentKind(p); k != "" {
			return p.pid, k
		}
		pid = p.ppid
	}
	return 0, ""
}

// ---- peer PID of a unix-socket connection (macOS LOCAL_PEERPID) ----

type ctxKey struct{}

const solLocal, localPeerPID = 0, 0x002

func peerPID(c net.Conn) int {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0
	}
	pid := 0
	_ = raw.Control(func(fd uintptr) {
		pid, _ = syscall.GetsockoptInt(int(fd), solLocal, localPeerPID)
	})
	return pid
}

func withPeer(ctx context.Context, c net.Conn) context.Context {
	return context.WithValue(ctx, ctxKey{}, peerPID(c))
}

// Caller is who is really on the other end of a request.
type Caller struct {
	PID      int    `json:"pid"`
	AgentPID int    `json:"agent_pid,omitempty"`
	Agent    string `json:"agent"` // claude, codex, or "you"
	Session  string `json:"session"`
	Human    bool   `json:"human"`
}

// identify resolves the caller and rewrites req's session and agent to match.
// Must be called with d.mu held.
func (d *Daemon) identify(pid int, req map[string]any) Caller {
	c := Caller{PID: pid}
	known := false
	if pid > 0 {
		procs := procCache.lookup(pid)
		known = procs[pid] != nil
		c.AgentPID, c.Agent = agentAncestor(procs, pid)
	}
	switch {
	case c.Agent == "claude":
		// Hooks carry Claude's own session id; remember which process it belongs to.
		if str(req, "via") == "hook" && str(req, "session") != "" {
			d.agentSessions[c.AgentPID] = str(req, "session")
		}
		c.Session = d.agentSessions[c.AgentPID]
		if c.Session == "" {
			c.Session = fmt.Sprintf("claude-%d", c.AgentPID)
		}
	case c.Agent == "codex":
		c.Session = fmt.Sprintf("codex-%d", c.AgentPID)
	case known:
		// Found in the process table with no agent above it: a person, or an app they run.
		c.Agent, c.Human = "you", true
		c.Session = str(req, "session")
		if c.Session == "" || strings.HasPrefix(c.Session, "manual-") {
			c.Session = "you"
		}
	default:
		// The caller could not be traced (it exited, or the peer PID was unreadable). Never
		// trust it as a person: use what it claimed, marked unverified.
		c.Agent, c.Session = str(req, "agent"), str(req, "session")
		if c.Agent == "you" || c.Agent == "" {
			c.Agent = "unknown"
		}
	}
	req["session"], req["agent"] = c.Session, c.Agent
	req["_human"] = c.Human
	return c
}

// ---- handles: the strings a command would contain if it used a resource ----

var portCtx = `(?:localhost|127\.0\.0\.1|0\.0\.0\.0|\[::1\]|host\.docker\.internal):`

func handles(r *Resource) (subs []string, res []*regexp.Regexp) {
	for _, p := range r.Ports {
		res = append(res, regexp.MustCompile(portCtx+regexp.QuoteMeta(p)+`\b`))
		res = append(res, regexp.MustCompile(`(?:--port[= ]|PORT=|-p\s*)`+regexp.QuoteMeta(p)+`\b`))
	}
	for _, c := range r.Containers {
		subs = append(subs, c)
	}
	switch r.Kind {
	case RKCluster:
		name := strings.TrimPrefix(r.ID, "kind:")
		subs = append(subs, "kind-"+name, name+"-control-plane")
		res = append(res, regexp.MustCompile(`\bkind\b.*--name[= ]`+regexp.QuoteMeta(name)+`\b`))
	case RKContainers:
		if proj, ok := strings.CutPrefix(r.ID, "compose:"); ok {
			proj = strings.TrimPrefix(proj, "supabase-")
			res = append(res, regexp.MustCompile(`(?:-p|--project-name)[= ]`+regexp.QuoteMeta(proj)+`\b`))
		}
	case RKSimulator:
		subs = append(subs, r.UDID, `"`+r.Name+`"`, `'`+r.Name+`'`)
	}
	return subs, res
}

var kubeToolRe = regexp.MustCompile(`(^|[\s;&|(])(kubectl|helm|k9s|kubens|stern|skaffold|tilt)\s`)
var kubeCtxFlagRe = regexp.MustCompile(`--(kube-)?context[= ]`)

// currentKubeContext reads current-context from the default kubeconfig.
func currentKubeContext() string {
	p := os.Getenv("KUBECONFIG")
	if p == "" || strings.Contains(p, ":") {
		p = home(".kube", "config")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "current-context:"); ok {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}

// touchedBy returns the resources a command (run in cwd) uses.
func touchedBy(cmd, cwd string, resources map[string]*Resource, kubeCtx string) []string {
	var out []string
	usesCurrentCtx := kubeToolRe.MatchString(cmd) && !kubeCtxFlagRe.MatchString(cmd)
	isGradle := classify(cmd) == "gradle"
	simAll := regexp.MustCompile(`\bsimctl\b.*\bbooted\b`).MatchString(cmd)
	composeHere := regexp.MustCompile(`\bdocker[- ]compose\b`).MatchString(cmd) && !regexp.MustCompile(`(?:-p|--project-name)[= ]`).MatchString(cmd)
	for id, r := range resources {
		hit := false
		switch {
		case r.Kind == RKCluster && usesCurrentCtx && kubeCtx == "kind-"+strings.TrimPrefix(id, "kind:"):
			hit = true
		case r.Kind == RKGradle && isGradle:
			hit = true
		case r.Kind == RKSimulator && simAll:
			hit = true
		case r.Kind == RKContainers && composeHere && cwd != "" && id == "compose:"+strings.ToLower(filepath.Base(cwd)):
			hit = true
		}
		if !hit {
			subs, res := handles(r)
			for _, s := range subs {
				if s != "" && s != `""` && s != `''` && strings.Contains(cmd, s) {
					hit = true
					break
				}
			}
			for _, re := range res {
				if !hit && re.MatchString(cmd) {
					hit = true
				}
			}
		}
		if hit {
			out = append(out, id)
		}
	}
	return out
}

// ---- activity: established connections on a resource's ports ----

// establishedPorts lists every local or remote port in an ESTABLISHED TCP connection.
func establishedPorts() map[string]bool {
	out, _ := run("lsof", "-nP", "-iTCP", "-sTCP:ESTABLISHED", "-Fn")
	ports := map[string]bool{}
	for _, l := range strings.Split(out, "\n") {
		if !strings.HasPrefix(l, "n") {
			continue
		}
		for _, side := range strings.Split(l[1:], "->") {
			if i := strings.LastIndex(side, ":"); i >= 0 {
				ports[side[i+1:]] = true
			}
		}
	}
	return ports
}

func isActive(r *Resource, est map[string]bool) bool {
	for _, p := range r.Ports {
		if est[p] {
			return true
		}
	}
	return r.Kind == RKGradle && r.CPU > 5
}

// ---- attribution of a new resource ----

type cmdRecord struct {
	Time time.Time `json:"time"`
	Cmd  string    `json:"cmd"`
	Cwd  string    `json:"cwd"`
}

// startedBy finds the session that started r: by process ancestry where r has a PID,
// otherwise by the most recent command (last 10 minutes) that names it.
func (d *Daemon) startedBy(r *Resource, procs map[int]*proc) string {
	if r.PID > 0 {
		if apid, kind := agentAncestor(procs, r.PID); apid > 0 {
			if kind == "claude" {
				if s := d.agentSessions[apid]; s != "" {
					return s
				}
				return fmt.Sprintf("claude-%d", apid)
			}
			return fmt.Sprintf("codex-%d", apid)
		}
		return ""
	}
	single := map[string]*Resource{r.ID: r}
	best, bestAt := "", time.Time{}
	for session, cmds := range d.recent {
		for _, c := range cmds {
			if time.Since(c.Time) > 10*time.Minute || !c.Time.After(bestAt) {
				continue
			}
			if len(touchedBy(c.Cmd, c.Cwd, single, "")) > 0 {
				best, bestAt = session, c.Time
			}
		}
	}
	return best
}

// ---- disk and load ----

func diskFreeGB(path string) float64 {
	var st syscall.Statfs_t
	if syscall.Statfs(path, &st) != nil {
		return -1
	}
	return float64(st.Bavail) * float64(st.Bsize) / (1 << 30)
}

var loadRe = regexp.MustCompile(`\{\s*([\d.]+)`)

func loadAvg() (float64, int) {
	cores := 0
	if out, err := run("sysctl", "-n", "hw.ncpu"); err == nil {
		cores, _ = strconv.Atoi(strings.TrimSpace(out))
	}
	if out, err := run("sysctl", "-n", "vm.loadavg"); err == nil {
		if m := loadRe.FindStringSubmatch(out); m != nil {
			l, _ := strconv.ParseFloat(m[1], 64)
			return l, cores
		}
	}
	return 0, cores
}
