package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

// Heavy commands and the lease kind each needs.
var classifiers = []struct {
	kind string
	re   *regexp.Regexp
}{
	{"cluster", regexp.MustCompile(`\bkind\s+create\s+cluster\b|\bminikube\s+start\b|\bk3d\s+cluster\s+create\b`)},
	{"compose", regexp.MustCompile(`\bdocker[- ]compose\b[^|;&]*\bup\b|\bsupabase\s+start\b`)},
	{"docker-build", regexp.MustCompile(`\bdocker\s+(buildx\s+)?build\b`)},
	{"cargo-build", regexp.MustCompile(`\bcargo\s+(build|test|clippy|run|nextest|bench|install)\b`)},
	{"gradle", regexp.MustCompile(`\bgradlew\b|(^|[\s;&|])gradle\s+\w`)},
	{"xcode-build", regexp.MustCompile(`\bxcodebuild\b`)},
	{"simulator", regexp.MustCompile(`\bsimctl\s+boot\b|open\s+-a\s+Simulator\b|\bflutter\s+run\b|\bexpo\s+run:ios\b|react-native\s+run-ios\b`)},
	{"loadtest", regexp.MustCompile(`\bk6\s+run\b|\bvegeta\s+attack\b|\blocust\b|(^|[\s;&|])hey\s+-`)},
	{"server", regexp.MustCompile(`\b(npm|pnpm|yarn|bun)\s+(run\s+)?(dev|start|start:dev|serve)\b|\bnext\s+dev\b|\bvite(\s+dev)?\s*$|\buvicorn\s|\bflask\s+run\b|\brails\s+s(erver)?\b|\bgo\s+run\b`)},
}

var extraClassifiers []struct {
	kind string
	re   *regexp.Regexp
}

// loadExtraPatterns reads extra_patterns from config.json so hooks see user additions.
func loadExtraPatterns() {
	b, err := os.ReadFile(homeDir() + "/config.json")
	if err != nil {
		return
	}
	var cfg Config
	if json.Unmarshal(b, &cfg) != nil {
		return
	}
	for kind, pats := range cfg.ExtraPatterns {
		for _, p := range pats {
			if re, err := regexp.Compile(p); err == nil {
				extraClassifiers = append(extraClassifiers, struct {
					kind string
					re   *regexp.Regexp
				}{kind, re})
			}
		}
	}
}

func classify(cmd string) string {
	for _, c := range extraClassifiers {
		if c.re.MatchString(cmd) {
			return c.kind
		}
	}
	for _, c := range classifiers {
		if c.re.MatchString(cmd) {
			return c.kind
		}
	}
	return ""
}

type hookInput struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	ToolName  string `json:"tool_name"`
	ToolInput struct {
		Command         string `json:"command"`
		RunInBackground bool   `json:"run_in_background"`
	} `json:"tool_input"`
}

func emit(v any) {
	b, _ := json.Marshal(v)
	fmt.Println(string(b))
}

func addContext(event, text string) {
	emit(map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": event, "additionalContext": text}})
}

func inboxText(session, cwd string) string {
	var resp struct{ Questions []string }
	if call("/inbox", map[string]any{"session": session, "agent": "claude", "cwd": cwd}, &resp) != nil {
		return ""
	}
	return strings.Join(resp.Questions, "\n")
}

// runHook handles Claude Code hook events. It always fails open: if the daemon is
// down or anything goes wrong, the tool call proceeds as if agentrm were not there.
func runHook(event string) {
	defer func() { _ = recover() }()
	raw, _ := io.ReadAll(os.Stdin)
	var in hookInput
	if json.Unmarshal(raw, &in) != nil || in.SessionID == "" {
		return
	}
	loadExtraPatterns()
	base := map[string]any{"session": in.SessionID, "agent": "claude", "cwd": in.Cwd}

	switch event {
	case "pre":
		if in.ToolName != "Bash" {
			return
		}
		kind := classify(in.ToolInput.Command)
		if kind == "" {
			return
		}
		req := map[string]any{"kind": kind, "cmd": in.ToolInput.Command,
			"purpose": "auto: " + trunc(strings.TrimSpace(in.ToolInput.Command), 80)}
		for k, v := range base {
			req[k] = v
		}
		var resp struct {
			Granted bool
			Reason  string
		}
		if call("/acquire", req, &resp) != nil || resp.Granted {
			return
		}
		emit(map[string]any{"hookSpecificOutput": map[string]any{
			"hookEventName":      "PreToolUse",
			"permissionDecision": "deny",
			"permissionDecisionReason": "agentrm refused a " + kind + " lease: " + resp.Reason +
				". Free something you hold (agentrm release <lease>), or wait and retry. " +
				"`agentrm status` shows who holds what. Do not try to get around this check.",
		}})

	case "post":
		_ = call("/renew", base, nil)
		if in.ToolName == "Bash" && !in.ToolInput.RunInBackground {
			if kind := classify(in.ToolInput.Command); buildKinds[kind] {
				_ = call("/release", map[string]any{"session": in.SessionID, "kind": kind}, nil)
			}
		}
		if q := inboxText(in.SessionID, in.Cwd); q != "" {
			addContext("PostToolUse", q)
		}

	case "prompt":
		_ = call("/renew", base, nil)
		if q := inboxText(in.SessionID, in.Cwd); q != "" {
			addContext("UserPromptSubmit", q)
		}

	case "end":
		_ = call("/release", map[string]any{"session": in.SessionID}, nil)
	}
}
