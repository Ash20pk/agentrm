package sys

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Who a caller is comes from real executable paths. A process can set its argv[0] to anything.
func TestCallerGUIUsesRealPaths(t *testing.T) {
	const term = "/System/Applications/Utilities/Terminal.app/Contents/MacOS/Terminal"
	procs := map[int]*Proc{
		1: {PID: 1, Path: "/sbin/launchd"},
		// You, in a terminal.
		10: {PID: 10, PPID: 1, Exe: term, Path: term},
		11: {PID: 11, PPID: 10, Exe: "-zsh", Path: "/bin/zsh"},
		12: {PID: 12, PPID: 11, Exe: "agentrm", Path: "/usr/local/bin/agentrm"},
		// exec -a /tmp/AgentRM.app/Contents/x agentrm ..., left hanging off launchd.
		20: {PID: 20, PPID: 1, Exe: "/tmp/AgentRM.app/Contents/x", Args: "/tmp/AgentRM.app/Contents/x config", Path: "/usr/local/bin/agentrm"},
		// A process whose own executable really is an app, but which was detached.
		21: {PID: 21, PPID: 1, Exe: term, Path: term},
		// A parent that only says it is an app.
		30: {PID: 30, PPID: 1, Exe: "/Applications/Warp.app/Contents/MacOS/stable", Path: "/bin/bash"},
		31: {PID: 31, PPID: 30, Exe: "agentrm", Path: "/usr/local/bin/agentrm"},
		// The real helper, run detached rather than by the app.
		40: {PID: 40, PPID: 1, Exe: "agentrm", Path: "/Applications/AgentRM.app/Contents/Helpers/agentrm"},
	}
	if g := CallerGUI(procs, 12); !g.Found || g.App != "Terminal.app" || g.AppPID != 10 || g.ViaApp {
		t.Fatalf("a caller in Terminal: %+v", g)
	}
	for _, pid := range []int{20, 21, 31, 40} {
		if g := CallerGUI(procs, pid); g.Found || g.ViaApp {
			t.Errorf("pid %d passed for a person: %+v", pid, g)
		}
	}
	if found, _, _ := GUIAncestor(procs, 30); found {
		t.Error("an app recognised by its argv[0]")
	}
}

// ps splits "/Applications/Visual Studio Code.app/..." at the space; the real path does not.
func TestVSCodeWithASpaceIsAGUIApp(t *testing.T) {
	const helper = "/Applications/Visual Studio Code.app/Contents/Frameworks/Code Helper (Plugin).app/Contents/MacOS/Code Helper (Plugin)"
	procs := map[int]*Proc{
		1:  {PID: 1},
		40: {PID: 40, PPID: 1, Exe: "/Applications/Visual", Args: helper + " --type=utility", Path: helper},
		41: {PID: 41, PPID: 40, Exe: "/bin/zsh", Path: "/bin/zsh"},
		42: {PID: 42, PPID: 41, Exe: "agentrm", Path: "/usr/local/bin/agentrm"},
	}
	if g := CallerGUI(procs, 42); !g.Found || g.App != "Visual Studio Code.app" {
		t.Fatalf("a VS Code terminal is not yours: %+v", g)
	}
}

// Against the kernel: a child that lies about its argv[0] still has its real path read.
func TestRealPathIgnoresArgv0(t *testing.T) {
	self, _ := os.Executable()
	self, _ = filepath.EvalSymlinks(self)
	if got := ExePath(os.Getpid()); got != self {
		t.Fatalf("ExePath(self) = %q, want %q", got, self)
	}
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep")
	}
	sleep, _ = filepath.EvalSymlinks(sleep)
	cmd := &exec.Cmd{Path: sleep, Args: []string{"/tmp/AgentRM.app/Contents/x", "30"}}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	pid := cmd.Process.Pid
	p := QuickProc(pid)
	if p == nil || p.Path != sleep {
		t.Fatalf("argv[0] spoof: %+v (want Path %q)", p, sleep)
	}
	if procs := ListProcs(); procs != nil {
		if q := procs[pid]; q == nil || q.Path != sleep {
			t.Fatalf("ListProcs: %+v", q)
		}
		if g := CallerGUI(procs, pid); g.ViaApp {
			t.Fatalf("a spoofed argv[0] is the app: %+v", g)
		}
	}
}
