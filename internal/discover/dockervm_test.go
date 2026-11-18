package discover

import (
	"errors"
	"strings"
	"testing"
	"time"

	"agentrm/internal/model"
	"agentrm/internal/sys"
)

// noFootprint keeps discovery away from the real kernel: footprints are what the table says.
func noFootprint(t *testing.T, fp map[int]float64) {
	t.Helper()
	old := footprintOf
	footprintOf = func(pid int) (float64, bool) { v, ok := fp[pid]; return v, ok }
	t.Cleanup(func() { footprintOf = old })
}

func TestDockerVMUnderAppleVirtualization(t *testing.T) {
	t0 := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	procs := map[int]*sys.Proc{
		1:   {PID: 1, Exe: "launchd"},
		100: {PID: 100, PPID: 1, Exe: "/Applications/Docker.app/Contents/MacOS/com.docker.backend", Start: t0},
		110: {PID: 110, PPID: 100, Exe: "/Applications/Docker.app/Contents/MacOS/com.docker.virtualization", Start: t0.Add(5 * time.Second), RSSMB: 40},
		// Docker's VM: started by launchd for com.docker.virtualization, a moment after it.
		120: {PID: 120, PPID: 1, Exe: "/System/Library/Frameworks/Virtualization.framework/Versions/A/XPCServices/com.apple.Virtualization.VirtualMachine.xpc/Contents/MacOS/com.apple.Virtualization.VirtualMachine",
			Start: t0.Add(7 * time.Second), RSSMB: 3000},
		// Another app's VM (Tart), started an hour later: not Docker's.
		130: {PID: 130, PPID: 1, Exe: "/System/Library/Frameworks/Virtualization.framework/Versions/A/XPCServices/com.apple.Virtualization.VirtualMachine.xpc/Contents/MacOS/com.apple.Virtualization.VirtualMachine",
			Start: t0.Add(time.Hour), RSSMB: 8000},
	}
	noFootprint(t, map[int]float64{120: 5000})
	r := dockerVM(procs)
	if r == nil || r.ID != DockerVMID || r.Kind != model.RKDockerVM || r.PID != 120 || r.MemMB != 5000 || r.ProcStart.IsZero() {
		t.Fatalf("Docker's VM not found by its host's start time, at its footprint: %+v", r)
	}

	// Without Docker's host process, no VM is Docker's.
	delete(procs, 110)
	if r := dockerVM(procs); r != nil {
		t.Fatalf("a VM was taken for Docker's with Docker's VM host not running: %+v", r)
	}
}

func TestDockerVMUnderDockerApp(t *testing.T) {
	procs := map[int]*sys.Proc{
		1:  {PID: 1, Exe: "launchd"},
		50: {PID: 50, PPID: 1, Exe: "/Applications/Docker.app/Contents/MacOS/com.docker.krun", RSSMB: 2500},
		60: {PID: 60, PPID: 1, Exe: "/opt/homebrew/bin/qemu-system-aarch64", Args: "qemu-system-aarch64 -m 4G disk.img", RSSMB: 900},
	}
	noFootprint(t, nil)
	got := map[string]*model.Resource{}
	for _, r := range Processes(model.DefaultConfig(), procs) {
		got[r.ID] = r
	}
	if r := got[DockerVMID]; r == nil || r.PID != 50 || r.MemMB != 2500 {
		t.Fatalf("Docker VMM's VM not found: %+v", r)
	}
	if r := got["vm:60"]; r == nil || r.Kind != model.RKVM {
		t.Fatalf("a QEMU VM of its own was taken for Docker's: %+v", got)
	}
}

func TestDockerVMRules(t *testing.T) {
	r := &model.Resource{ID: DockerVMID, Kind: model.RKDockerVM, PID: 120}
	if CanPause(r) || CanAutoStop(r) || !SelfRestarting(r) || NoAutoStopWhy(r) == "" {
		t.Fatal("Docker's VM is frozen or stopped on the ordinary path")
	}
	if err := Pause(r); err == nil {
		t.Fatal("Docker's VM was paused")
	}
	if err := Restore(r); err == nil {
		t.Fatal("Docker's VM was restored; it comes back by itself")
	}
}

// fakeEngine records docker and other commands instead of running them.
type fakeEngine struct {
	state   []EngineState // engine state on each read; the last one repeats
	ps      string
	psErr   error
	restart error
	calls   []string
}

func (f *fakeEngine) install(t *testing.T) {
	t.Helper()
	oldState, oldDocker, oldCmd, oldWait, oldPoll, oldSleep := engineState, runDocker, runCommand, quitWait, quitPoll, sleepFor
	engineState = func() EngineState {
		s := f.state[0]
		if len(f.state) > 1 {
			f.state = f.state[1:]
		}
		return s
	}
	runDocker = func(_ time.Duration, args ...string) (string, error) {
		f.calls = append(f.calls, "docker "+strings.Join(args, " "))
		switch args[0] {
		case "ps":
			return f.ps, f.psErr
		case "desktop":
			return "", f.restart
		}
		return "", nil
	}
	runCommand = func(_ time.Duration, name string, args ...string) (string, error) {
		f.calls = append(f.calls, name+" "+strings.Join(args, " "))
		return "", nil
	}
	quitWait, quitPoll, sleepFor = time.Second, time.Millisecond, func(time.Duration) {}
	t.Cleanup(func() {
		engineState, runDocker, runCommand, quitWait, quitPoll, sleepFor = oldState, oldDocker, oldCmd, oldWait, oldPoll, oldSleep
	})
}

func (f *fakeEngine) ran(prefix string) bool {
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func TestRestartDockerEngineRefusesWhileInUse(t *testing.T) {
	cases := []struct {
		name string
		f    fakeEngine
	}{
		{"engine down", fakeEngine{state: []EngineState{EngineDown}}},
		{"engine hung", fakeEngine{state: []EngineState{EngineHung}}},
		{"containers running", fakeEngine{state: []EngineState{EngineUp}, ps: "abc123\ndef456\n"}},
		{"cannot list containers", fakeEngine{state: []EngineState{EngineUp}, psErr: errors.New("docker: timed out")}},
	}
	for _, tc := range cases {
		f := tc.f
		f.install(t)
		if err := RestartDockerEngine(); err == nil {
			t.Errorf("%s: restarted", tc.name)
		}
		if f.ran("docker desktop") || f.ran("osascript") || f.ran("open") {
			t.Errorf("%s: ran %v", tc.name, f.calls)
		}
	}
	f := fakeEngine{state: []EngineState{EngineUp}, ps: "abc\n"}
	f.install(t)
	if err := RestartDockerEngine(); !errors.Is(err, ErrDockerInUse) {
		t.Errorf("running containers should be ErrDockerInUse: %v", err)
	}
}

func TestRestartDockerEngineWithTheDesktopCLI(t *testing.T) {
	f := fakeEngine{state: []EngineState{EngineUp}, ps: "\n"}
	f.install(t)
	if err := RestartDockerEngine(); err != nil {
		t.Fatal(err)
	}
	if !f.ran("docker desktop restart") || f.ran("osascript") {
		t.Fatalf("should restart with docker desktop restart alone: %v", f.calls)
	}
	// A restart that fails for another reason is reported, not retried another way.
	f = fakeEngine{state: []EngineState{EngineUp}, restart: errors.New("docker: exit status 1: engine failed to start")}
	f.install(t)
	if err := RestartDockerEngine(); err == nil || f.ran("osascript") {
		t.Fatalf("a failed restart was hidden or retried: %v %v", err, f.calls)
	}
}

func TestRestartDockerEngineWithoutTheDesktopCLI(t *testing.T) {
	f := fakeEngine{state: []EngineState{EngineUp, EngineUp, EngineDown}, ps: "",
		restart: errors.New(`docker: exit status 1: docker: 'desktop' is not a docker command.`)}
	f.install(t)
	if err := RestartDockerEngine(); err != nil {
		t.Fatal(err)
	}
	want := []string{"docker ps -q", "docker desktop restart --timeout 180", `osascript -e quit app "Docker"`, "open -a Docker"}
	if strings.Join(f.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls %v, want %v", f.calls, want)
	}
	// Docker Desktop never quits: it is not opened again over itself.
	f = fakeEngine{state: []EngineState{EngineUp}, restart: errors.New("unknown command: desktop")}
	f.install(t)
	if err := RestartDockerEngine(); err == nil || f.ran("open") {
		t.Fatalf("opened Docker while it had not quit: %v %v", err, f.calls)
	}
}
