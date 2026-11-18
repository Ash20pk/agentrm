package discover

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"agentrm/internal/model"
	"agentrm/internal/sys"
)

// Signalling processes. A resource's memory is counted for its whole process tree (`npm run
// dev` is npm, a shell and vite), so a pause or a stop acts on the whole tree too: freezing npm
// alone leaves vite serving, and a SIGTERM to `sh -c vite` alone orphans vite on its port.
//
// A PID is reused once its process exits, so nothing is signalled on a PID alone: the process
// is read again right before, and must still have the start time recorded when it was found.
// Signals are sent with kill(2) directly, never through a kill(1) found on the PATH.

// Overridden by tests.
var (
	quickProc = sys.QuickProc
	listProcs = sys.ListProcs
	sendSig   = syscall.Kill
)

// errGone: the process is no longer running.
var errGone = errors.New("no longer running")

// errReused: the PID now belongs to another process.
var errReused = errors.New("is now a different process")

// liveRoot reads r's process now and checks it is the one r recorded. It refuses a PID that is
// now another process, and one whose start was never recorded (it cannot be told apart).
func liveRoot(r *model.Resource) (*sys.Proc, error) {
	if r.PID <= 1 || r.PID == os.Getpid() {
		return nil, fmt.Errorf("%s: refusing to signal PID %d", r.ID, r.PID)
	}
	p := quickProc(r.PID)
	if p == nil {
		return nil, fmt.Errorf("%s: process %d %w", r.ID, r.PID, errGone)
	}
	if r.ProcStart.IsZero() {
		return nil, fmt.Errorf("%s: when process %d started is not known, so it cannot be told from a new process with the same PID; not signalling it", r.ID, r.PID)
	}
	if p.Start.IsZero() || !sys.SameProcess(p, r.ProcStart) {
		return nil, fmt.Errorf("%s: PID %d %w (started %s, not %s); not signalling it",
			r.ID, r.PID, errReused, p.Start.Format("Jan 2 15:04:05"), r.ProcStart.Format("Jan 2 15:04:05"))
	}
	return p, nil
}

// procTree is root and its descendants, parents before children, from a fresh process table.
// AgentRM itself and its ancestors are never in it. With no table, it is root alone.
func procTree(root *sys.Proc) []*sys.Proc {
	out := []*sys.Proc{root}
	procs := listProcs()
	if procs == nil {
		return out
	}
	protected := map[int]bool{}
	for pid, i := os.Getpid(), 0; pid > 1 && i < 64; i++ {
		protected[pid] = true
		p := procs[pid]
		if p == nil {
			break
		}
		pid = p.PPID
	}
	children := map[int][]*sys.Proc{}
	for _, p := range procs {
		if p.PID > 1 && p.PID != p.PPID {
			children[p.PPID] = append(children[p.PPID], p)
		}
	}
	seen := map[int]bool{root.PID: true}
	for i := 0; i < len(out) && len(out) < 4096; i++ {
		for _, c := range children[out[i].PID] {
			if seen[c.PID] || protected[c.PID] {
				continue
			}
			seen[c.PID] = true
			out = append(out, c)
		}
	}
	return out
}

// signalTree sends sig to each process in order. The first is the root, already checked; each
// other is checked against the start the table gave it (it may have exited and its PID been
// reused since the table was read). Only the root's error is returned: a child may exit at
// any moment.
func signalTree(tree []*sys.Proc, sig syscall.Signal) error {
	var rootErr error
	for i, p := range tree {
		if i > 0 {
			if now := quickProc(p.PID); now == nil || !sys.SameProcess(now, p.Start) {
				continue
			}
		}
		if err := sendSig(p.PID, sig); err != nil && i == 0 {
			rootErr = fmt.Errorf("signal %d to %d: %w", sig, p.PID, err)
		}
	}
	return rootErr
}

func reversed(ps []*sys.Proc) []*sys.Proc {
	out := make([]*sys.Proc, len(ps))
	for i, p := range ps {
		out[len(ps)-1-i] = p
	}
	return out
}

// pauseTree freezes r's process and its descendants, parent first, so that no parent sees a
// child stop and reacts (respawns it, or exits).
func pauseTree(r *model.Resource) error {
	root, err := liveRoot(r)
	if err != nil {
		return err
	}
	return signalTree(procTree(root), syscall.SIGSTOP)
}

// resumeTree thaws r's process and its descendants, children first, so that a parent never
// runs while what it waits on is still frozen. A process that is gone, or whose PID is now
// another process, has nothing left to resume.
func resumeTree(r *model.Resource) error {
	root, err := liveRoot(r)
	if err != nil {
		if errors.Is(err, errGone) || errors.Is(err, errReused) {
			return nil
		}
		return err
	}
	return signalTree(reversed(procTree(root)), syscall.SIGCONT)
}

// stopTree ends r's process tree: frozen first (parent first, so nothing respawns what is
// being stopped), then SIGTERM to every process, then thawed (children first) so that each
// handles its TERM. A process that was already frozen only sees the TERM once it runs again.
func stopTree(r *model.Resource) error {
	root, err := liveRoot(r)
	if err != nil {
		return err
	}
	tree := procTree(root)
	_ = signalTree(tree, syscall.SIGSTOP)
	err = signalTree(tree, syscall.SIGTERM)
	_ = signalTree(reversed(tree), syscall.SIGCONT)
	return err
}
