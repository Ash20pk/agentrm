package discover

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"agentrm/internal/model"
	"agentrm/internal/sys"
)

// Acting on resources. Every action is reversible: containers are stopped or paused (never
// removed), simulators shut down, processes get SIGTERM or SIGSTOP, models are unloaded.

// CanPause says whether a resource can be frozen in place.
func CanPause(r *model.Resource) bool {
	if r.AskOnly {
		return false
	}
	switch r.Kind {
	case model.RKDockerVM: // a frozen engine hangs every docker command
		return false
	case model.RKXcodeHelper: // a frozen SourceKit hangs Xcode's editor
		return false
	case model.RKSimulator, model.RKDatabase: // simctl has no freeze; freezing a DB master alone leaves its workers
		return false
	case model.RKGradle:
		// A frozen Gradle or Kotlin daemon still holds its locks, and every other build then
		// waits on it. They are cheap to start again: stopped instead.
		return false
	case model.RKVM:
		return strings.HasPrefix(r.ID, "vm:") && !strings.HasPrefix(r.ID, "vm:lima-") && !strings.HasPrefix(r.ID, "vm:tart-")
	}
	return true
}

// CanAutoStop says whether AgentRM may stop a resource without the user on the ordinary path
// (asked, paused, stopped). A VM is never stopped automatically: for QEMU that is a hard
// power-off of a disk image. What comes back by itself has a path of its own, with its own
// trust level (manager/selfrestart.go), and is never stopped on this one.
func CanAutoStop(r *model.Resource) bool {
	return r.Kind != model.RKVM && !SelfRestarting(r) && !r.AskOnly
}

// SelfRestarting says whether a resource comes back by itself once stopped: Docker Desktop's VM
// with its engine, an Xcode helper when Xcode next needs it. Such a thing is reclaimed, never
// restored.
func SelfRestarting(r *model.Resource) bool {
	return r.Kind == model.RKDockerVM || r.Kind == model.RKXcodeHelper
}

// NoAutoStopWhy says why AgentRM leaves a resource to you.
func NoAutoStopWhy(r *model.Resource) string {
	switch {
	case r.Kind == model.RKVM:
		return "AgentRM never stops a VM on its own"
	case r.Kind == model.RKStaleAgent:
		return "AgentRM never stops an agent"
	case r.Kind == model.RKDockerVM:
		return "restarting Docker is yours to allow (its own auto-stop level, off until you turn it on)"
	case r.Kind == model.RKXcodeHelper:
		return "ending Xcode's helpers is yours to allow (its own auto-stop level, off until you turn it on)"
	case r.AskOnly:
		return "it is an app, and AgentRM never stops an app on its own"
	}
	return ""
}

func Pause(r *model.Resource) error {
	switch r.Kind {
	case model.RKCluster, model.RKContainers:
		return eachContainer("pause", r.Containers, func(c string) error {
			err := dockerRun(30*time.Second, "pause", c)
			if err == nil {
				return nil
			}
			switch containerState(c) {
			case "paused", "gone", "exited", "created", "dead":
				return nil // already paused, or nothing running to pause
			}
			return err
		})
	case model.RKSimulator, model.RKDatabase, model.RKGradle, model.RKDockerVM, model.RKXcodeHelper:
		return fmt.Errorf("%s cannot be paused", r.Kind)
	}
	if r.PID <= 0 {
		return fmt.Errorf("%s has no process to pause", r.ID)
	}
	return pauseTree(r)
}

// containerState is a container's state as Docker reports it ("running", "paused", "exited"...),
// "gone" if Docker does not know it, "" if Docker did not answer.
var containerState = func(c string) string {
	out, err := sys.Run(10*time.Second, "docker", "inspect", "-f", "{{.State.Status}}", c)
	if err != nil {
		if strings.Contains(err.Error(), "No such") {
			return "gone"
		}
		return ""
	}
	return strings.TrimSpace(out)
}

// dockerRun runs one docker command (tests replace it).
var dockerRun = func(timeout time.Duration, args ...string) error {
	_, err := sys.Run(timeout, "docker", args...)
	return err
}

// eachContainer runs do on every container, in parallel, so that one already paused or
// already gone does not fail the rest. The error says which failed and that the others did not.
func eachContainer(verb string, cs []string, do func(string) error) error {
	errs := make([]error, len(cs))
	var wg sync.WaitGroup
	for i, c := range cs {
		wg.Add(1)
		go func(i int, c string) {
			defer wg.Done()
			errs[i] = do(c)
		}(i, c)
	}
	wg.Wait()
	var failed []string
	for i, err := range errs {
		if err != nil {
			failed = append(failed, cs[i]+": "+err.Error())
		}
	}
	if len(failed) == 0 {
		return nil
	}
	return fmt.Errorf("could not %s %d of %d containers (the rest were): %s",
		verb, len(failed), len(cs), strings.Join(failed, "; "))
}

func Resume(r *model.Resource) error {
	switch r.Kind {
	case model.RKCluster, model.RKContainers:
		// One by one: some may already be running, or gone.
		return eachContainer("unpause", r.Containers, func(c string) error {
			if err := dockerRun(15*time.Second, "unpause", c); err != nil && containerState(c) == "paused" {
				return err
			}
			return nil
		})
	case model.RKSimulator, model.RKDatabase, model.RKDockerVM:
		return nil
	}
	if r.PID <= 0 {
		return nil
	}
	return resumeTree(r)
}

// Stop shuts a resource down so that it can be brought back (see Restart).
func Stop(r *model.Resource) error {
	switch r.Kind {
	case model.RKCluster, model.RKContainers:
		return eachContainer("stop", r.Containers, func(c string) error {
			err := dockerRun(60*time.Second, "stop", c)
			if err == nil {
				return nil
			}
			switch containerState(c) {
			case "gone", "exited", "created", "dead":
				return nil // already stopped
			}
			return err
		})
	case model.RKSimulator:
		_, err := sys.Run(30*time.Second, "xcrun", "simctl", "shutdown", r.UDID)
		return err
	case model.RKDockerVM:
		// Never a signal to the VM's process: that is a power-off under a running engine.
		return RestartDockerEngine()
	case model.RKDatabase:
		// Through brew, or launchd restarts it at once.
		f := strings.TrimPrefix(r.Restart, "brew services start ")
		_, err := sys.Run(30*time.Second, "brew", "services", "stop", f)
		return err
	case model.RKVM:
		switch {
		case strings.HasPrefix(r.ID, "vm:lima-"):
			_, err := sys.Run(60*time.Second, "limactl", "stop", strings.TrimPrefix(r.ID, "vm:lima-"))
			return err
		case strings.HasPrefix(r.ID, "vm:tart-"):
			_, err := sys.Run(60*time.Second, "tart", "stop", strings.TrimPrefix(r.ID, "vm:tart-"))
			return err
		}
	case model.RKModel:
		if r.ID == "model:ollama" {
			return stopOllama()
		}
	}
	if r.PID <= 0 {
		return fmt.Errorf("don't know how to stop %s", r.ID)
	}
	return stopTree(r)
}

// stopOllama unloads every loaded model; the server stays up and reloads on demand.
func stopOllama() error {
	out, err := sys.Run(10*time.Second, "ollama", "ps")
	if err != nil {
		return err
	}
	var firstErr error
	for i, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if i == 0 || strings.TrimSpace(l) == "" {
			continue // header
		}
		name := strings.Fields(l)[0]
		if _, err := sys.Run(30*time.Second, "ollama", "stop", name); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Restore brings back something AgentRM stopped, where a command can do it.
func Restore(r *model.Resource) error {
	switch r.Kind {
	case model.RKDockerVM:
		return fmt.Errorf("%s comes back with Docker Desktop's engine; open Docker Desktop if it is not running", r.ID)
	case model.RKXcodeHelper:
		return fmt.Errorf("%s: Xcode starts it again when it needs it", r.ID)
	case model.RKLeftover:
		if !Restartable(r) {
			return fmt.Errorf("%s is an app, and AgentRM does not start apps again: %s", r.ID, r.Restart)
		}
		return restartLeftover(r)
	case model.RKCluster, model.RKContainers:
		_, err := sys.Run(60*time.Second, "docker", append([]string{"start"}, r.Containers...)...)
		return err
	case model.RKSimulator:
		_, err := sys.Run(60*time.Second, "xcrun", "simctl", "boot", r.UDID)
		return err
	case model.RKDatabase:
		_, err := sys.Run(30*time.Second, "brew", "services", "start", strings.TrimPrefix(r.Restart, "brew services start "))
		return err
	case model.RKVM:
		if strings.HasPrefix(r.ID, "vm:lima-") {
			_, err := sys.Run(120*time.Second, "limactl", "start", strings.TrimPrefix(r.ID, "vm:lima-"))
			return err
		}
	case model.RKModel:
		if r.ID == "model:ollama" {
			return nil // loads on the next request
		}
	}
	if len(r.RestartArgv) > 0 && Restartable(r) {
		return restartLeftover(r) // its command, directory and environment, as AgentRM saw them
	}
	return fmt.Errorf("%s cannot be restored by AgentRM: %s", r.ID, r.Restart)
}

// restartWait is how long Restart waits for a process to exit after its SIGTERM.
var restartWait = 10 * time.Second

// Restart stops a process resource gracefully (SIGTERM to its tree, start time checked) and,
// once it has exited, starts it again with the command, environment and directory it was
// running with. A process that does not exit in time is not started a second time beside it.
func Restart(r *model.Resource) error {
	if !Restartable(r) || len(r.RestartArgv) == 0 {
		return fmt.Errorf("%s has no known restart", r.ID)
	}
	if err := stopTree(r); err != nil {
		return err
	}
	for deadline := time.Now().Add(restartWait); ; {
		if p := quickProc(r.PID); p == nil || !sys.SameProcess(p, r.ProcStart) {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not exit within %s of SIGTERM, so it was not started again", r.ID, restartWait)
		}
		time.Sleep(200 * time.Millisecond)
	}
	return restartLeftover(r)
}

// Restartable says whether AgentRM may start a stopped process again from the command it was
// running: leftovers and dev servers only. Never an agent (that would re-run `claude -p ...`
// with nobody watching), a headless browser (its driver is gone), a Gradle or Kotlin daemon (the
// next build starts its own), or an app.
func Restartable(r *model.Resource) bool {
	return !r.AskOnly && (r.Kind == model.RKLeftover || r.Kind == model.RKServer)
}

func HowToResume(r *model.Resource) string { return "agentrm resume " + r.ID }
