package main

import (
	"os/exec"
	"testing"
	"time"
)

func TestStopSendsTERM(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	if err := stop(&Resource{Kind: RKServer, PID: cmd.Process.Pid}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("process still running after stop")
	}
}

func TestDevServerFilter(t *testing.T) {
	cfg := defaultConfig()
	yes := []*proc{{exe: "node", args: "node dist/main.js"}, {exe: "/tmp/go-build123/exe/main", args: "main"},
		{exe: "/opt/homebrew/bin/python3.12", args: "python3.12 -m http.server"}}
	no := []*proc{{exe: "/Applications/Slack.app/x", args: "x"}, {exe: "node", args: "node /x/mcp-server.js"},
		{exe: "node", args: "node /usr/local/bin/claude"}, {exe: "postgres", args: "postgres"}}
	for _, p := range yes {
		if !isDevServer(p, cfg) {
			t.Errorf("%s should be a dev server", p.args)
		}
	}
	for _, p := range no {
		if isDevServer(p, cfg) {
			t.Errorf("%s should not be a dev server", p.args)
		}
	}
}
