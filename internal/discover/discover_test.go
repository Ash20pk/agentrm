package discover

import (
	"testing"
	"time"

	"agentrm/internal/model"
	"agentrm/internal/sys"
)

func TestDevServerFilter(t *testing.T) {
	cfg := model.DefaultConfig()
	yes := []*sys.Proc{{Exe: "node", Args: "node dist/main.js"}, {Exe: "/tmp/go-build123/exe/main", Args: "main"},
		{Exe: "/opt/homebrew/bin/python3.12", Args: "python3.12 -m http.server"}}
	no := []*sys.Proc{{Exe: "/Applications/Slack.app/x", Args: "x"}, {Exe: "node", Args: "node /x/mcp-server.js"},
		{Exe: "node", Args: "node /usr/local/bin/claude"}, {Exe: "/opt/homebrew/bin/postgres", Args: "postgres -D x"},
		{Exe: "/usr/local/bin/ollama", Args: "ollama serve"}}
	for _, p := range yes {
		if !IsDevServer(p, cfg) {
			t.Errorf("%s should be a dev server", p.Args)
		}
	}
	for _, p := range no {
		if IsDevServer(p, cfg) {
			t.Errorf("%s should not be a dev server", p.Args)
		}
	}
}

func TestProcessKinds(t *testing.T) {
	procs := map[int]*sys.Proc{
		1:  {PID: 1, Exe: "launchd"},
		10: {PID: 10, PPID: 1, Exe: "/Users/x/Library/Android/sdk/emulator/qemu/darwin-aarch64/qemu-system-aarch64", Args: "qemu-system-aarch64 -avd Pixel_8 -port 5554", RSSMB: 2000},
		20: {PID: 20, PPID: 1, Exe: "/opt/homebrew/bin/limactl", Args: "/opt/homebrew/bin/limactl hostagent --pidfile x default", RSSMB: 50},
		30: {PID: 30, PPID: 1, Exe: "/opt/homebrew/opt/postgresql@16/bin/postgres", Args: "postgres -D /opt/homebrew/var/postgresql@16", RSSMB: 40},
		31: {PID: 31, PPID: 30, Exe: "/opt/homebrew/opt/postgresql@16/bin/postgres", Args: "postgres: checkpointer", RSSMB: 10},
		40: {PID: 40, PPID: 1, Exe: "/Users/x/Library/Caches/ms-playwright/chromium-1/chrome-mac/Chromium.app/Contents/MacOS/Chromium", Args: "Chromium --headless --remote-debugging-pipe", RSSMB: 200},
		41: {PID: 41, PPID: 40, Exe: "/Users/x/Library/Caches/ms-playwright/chromium-1/chrome-mac/Chromium.app/Contents/Frameworks/x/Helper", Args: "Chromium Helper --type=renderer --headless", RSSMB: 300},
		50: {PID: 50, PPID: 1, Exe: "/Applications/Docker.app/Contents/MacOS/qemu-system-aarch64", Args: "qemu-system-aarch64 docker vm", RSSMB: 4000},
	}
	noFootprint(t, nil)
	got := map[string]*model.Resource{}
	for _, r := range Processes(model.DefaultConfig(), procs) {
		got[r.ID] = r
	}
	if r := got["emu:10"]; r == nil || r.Kind != model.RKEmulator || r.MemMB != 2000 {
		t.Errorf("Android emulator not found with the 2000 MB its qemu holds: %+v", r)
	}
	if r := got["vm:lima-default"]; r == nil || r.Kind != model.RKVM {
		t.Error("Lima VM not found")
	}
	if r := got["db:postgres"]; r == nil || r.MemMB != 50 || r.Restart != "brew services start postgresql@16" {
		t.Errorf("Homebrew postgres not found as one resource with its workers: %+v", r)
	}
	if r := got["browser:40"]; r == nil || r.MemMB != 500 {
		t.Errorf("headless browser not found with its helpers: %+v", r)
	}
	if got["browser:41"] != nil {
		t.Error("a browser helper was listed separately")
	}
	if got["vm:50"] != nil {
		t.Error("Docker Desktop's own VM was listed as a VM to manage")
	}
	if r := got[DockerVMID]; r == nil || r.PID != 50 || r.Kind != model.RKDockerVM {
		t.Errorf("Docker Desktop's own VM was not listed as itself: %+v", r)
	}
}

// A Lima VM on Apple's Virtualization runs its guest in a process under launchd, paired by
// start time; on qemu the guest is the host agent's child, and not a VM of its own.
func TestVMsCountTheirGuest(t *testing.T) {
	t0 := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	procs := map[int]*sys.Proc{
		1:  {PID: 1, Exe: "launchd"},
		20: {PID: 20, PPID: 1, Exe: "/opt/homebrew/bin/limactl", Args: "/opt/homebrew/bin/limactl hostagent --pidfile x vz1", RSSMB: 50, Start: t0},
		21: {PID: 21, PPID: 1, Exe: "/System/Library/Frameworks/Virtualization.framework/Versions/A/XPCServices/com.apple.Virtualization.VirtualMachine.xpc/Contents/MacOS/com.apple.Virtualization.VirtualMachine",
			RSSMB: 900, Start: t0.Add(3 * time.Second)},
		30: {PID: 30, PPID: 1, Exe: "/opt/homebrew/bin/limactl", Args: "/opt/homebrew/bin/limactl hostagent --pidfile y q1", RSSMB: 40, Start: t0.Add(time.Hour)},
		31: {PID: 31, PPID: 30, Exe: "/opt/homebrew/bin/qemu-system-aarch64", Args: "qemu-system-aarch64 -m 4096", RSSMB: 3000, Start: t0.Add(time.Hour)},
	}
	noFootprint(t, map[int]float64{21: 4096})
	got := map[string]*model.Resource{}
	for _, r := range Processes(model.DefaultConfig(), procs) {
		got[r.ID] = r
	}
	if r := got["vm:lima-vz1"]; r == nil || r.MemMB != 50+4096 {
		t.Errorf("Lima on vz without its guest's 4096 MB footprint: %+v", r)
	}
	if r := got["vm:lima-q1"]; r == nil || r.MemMB != 40+3000 {
		t.Errorf("Lima on qemu without its qemu: %+v", r)
	}
	if got["vm:31"] != nil {
		t.Error("Lima's qemu was listed as a second VM")
	}
}

// Two VMs started close together: each gets its own guest whatever order they are looked at in
// (A at 0 s with its guest at 50 s, B at 40 s with its guest at 45 s).
func TestVMGuestPairingIsGlobal(t *testing.T) {
	t0 := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	vz := "/System/Library/Frameworks/Virtualization.framework/Versions/A/XPCServices/com.apple.Virtualization.VirtualMachine.xpc/Contents/MacOS/com.apple.Virtualization.VirtualMachine"
	procs := map[int]*sys.Proc{
		1:  {PID: 1, Exe: "launchd"},
		20: {PID: 20, PPID: 1, Exe: "/opt/homebrew/bin/limactl", Args: "/opt/homebrew/bin/limactl hostagent --pidfile x a", RSSMB: 10, Start: t0},
		21: {PID: 21, PPID: 1, Exe: vz, RSSMB: 1000, Start: t0.Add(50 * time.Second)},
		30: {PID: 30, PPID: 1, Exe: "/opt/homebrew/bin/limactl", Args: "/opt/homebrew/bin/limactl hostagent --pidfile y b", RSSMB: 10, Start: t0.Add(40 * time.Second)},
		31: {PID: 31, PPID: 1, Exe: vz, RSSMB: 3000, Start: t0.Add(45 * time.Second)},
	}
	noFootprint(t, nil)
	for i := 0; i < 50; i++ { // map order varies run to run
		got := map[string]float64{}
		for _, r := range Processes(model.DefaultConfig(), procs) {
			got[r.ID] = r.MemMB
		}
		if got["vm:lima-a"] != 1010 || got["vm:lima-b"] != 3010 {
			t.Fatalf("guests swapped: %v", got)
		}
	}
}

// Docker Desktop and a Lima VM starting together: each gets its own guest (Docker used to be
// paired first, on its own, and could take Lima's).
func TestDockerGuestIsPairedWithTheOthers(t *testing.T) {
	t0 := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	vz := "/System/Library/Frameworks/Virtualization.framework/Versions/A/XPCServices/com.apple.Virtualization.VirtualMachine.xpc/Contents/MacOS/com.apple.Virtualization.VirtualMachine"
	procs := map[int]*sys.Proc{
		1:  {PID: 1, Exe: "launchd"},
		10: {PID: 10, PPID: 1, Exe: "/Applications/Docker.app/Contents/MacOS/com.docker.virtualization", Start: t0},
		11: {PID: 11, PPID: 1, Exe: vz, RSSMB: 4000, Start: t0.Add(40 * time.Second)},
		20: {PID: 20, PPID: 1, Exe: "/opt/homebrew/bin/limactl", Args: "/opt/homebrew/bin/limactl hostagent --pidfile x a", RSSMB: 10, Start: t0.Add(5 * time.Second)},
		21: {PID: 21, PPID: 1, Exe: vz, RSSMB: 1000, Start: t0.Add(10 * time.Second)},
	}
	noFootprint(t, nil)
	if d := dockerVMProc(procs); d == nil || d.PID != 11 {
		t.Fatalf("Docker's guest: %+v", d)
	}
	found := false
	for _, r := range Processes(model.DefaultConfig(), procs) {
		if r.ID == "vm:lima-a" {
			found = true
			if r.MemMB != 1010 {
				t.Fatalf("Lima: %.0f MB", r.MemMB)
			}
		}
	}
	if !found {
		t.Fatal("Lima VM not found")
	}
}

// A headless browser an agent's shell started is listed: the shell is not (shells), so the
// browser must not be taken for a helper of it.
func TestHeadlessBrowserFromAShell(t *testing.T) {
	procs := map[int]*sys.Proc{
		1: {PID: 1, Exe: "launchd"},
		5: {PID: 5, PPID: 1, Exe: "/bin/zsh", Args: "/bin/zsh -c chromium --headless --dump-dom https://example.com", RSSMB: 5},
		6: {PID: 6, PPID: 5, Exe: "/opt/homebrew/bin/chromium", Args: "chromium --headless --dump-dom https://example.com", RSSMB: 400},
	}
	noFootprint(t, nil)
	rs := Processes(model.DefaultConfig(), procs)
	if len(rs) != 1 || rs[0].ID != "browser:6" || rs[0].MemMB != 400 {
		t.Fatalf("want the browser alone, got %d: %+v", len(rs), rs)
	}
}

// Gradle and Kotlin daemons are JVMs; anything else that mentions them is not one.
func TestDaemonsAreJava(t *testing.T) {
	jdk := "/Library/Java/JavaVirtualMachines/temurin-21.jdk/Contents/Home/bin/java"
	procs := map[int]*sys.Proc{
		1:  {PID: 1, Exe: "launchd"},
		10: {PID: 10, PPID: 1, Exe: jdk, Args: "java -Xmx2g -cp gradle-launcher.jar org.gradle.launcher.daemon.bootstrap.GradleDaemon 8.10", RSSMB: 900},
		11: {PID: 11, PPID: 1, Exe: jdk, Args: "java -cp kotlin-daemon.jar org.jetbrains.kotlin.daemon.KotlinCompileDaemon", RSSMB: 600},
		20: {PID: 20, PPID: 1, Exe: "/usr/bin/grep", Args: "grep -r KotlinCompileDaemon .", RSSMB: 3},
		21: {PID: 21, PPID: 1, Exe: "/opt/homebrew/bin/rg", Args: "rg org.gradle.launcher.daemon GradleDaemon", RSSMB: 3},
	}
	noFootprint(t, nil)
	got := map[string]bool{}
	for _, r := range Processes(model.DefaultConfig(), procs) {
		got[r.ID] = true
	}
	if !got["gradle:10"] || !got["gradle:11"] || len(got) != 2 {
		t.Fatalf("want the two JVM daemons only: %v", got)
	}
}

// An agent's shell whose command names these tools is not one of them (it was listed as a Lima
// VM, and given a lease).
func TestAShellIsNotWhatItMentions(t *testing.T) {
	procs := map[int]*sys.Proc{
		1: {PID: 1, Exe: "launchd"},
		5: {PID: 5, PPID: 1, Exe: "/bin/zsh", Args: "/bin/zsh -c limactl hostagent --pidfile x a && java GradleDaemon && tart run v && pwd -P >| /tmp/claude-3c15-cwd", RSSMB: 5},
		6: {PID: 6, PPID: 1, Exe: "/usr/bin/python3", Args: "python3 -c print('limactl hostagent --pidfile x b')", RSSMB: 5},
	}
	noFootprint(t, nil)
	if rs := Processes(model.DefaultConfig(), procs); len(rs) != 0 {
		t.Fatalf("found %d: %s", len(rs), rs[0].ID)
	}
}

// A booted simulator is sized by its launchd_sim and what runs under it, not the estimate.
func TestSimulatorsAreMeasured(t *testing.T) {
	udid := "0F1E2D3C-AAAA-BBBB-CCCC-123456789ABC"
	rs := []*model.Resource{{ID: "sim:" + udid, UDID: udid, MemMB: 1536, Detail: "iOS-18-0, size estimated"},
		{ID: "sim:other", UDID: "other", MemMB: 1536, Detail: "iOS-18-0, size estimated"}}
	procs := map[int]*sys.Proc{
		1: {PID: 1, Exe: "launchd"},
		60: {PID: 60, PPID: 1, Exe: "/Library/Developer/CoreSimulator/Volumes/iOS_22A/Library/Developer/CoreSimulator/Profiles/Runtimes/iOS 18.0.simruntime/Contents/Resources/RuntimeRoot/sbin/launchd_sim",
			Args: "launchd_sim /Users/x/Library/Developer/CoreSimulator/Devices/" + udid + "/data/var/run/com.apple.CoreSimulator.simdevice." + udid + ".plist", RSSMB: 20},
		61: {PID: 61, PPID: 60, Exe: "SpringBoard", RSSMB: 300},
		62: {PID: 62, PPID: 61, Exe: "MyApp", RSSMB: 200},
	}
	measureSimulators(rs, procs)
	if rs[0].MemMB != 520 || rs[0].Detail != "iOS-18-0" {
		t.Errorf("simulator not measured: %+v", rs[0])
	}
	if rs[1].MemMB != 1536 || rs[1].Detail != "iOS-18-0, size estimated" {
		t.Errorf("a simulator with no process found lost its estimate: %+v", rs[1])
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]float64{"1.5GiB / 8GiB": 1536, "512MiB": 512, "100kB": 100.0 / 1024, "1.2GB": 1.2 * 1024} {
		if got := parseSize(in); got-want > 0.01 || want-got > 0.01 {
			t.Errorf("parseSize(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestPauseAndStopRules(t *testing.T) {
	if CanPause(&model.Resource{Kind: model.RKSimulator}) || CanPause(&model.Resource{Kind: model.RKDatabase}) {
		t.Error("a simulator or database is said to be pausable")
	}
	if CanAutoStop(&model.Resource{Kind: model.RKVM}) {
		t.Error("a VM may be stopped automatically")
	}
	if !CanPause(&model.Resource{Kind: model.RKServer}) || !CanAutoStop(&model.Resource{Kind: model.RKServer}) {
		t.Error("a dev server should be pausable and stoppable")
	}
}
