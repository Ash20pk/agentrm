// Package sys reads facts about the machine and runs external commands safely.
package sys

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// DefaultTimeout bounds any external command that does not ask for its own.
const DefaultTimeout = 10 * time.Second

// ErrTimeout is returned when a command was killed for running too long.
var ErrTimeout = errors.New("timed out")

// Run runs name with args, killing its whole process group if it outlives timeout (0 means
// DefaultTimeout). A hung docker or simctl can therefore never hang the daemon.
func Run(timeout time.Duration, name string, args ...string) (string, error) {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// Kill the group, not just the child: docker and xcrun spawn helpers.
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	cmd.WaitDelay = time.Second
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		recordFailure(name, true)
		return out.String(), fmt.Errorf("%s %w after %s", name, ErrTimeout, timeout)
	}
	if err != nil {
		msg := bytes.TrimSpace(errb.Bytes())
		if len(msg) > 300 {
			msg = msg[:300]
		}
		if len(msg) > 0 {
			return out.String(), fmt.Errorf("%s: %w: %s", name, err, msg)
		}
		return out.String(), fmt.Errorf("%s: %w", name, err)
	}
	recordFailure(name, false)
	return out.String(), nil
}

// ---- back-off: a command that keeps timing out is skipped for a while ----

var backoff = struct {
	sync.Mutex
	until map[string]time.Time
	fails map[string]int
}{until: map[string]time.Time{}, fails: map[string]int{}}

func recordFailure(name string, timedOut bool) {
	backoff.Lock()
	defer backoff.Unlock()
	if !timedOut {
		backoff.fails[name] = 0
		delete(backoff.until, name)
		return
	}
	backoff.fails[name]++
	// 30 s, 60 s, 120 s ... capped at 10 minutes.
	d := 30 * time.Second << min(backoff.fails[name]-1, 5)
	backoff.until[name] = time.Now().Add(min(d, 10*time.Minute))
}

// BackedOff reports whether name has been timing out and should be skipped for now.
func BackedOff(name string) (bool, time.Time) {
	backoff.Lock()
	defer backoff.Unlock()
	u, ok := backoff.until[name]
	return ok && time.Now().Before(u), u
}
