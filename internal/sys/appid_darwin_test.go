package sys

import "testing"

// The app is its bundled helper run by its main executable, both in a trusted bundle and signed
// by this daemon's team, as the kernel sees them.
func TestAppCaller(t *testing.T) {
	const b = "/Applications/AgentRM.app"
	live := map[int]struct {
		path string
		ppid int
	}{
		1:  {"/sbin/launchd", 0},
		50: {b + appMain, 1},
		51: {b + appHelper, 50}, // the app's helper, run by the app
		52: {b + appHelper, 1},  // the real helper, run detached
		60: {"/tmp/AgentRM.app" + appMain, 1},
		61: {"/tmp/AgentRM.app" + appHelper, 60}, // a copy outside a trusted bundle
		70: {"/usr/local/bin/agentrm", 1},        // says it is the helper (argv), is not
	}
	signed := true
	oldLive, oldBundles, oldSigned := liveExe, appBundles, appSigned
	t.Cleanup(func() { liveExe, appBundles, appSigned = oldLive, oldBundles, oldSigned })
	liveExe = func(pid int) (string, int, bool) { l, ok := live[pid]; return l.path, l.ppid, ok }
	appBundles = func() []string { return []string{b} }
	appSigned = func(string) bool { return signed }

	procs := map[int]*Proc{}
	for pid, l := range live {
		procs[pid] = &Proc{PID: pid, PPID: l.ppid, Path: l.path}
	}
	procs[70].Exe = b + appHelper
	if !AppCaller(procs, 51) {
		t.Fatal("the app's own helper is not the app")
	}
	if g := CallerGUI(procs, 51); !g.ViaApp || !g.Found {
		t.Fatalf("CallerGUI for the app: %+v", g)
	}
	for _, pid := range []int{50, 52, 61, 70} {
		if AppCaller(procs, pid) {
			t.Errorf("pid %d passed for the app", pid)
		}
	}
	// The table is stale (the PID was reused): the kernel's answer wins.
	procs[71] = &Proc{PID: 71, PPID: 50, Path: b + appHelper}
	live[71] = struct {
		path string
		ppid int
	}{"/usr/local/bin/agentrm", 50}
	if AppCaller(procs, 71) {
		t.Error("a reused PID passed for the app")
	}
	signed = false
	if AppCaller(procs, 51) {
		t.Error("an app not signed by this daemon's team passed for the app")
	}
}
