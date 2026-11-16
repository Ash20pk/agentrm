// Package hook is the Claude Code hook entry point. It always fails open: if the daemon is
// down, a different version, slow, or anything goes wrong, the tool call proceeds as if
// AgentRM were not there.
package hook

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"agentrm/internal/api"
)

type input struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	ToolName  string `json:"tool_name"`
	ToolInput struct {
		Command         string `json:"command"`
		RunInBackground bool   `json:"run_in_background"`
	} `json:"tool_input"`
}

// The hook is on the path of every tool call: keep each daemon call short.
const budget = 3 * time.Second

func emit(v any) {
	b, _ := json.Marshal(v)
	fmt.Println(string(b))
}

func addContext(event, text string) {
	emit(map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": event, "additionalContext": text}})
}

func inbox(base api.Req) string {
	var resp api.InboxResp
	if api.CallTimeout(budget, "/inbox", base, &resp) != nil {
		return ""
	}
	return strings.Join(resp.Questions, "\n")
}

// Run handles one hook event read from stdin.
func Run(event string) {
	defer func() { _ = recover() }()
	raw, _ := io.ReadAll(io.LimitReader(os.Stdin, 4<<20))
	var in input
	if json.Unmarshal(raw, &in) != nil || in.SessionID == "" {
		return
	}
	base := api.Req{Session: in.SessionID, Agent: "claude", Cwd: in.Cwd, Via: "hook"}

	switch event {
	case "pre":
		if in.ToolName != "Bash" || in.ToolInput.Command == "" {
			return
		}
		req := base
		req.Cmd, req.Gate = in.ToolInput.Command, true
		var resp api.TouchResp
		if api.CallTimeout(budget, "/touch", req, &resp) != nil || resp.Acquire == nil || resp.Acquire.Granted {
			return
		}
		emit(map[string]any{"hookSpecificOutput": map[string]any{
			"hookEventName":      "PreToolUse",
			"permissionDecision": "deny",
			"permissionDecisionReason": "agentrm refused a " + resp.Kind + " lease: " + resp.Acquire.Reason +
				". Free something you hold (agentrm release <lease>), or wait and retry. " +
				"`agentrm status` shows who holds what. Do not try to get around this check.",
		}})

	case "post":
		req := base
		if in.ToolName == "Bash" {
			req.Cmd, req.RunInBackground = in.ToolInput.Command, in.ToolInput.RunInBackground
		}
		_ = api.CallTimeout(budget, "/renew", req, nil)
		if q := inbox(base); q != "" {
			addContext("PostToolUse", q)
		}

	case "prompt":
		_ = api.CallTimeout(budget, "/renew", base, nil)
		if q := inbox(base); q != "" {
			addContext("UserPromptSubmit", q)
		}

	case "end":
		_ = api.CallTimeout(budget, "/release", base, nil)
	}
}
