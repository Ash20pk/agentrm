package setup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentHooksMergeAndRemove(t *testing.T) {
	h := t.TempDir()
	t.Setenv("HOME", h)
	// Each agent already has a hook of its own that must survive.
	os.MkdirAll(filepath.Join(h, ".codex"), 0o755)
	os.WriteFile(filepath.Join(h, ".codex", "hooks.json"),
		[]byte(`{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"policy.py"}]}]}}`), 0o600)
	os.MkdirAll(filepath.Join(h, ".cursor"), 0o755)
	os.WriteFile(filepath.Join(h, ".cursor", "hooks.json"),
		[]byte(`{"version":1,"hooks":{"beforeShellExecution":[{"command":"./approve.sh"}]}}`), 0o600)
	os.MkdirAll(filepath.Join(h, ".gemini"), 0o755)
	os.WriteFile(filepath.Join(h, ".gemini", "settings.json"), []byte(`{"theme":"dark"}`), 0o600)

	for _, a := range []string{"codex", "gemini", "cursor"} {
		for i := 0; i < 2; i++ { // installing twice changes nothing
			if err := SetupAgentHooks(a, false); err != nil {
				t.Fatal(a, err)
			}
		}
		if !AgentHooksInstalled(a) {
			t.Fatalf("%s: not installed", a)
		}
	}
	read := func(p string) map[string]any {
		b, _ := os.ReadFile(filepath.Join(h, p))
		m := map[string]any{}
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(p, err)
		}
		return m
	}
	codex := read(".codex/hooks.json")["hooks"].(map[string]any)
	if pre := codex["PreToolUse"].([]any); len(pre) != 2 || !strings.Contains(mustJSON(pre), "policy.py") || !strings.Contains(mustJSON(pre), "--agent codex") {
		t.Fatalf("codex PreToolUse: %s", mustJSON(pre))
	}
	cursor := read(".cursor/hooks.json")
	cpre := cursor["hooks"].(map[string]any)["beforeShellExecution"].([]any)
	if len(cpre) != 2 || cursor["version"] == nil || strings.Contains(mustJSON(cpre), `"type"`) {
		t.Fatalf("cursor: %s", mustJSON(cursor))
	}
	gem := read(".gemini/settings.json")
	if gem["theme"] != "dark" || !strings.Contains(mustJSON(gem), `"matcher":"run_shell_command"`) || !strings.Contains(mustJSON(gem), `"timeout":10000`) {
		t.Fatalf("gemini: %s", mustJSON(gem))
	}
	for _, a := range []string{"codex", "gemini", "cursor"} {
		if err := SetupAgentHooks(a, true); err != nil {
			t.Fatal(err)
		}
		if AgentHooksInstalled(a) {
			t.Fatalf("%s: still installed after removal", a)
		}
	}
	if !strings.Contains(mustJSON(read(".codex/hooks.json")), "policy.py") || !strings.Contains(mustJSON(read(".cursor/hooks.json")), "approve.sh") {
		t.Fatal("removing AgentRM's hooks removed the user's")
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
