package discover

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"agentrm/internal/model"
	"agentrm/internal/sys"
)

// Docker Desktop's Linux VM. Stopping containers frees memory inside the VM, and the VM keeps
// much of it (see manager/returns.go). Restarting Docker Desktop is the only way to give all of
// it back to macOS, and it is safe only while no container runs.

// DockerVMID is the one resource that stands for Docker Desktop's VM.
const DockerVMID = "docker-vm"

// footprintOf reads a process's footprint (compressed pages included). Tests replace it.
var footprintOf = sys.Footprint

// dockerVMProc finds the process that holds Docker Desktop's VM, or nil:
//
//   - Docker VMM (com.docker.krun) and QEMU run the VM in a process under Docker.app;
//   - Apple's Virtualization framework (the default on Apple silicon) runs it in an XPC service
//     that launchd starts for com.docker.virtualization. It is not under Docker.app, so it is
//     paired with Docker's host process by start time: the one that started within a minute of
//     it. A VM another app runs (Tart, UTM, Lima) starts at another time and is not taken.
func dockerVMProc(procs map[int]*sys.Proc) *sys.Proc {
	var best *sys.Proc
	for _, p := range procs {
		base := filepath.Base(p.Exe)
		if (base == "com.docker.krun" || strings.HasPrefix(base, "qemu-system")) && underApp(procs, p, "Docker.app") {
			if best == nil || p.RSSMB > best.RSSMB {
				best = p
			}
		}
	}
	if best != nil {
		return best
	}
	var host *sys.Proc
	for _, p := range procs {
		if filepath.Base(p.Exe) == "com.docker.virtualization" && strings.Contains(p.Exe, "/Docker.app/") {
			host = p
		}
	}
	if host == nil || host.Start.IsZero() {
		return nil
	}
	var bestGap time.Duration
	for _, p := range procs {
		if filepath.Base(p.Exe) != "com.apple.Virtualization.VirtualMachine" || p.Start.IsZero() {
			continue
		}
		gap := p.Start.Sub(host.Start)
		if gap < -2*time.Second || gap > time.Minute {
			continue
		}
		if gap < 0 {
			gap = -gap
		}
		if best == nil || gap < bestGap {
			best, bestGap = p, gap
		}
	}
	return best
}

// dockerVM is the resource for Docker Desktop's VM, or nil when it is not running. MemMB is the
// VM's whole footprint; the manager counts on getting back only what it holds above its idle
// size.
func dockerVM(procs map[int]*sys.Proc) *model.Resource {
	p := dockerVMProc(procs)
	if p == nil {
		return nil
	}
	mb := p.RSSMB
	if fp, ok := footprintOf(p.PID); ok && fp > mb {
		mb = fp
	}
	r := procRes(p, "docker-vm", model.RKDockerVM, "Docker Desktop VM", filepath.Base(p.Exe), mb)
	r.ID = DockerVMID
	r.Restart = "it comes back with Docker Desktop's engine"
	return r
}

// Overridden by tests: nothing here may touch the real Docker.
var (
	engineState = func() EngineState { s, _ := DockerEngine(); return s }
	runDocker   = func(timeout time.Duration, args ...string) (string, error) {
		return sys.Run(timeout, "docker", args...)
	}
	runCommand  = sys.Run
	quitWait    = 90 * time.Second
	quitPoll    = 2 * time.Second
	sleepFor    = time.Sleep
	restartWait = 180 // seconds docker desktop restart may take
)

// ErrDockerInUse: something runs in Docker, so its VM is not restarted.
var ErrDockerInUse = errors.New("Docker is in use")

// RestartDockerEngine restarts Docker Desktop so that its VM gives all its memory back, and
// brings the engine up again. It refuses unless the engine answers and no container runs,
// checked here, right before, whatever the caller saw.
//
// `docker desktop restart` is used where the Docker Desktop CLI has it (4.37 and later): it
// stops and starts the engine and waits until it is up. Older versions have no such command;
// Docker Desktop is then quit, and opened again once its engine is down.
func RestartDockerEngine() error {
	if s := engineState(); s != EngineUp {
		return fmt.Errorf("the Docker engine is %s; not restarting it", s)
	}
	out, err := runDocker(15*time.Second, "ps", "-q")
	if err != nil {
		return fmt.Errorf("could not list running containers (%v); not restarting Docker", err)
	}
	if ids := strings.Fields(out); len(ids) > 0 {
		return fmt.Errorf("%w: %d container(s) running; not restarting it", ErrDockerInUse, len(ids))
	}
	_, err = runDocker(time.Duration(restartWait+20)*time.Second, "desktop", "restart", "--timeout", fmt.Sprint(restartWait))
	if err == nil {
		return nil
	}
	if !noDesktopCLI(err) {
		return fmt.Errorf("docker desktop restart: %w", err)
	}
	if _, err := runCommand(30*time.Second, "osascript", "-e", `quit app "Docker"`); err != nil {
		return fmt.Errorf("could not quit Docker Desktop: %w", err)
	}
	for deadline := time.Now().Add(quitWait); engineState() != EngineDown; {
		if time.Now().After(deadline) {
			return fmt.Errorf("Docker Desktop did not quit within %s; open it again yourself if it does not come back", quitWait)
		}
		sleepFor(quitPoll)
	}
	if _, err := runCommand(30*time.Second, "open", "-a", "Docker"); err != nil {
		return fmt.Errorf("Docker Desktop quit, but could not be opened again (%w); open it yourself", err)
	}
	return nil
}

// noDesktopCLI says whether an error is the docker CLI not knowing `docker desktop`.
func noDesktopCLI(err error) bool {
	s := err.Error()
	return strings.Contains(s, "is not a docker command") || strings.Contains(s, "unknown command") ||
		strings.Contains(s, "unknown flag")
}
