package sys

import (
	"errors"
	"sync"

	"golang.org/x/sys/unix"
)

// ExitWatcher reports processes exiting the moment they do, from the kernel (kqueue,
// EVFILT_PROC/NOTE_EXIT), instead of noticing on the next process-table sample. Any process
// of this user can be watched.
type ExitWatcher struct {
	kq int
	C  chan int // PIDs that exited

	mu      sync.Mutex
	watched map[int]bool
}

func NewExitWatcher() (*ExitWatcher, error) {
	kq, err := unix.Kqueue()
	if err != nil {
		return nil, err
	}
	w := &ExitWatcher{kq: kq, C: make(chan int, 64), watched: map[int]bool{}}
	go w.run()
	return w, nil
}

// Watch asks to be told when pid exits. Watching the same PID twice is a no-op. If the
// process is already gone, that is reported at once.
func (w *ExitWatcher) Watch(pid int) {
	if pid <= 1 {
		return
	}
	w.mu.Lock()
	if w.watched[pid] {
		w.mu.Unlock()
		return
	}
	w.watched[pid] = true
	w.mu.Unlock()
	ev := unix.Kevent_t{}
	unix.SetKevent(&ev, pid, unix.EVFILT_PROC, unix.EV_ADD|unix.EV_ONESHOT)
	ev.Fflags = unix.NOTE_EXIT
	if _, err := unix.Kevent(w.kq, []unix.Kevent_t{ev}, nil, nil); err != nil {
		w.forget(pid)
		if errors.Is(err, unix.ESRCH) {
			w.send(pid)
		}
	}
}

func (w *ExitWatcher) forget(pid int) {
	w.mu.Lock()
	delete(w.watched, pid)
	w.mu.Unlock()
}

func (w *ExitWatcher) send(pid int) {
	select {
	case w.C <- pid:
	default: // the reader is behind; the next sweep notices instead
	}
}

func (w *ExitWatcher) run() {
	events := make([]unix.Kevent_t, 16)
	for {
		n, err := unix.Kevent(w.kq, nil, events, nil)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return
		}
		for _, ev := range events[:n] {
			if ev.Filter == unix.EVFILT_PROC && ev.Fflags&unix.NOTE_EXIT != 0 {
				pid := int(ev.Ident)
				w.forget(pid)
				w.send(pid)
			}
		}
	}
}
