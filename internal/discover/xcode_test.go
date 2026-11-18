package discover

import (
	"testing"
	"time"

	"agentrm/internal/model"
	"agentrm/internal/sys"
)

const toolchain = "/Applications/Xcode.app/Contents/Developer/Toolchains/XcodeDefault.xctoolchain"

func TestXcodeHelpersFound(t *testing.T) {
	procs := map[int]*sys.Proc{
		1:  {PID: 1, Exe: "launchd"},
		10: {PID: 10, PPID: 1, Exe: "/Applications/Xcode.app/Contents/MacOS/Xcode", RSSMB: 3000},
		11: {PID: 11, PPID: 1, Exe: toolchain + "/usr/lib/sourcekitd.framework/Versions/A/XPCServices/SourceKitService.xpc/Contents/MacOS/SourceKitService", RSSMB: 2500},
		12: {PID: 12, PPID: 10, Exe: "/Applications/Xcode.app/Contents/SharedFrameworks/SwiftBuild.framework/Versions/A/PlugIns/SWBBuildService.bundle/Contents/MacOS/SWBBuildService", RSSMB: 1800},
		// Compiling under the build service: a build, not a helper.
		13: {PID: 13, PPID: 12, Exe: toolchain + "/usr/bin/swift-frontend", RSSMB: 1500},
		// Left behind by a build that is gone.
		14: {PID: 14, PPID: 1, Exe: toolchain + "/usr/bin/swift-frontend", RSSMB: 1200},
		// Small: not worth listing.
		15: {PID: 15, PPID: 1, Exe: "/Applications/Xcode.app/Contents/SharedFrameworks/SourceKit.framework/Versions/A/XPCServices/com.apple.dt.SKAgent.xpc/Contents/MacOS/com.apple.dt.SKAgent", RSSMB: 200},
		// sourcekit-lsp that is not Xcode's or a toolchain's: someone's own build, left alone.
		16: {PID: 16, PPID: 1, Exe: "/Users/x/src/sourcekit-lsp/.build/debug/sourcekit-lsp", RSSMB: 3000},
		17: {PID: 17, PPID: 1, Exe: toolchain + "/usr/bin/sourcekit-lsp", RSSMB: 900},
	}
	noFootprint(t, map[int]float64{17: 1400}) // compressed: its footprint is what counts
	got := map[string]*model.Resource{}
	for _, r := range Processes(model.DefaultConfig(), procs) {
		got[r.ID] = r
	}
	for _, id := range []string{"xcode:11", "xcode:12", "xcode:14", "xcode:17"} {
		r := got[id]
		if r == nil || r.Kind != model.RKXcodeHelper || r.ProcStart != procs[r.PID].Start || r.Restart == "" {
			t.Errorf("%s not found as an Xcode helper: %+v", id, r)
		}
	}
	for _, id := range []string{"xcode:10", "xcode:13", "xcode:15", "xcode:16"} {
		if got[id] != nil {
			t.Errorf("%s should not be listed: %+v", id, got[id])
		}
	}
	if r := got["xcode:17"]; r != nil && r.MemMB != 1400 {
		t.Errorf("sourcekit-lsp counted at %v MB, not its footprint", r.MemMB)
	}
}

func TestXcodeHelperRules(t *testing.T) {
	r := &model.Resource{ID: "xcode:11", Kind: model.RKXcodeHelper, PID: 11}
	if CanPause(r) || CanAutoStop(r) || !SelfRestarting(r) || NoAutoStopWhy(r) == "" {
		t.Fatal("an Xcode helper is frozen or stopped on the ordinary path")
	}
	if err := Pause(r); err == nil {
		t.Fatal("an Xcode helper was frozen")
	}
	if err := Restore(r); err == nil {
		t.Fatal("an Xcode helper was restored; Xcode starts it")
	}
}

// Ending a helper signals only the process that was found: a PID that is now another process,
// or one whose start was never recorded, is not signalled.
func TestXcodeHelperStopChecksTheProcess(t *testing.T) {
	t0 := time.Date(2026, 11, 18, 8, 0, 0, 0, time.UTC)
	sent := fakeSignals(t, map[int]*sys.Proc{11: {PID: 11, PPID: 1, Start: t0.Add(time.Hour)}})
	r := &model.Resource{ID: "xcode:11", Kind: model.RKXcodeHelper, PID: 11, ProcStart: t0}
	if err := Stop(r); err == nil || len(*sent) != 0 {
		t.Fatalf("signalled a reused PID: %v %v", err, *sent)
	}
	r.ProcStart = time.Time{}
	if err := Stop(r); err == nil || len(*sent) != 0 {
		t.Fatalf("signalled a process whose start was not known: %v %v", err, *sent)
	}
	r.ProcStart = t0.Add(time.Hour)
	if err := Stop(r); err != nil || len(*sent) == 0 {
		t.Fatalf("the helper itself was not ended: %v %v", err, *sent)
	}
}
