package sys

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

// On Linux every process of this user exposes its environment, so a detached command an
// agent started (even /bin/sleep, which macOS hides) names its session with no sampling at all.
func TestDetachedSleepNamesItsSession(t *testing.T) {
	cmd := exec.Command("sleep", "5")
	cmd.Env = append(os.Environ(), "CLAUDE_CODE_SESSION_ID=sess-1", "CLAUDE_PID=4242")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	time.Sleep(100 * time.Millisecond)
	info, ok := ReadProcInfo(cmd.Process.Pid)
	if !ok || info.Session != "sess-1" || info.AgentPID != 4242 || len(info.Argv) != 2 {
		t.Fatalf("%+v %v", info, ok)
	}
	p := ListProcs()[cmd.Process.Pid]
	if p == nil || p.Start.IsZero() || p.PPID != os.Getpid() {
		t.Fatalf("process table entry: %+v", p)
	}
	if fp, ok := Footprint(cmd.Process.Pid); !ok || fp <= 0 {
		t.Fatalf("footprint: %v %v", fp, ok)
	}
}
