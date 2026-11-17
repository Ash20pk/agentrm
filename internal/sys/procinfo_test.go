package sys

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestReadOwnProcInfo(t *testing.T) {
	info, ok := ReadProcInfo(os.Getpid())
	if !ok || len(info.Argv) == 0 {
		t.Fatalf("could not read this process: %+v", info)
	}
}

func TestExitWatcher(t *testing.T) {
	w, err := NewExitWatcher()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sleep", "0.3")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	w.Watch(cmd.Process.Pid)
	go func() { _ = cmd.Wait() }()
	select {
	case pid := <-w.C:
		if pid != cmd.Process.Pid {
			t.Fatalf("wrong pid %d", pid)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("exit not reported")
	}
	// A process that is already gone is reported at once.
	w.Watch(cmd.Process.Pid)
	select {
	case <-w.C:
	case <-time.After(time.Second):
		t.Fatal("an already-gone process was not reported")
	}
}

func TestTruncatedTableIsRejected(t *testing.T) {
	full := map[int]*Proc{1: {PID: 1}, 77: {PID: 77}, 500: {PID: 500}}
	if !CompleteTable(full, 77) {
		t.Fatal("a whole table was rejected")
	}
	cut := map[int]*Proc{1: {PID: 1}, 500: {PID: 500}} // ps died before reaching its own line
	if CompleteTable(cut, 77) {
		t.Fatal("a table without the reader was accepted")
	}
	if CompleteTable(map[int]*Proc{77: {PID: 77}}, 77) {
		t.Fatal("a table without launchd was accepted")
	}
	if procs := ListProcs(); procs == nil || !CompleteTable(procs, os.Getpid()) {
		t.Fatal("the real table was rejected")
	}
}

func TestFootprintCountsWhatRSSMisses(t *testing.T) {
	mb, ok := Footprint(os.Getpid())
	if !ok || mb <= 0 {
		t.Fatalf("own footprint unreadable: %v %v", mb, ok)
	}
	if _, ok := Footprint(1); ok {
		t.Log("launchd's footprint is readable (fine either way)")
	}
}

func TestGitHead(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(root+"/.git", 0o755)
	os.WriteFile(root+"/.git/HEAD", []byte("ref: refs/heads/feature/x\n"), 0o644)
	os.MkdirAll(root+"/a/b", 0o755)
	if repo, br := GitHead(root + "/a/b"); repo != root || br != "feature/x" {
		t.Fatalf("got %q %q", repo, br)
	}
	// A worktree: .git is a file.
	wt := t.TempDir()
	os.MkdirAll(root+"/.git/worktrees/wt", 0o755)
	os.WriteFile(root+"/.git/worktrees/wt/HEAD", []byte("ref: refs/heads/fix\n"), 0o644)
	os.WriteFile(wt+"/.git", []byte("gitdir: "+root+"/.git/worktrees/wt\n"), 0o644)
	if repo, br := GitHead(wt); repo != wt || br != "fix" {
		t.Fatalf("worktree: %q %q", repo, br)
	}
	if repo, _ := GitHead("/"); repo != "" {
		t.Fatal("found a repository at /")
	}
}
