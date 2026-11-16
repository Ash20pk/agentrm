package setup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentrm/internal/api"
)

func fakeHome(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv("AGENTRM_HOME", filepath.Join(h, ".agentrm"))
	_ = os.MkdirAll(filepath.Join(h, ".agentrm"), 0o755)
	_ = os.MkdirAll(filepath.Join(h, ".claude"), 0o755)
	return h
}

func TestClaudeHooksMergeAndRemove(t *testing.T) {
	h := fakeHome(t)
	orig := `{"model":"opus","hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"my-hook.sh"}]}]},"permissions":{"allow":["Bash(ls:*)"]}}`
	p := filepath.Join(h, ".claude", "settings.json")
	_ = os.WriteFile(p, []byte(orig), 0o600)
	if err := SetupClaude(false); err != nil {
		t.Fatal(err)
	}
	if err := SetupClaude(false); err != nil { // twice: no duplicates
		t.Fatal(err)
	}
	var m map[string]any
	b, _ := os.ReadFile(p)
	_ = json.Unmarshal(b, &m)
	pre := m["hooks"].(map[string]any)["PreToolUse"].([]any)
	if len(pre) != 2 || m["model"] != "opus" || m["permissions"] == nil {
		t.Fatalf("merge lost or duplicated something: %s", b)
	}
	ok, cmd := ClaudeHooksInstalled()
	if bin, proto := hookParts(cmd); !ok || proto != api.HookProto || bin != SelfPath() {
		t.Fatalf("installed hook not recognised: %q", cmd)
	}
	if err := SetupClaude(true); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(p)
	_ = json.Unmarshal(b, &m)
	if !strings.Contains(string(b), "my-hook.sh") || strings.Contains(string(b), " hook pre") {
		t.Fatalf("uninstall did not restore the user's own hooks: %s", b)
	}
	if !RemovedOnPurpose(3600e9) {
		t.Error("removal through AgentRM was not marked as intended")
	}
	backups, _ := filepath.Glob(p + ".agentrm-backup-*")
	if len(backups) != 3 {
		t.Errorf("expected a backup per write, got %d", len(backups))
	}
}

func TestOldHooksAreSeenAsStale(t *testing.T) {
	bin, proto := hookParts(`"/Applications/AgentRM.app/Contents/Helpers/agentrm" hook pre`)
	if bin != "/Applications/AgentRM.app/Contents/Helpers/agentrm" || proto != 1 {
		t.Fatalf("got %q %d", bin, proto)
	}
}

func TestPreviewChangesNothing(t *testing.T) {
	h := fakeHome(t)
	s, err := PreviewClaude(false)
	if err != nil || !strings.Contains(s, "PreToolUse") {
		t.Fatalf("%v %q", err, s)
	}
	if _, err := os.Stat(filepath.Join(h, ".claude", "settings.json")); err == nil {
		t.Fatal("preview wrote the settings")
	}
}

func TestShimInstallAndRemove(t *testing.T) {
	h := fakeHome(t)
	_ = os.WriteFile(filepath.Join(h, ".zshenv"), []byte("export FOO=1\n"), 0o644)
	if err := SetupShim(false); err != nil {
		t.Fatal(err)
	}
	if !ShimInstalled() {
		t.Fatal("shim not installed")
	}
	if _, err := os.Stat(filepath.Join(h, ".bash_profile")); err == nil {
		t.Error("created a bash profile that did not exist (it would hide ~/.profile)")
	}
	if err := SetupShim(true); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(h, ".zshenv"))
	if string(b) != "export FOO=1\n" || ShimInstalled() {
		t.Fatalf("uninstall left something: %q", b)
	}
}

func TestCodexNote(t *testing.T) {
	h := fakeHome(t)
	_ = os.MkdirAll(filepath.Join(h, ".codex"), 0o755)
	_ = os.WriteFile(filepath.Join(h, ".codex", "AGENTS.md"), []byte("# Mine\n\nBe nice.\n"), 0o644)
	_ = SetupCodex(false)
	_ = SetupCodex(false)
	b, _ := os.ReadFile(filepath.Join(h, ".codex", "AGENTS.md"))
	if strings.Count(string(b), codexStart) != 1 {
		t.Fatal("note duplicated")
	}
	_ = SetupCodex(true)
	b, _ = os.ReadFile(filepath.Join(h, ".codex", "AGENTS.md"))
	if string(b) != "# Mine\n\nBe nice.\n" {
		t.Fatalf("uninstall changed the user's text: %q", b)
	}
}
