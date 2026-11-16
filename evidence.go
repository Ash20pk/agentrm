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

// guiAncestor reports whether pid descends from a GUI app (a terminal, an IDE, the AgentRM
// app) and whether that app is AgentRM itself. A process that was detached and now hangs off
// launchd has no such ancestor: it is nobody's, and is never treated as a person.
func guiAncestor(procs map[int]*proc, pid int) (found, viaApp bool) {
	for i := 0; i < 64 && pid > 1; i++ {
		p := procs[pid]
		if p == nil {
			return false, false
		}
		if strings.Contains(p.exe, ".app/Contents/") {
			return true, strings.Contains(p.exe, "/AgentRM.app/Contents/")
		}
		pid = p.ppid
	}
	return false, false
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
	gui, viaApp := false, false
	if pid > 0 {
		procs := procCache.lookup(pid)
		c.AgentPID, c.Agent = agentAncestor(procs, pid)
		gui, viaApp = guiAncestor(procs, pid)
	}
	if str(req, "via") == "hook" && c.Agent != "" {
		d.hookSeen = time.Now()
	}
	switch {
	case c.Agent == "claude":
		// Hooks carry Claude's own session id; remember which process it belongs to.
		if real := str(req, "session"); str(req, "via") == "hook" && real != "" && d.agentSessions[c.AgentPID] != real {
			d.agentSessions[c.AgentPID] = real
			d.st.AgentSessions[strconv.Itoa(c.AgentPID)] = real
			// Anything recorded under the placeholder name belongs to the real session.
			placeholder := fmt.Sprintf("claude-%d", c.AgentPID)
			delete(d.st.Sessions, placeholder)
			for _, l := range d.st.Leases {
				if l.Session == placeholder {
					l.Session = real
				}
			}
			for _, r := range d.st.Resources {
				if r.StartedBy == placeholder {
					r.StartedBy = real
				}
			}
		}
		c.Session = d.agentSessions[c.AgentPID]
		if c.Session == "" {
			c.Session = fmt.Sprintf("claude-%d", c.AgentPID)
		}
	case c.Agent == "codex":
		c.Session = fmt.Sprintf("codex-%d", c.AgentPID)
	case gui:
		// Run from a terminal or the AgentRM app, with no agent above it: a person.
		c.Agent, c.Human = "you", true
		req["_app"] = viaApp
		c.Session = str(req, "session")
		if c.Session == "" || strings.HasPrefix(c.Session, "manual-") {
			c.Session = "you"
		}
	default:
		// Detached from whatever started it (or untraceable). It gets no one's identity:
		// not a person's, and not the session it says it is.
		c.Agent, c.Session = "detached", "detached"
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
	cmd = activeText(cmd)
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
func (d *Daemon) startedBy(r *Resource, procs map[int]*proc, now time.Time, isNew bool) string {
	if r.PID > 0 {
		if apid, kind := agentAncestor(procs, r.PID); apid > 0 {
			return d.sessionOf(apid, kind)
		}
		if gui, _ := guiAncestor(procs, r.PID); gui {
			return "you" // started from your terminal or IDE, with no agent above it
		}
		// Detached (Gradle daemons always are): fall through to what recent commands say.
	}
	if !isNew {
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
	if best == "" && d.agentsQuiet(procs, now) {
		return "you" // no agent ran anything that could have started it
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

// agentsQuiet reports whether it is certain no agent started something that just appeared:
// either no agent is running at all, or every running agent reports its commands through
// hooks and none has run one in the last 10 minutes. Codex has no hooks, so a running Codex
// is never certain.
func (d *Daemon) agentsQuiet(procs map[int]*proc, now time.Time) bool {
	agents := 0
	for _, p := range procs {
		switch agentKind(p) {
		case "codex":
			return false
		case "claude":
			agents++
		}
	}
	if agents == 0 {
		return true
	}
	if d.hookSeen.IsZero() {
		return false // agents are running but have never reported: hooks are not installed
	}
	for _, cmds := range d.recent {
		for _, c := range cmds {
			if now.Sub(c.Time) < 10*time.Minute {
				return false
			}
		}
	}
	return true
}

// ---- which parts of a command line actually execute something ----

// inert commands only read or print: naming a resource in one is not using it, and naming
// a heavy command in one is not running it.
var inert = map[string]bool{}

func init() {
	for _, w := range strings.Fields("echo printf grep egrep fgrep rg ag cat bat less more head tail ls find fd " +
		"sed awk wc sort uniq cut tr git diff cd pwd true false test [ [[ export set unset alias which type man " +
		"tee touch mkdir rm cp mv ln chmod read sleep date basename dirname jq yq agentrm :") {
		inert[w] = true
	}
}

var wrappers = map[string]bool{"sudo": true, "time": true, "nohup": true, "env": true, "exec": true, "command": true,
	"nice": true, "caffeinate": true, "then": true, "do": true, "else": true, "{": true, "(": true, "!": true}

var assignRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

var heredocRe = regexp.MustCompile(`<<-?\s*['"]?([A-Za-z_][A-Za-z0-9_]*)['"]?`)

// stripHeredocs removes heredoc bodies: they are data written to a file or fed to a
// program, not commands. A heredoc fed to a shell is the exception, and is kept.
func stripHeredocs(cmd string) string {
	lines := strings.Split(cmd, "\n")
	var out []string
	for i := 0; i < len(lines); i++ {
		out = append(out, lines[i])
		m := heredocRe.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		segs := segments(lines[i][:strings.Index(lines[i], "<<")])
		toShell := false
		if len(segs) > 0 {
			switch commandWord(segs[len(segs)-1]) {
			case "sh", "bash", "zsh", "dash", "ksh":
				toShell = true
			}
		}
		for i++; i < len(lines) && strings.TrimSpace(lines[i]) != m[1]; i++ {
			if toShell {
				out = append(out, lines[i])
			}
		}
	}
	return strings.Join(out, "\n")
}

// commandWord is the program a shell segment runs, past assignments and wrappers.
func commandWord(seg string) string {
	for _, tok := range strings.Fields(seg) {
		tok = strings.TrimLeft(tok, "({")
		if tok == "" || wrappers[tok] || assignRe.MatchString(tok) {
			continue
		}
		return filepath.Base(tok)
	}
	return ""
}

// segments splits a shell command at ; | & newlines, $( and backticks, outside quotes.
func segments(cmd string) []string {
	var out []string
	var cur strings.Builder
	var quote byte
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			out = append(out, s)
		}
		cur.Reset()
	}
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			}
			cur.WriteByte(c)
		case c == '\\' && i+1 < len(cmd):
			cur.WriteByte(c)
			i++
			cur.WriteByte(cmd[i])
		case c == '$' && i+1 < len(cmd) && cmd[i+1] == '(':
			flush()
			i++
		case c == '`':
			flush()
		case quote == '"':
			if c == '"' {
				quote = 0
			}
			cur.WriteByte(c)
		case c == '\'' || c == '"':
			quote = c
			cur.WriteByte(c)
		case c == ';' || c == '|' || c == '&' || c == '\n':
			flush()
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return out
}

// activeText keeps only the segments of cmd whose command word can start or use something.
func activeText(cmd string) string {
	var keep []string
	for _, seg := range segments(stripHeredocs(cmd)) {
		word := commandWord(seg)
		if word == "" || inert[word] || strings.HasPrefix(word, "#") {
			continue
		}
		keep = append(keep, seg)
	}
	return strings.Join(keep, "\n")
}
