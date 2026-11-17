package discover

import (
	"fmt"
	"strconv"
	"strings"
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
	case model.RKSimulator, model.RKDatabase: // simctl has no freeze; freezing a DB master alone leaves its workers
		return false
	case model.RKVM:
		return strings.HasPrefix(r.ID, "vm:") && !strings.HasPrefix(r.ID, "vm:lima-") && !strings.HasPrefix(r.ID, "vm:tart-")
	}
	return true
}

// CanAutoStop says whether AgentRM may stop a resource without the user. A VM is never
// stopped automatically: for QEMU that is a hard power-off of a disk image.
func CanAutoStop(r *model.Resource) bool { return r.Kind != model.RKVM && !r.AskOnly }

// NoAutoStopWhy says why AgentRM leaves a resource to you.
func NoAutoStopWhy(r *model.Resource) string {
	switch {
	case r.Kind == model.RKVM:
		return "AgentRM never stops a VM on its own"
	case r.Kind == model.RKStaleAgent:
		return "AgentRM never stops an agent"
	case r.AskOnly:
		return "it is an app, and AgentRM never stops an app on its own"
	}
	return ""
}

func Pause(r *model.Resource) error {
	switch r.Kind {
	case model.RKCluster, model.RKContainers:
		_, err := sys.Run(30*time.Second, "docker", append([]string{"pause"}, r.Containers...)...)
		return err
	case model.RKSimulator, model.RKDatabase:
		return fmt.Errorf("%s cannot be paused", r.Kind)
	}
	if r.PID <= 0 {
		return fmt.Errorf("%s has no process to pause", r.ID)
	}
	_, err := sys.Run(5*time.Second, "kill", "-STOP", strconv.Itoa(r.PID))
	return err
}

func Resume(r *model.Resource) error {
	switch r.Kind {
	case model.RKCluster, model.RKContainers:
		// Unpause one by one: some may already be running.
		var firstErr error
		for _, c := range r.Containers {
			if _, err := sys.Run(15*time.Second, "docker", "unpause", c); err != nil && firstErr == nil {
				if state, _ := sys.Run(5*time.Second, "docker", "inspect", "-f", "{{.State.Paused}}", c); strings.TrimSpace(state) == "true" {
					firstErr = err
				}
			}
		}
		return firstErr
	case model.RKSimulator, model.RKDatabase:
		return nil
	}
	if r.PID <= 0 {
		return nil
	}
	_, err := sys.Run(5*time.Second, "kill", "-CONT", strconv.Itoa(r.PID))
	return err
}

// Stop shuts a resource down so that it can be brought back (see Restart).
func Stop(r *model.Resource) error {
	switch r.Kind {
	case model.RKCluster, model.RKContainers:
		_, err := sys.Run(60*time.Second, "docker", append([]string{"stop"}, r.Containers...)...)
		return err
	case model.RKSimulator:
		_, err := sys.Run(30*time.Second, "xcrun", "simctl", "shutdown", r.UDID)
		return err
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
	_, err := sys.Run(5*time.Second, "kill", "-TERM", strconv.Itoa(r.PID))
	if err == nil && r.PausedAt != nil {
		// A frozen process only sees the TERM once it runs again. (It may already be gone.)
		_, _ = sys.Run(5*time.Second, "kill", "-CONT", strconv.Itoa(r.PID))
	}
	return err
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
	case model.RKLeftover:
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
	return fmt.Errorf("%s cannot be restored by AgentRM: %s", r.ID, r.Restart)
}

func HowToResume(r *model.Resource) string { return "agentrm resume " + r.ID }
