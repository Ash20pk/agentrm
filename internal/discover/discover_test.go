package discover

import (
	"testing"

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
	if r := got["emu:10"]; r == nil || r.Kind != model.RKEmulator {
		t.Error("Android emulator not found")
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
