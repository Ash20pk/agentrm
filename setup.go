package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// selfPath is the absolute, symlink-resolved path of this binary. Hooks call it directly.
func selfPath() string {
	p, err := os.Executable()
	if err != nil {
		return "agentrm"
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

func home(parts ...string) string {
	h, _ := os.UserHomeDir()
	return filepath.Join(append([]string{h}, parts...)...)
}

func claudeSettingsPath() string { return home(".claude", "settings.json") }
func codexAgentsPath() string    { return home(".codex", "AGENTS.md") }
func cliLinkPath() string        { return home(".local", "bin", "agentrm") }

func isOurHook(h any) bool {
	m, ok := h.(map[string]any)
	if !ok {
		return false
	}
	c, _ := m["command"].(string)
	return strings.Contains(c, "agentrm") && strings.Contains(c, " hook ")
}

// stripOurs removes agentrm hook entries from one event's matcher list.
func stripOurs(list []any) []any {
	var out []any
	for _, g := range list {
		gm, ok := g.(map[string]any)
		if !ok {
			out = append(out, g)
			continue
		}
		hs, _ := gm["hooks"].([]any)
		var keep []any
		for _, h := range hs {
			if !isOurHook(h) {
				keep = append(keep, h)
			}
		}
		if len(keep) == 0 && len(hs) > 0 {
			continue
		}
		gm["hooks"] = keep
		out = append(out, gm)
	}
	return out
}

func readSettings() (map[string]any, []byte, error) {
	raw, err := os.ReadFile(claudeSettingsPath())
	if os.IsNotExist(err) {
		return map[string]any{}, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	m := map[string]any{}
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, nil, fmt.Errorf("%s is not valid JSON, not touching it: %v", claudeSettingsPath(), err)
		}
	}
	return m, raw, nil
}

func writeSettings(m map[string]any, original []byte) error {
	if original != nil {
		bak := claudeSettingsPath() + ".agentrm-backup-" + time.Now().Format("20060102-150405.000000")
		if err := os.WriteFile(bak, original, 0o600); err != nil {
			return err
		}
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	_ = os.MkdirAll(filepath.Dir(claudeSettingsPath()), 0o755)
	tmp := claudeSettingsPath() + ".agentrm-tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, claudeSettingsPath())
}

var hookEvents = []struct{ event, matcher, arg string }{
	{"PreToolUse", "Bash", "pre"},
	{"PostToolUse", "*", "post"},
	{"UserPromptSubmit", "", "prompt"},
	{"SessionEnd", "", "end"},
}

func setupClaude(uninstall bool) error {
	m, raw, err := readSettings()
	if err != nil {
		return err
	}
	hooks, _ := m["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	bin := selfPath()
	for _, he := range hookEvents {
		list, _ := hooks[he.event].([]any)
		list = stripOurs(list)
		if !uninstall {
			g := map[string]any{"hooks": []any{map[string]any{
				"type": "command", "command": fmt.Sprintf("%q hook %s", bin, he.arg), "timeout": 10,
			}}}
			if he.matcher != "" {
				g["matcher"] = he.matcher
			}
			list = append(list, g)
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
	return writeSettings(m, raw)
}

func claudeHooksInstalled() (bool, string) {
	m, _, err := readSettings()
	if err != nil {
		return false, ""
	}
	hooks, _ := m["hooks"].(map[string]any)
	list, _ := hooks["PreToolUse"].([]any)
	for _, g := range list {
		gm, _ := g.(map[string]any)
		hs, _ := gm["hooks"].([]any)
		for _, h := range hs {
			if isOurHook(h) {
				c, _ := h.(map[string]any)["command"].(string)
				return true, c
			}
		}
	}
	return false, ""
}

const codexStart, codexEnd = "<!-- agentrm:start -->", "<!-- agentrm:end -->"

const codexNote = `## Shared machine resources (agentrm)

This Mac runs agentrm, a resource manager shared by every coding agent on it.

- Before starting anything heavy (a kind/minikube cluster, docker compose up, docker build,
  cargo build/test, Gradle, xcodebuild, a simulator, a load test or a dev server), run
  ` + "`agentrm acquire --agent codex --session codex-<task> --kind <kind> --purpose \"<why>\"`" + `.
  Kinds: cluster compose docker-build cargo-build gradle xcode-build simulator loadtest server.
- If it is refused, do not start the job. Tell the user what agentrm said.
- When the job is done, run ` + "`agentrm release <lease id>`" + `.
- At the start of each task run ` + "`agentrm inbox --session codex-<task>`" + ` and answer every question:
  ` + "`agentrm claim <resource> --purpose \"<why>\"`" + ` for what you use, ` + "`agentrm disown <resource>`" + ` for what you don't.
`

func setupCodex(uninstall bool) error {
	p := codexAgentsPath()
	raw, err := os.ReadFile(p)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	s := string(raw)
	if i := strings.Index(s, codexStart); i >= 0 {
		if j := strings.Index(s, codexEnd); j > i {
			s = strings.TrimRight(s[:i], "\n") + s[j+len(codexEnd):]
		}
	}
	s = strings.TrimRight(s, "\n")
	if !uninstall {
		if s != "" {
			s += "\n\n"
		}
		s += codexStart + "\n" + codexNote + codexEnd
	}
	if s != "" {
		s += "\n"
	}
	if raw != nil {
		_ = os.WriteFile(p+".agentrm-backup-"+time.Now().Format("20060102-150405.000000"), raw, 0o644)
	}
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	return os.WriteFile(p, []byte(s), 0o644)
}

func codexNoteInstalled() bool {
	raw, err := os.ReadFile(codexAgentsPath())
	return err == nil && strings.Contains(string(raw), codexStart)
}

func setupPath(uninstall bool) error {
	link := cliLinkPath()
	if fi, err := os.Lstat(link); err == nil {
		if fi.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("%s exists and is not a symlink; not replacing it", link)
		}
		if err := os.Remove(link); err != nil {
			return err
		}
	}
	if uninstall {
		return nil
	}
	_ = os.MkdirAll(filepath.Dir(link), 0o755)
	return os.Symlink(selfPath(), link)
}

func cliOnPath() string {
	if t, err := os.Readlink(cliLinkPath()); err == nil {
		return t
	}
	return ""
}

func setupCheck() map[string]any {
	installed, cmd := claudeHooksInstalled()
	bin := selfPath()
	running := call("/status", map[string]any{}, nil) == nil
	inPath := strings.Contains(":"+os.Getenv("PATH")+":", ":"+filepath.Dir(cliLinkPath())+":")
	return map[string]any{
		"binary":               bin,
		"claude_hooks":         installed,
		"claude_hooks_current": installed && strings.Contains(cmd, bin),
		"codex_note":           codexNoteInstalled(),
		"cli_link":             cliOnPath(),
		"cli_link_dir_on_path": inPath,
		"daemon_running":       running,
	}
}

func runSetup(what string, uninstall bool) error {
	switch what {
	case "claude":
		return setupClaude(uninstall)
	case "codex":
		return setupCodex(uninstall)
	case "path":
		return setupPath(uninstall)
	case "check":
		b, _ := json.MarshalIndent(setupCheck(), "", "  ")
		fmt.Println(string(b))
		return nil
	}
	return fmt.Errorf("setup what? claude | codex | path | check")
}
