package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// ErrDaemonRunning is returned by LockDaemon when another daemon holds the lock.
var ErrDaemonRunning = errors.New("another AgentRM daemon is already running")

// DaemonLock is the exclusive lock one daemon holds on the state directory for as long as it
// runs. Two daemons on the same state would both sweep, pause and write state.json, and the
// second would remove the first's socket. The kernel releases the lock when the process dies,
// so a crash never leaves it stuck.
type DaemonLock struct{ f *os.File }

func lockPath() string { return filepath.Join(Dir(), "daemon.lock") }

// LockDaemon takes the daemon lock without waiting. If another process holds it, the error
// wraps ErrDaemonRunning and names that process when it can.
func LockDaemon() (*DaemonLock, error) {
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(lockPath(), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			holder := ""
			if b, err := os.ReadFile(lockPath()); err == nil {
				if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
					holder = fmt.Sprintf(" (pid %d)", pid)
				}
			}
			return nil, fmt.Errorf("%w%s on %s; this one exits", ErrDaemonRunning, holder, Dir())
		}
		return nil, fmt.Errorf("lock %s: %w", lockPath(), err)
	}
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	return &DaemonLock{f: f}, nil
}

// Release gives the lock up. Safe to call on nil and more than once.
func (l *DaemonLock) Release() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
	l.f = nil
}
