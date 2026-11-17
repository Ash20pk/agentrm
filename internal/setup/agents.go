package setup

// Hooks for agents other than Claude Code: Codex, Gemini CLI and Cursor each run a command
// before and after shell commands, on prompt submit and on session end. The same AgentRM hook
// serves them all (`agentrm hook <event> --agent <name>`); only where it is registered, and
// under which event names, differs.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"agentrm/internal/api"
	"agentrm/internal/store"
)

type agentHooks struct {
	name   string // codex, gemini, cursor
	title  string
	path   func() string
	flat   bool // Cursor: {"version":1,"hooks":{event:[{command,...}]}}; others nest matcher → hooks
	events []hookEvent
	// timeout is written as this many units per hook (Gemini counts milliseconds).
	timeout int
}

type hookEvent struct{ event, matcher, arg string }

var otherAgents = []agentHooks{
	{name: "codex", title: "Codex", path: func() string { return home(".codex", "hooks.json") }, timeout: 10,
		events: []hookEvent{{"PreToolUse", "Bash", "pre"}, {"PostToolUse", "Bash", "post"}, {"UserPromptSubmit", "", "prompt"}, {"SessionEnd", "", "end"}}},
	{name: "gemini", title: "Gemini CLI", path: func() string { return home(".gemini", "settings.json") }, timeout: 10000,
		events: []hookEvent{{"BeforeTool", "run_shell_command", "pre"}, {"AfterTool", "run_shell_command", "post"}, {"BeforeAgent", "", "prompt"}, {"SessionEnd", "", "end"}}},
	{name: "cursor", title: "Cursor", path: func() string { return home(".cursor", "hooks.json") }, flat: true, timeout: 10,
		events: []hookEvent{{"beforeShellExecution", "", "pre"}, {"postToolUse", "", "post"}, {"beforeSubmitPrompt", "", "prompt"}, {"sessionEnd", "", "end"}}},
}

func findAgent(name string) (agentHooks, bool) {
	for _, a := range otherAgents {
		if a.name == name {
			return a, true
		}
	}
	return agentHooks{}, false
}

// HookCommandFor is the hook command for another agent.
func HookCommandFor(bin, arg, agent string) string {
	return fmt.Sprintf("%q hook %s --proto %d --agent %s", bin, arg, api.HookProto, agent)
}

func readJSON(path string) (map[string]any, []byte, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]any{}, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	m := map[string]any{}
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, nil, fmt.Errorf("%s is not valid JSON, not touching it: %v", path, err)
		}
	}
	return m, raw, nil
}

// stripOursFlat removes AgentRM entries from a Cursor event list.
func stripOursFlat(list []any) []any {
	var out []any
	for _, h := range list {
		if !isOurHook(h) {
			out = append(out, h)
		}
	}
	return out
}

// SetupAgentHooks installs (or removes) AgentRM's hooks for codex, gemini or cursor, keeping a
// backup and every other setting and hook as it is.
func SetupAgentHooks(name string, uninstall bool) error {
	a, ok := findAgent(name)
	if !ok {
		return fmt.Errorf("no hooks for %q (codex, gemini, cursor)", name)
	}
	path := a.path()
	m, raw, err := readJSON(path)
	if err != nil {
		return err
	}
	hooks, _ := m["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	bin := SelfPath()
	for _, he := range a.events {
		list, _ := hooks[he.event].([]any)
		if a.flat {
			list = stripOursFlat(list)
		} else {
			list = stripOurs(list)
		}
		if !uninstall {
			h := map[string]any{"type": "command", "command": HookCommandFor(bin, he.arg, a.name), "timeout": a.timeout}
			if a.flat {
				delete(h, "type")
				list = append(list, h)
			} else {
				g := map[string]any{"hooks": []any{h}}
				if he.matcher != "" {
					g["matcher"] = he.matcher
				}
				list = append(list, g)
			}
		}
		if len(list) == 0 {
			delete(hooks, he.event)
		} else {
			hooks[he.event] = list
		}
	}
	if len(hooks) == 0 {
		delete(m, "hooks")
	} else {
		m["hooks"] = hooks
	}
	if a.flat {
		if _, ok := m["version"]; !ok && len(hooks) > 0 {
			m["version"] = 1
		}
	}
	if err := backup(path, raw); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	return store.WriteAtomic(path, append(b, '\n'), 0o600)
}

// AgentHooksInstalled reports whether AgentRM's hooks are in an agent's config.
func AgentHooksInstalled(name string) bool {
	a, ok := findAgent(name)
	if !ok {
		return false
	}
	raw, err := os.ReadFile(a.path())
	return err == nil && strings.Contains(string(raw), "--agent "+name)
}

// PreviewAgentHooks says what SetupAgentHooks would change.
func PreviewAgentHooks(name string) string {
	a, ok := findAgent(name)
	if !ok {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Add four hooks to %s for %s (a backup is kept; your other settings stay as they are):\n", a.path(), a.title)
	for _, he := range a.events {
		fmt.Fprintf(&b, "  %-21s %s\n", he.event, HookCommandFor(SelfPath(), he.arg, a.name))
	}
	if name == "codex" {
		b.WriteString("Codex asks you to trust new hooks: run /hooks in Codex once and approve them.\n")
	}
	return b.String()
}
