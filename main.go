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
	"time"

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
	// Before anything else: a second daemon must not sweep, pause or write the state either.
	lock, err := store.LockDaemon()
	if err != nil {
		return err
	}
	defer lock.Release()
	logw := store.NewRotatingFile(filepath.Join(dir, "agentrm.log"), 5<<20, 3)
	defer logw.Close()
	m := manager.New(manager.Options{Dir: dir, Version: version, Notify: manager.OSANotify, Log: logw})
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	ctx, replaced := context.WithCancel(ctx)
	defer replaced()
	if exe, err := os.Executable(); err == nil {
		go watchExecutable(ctx, exe, 15*time.Second, func(why string) {
			fmt.Fprintf(logw, "%s daemon: %s; exiting so launchd starts the new one\n", time.Now().Format(time.RFC3339), why)
			replaced()
		})
	}
	go m.Run(ctx)
	err = server.ServeLocked(ctx, m, lock)
	m.Close()
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// watchExecutable calls changed once if the daemon's own binary is replaced or removed: a
// Sparkle update, a Homebrew upgrade or a rebuild swaps the file, and the old daemon would
// otherwise keep running old code, speaking an API version the new hooks refuse (so they let
// everything through). launchd's KeepAlive starts the new binary once this one exits.
func watchExecutable(ctx context.Context, path string, every time.Duration, changed func(why string)) {
	orig, err := os.Stat(path)
	if err != nil {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if why := exeChanged(path, orig); why != "" {
			changed(why)
			return
		}
	}
}

// exeChanged says how the file at path differs from orig, or "" if it is the same file.
func exeChanged(path string, orig os.FileInfo) string {
	now, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		return path + " was removed"
	case err != nil:
		return "" // unreadable for a moment: try again next time
	case !os.SameFile(orig, now):
		return path + " was replaced"
	case !now.ModTime().Equal(orig.ModTime()) || now.Size() != orig.Size():
		return path + " was rewritten"
	}
	return ""
}
