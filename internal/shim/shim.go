// Package shim is the PATH shim for agents without hooks (Codex, Gemini CLI, Aider, OpenCode,
// Cursor). Each shimmed command (docker, cargo, ...) is a link to agentrm; run as that name,
// agentrm finds the real program and:
//
//   - for you, and for agents that have hooks: runs it at once (a few sysctl calls, no fork);
//   - for other agents: if the command is heavy, asks the daemon for a lease first, and refuses
//     to run it if there is no room; builds release their lease when they finish.
//
// It fails open: if the daemon cannot be reached, the command runs.
package shim

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"agentrm/internal/api"
	"agentrm/internal/model"
	"agentrm/internal/setup"
	"agentrm/internal/sys"
)

// ExitRefused is the exit code when AgentRM refuses the command (EX_TEMPFAIL).
const ExitRefused = 75

// Run runs the real program called name with args, and returns its exit code.
func Run(name string, args []string) int {
	real := findReal(name)
	if real == "" {
		fmt.Fprintf(os.Stderr, "agentrm shim: %s not found on PATH (outside %s)\n", name, setup.ShimDir())
		return 127
	}
	_, agent, _ := sys.QuickAncestry(os.Getppid())
	if agent == "" || sys.Hooked(agent) || setup.AgentHooksInstalled(agent) {
		return execReal(real, name, args)
	}
	cmdline := name + " " + strings.Join(quoteAll(args), " ")
	cwd, _ := os.Getwd()
	var resp api.TouchResp
	err := api.CallTimeout(3*time.Second, "/touch", api.Req{Cmd: cmdline, Cwd: cwd, Via: "shim", Gate: true}, &resp)
	if err != nil || resp.Acquire == nil {
		return execReal(real, name, args) // not heavy, or the daemon is unavailable: fail open
	}
	if !resp.Acquire.Granted {
		fmt.Fprintf(os.Stderr, "agentrm refused a %s lease for `%s`: %s\n"+
			"If you hold something you no longer need, free it (agentrm release <lease>). Do not try to get around this check.\n",
			resp.Kind, cmdline, resp.Acquire.Reason)
		return ExitRefused
	}
	if !model.BuildKinds[resp.Kind] {
		return execReal(real, name, args) // the lease stays; it is released on idle or by the agent
	}
	// A build: run it as a child so its lease can be released when it ends.
	code := runChild(real, name, args)
	_ = api.CallTimeout(3*time.Second, "/renew", api.Req{Cmd: cmdline, Via: "hook", Cwd: cwd}, nil)
	return code
}

// findReal finds name on PATH, skipping the shim directory and anything that is this binary.
func findReal(name string) string {
	self := setup.SelfPath()
	shimDir := setup.ShimDir()
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" || filepath.Clean(dir) == filepath.Clean(shimDir) {
			continue
		}
		p := filepath.Join(dir, name)
		fi, err := os.Stat(p)
		if err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 {
			continue
		}
		if r, err := filepath.EvalSymlinks(p); err == nil && r == self {
			continue
		}
		return p
	}
	return ""
}

func execReal(real, name string, args []string) int {
	err := syscall.Exec(real, append([]string{name}, args...), os.Environ())
	fmt.Fprintf(os.Stderr, "agentrm shim: could not run %s: %v\n", real, err)
	return 126
}

func runChild(real, name string, args []string) int {
	cmd := exec.Command(real, args...)
	cmd.Args[0] = name
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer signal.Stop(sigs)
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "agentrm shim: could not run %s: %v\n", real, err)
		return 126
	}
	go func() {
		for s := range sigs {
			_ = cmd.Process.Signal(s)
		}
	}()
	err := cmd.Wait()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee):
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	}
	return 1
}

func quoteAll(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if a == "" || strings.ContainsAny(a, " \t\n'\"$`\\|&;<>()*?[]#~") {
			out[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		} else {
			out[i] = a
		}
	}
	return out
}
