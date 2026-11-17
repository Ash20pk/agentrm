// Command agentrm is AgentRM's daemon, command line, Claude Code hook, PATH shim and MCP server.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"agentrm/internal/hook"
	"agentrm/internal/manager"
	"agentrm/internal/mcp"
	"agentrm/internal/server"
	"agentrm/internal/setup"
	"agentrm/internal/shim"
	"agentrm/internal/store"
)

var version = "dev"

func main() {
	// Run under a shimmed command's name (docker, cargo, ...): act as the PATH shim.
	if name := filepath.Base(os.Args[0]); shimmed(name) {
		os.Exit(shim.Run(name, os.Args[1:]))
	}
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(1)
	}
	switch os.Args[1] {
	case "daemon":
		if err := runDaemon(); err != nil {
			fmt.Fprintln(os.Stderr, "agentrm daemon:", err)
			os.Exit(1)
		}
	case "hook":
		event, agent := "", ""
		if len(os.Args) > 2 {
			event = os.Args[2]
		}
		for i := 3; i+1 < len(os.Args); i++ {
			if os.Args[i] == "--agent" {
				agent = os.Args[i+1]
			}
		}
		hook.Run(event, agent)
	case "mcp":
		if err := mcp.Serve(os.Stdin, os.Stdout, version); err != nil {
			fmt.Fprintln(os.Stderr, "agentrm mcp:", err)
			os.Exit(1)
		}
	default:
		os.Exit(runCLI(os.Args[1], os.Args[2:]))
	}
}

func shimmed(name string) bool {
	for _, c := range setup.ShimmedCommands {
		if name == c {
			return true
		}
	}
	return false
}

func runDaemon() error {
	dir := store.Dir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	logw := store.NewRotatingFile(filepath.Join(dir, "agentrm.log"), 5<<20, 3)
	defer logw.Close()
	m := manager.New(manager.Options{Dir: dir, Version: version, Notify: manager.OSANotify, Log: logw})
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go m.Run(ctx)
	err := server.Serve(ctx, m)
	m.Close()
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
