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
}

// ListProcs reads the whole process table.
func ListProcs() map[int]*Proc {
	out, err := Run(5*time.Second, "ps", "-axww", "-o", "pid=,ppid=,rss=,%cpu=,args=")
	if err != nil && out == "" {
		return nil
	}
	procs := map[int]*Proc{}
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) < 5 {
			continue
		}
		pid, _ := strconv.Atoi(f[0])
		ppid, _ := strconv.Atoi(f[1])
		rss, _ := strconv.ParseFloat(f[2], 64)
		cpu, _ := strconv.ParseFloat(f[3], 64)
		procs[pid] = &Proc{PID: pid, PPID: ppid, RSSMB: rss / 1024, CPU: cpu, Exe: f[4], Args: strings.Join(f[4:], " ")}
	}
	return procs
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

// GUIAncestor reports whether pid descends from a GUI app (a terminal, an IDE, the AgentRM
// app), which app that is, and whether it is AgentRM itself. A process that was detached and
// now hangs off launchd has no such ancestor: it is nobody's, and is never treated as a person.
func GUIAncestor(procs map[int]*Proc, pid int) (found bool, app string, viaAgentRM bool) {
	for i := 0; i < 64 && pid > 1; i++ {
		p := procs[pid]
		if p == nil {
			return false, "", false
		}
		if i := strings.Index(p.Exe, ".app/Contents/"); i >= 0 {
			app = filepath.Base(p.Exe[:i+4])
			return true, app, strings.Contains(p.Exe, "/AgentRM.app/Contents/")
		}
		pid = p.PPID
	}
	return false, "", false
}

// SubtreeRSS sums the resident memory of every descendant of each agent process.
func SubtreeRSS(procs map[int]*Proc) map[int]float64 {
	out := map[int]float64{}
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
				out[pid] = 0
			}
			continue
		}
		if a := find(pid, 0); a != 0 {
			out[a] += p.RSSMB
		}
	}
	return out
}

// Cwds returns the working directory of each pid, in one lsof call.
func Cwds(pids []int) map[int]string {
	out := map[int]string{}
	if len(pids) == 0 {
		return out
	}
	ps := make([]string, len(pids))
	for i, p := range pids {
		ps[i] = strconv.Itoa(p)
	}
	s, _ := Run(5*time.Second, "lsof", "-a", "-d", "cwd", "-Fpn", "-p", strings.Join(ps, ","))
	pid := 0
	for _, l := range strings.Split(s, "\n") {
		switch {
		case strings.HasPrefix(l, "p"):
			pid, _ = strconv.Atoi(l[1:])
		case strings.HasPrefix(l, "n") && pid != 0:
			out[pid] = l[1:]
		}
	}
	return out
}
