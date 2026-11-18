package discover

import (
	"syscall"
	"testing"
	"time"

	"agentrm/internal/model"
	"agentrm/internal/sys"
)

func fakeSignals(t *testing.T, procs map[int]*sys.Proc) *[]string {
	t.Helper()
	var sent []string
	oq, ol, os := quickProc, listProcs, sendSig
	quickProc = func(pid int) *sys.Proc { return procs[pid] }
	listProcs = func() map[int]*sys.Proc { return procs }
	sendSig = func(pid int, sig syscall.Signal) error {
		sent = append(sent, sig.String()+" "+itoa(pid))
		return nil
	}
	t.Cleanup(func() { quickProc, listProcs, sendSig = oq, ol, os })
	return &sent
}

func itoa(n int) string {
	return string(rune('0'+n/100%10)) + string(rune('0'+n/10%10)) + string(rune('0'+n%10))
}

// Paused by a 0.1 daemon, which recorded no start time: it must still be thawed (SIGCONT harms
// nothing), while a pause or a stop with no start time is refused.
func TestResumeWithoutAStartTimeThaws(t *testing.T) {
	start := time.Date(2026, 11, 18, 8, 0, 0, 0, time.UTC)
	procs := map[int]*sys.Proc{
		400: {PID: 400, PPID: 1, Start: start},
		401: {PID: 401, PPID: 400, Start: start},
	}
	sent := fakeSignals(t, procs)
	r := &model.Resource{ID: "srv:400", Kind: model.RKServer, PID: 400}
	if err := resumeTree(r); err != nil {
		t.Fatal(err)
	}
	if len(*sent) != 2 || (*sent)[0] != "continued 401" {
		t.Fatalf("want the child thawed then the parent: %v", *sent)
	}
	*sent = nil
	if err := pauseTree(r); err == nil || len(*sent) != 0 {
		t.Fatalf("paused a process whose start was never recorded: %v", *sent)
	}
}

// A restart for a leak: SIGTERM first, and nothing started beside a process that did not exit.
func TestRestartWaitsForTheOldProcess(t *testing.T) {
	start := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	procs := map[int]*sys.Proc{300: {PID: 300, PPID: 1, Start: start}}
	sent := fakeSignals(t, procs)
	old := restartWait
	restartWait = 300 * time.Millisecond
	t.Cleanup(func() { restartWait = old })

	if err := Restart(&model.Resource{ID: "srv:300", Kind: model.RKServer, PID: 300, ProcStart: start}); err == nil || len(*sent) != 0 {
		t.Fatalf("restarted something with no known restart: %v %v", err, *sent)
	}
	r := &model.Resource{ID: "srv:300", Kind: model.RKServer, PID: 300, ProcStart: start, RestartArgv: []string{"/bin/true"}}
	if err := Restart(r); err == nil {
		t.Fatal("started again while the old process was still running")
	}
	term := false
	for _, s := range *sent {
		term = term || s == syscall.SIGTERM.String()+" 300"
	}
	if !term {
		t.Fatalf("no SIGTERM: %v", *sent)
	}
}
