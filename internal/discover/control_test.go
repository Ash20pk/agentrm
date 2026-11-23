package discover

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agentrm/internal/model"
)

// A program that is only on the PATH the process was started with (node from nvm) is found
// there, and a program of the same name on AgentRM's own PATH is not run in its place.
func TestRestoreUsesOnlyTheRecordedPath(t *testing.T) {
	home, work, bin := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("AGENTRM_HOME", home)
	out := filepath.Join(work, "out")
	if err := os.WriteFile(filepath.Join(bin, "nvmnode"), []byte("#!/bin/sh\necho recorded \"$@\" > out\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	daemonBin := t.TempDir()
	if err := os.WriteFile(filepath.Join(daemonBin, "nvmnode"), []byte("#!/bin/sh\necho daemon > out\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", daemonBin+":/bin:/usr/bin")
	r := &model.Resource{ID: "left:1", Kind: model.RKLeftover, Cwd: work,
		RestartArgv: []string{"nvmnode", "server.js"}, RestartEnv: []string{"PATH=" + bin + ":/bin:/usr/bin"}}
	if err := Restore(r); err != nil {
		t.Fatal(err)
	}
	var b []byte
	for i := 0; i < 100 && len(b) == 0; i++ {
		time.Sleep(20 * time.Millisecond)
		b, _ = os.ReadFile(out)
	}
	if got := strings.TrimSpace(string(b)); got != "recorded server.js" {
		t.Fatalf("ran the wrong program: %q", got)
	}

	// Only on the daemon's PATH: not run.
	r.RestartArgv = []string{"nvmnode"}
	r.RestartEnv = []string{"PATH=/bin:/usr/bin"}
	if err := Restore(r); err == nil {
		t.Fatal("ran a program found only on AgentRM's PATH")
	}
	// No PATH recorded: not guessed.
	r.RestartEnv = nil
	if err := Restore(r); err == nil {
		t.Fatal("ran a bare command with no recorded PATH")
	}
	// A process that rewrote its arguments names no program.
	r.RestartArgv, r.RestartEnv = []string{"postgres: checkpointer"}, []string{"PATH=/bin:/usr/bin"}
	if err := Restore(r); err == nil {
		t.Fatal("restarted a process from a rewritten command line")
	}
	r.RestartArgv = []string{"", "x"}
	if err := Restore(r); err == nil {
		t.Fatal("restarted a process with an empty argv[0]")
	}
}

// Only leftovers and dev servers are started again from their command line.
func TestRestoreAllowList(t *testing.T) {
	t.Setenv("AGENTRM_HOME", t.TempDir())
	argv, env := []string{"sh", "-c", "exit 0"}, []string{"PATH=/bin:/usr/bin"}
	for _, r := range []*model.Resource{
		{ID: "agent:1", Kind: model.RKStaleAgent, AskOnly: true},
		{ID: "agent:2", Kind: model.RKStaleAgent},
		{ID: "browser:3", Kind: model.RKBrowser},
		{ID: "gradle:4", Kind: model.RKGradle},
		{ID: "left:5", Kind: model.RKLeftover, AskOnly: true},
		{ID: "srv:6", Kind: model.RKServer, AskOnly: true},
	} {
		r.RestartArgv, r.RestartEnv = argv, env
		if err := Restore(r); err == nil {
			t.Errorf("%s (%s) was re-run", r.ID, r.Kind)
		}
	}
	srv := &model.Resource{ID: "srv:7", Kind: model.RKServer, RestartArgv: argv, RestartEnv: env, Cwd: t.TempDir()}
	if err := Restore(srv); err != nil {
		t.Fatalf("a dev server was not restored: %v", err)
	}
}

func TestGradleIsStoppedNotPaused(t *testing.T) {
	r := &model.Resource{ID: "gradle:1", Kind: model.RKGradle, PID: 1234}
	if CanPause(r) || Pause(r) == nil {
		t.Fatal("a Gradle daemon can be frozen, holding its locks")
	}
	if !CanAutoStop(r) {
		t.Fatal("a Gradle daemon cannot be stopped")
	}
}

// fakeDocker answers docker commands from a table of container states.
func fakeDocker(t *testing.T, states map[string]string) *[]string {
	t.Helper()
	var mu sync.Mutex
	var calls []string
	oldRun, oldState := dockerRun, containerState
	dockerRun = func(_ time.Duration, args ...string) error {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, strings.Join(args, " "))
		verb, c := args[0], args[len(args)-1]
		switch st := states[c]; {
		case st == "gone":
			return errors.New("No such container: " + c)
		case verb == "pause" && st != "running":
			return errors.New("container is not running or already paused")
		case verb == "pause":
			states[c] = "paused"
		case verb == "stop" && st == "stuck":
			return errors.New("timed out")
		case verb == "stop":
			states[c] = "exited"
		case verb == "unpause" && st != "paused":
			return errors.New("not paused")
		case verb == "unpause":
			states[c] = "running"
		}
		return nil
	}
	containerState = func(c string) string {
		mu.Lock()
		defer mu.Unlock()
		return states[c]
	}
	t.Cleanup(func() { dockerRun, containerState = oldRun, oldState })
	return &calls
}

// One container already paused, or already gone, does not fail the rest of a group.
func TestContainerGroupsAreActedOnOneByOne(t *testing.T) {
	states := map[string]string{"a": "running", "b": "paused", "c": "gone", "d": "running"}
	calls := fakeDocker(t, states)
	r := &model.Resource{ID: "compose:x", Kind: model.RKContainers, Containers: []string{"a", "b", "c", "d"}}
	if err := Pause(r); err != nil {
		t.Fatalf("pause failed on an already-paused and a gone container: %v", err)
	}
	if states["a"] != "paused" || states["d"] != "paused" || len(*calls) != 4 {
		t.Fatalf("not every container was paused: %v %v", states, *calls)
	}
	if err := Resume(r); err != nil {
		t.Fatal(err)
	}
	if err := Stop(r); err != nil {
		t.Fatalf("stop failed on a gone container: %v", err)
	}
	if states["a"] != "exited" || states["b"] != "exited" || states["d"] != "exited" {
		t.Fatalf("not every container was stopped: %v", states)
	}
	// One that really fails is reported, and the others are still stopped.
	states = map[string]string{"a": "running", "b": "stuck"}
	fakeDocker(t, states)
	r.Containers = []string{"a", "b"}
	err := Stop(r)
	if err == nil || !strings.Contains(err.Error(), "1 of 2") || !strings.Contains(err.Error(), "b:") || states["a"] != "exited" {
		t.Fatalf("a partial stop was not reported honestly: %v %v", err, states)
	}
}
