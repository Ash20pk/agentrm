// Package setup installs, checks and removes AgentRM's hooks into other tools: the Claude Code
// hooks, the Codex note, the command-line link and the PATH shim.
package setup

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"agentrm/internal/api"
	"agentrm/internal/store"
)

// SelfPath is the absolute, symlink-resolved path of this binary. Hooks call it directly.
func SelfPath() string {
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

func ClaudeSettingsPath() string { return home(".claude", "settings.json") }
func CodexAgentsPath() string    { return home(".codex", "AGENTS.md") }
func CLILinkPath() string        { return home(".local", "bin", "agentrm") }
func ShimDir() string            { return filepath.Join(store.Dir(), "shims") }

// ---- Claude Code hooks ----

// Our hook commands end in "hook <event>" (protocol 1) or "hook <event> --proto N". Matching
// on that shape, not on the binary's name, finds them wherever AgentRM is installed.
var hookRe = regexp.MustCompile(`\shook\s+(pre|post|prompt|end)(\s+--proto\s+\d+)?(\s+--agent\s+\w+)?\s*$`)

func isOurHook(h any) bool {
	m, ok := h.(map[string]any)
	if !ok {
		return false
	}
	c, _ := m["command"].(string)
	return hookRe.MatchString(c) && (strings.Contains(c, "--proto") || strings.Contains(c, "agentrm"))
}

// stripOurs removes AgentRM hook entries from one event's matcher list.
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
	raw, err := os.ReadFile(ClaudeSettingsPath())
	if os.IsNotExist(err) {
		return map[string]any{}, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	m := map[string]any{}
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, nil, fmt.Errorf("%s is not valid JSON, not touching it: %v", ClaudeSettingsPath(), err)
		}
	}
	return m, raw, nil
}

func backup(path string, b []byte) error {
	if b == nil {
		return nil
	}
	return os.WriteFile(path+".agentrm-backup-"+time.Now().Format("20060102-150405.000000"), b, 0o600)
}

var hookEvents = []hookEvent{
	{"PreToolUse", "Bash", "pre"},
	{"PostToolUse", "*", "post"},
	{"UserPromptSubmit", "", "prompt"},
	{"SessionEnd", "", "end"},
}

// HookCommand is the command written for a hook event.
func HookCommand(bin, arg string) string {
	return fmt.Sprintf("%q hook %s --proto %d", bin, arg, api.HookProto)
}

// PreviewClaude describes what installing (or removing) the hooks would change, without
// changing anything: the first-run screen shows it before you agree.
func PreviewClaude(uninstall bool) (string, error) {
	if _, _, err := readSettings(); err != nil {
		return "", err
	}
	bin := SelfPath()
	var b strings.Builder
	if uninstall {
		fmt.Fprintf(&b, "Remove AgentRM's four hooks from %s. Your other settings and hooks stay as they are.\n", ClaudeSettingsPath())
		return b.String(), nil
	}
	fmt.Fprintf(&b, "Add four hooks to %s (a backup is kept; your other settings and hooks stay as they are):\n", ClaudeSettingsPath())
	for _, he := range hookEvents {
		m := he.matcher
		if m == "" {
			m = "(all)"
		}
		fmt.Fprintf(&b, "  %-17s %-6s %s\n", he.event, m, HookCommand(bin, he.arg))
	}
	return b.String(), nil
}

func SetupClaude(uninstall bool) error {
	m, raw, err := readSettings()
	if err != nil {
		return err
	}
	hooks, _ := m["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	bin := SelfPath()
	for _, he := range hookEvents {
		list, _ := hooks[he.event].([]any)
		list = stripOurs(list)
		if !uninstall {
			g := map[string]any{"hooks": []any{map[string]any{
				"type": "command", "command": HookCommand(bin, he.arg), "timeout": 10,
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
	if err := backup(ClaudeSettingsPath(), raw); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	_ = os.MkdirAll(filepath.Dir(ClaudeSettingsPath()), 0o755)
	if err := store.WriteAtomic(ClaudeSettingsPath(), append(b, '\n'), 0o600); err != nil {
		return err
	}
	if uninstall {
		// Tell the daemon this was on purpose, so it does not report tampering.
		_ = os.WriteFile(filepath.Join(store.Dir(), "hooks-removed"), []byte(time.Now().Format(time.RFC3339)), 0o644)
	}
	return nil
}

// RemovedOnPurpose reports whether the hooks were removed through AgentRM within d.
func RemovedOnPurpose(d time.Duration) bool {
	fi, err := os.Stat(filepath.Join(store.Dir(), "hooks-removed"))
	return err == nil && time.Since(fi.ModTime()) < d
}

// ClaudeHooksInstalled reports whether AgentRM's PreToolUse hook is in the settings, and its
// command.
func ClaudeHooksInstalled() (bool, string) {
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

var protoRe = regexp.MustCompile(`--proto\s+(\d+)`)
var quotedRe = regexp.MustCompile(`^"([^"]+)"`)

// hookParts splits an installed hook command into the binary it runs and its protocol.
func hookParts(cmd string) (bin string, proto int) {
	if m := quotedRe.FindStringSubmatch(cmd); m != nil {
		bin = m[1]
	} else if f := strings.Fields(cmd); len(f) > 0 {
		bin = f[0]
	}
	proto = 1
	if m := protoRe.FindStringSubmatch(cmd); m != nil {
		proto, _ = strconv.Atoi(m[1])
	}
	return bin, proto
}

// ---- Codex ----

const codexStart, codexEnd = "<!-- agentrm:start -->", "<!-- agentrm:end -->"

const codexNote = `## Shared machine resources (agentrm)

This Mac runs AgentRM, a resource manager shared by every coding agent on it. Heavy commands
you run may be checked by it (and refused when the machine is short of memory).

- Before starting anything heavy that AgentRM may not recognise (a script that starts a cluster
  or a stack, a long build), run ` + "`agentrm acquire --kind <kind> --purpose \"<why>\"`" + `.
  Kinds: cluster compose docker-build cargo-build gradle xcode-build simulator emulator vm model
  loadtest server browser. Release it with ` + "`agentrm release <lease id>`" + ` when done.
- If a command is refused by AgentRM, do not try to get around it. Tell the user what it said,
  or free something you hold. If it says you are queued, do not retry in a loop: run
  ` + "`agentrm acquire --kind <kind> --purpose \"<why>\" --wait`" + `, which returns when the lease is
  yours, then run the command again.
- At the start of each task run ` + "`agentrm inbox`" + ` and answer every question:
  ` + "`agentrm claim <resource> --purpose \"<why>\"`" + ` for what you use, ` + "`agentrm disown <resource>`" + ` for what you don't.
`

func SetupCodex(uninstall bool) error {
	p := CodexAgentsPath()
	raw, err := os.ReadFile(p)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	s := removeBlock(string(raw), codexStart, codexEnd)
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
		if err := backup(p, raw); err != nil {
			return err
		}
	}
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	return store.WriteAtomic(p, []byte(s), 0o644)
}

func removeBlock(s, start, end string) string {
	if i := strings.Index(s, start); i >= 0 {
		if j := strings.Index(s, end); j > i {
			s = strings.TrimRight(s[:i], "\n") + s[j+len(end):]
		}
	}
	return strings.TrimRight(s, "\n")
}

func CodexNoteInstalled() bool {
	raw, err := os.ReadFile(CodexAgentsPath())
	return err == nil && strings.Contains(string(raw), codexStart)
}

// ---- command-line link ----

func SetupPath(uninstall bool) error {
	link := CLILinkPath()
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
	return os.Symlink(SelfPath(), link)
}

func CLILink() string {
	if t, err := os.Readlink(CLILinkPath()); err == nil {
		return t
	}
	return ""
}

// ---- PATH shim, for agents without hooks (Codex, Gemini CLI, Aider, OpenCode, Cursor) ----

// ShimmedCommands are the programs the shim stands in front of.
var ShimmedCommands = []string{"docker", "docker-compose", "kind", "minikube", "k3d", "colima", "limactl",
	"supabase", "cargo", "gradle", "xcodebuild", "swift", "k6", "vegeta", "locust", "ollama", "emulator",
	"npm", "pnpm", "yarn", "bun", "npx", "go", "flutter", "playwright", "utmctl", "vagrant", "tart"}

const shimStart, shimEnd = "# >>> agentrm shim >>>", "# <<< agentrm shim <<<"

func shimRCFiles() []string { return []string{home(".zshenv"), home(".bash_profile")} }

func shimBlock() string {
	return shimStart + "\n" +
		"# Put AgentRM's shims first on PATH. They pass straight through for you and for agents with\n" +
		"# hooks; for other agents they ask AgentRM before a heavy command. Remove with: agentrm setup shim --uninstall\n" +
		fmt.Sprintf("case \":$PATH:\" in *\":%s:\"*) ;; *) export PATH=\"%s:$PATH\" ;; esac\n", ShimDir(), ShimDir()) +
		shimEnd
}

// SetupShim creates the shim links and puts the shim directory first on PATH for zsh (every
// shell, through ~/.zshenv) and bash login shells.
func SetupShim(uninstall bool) error {
	for _, rc := range shimRCFiles() {
		raw, err := os.ReadFile(rc)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if os.IsNotExist(err) && (uninstall || strings.HasSuffix(rc, ".bash_profile")) {
			continue // do not create a bash profile that did not exist: it would hide ~/.profile
		}
		s := removeBlock(string(raw), shimStart, shimEnd)
		if !uninstall {
			if s != "" {
				s += "\n\n"
			}
			s += shimBlock()
		}
		if s != "" {
			s += "\n"
		}
		if raw != nil {
			if err := backup(rc, raw); err != nil {
				return err
			}
		}
		if err := store.WriteAtomic(rc, []byte(s), 0o644); err != nil {
			return err
		}
	}
	dir := ShimDir()
	if uninstall {
		return os.RemoveAll(dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	bin := SelfPath()
	for _, c := range ShimmedCommands {
		p := filepath.Join(dir, c)
		_ = os.Remove(p)
		if err := os.Symlink(bin, p); err != nil {
			return err
		}
	}
	return nil
}

func ShimInstalled() bool {
	fi, err := os.Lstat(filepath.Join(ShimDir(), "docker"))
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		return false
	}
	raw, _ := os.ReadFile(home(".zshenv"))
	return strings.Contains(string(raw), shimStart)
}

// ---- signature of the binary the hooks run ----

var teamRe = regexp.MustCompile(`(?m)^TeamIdentifier=(\S+)`)

// signatureTeam returns the signing team of a binary, or "" if it is not validly signed.
func signatureTeam(path string) string {
	if err := exec.Command("codesign", "--verify", "--strict", path).Run(); err != nil {
		return ""
	}
	out, _ := exec.Command("codesign", "-dv", path).CombinedOutput()
	if m := teamRe.FindSubmatch(out); m != nil && string(m[1]) != "not" {
		return string(m[1])
	}
	return ""
}

// Check reports what is installed, and whether the hooks run this very binary, signed by the
// same team, with the current protocol.
type Check struct {
	Binary             string   `json:"binary"`
	ClaudeHooks        bool     `json:"claude_hooks"`
	ClaudeHooksCurrent bool     `json:"claude_hooks_current"`
	HookBinary         string   `json:"hook_binary,omitempty"`
	HookProto          int      `json:"hook_proto,omitempty"`
	HookSigned         bool     `json:"hook_signed"`
	HookSameTeam       bool     `json:"hook_same_team"`
	CodexNote          bool     `json:"codex_note"`
	CLILink            string   `json:"cli_link"`
	CLILinkDirOnPath   bool     `json:"cli_link_dir_on_path"`
	CLILinkCurrent     bool     `json:"cli_link_current"`
	Shim               bool     `json:"shim"`
	DaemonRunning      bool     `json:"daemon_running"`
	Problems           []string `json:"problems"`
}

func RunCheck() Check {
	bin := SelfPath()
	c := Check{Binary: bin}
	installed, cmd := ClaudeHooksInstalled()
	c.ClaudeHooks = installed
	if installed {
		hb, proto := hookParts(cmd)
		if r, err := filepath.EvalSymlinks(hb); err == nil {
			hb = r
		}
		c.HookBinary, c.HookProto = hb, proto
		ourTeam := signatureTeam(bin)
		theirs := signatureTeam(hb)
		c.HookSigned = theirs != ""
		c.HookSameTeam = theirs != "" && theirs == ourTeam
		c.ClaudeHooksCurrent = hb == bin && proto == api.HookProto
		switch {
		case hb != bin && ourTeam != "" && !c.HookSameTeam:
			c.Problems = append(c.Problems, "The Claude Code hooks run "+hb+", which is not signed by AgentRM's developer. Repair them.")
		case hb != bin:
			c.Problems = append(c.Problems, "The Claude Code hooks point at "+hb+", not this copy of AgentRM. Repair them.")
		case proto != api.HookProto:
			c.Problems = append(c.Problems, "The Claude Code hooks are from an older version. Repair them.")
		}
		if _, err := os.Stat(hb); err != nil {
			c.Problems = append(c.Problems, "The Claude Code hooks point at "+hb+", which no longer exists: agents are not being checked.")
		}
	}
	c.CodexNote = CodexNoteInstalled()
	c.CLILink = CLILink()
	if c.CLILink != "" {
		if r, err := filepath.EvalSymlinks(c.CLILink); err == nil && r == bin {
			c.CLILinkCurrent = true
		} else {
			c.Problems = append(c.Problems, "~/.local/bin/agentrm points at "+c.CLILink+", not this copy. Repair it.")
		}
	}
	c.CLILinkDirOnPath = strings.Contains(":"+os.Getenv("PATH")+":", ":"+filepath.Dir(CLILinkPath())+":")
	c.Shim = ShimInstalled()
	c.DaemonRunning = api.Call("/whoami", api.Req{}, nil) == nil
	return c
}

// Repair re-points whatever is installed at this binary.
func Repair() error {
	c := RunCheck()
	if c.ClaudeHooks && !c.ClaudeHooksCurrent {
		if err := SetupClaude(false); err != nil {
			return err
		}
	}
	if c.CLILink != "" && !c.CLILinkCurrent {
		if err := SetupPath(false); err != nil {
			return err
		}
	}
	if c.Shim {
		if err := SetupShim(false); err != nil { // re-links the shims to this binary
			return err
		}
	}
	return nil
}

// Uninstall removes everything AgentRM installed into other tools, and thaws anything paused.
func Uninstall() []error {
	var errs []error
	if err := api.Call("/resume", api.Req{All: true}, nil); err != nil && err != api.ErrDaemonDown {
		errs = append(errs, fmt.Errorf("resume paused resources: %w", err))
	}
	if ok, _ := ClaudeHooksInstalled(); ok {
		if err := SetupClaude(true); err != nil {
			errs = append(errs, err)
		}
	}
	if CodexNoteInstalled() {
		if err := SetupCodex(true); err != nil {
			errs = append(errs, err)
		}
	}
	if CLILink() != "" {
		if err := SetupPath(true); err != nil {
			errs = append(errs, err)
		}
	}
	if ShimInstalled() {
		if err := SetupShim(true); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}
