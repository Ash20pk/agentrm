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
	cmd := exec.Command(r.RestartArgv[0], r.RestartArgv[1:]...)
	if !strings.Contains(r.RestartArgv[0], "/") {
		// argv[0] as typed: look it up on the PATH it had.
		for _, kv := range r.RestartEnv {
			if p, ok := strings.CutPrefix(kv, "PATH="); ok {
				for _, dir := range filepath.SplitList(p) {
					if fi, err := os.Stat(filepath.Join(dir, r.RestartArgv[0])); err == nil && !fi.IsDir() {
						cmd.Path = filepath.Join(dir, r.RestartArgv[0])
						break
					}
				}
			}
		}
	}
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
