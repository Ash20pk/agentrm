package sys

import (
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Proc is one row of the process table.
type Proc struct {
	PID, PPID int
	RSSMB     float64
	CPU       float64
	Exe, Args string
	// Path is the executable's real path, from the kernel (proc_pidpath on macOS,
	// /proc/<pid>/exe on Linux), or "" when it could not be read. Exe and Args are what the
	// process says about itself (argv) and are only good for display and for naming agents:
	// any process can set its argv[0] to "/Applications/AgentRM.app/...". Every decision about
	// who a caller is uses Path.
	Path string
	// Start is when the process started (to the second). A PID is reused after a process
	// exits; the pair (PID, Start) is what identifies one process over time.
	Start time.Time
}

// CompleteTable reports whether a process table looks whole: it has launchd and the process
// that read it. A table missing either was cut short.
func CompleteTable(procs map[int]*Proc, self int) bool {
	return procs[1] != nil && procs[self] != nil
}

// ParseElapsed reads ps's etime column: [[dd-]hh:]mm:ss.
func ParseElapsed(s string) (time.Duration, bool) {
	days := 0
	if i := strings.IndexByte(s, '-'); i >= 0 {
		d, err := strconv.Atoi(s[:i])
		if err != nil {
			return 0, false
		}
		days, s = d, s[i+1:]
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false
	}
	total := 0
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return 0, false
		}
		total = total*60 + n
	}
	return time.Duration(days)*24*time.Hour + time.Duration(total)*time.Second, true
}

// SameProcess reports whether a live process is the one that started at want, allowing for
// ps's one-second resolution. An unknown start on either side counts as the same.
func SameProcess(p *Proc, want time.Time) bool {
	if p == nil {
		return false
	}
	if p.Start.IsZero() || want.IsZero() {
		return true
	}
	d := p.Start.Sub(want)
	return d > -3*time.Second && d < 3*time.Second
}

// ProcTable caches the process table briefly, because every request may walk it.
type ProcTable struct {
	mu    sync.Mutex
	at    time.Time
	procs map[int]*Proc
}

var Procs = &ProcTable{}

func (t *ProcTable) Get() map[int]*Proc {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.procs == nil || time.Since(t.at) > 2*time.Second {
		if p := ListProcs(); p != nil {
			t.procs, t.at = p, time.Now()
		}
	}
	return t.procs
}

// Lookup returns the table, refreshed if pid is not in it (a caller that just started).
func (t *ProcTable) Lookup(pid int) map[int]*Proc {
	if procs := t.Get(); procs[pid] != nil {
		return procs
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if p := ListProcs(); p != nil {
		t.procs, t.at = p, time.Now()
	}
	return t.procs
}

// Set replaces the cached table (tests).
func (t *ProcTable) Set(p map[int]*Proc) {
	t.mu.Lock()
	t.procs, t.at = p, time.Now()
	t.mu.Unlock()
}

// AgentKind says whether a process is a coding agent, and which.
func AgentKind(p *Proc) string {
	if p == nil {
		return ""
	}
	base := filepath.Base(p.Exe)
	a := p.Args
	switch {
	case a == "claude" || strings.HasPrefix(a, "claude ") || base == "claude" || strings.Contains(p.Exe, "/claude/versions/"):
		return "claude"
	case base == "codex" || strings.Contains(a, "/bin/codex") || strings.Contains(a, "@openai/codex"):
		return "codex"
	case base == "gemini" || strings.Contains(a, "/bin/gemini") || strings.Contains(a, "@google/gemini-cli"):
		return "gemini"
	case base == "aider" || strings.Contains(a, "/bin/aider") || strings.HasPrefix(a, "python") && strings.Contains(a, " -m aider"):
		return "aider"
	case base == "opencode" || strings.Contains(a, "/bin/opencode"):
		return "opencode"
	case base == "cursor-agent" || strings.Contains(a, "/cursor-agent"):
		return "cursor"
	}
	return ""
}

// Hooked reports whether an agent reports its commands to AgentRM before running them.
// The others are gated by the PATH shim and checked after the fact.
func Hooked(agent string) bool { return agent == "claude" }

// AgentAncestor walks up from pid to the nearest agent process.
func AgentAncestor(procs map[int]*Proc, pid int) (int, string) {
	for i := 0; i < 64 && pid > 1; i++ {
		p := procs[pid]
		if p == nil {
			return 0, ""
		}
		if k := AgentKind(p); k != "" {
			return p.PID, k
		}
		pid = p.PPID
	}
	return 0, ""
}

// GUIAncestor reports whether pid or one of its ancestors is a GUI app (a terminal, an IDE, the
// AgentRM app), which app that is, and whether it is the AgentRM app. A process that was
// detached and now hangs off launchd has no such ancestor: it is nobody's, and is never treated
// as a person. Apps are recognised by their real executable path (Path), never by argv.
func GUIAncestor(procs map[int]*Proc, pid int) (found bool, app string, viaAgentRM bool) {
	if _, app := guiAbove(procs, pid); app != "" {
		return true, app, app == "AgentRM.app"
	}
	return false, "", false
}

// guiAbove walks up from pid (included) to the nearest GUI app, by real executable path.
func guiAbove(procs map[int]*Proc, pid int) (appPID int, app string) {
	for i := 0; i < 64 && pid > 1; i++ {
		p := procs[pid]
		if p == nil {
			return 0, ""
		}
		if app, ok := guiApp(p.Path); ok {
			return p.PID, app
		}
		pid = p.PPID
	}
	return 0, ""
}

// GUICaller is what the process table says about whether a person is behind a caller.
type GUICaller struct {
	Found  bool   // a GUI app is above the caller
	App    string // which app ("Warp.app")
	AppPID int    // that app's process
	ViaApp bool   // the caller is the AgentRM app itself (see AppCaller)
}

// CallerGUI decides, for a caller on the socket, whether a person is behind it.
//
// The caller's own executable never makes it a GUI app: the walk starts at its parent, so a
// process that dressed itself up as an app and hangs off launchd is detached. The one
// exception is the AgentRM app, recognised by AppCaller from the kernel's view of the caller
// and its parent (real paths, a trusted bundle, the same signing team as this daemon).
func CallerGUI(procs map[int]*Proc, pid int) GUICaller {
	p := procs[pid]
	if p == nil {
		return GUICaller{}
	}
	if AppCaller(procs, pid) {
		return GUICaller{Found: true, App: "AgentRM.app", AppPID: p.PPID, ViaApp: true}
	}
	if p.PPID <= 1 {
		return GUICaller{}
	}
	appPID, app := guiAbove(procs, p.PPID)
	return GUICaller{Found: app != "", App: app, AppPID: appPID}
}

// SubtreeRSS sums the resident memory of every descendant of each agent process.
func SubtreeRSS(procs map[int]*Proc) map[int]float64 {
	out := map[int]float64{}
	for a, ps := range AgentTrees(procs) {
		out[a] = 0
		for _, p := range ps {
			out[a] += p.RSSMB
		}
	}
	return out
}

// AgentTrees lists the descendants of each agent process (every agent process has an entry,
// empty if it has no children). A descendant belongs to its nearest agent ancestor.
func AgentTrees(procs map[int]*Proc) map[int][]*Proc {
	out := map[int][]*Proc{}
	owner := map[int]int{} // pid -> agent pid (0 = none), memoised
	var find func(pid, depth int) int
	find = func(pid, depth int) int {
		if v, ok := owner[pid]; ok {
			return v
		}
		p := procs[pid]
		if p == nil || pid <= 1 || depth > 64 {
			return 0
		}
		a := 0
		if par := procs[p.PPID]; par != nil && AgentKind(par) != "" {
			a = par.PID
		} else {
			a = find(p.PPID, depth+1)
		}
		owner[pid] = a
		return a
	}
	for pid, p := range procs {
		if AgentKind(p) != "" {
			if _, ok := out[pid]; !ok {
				out[pid] = nil
			}
			continue
		}
		if a := find(pid, 0); a != 0 {
			out[a] = append(out[a], p)
		}
	}
	return out
}

// linuxGUI are the terminals, editors and desktop apps a person works in on Linux: what an
// .app bundle is on macOS.
var linuxGUI = map[string]bool{
	"gnome-terminal-server": true, "konsole": true, "xterm": true, "alacritty": true, "kitty": true, "wezterm-gui": true,
	"foot": true, "tilix": true, "terminator": true, "xfce4-terminal": true, "ghostty": true, "warp-terminal": true,
	"code": true, "cursor": true, "zed": true, "zeditor": true, "idea": true, "goland": true, "agentrm-app": true,
}

// guiApp says whether the executable at path is an app a person works in, and which. path is
// a real executable path from the kernel; "" (unknown) is no app.
func guiApp(exe string) (string, bool) {
	if exe == "" {
		return "", false
	}
	if i := strings.Index(exe, ".app/Contents/"); i >= 0 {
		return filepath.Base(exe[:i+4]), true
	}
	if base := filepath.Base(exe); linuxGUI[base] {
		return base, true
	}
	return "", false
}

func isGUIApp(exe string) bool {
	_, ok := guiApp(exe)
	return ok
}
