package discover

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"agentrm/internal/model"
	"agentrm/internal/store"
)

// restartLeftover starts a stopped leftover again with the command, environment and directory
// it was running with, detached, its output in ~/.agentrm/restored/.
func restartLeftover(r *model.Resource) error {
	if len(r.RestartArgv) == 0 {
		return fmt.Errorf("AgentRM did not see how %s was started; re-run the command that started it", r.ID)
	}
	path, err := resolveExec(r.RestartArgv[0], r.RestartEnv, r.Cwd)
	if err != nil {
		return err
	}
	// Built by hand, not with exec.Command: that looks argv[0] up on AgentRM's own PATH, and
	// keeps the failure (or runs a different binary of the same name) even once Path is set.
	cmd := &exec.Cmd{Path: path, Args: r.RestartArgv}
	cmd.Env, cmd.Dir = r.RestartEnv, r.Cwd
	if cmd.Dir != "" {
		if _, err := os.Stat(cmd.Dir); err != nil {
			return fmt.Errorf("its directory %s is gone", cmd.Dir)
		}
	}
	dir := filepath.Join(store.Dir(), "restored")
	_ = os.MkdirAll(dir, 0o700)
	logPath := filepath.Join(dir, fmt.Sprintf("%s-%s.log", filepath.Base(r.RestartArgv[0]), time.Now().Format("20060102-150405")))
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	cmd.Stdout, cmd.Stderr = f, f
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // its own session: AgentRM's exit does not take it down
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// resolveExec finds the program a process was started as, the way its shell did: a name with a
// slash is a path (relative to where it ran), a bare name is looked up on the PATH it was
// started with, and only that PATH. A process that rewrote its arguments ("postgres: writer")
// names no program, and is not started again.
func resolveExec(argv0 string, env []string, cwd string) (string, error) {
	if argv0 == "" {
		return "", fmt.Errorf("its command line was empty or rewritten by the process; re-run the command that started it")
	}
	if strings.Contains(argv0, "/") {
		p := argv0
		if !filepath.IsAbs(p) {
			if cwd == "" {
				return "", fmt.Errorf("it was started as %q, and the directory it ran in is not known", argv0)
			}
			p = filepath.Join(cwd, p)
		}
		if !isExecutable(p) {
			return "", fmt.Errorf("%q is not an executable (the process may have rewritten its command line); re-run the command that started it", p)
		}
		return p, nil
	}
	path, ok := "", false
	for _, kv := range env {
		if v, found := strings.CutPrefix(kv, "PATH="); found {
			path, ok = v, true
		}
	}
	if !ok {
		return "", fmt.Errorf("it was started as %q, and the PATH it had is not known; re-run the command that started it", argv0)
	}
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			dir = "."
		}
		if !filepath.IsAbs(dir) {
			if cwd == "" {
				continue
			}
			dir = filepath.Join(cwd, dir)
		}
		if p := filepath.Join(dir, argv0); isExecutable(p) {
			return p, nil
		}
	}
	return "", fmt.Errorf("%q is not on the PATH it was started with (or the process rewrote its command line); re-run the command that started it", argv0)
}

func isExecutable(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0
}
