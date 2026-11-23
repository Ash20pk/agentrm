package setup

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"agentrm/internal/model"
	"agentrm/internal/store"
)

// procState is the ps state letter of pid: T when stopped.
func procState(t *testing.T, pid int) string {
	t.Helper()
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		t.Fatalf("ps %d: %v", pid, err)
	}
	return strings.TrimSpace(string(out))
}

func TestUninstallRemovesEverythingAndThawsWithoutDaemon(t *testing.T) {
	h := fakeHome(t)
	t.Setenv("AGENTRM_SOCK", filepath.Join(h, "no-daemon.sock"))

	// ~/.zshenv is a link into a dotfiles repository: it must stay one.
	dots := filepath.Join(h, "dotfiles")
	_ = os.MkdirAll(dots, 0o755)
	_ = os.WriteFile(filepath.Join(dots, "zshenv"), []byte("export FOO=1\n"), 0o644)
	if err := os.Symlink(filepath.Join(dots, "zshenv"), filepath.Join(h, ".zshenv")); err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(filepath.Join(h, ".codex"), 0o755)
	_ = os.WriteFile(filepath.Join(h, ".codex", "hooks.json"),
		[]byte(`{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"policy.py"}]}]}}`), 0o600)

	for _, f := range []func(bool) error{SetupClaude, SetupCodex, SetupPath, SetupShim} {
		if err := f(false); err != nil {
			t.Fatal(err)
		}
	}
	for _, a := range []string{"codex", "gemini", "cursor"} {
		if err := SetupAgentHooks(a, false); err != nil {
			t.Fatal(err)
		}
	}

	// A process AgentRM froze, recorded in state.json, with the daemon not running.
	sleeper := exec.Command("sleep", "60")
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sleeper.Process.Kill(); _, _ = sleeper.Process.Wait() }()
	if err := sleeper.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	pid := sleeper.Process.Pid
	for i := 0; i < 50 && !strings.HasPrefix(procState(t, pid), "T"); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	paused := time.Now()
	st := model.NewState()
	st.Resources["proc:x"] = &model.Resource{ID: "proc:x", Kind: model.RKServer, PID: pid, PausedAt: &paused}
	b, _ := json.Marshal(st)
	if err := os.WriteFile(filepath.Join(store.Dir(), "state.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}

	r := Uninstall()
	if len(r.Errs) > 0 {
		t.Fatalf("uninstall failed: %v", r.Errs)
	}
	if ok, _ := ClaudeHooksInstalled(); ok {
		t.Error("Claude hooks left")
	}
	for _, a := range []string{"codex", "gemini", "cursor"} {
		if AgentHooksInstalled(a) {
			t.Errorf("%s hooks left", a)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(h, ".codex", "hooks.json")); !strings.Contains(string(b), "policy.py") {
		t.Errorf("the user's own Codex hook was removed: %s", b)
	}
	if CodexNoteInstalled() || CLILink() != "" || ShimInstalled() || shimBlockInstalled() {
		t.Error("the Codex note, the CLI link or the shim was left")
	}
	if fi, err := os.Lstat(filepath.Join(h, ".zshenv")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Error("~/.zshenv was replaced by a plain file")
	}
	if b, _ := os.ReadFile(filepath.Join(dots, "zshenv")); string(b) != "export FOO=1\n" {
		t.Errorf("the linked zshenv was not restored: %q", b)
	}

	if s := procState(t, pid); strings.HasPrefix(s, "T") {
		t.Errorf("the paused process is still stopped: %s", s)
	}
	after, _ := store.LoadState(store.Dir(), time.Now())
	if res := after.Resources["proc:x"]; res == nil || res.PausedAt != nil {
		t.Errorf("state.json still records it as paused: %+v", res)
	}
	if !strings.Contains(strings.Join(r.Done, "\n"), "proc:x") {
		t.Errorf("the thaw was not reported: %v", r.Done)
	}

	// Nothing left: a second run does nothing and fails nothing.
	if r := Uninstall(); len(r.Errs) > 0 || len(r.Done) > 0 {
		t.Errorf("second uninstall: %+v", r)
	}
}

func TestUninstallReportsWhatItCannotDo(t *testing.T) {
	h := fakeHome(t)
	t.Setenv("AGENTRM_SOCK", filepath.Join(h, "no-daemon.sock"))
	if err := SetupAgentHooks("cursor", false); err != nil {
		t.Fatal(err)
	}
	// The daemon is "running" (holds its lock) but does not answer: nothing can be thawed safely.
	lock, err := store.LockDaemon()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	r := Uninstall()
	if len(r.Errs) != 1 || !strings.Contains(r.Errs[0].Error(), "resume") {
		t.Fatalf("want one resume error, got %v", r.Errs)
	}
	if AgentHooksInstalled("cursor") {
		t.Error("one failure stopped the rest of the uninstall")
	}
}
