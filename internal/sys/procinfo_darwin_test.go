package sys

import (
	"encoding/binary"
	"os"
	"testing"
	"time"
)

func procargs(argv, env []string) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, uint32(len(argv)))
	b = append(b, "/usr/local/bin/node"...)
	b = append(b, 0, 0, 0, 0)
	for _, s := range append(argv, env...) {
		b = append(append(b, s...), 0)
	}
	return append(b, 0, 0, 0)
}

// An empty argument is kept, and does not cut off the arguments or the environment after it.
func TestParseProcArgsEmptyArgument(t *testing.T) {
	info, ok := parseProcArgs(procargs([]string{"node", "", "server.js"}, []string{"PATH=/usr/bin", "NODE_ENV=dev"}))
	if !ok || len(info.Argv) != 3 || info.Argv[1] != "" || info.Argv[2] != "server.js" {
		t.Fatalf("argv: %q", info.Argv)
	}
	if info.Env["PATH"] != "/usr/bin" || info.Env["NODE_ENV"] != "dev" {
		t.Fatalf("env lost after an empty argument: %v", info.Env)
	}
}

func TestQuickProcHasStart(t *testing.T) {
	p := QuickProc(os.Getpid())
	if p == nil || p.Start.IsZero() || time.Since(p.Start) < 0 || time.Since(p.Start) > time.Hour {
		t.Fatalf("no start time: %+v", p)
	}
	if lp := ListProcs()[os.Getpid()]; lp == nil || !SameProcess(lp, p.Start) {
		t.Fatalf("QuickProc and ps disagree on when this process started: %+v %+v", lp, p)
	}
}

func TestParseProcArgs(t *testing.T) {
	info, ok := parseProcArgs(procargs([]string{"node", "server.js", "--port=3000"},
		[]string{"PATH=/usr/bin", "NODE_ENV=development", "CLAUDE_CODE_SESSION_ID=abc", "CLAUDE_PID=42",
			"GITHUB_TOKEN=ghp_x", "AWS_SECRET_ACCESS_KEY=y", "CLAUDE_CODE_MESSAGING_TOKEN=z"}))
	if !ok || len(info.Argv) != 3 || info.Argv[1] != "server.js" {
		t.Fatalf("argv: %+v", info)
	}
	if info.Session != "abc" || info.AgentPID != 42 || info.Agent != "claude" || !info.Owned() {
		t.Fatalf("agent markers: %+v", info)
	}
	if info.Env["NODE_ENV"] != "development" || info.Env["PATH"] != "/usr/bin" {
		t.Fatalf("restart env lost what it needs: %v", info.Env)
	}
	for _, k := range []string{"GITHUB_TOKEN", "AWS_SECRET_ACCESS_KEY", "CLAUDE_CODE_MESSAGING_TOKEN", "CLAUDE_PID"} {
		if _, ok := info.Env[k]; ok {
			t.Errorf("%s was kept", k)
		}
	}
}
