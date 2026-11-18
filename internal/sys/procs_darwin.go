package sys

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// ListProcs reads the whole process table.
func ListProcs() map[int]*Proc {
	// lstart is the absolute start time, in the C locale's fixed format. (Start computed from
	// the elapsed time drifts by a second between samples, which makes one process look like
	// a different one.)
	out, err := Run(5*time.Second, "/usr/bin/env", "LC_ALL=C", "ps", "-axww", "-o", "pid=,ppid=,rss=,%cpu=,lstart=,args=")
	if err != nil {
		// A ps that failed or timed out may have printed part of the table. Part of a table
		// reads as "those processes are gone", which ends live sessions: no table is better.
		return nil
	}
	procs := map[int]*Proc{}
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) < 10 {
			continue
		}
		pid, _ := strconv.Atoi(f[0])
		ppid, _ := strconv.Atoi(f[1])
		rss, _ := strconv.ParseFloat(f[2], 64)
		cpu, _ := strconv.ParseFloat(f[3], 64)
		p := &Proc{PID: pid, PPID: ppid, RSSMB: rss / 1024, CPU: cpu, Exe: f[9], Args: strings.Join(f[9:], " ")}
		// The real executable, from the kernel: one proc_info call, microseconds. Identity
		// decisions use it, never argv.
		p.Path = ExePath(pid)
		p.Exe = fullExe(p.Exe, p.Args, p.Path)
		if st, ok := ParseLstart(strings.Join(f[4:9], " ")); ok {
			p.Start = st
		}
		procs[pid] = p
	}
	if !CompleteTable(procs, os.Getpid()) {
		return nil
	}
	return procs
}

// fullExe mends an argv[0] that ps split at a space ("/Applications/Visual" for
// "/Applications/Visual Studio Code.app/..."): when the command line starts with the real
// executable path, that path is argv[0]. Display only; identity uses Path.
func fullExe(exe, args, path string) string {
	if path != "" && strings.HasPrefix(path, exe+" ") && (args == path || strings.HasPrefix(args, path+" ")) {
		return path
	}
	return exe
}

// ParseLstart reads ps's lstart column in the C locale: "Tue Nov 17 00:30:24 2026", local time.
func ParseLstart(s string) (time.Time, bool) {
	t, err := time.ParseInLocation("Mon Jan _2 15:04:05 2006", s, time.Local)
	if err != nil {
		t, err = time.ParseInLocation("Mon Jan 2 15:04:05 2006", s, time.Local)
	}
	return t, err == nil
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
