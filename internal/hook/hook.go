// Package hook is the agents' hook entry point: Claude Code, Codex, Gemini CLI and Cursor all
// run a command before and after a shell command, when a prompt is submitted and when a session
// ends; each with its own input and output format, translated here. It always fails open: if
// the daemon is down, a different version, slow, or anything goes wrong, the command proceeds
// as if AgentRM were not there.
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

// input is the union of what the agents send. Claude Code and Codex share a format; Gemini CLI
// is close to it; Cursor sends the command at the top level and names the session
// conversation_id.
type input struct {
	SessionID      string   `json:"session_id"`
	ConversationID string   `json:"conversation_id"` // Cursor
	Cwd            string   `json:"cwd"`
	WorkspaceRoots []string `json:"workspace_roots"` // Cursor
	Reason         string   `json:"reason"`          // session end
	ToolName       string   `json:"tool_name"`
	Command        string   `json:"command"` // Cursor's shell events
	ToolInput      struct {
		Command         string `json:"command"`
		RunInBackground bool   `json:"run_in_background"`
		IsBackground    bool   `json:"is_background"`
	} `json:"tool_input"`
}

func (in input) session() string {
	if in.SessionID != "" {
		return in.SessionID
	}
	return in.ConversationID // Cursor sometimes sends it empty: the daemon then goes by process
}

func (in input) cwd() string {
	if in.Cwd != "" {
		return in.Cwd
	}
	if len(in.WorkspaceRoots) > 0 {
		return in.WorkspaceRoots[0]
	}
	return ""
}

func (in input) command() string {
	if in.ToolInput.Command != "" {
		return in.ToolInput.Command
	}
	return in.Command
}

// shellTools are the agents' names for running a shell command.
var shellTools = map[string]bool{"Bash": true, "run_shell_command": true, "Shell": true, "shell": true}

// The hook is on the path of every tool call: keep each daemon call short.
const budget = 3 * time.Second

func emit(v any) {
	b, _ := json.Marshal(v)
	fmt.Println(string(b))
}

func inbox(base api.Req) string {
	var resp api.InboxResp
	if api.CallTimeout(budget, "/inbox", base, &resp) != nil {
		return ""
	}
	return strings.Join(resp.Questions, "\n")
}

// out writes answers in one agent's format.
type out struct {
	agent string
	said  bool
}

// deny refuses the command about to run.
func (o *out) deny(reason string) {
	o.said = true
	switch o.agent {
	case "gemini":
		emit(map[string]any{"decision": "deny", "reason": reason})
	case "cursor":
		emit(map[string]any{"continue": true, "permission": "deny", "user_message": reason, "agent_message": reason})
	default: // claude, codex
		emit(map[string]any{"hookSpecificOutput": map[string]any{
			"hookEventName": "PreToolUse", "permissionDecision": "deny", "permissionDecisionReason": reason}})
	}
}

// context adds text the model sees after a tool call or with a prompt.
func (o *out) context(event, text string) {
	if text == "" {
		return
	}
	o.said = true
	switch o.agent {
	case "cursor":
		if event == "prompt" {
			o.said = false // beforeSubmitPrompt carries nothing to the model; the next tool call will
			return
		}
		emit(map[string]any{"additional_context": text})
	case "gemini":
		name := map[string]string{"post": "AfterTool", "prompt": "BeforeAgent"}[event]
		emit(map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": name, "additionalContext": text}})
	default:
		name := map[string]string{"post": "PostToolUse", "prompt": "UserPromptSubmit"}[event]
		emit(map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": name, "additionalContext": text}})
	}
}

// done answers when nothing else was said. Cursor treats a permission hook that prints nothing
// valid as a refusal, so it is told "allow" explicitly.
func (o *out) done(event string) {
	if o.said || o.agent != "cursor" {
		return
	}
	switch event {
	case "pre":
		emit(map[string]any{"continue": true, "permission": "allow"})
	case "prompt":
		emit(map[string]any{"continue": true})
	default:
		fmt.Println("{}")
	}
}

// Run handles one hook event read from stdin, for agent ("" is Claude Code).
func Run(event, agent string) {
	if agent == "" {
		agent = "claude"
	}
	o := &out{agent: agent}
	defer func() {
		_ = recover()
		o.done(event) // fail open, in every agent's terms
	}()
	raw, _ := io.ReadAll(io.LimitReader(os.Stdin, 4<<20))
	var in input
	if json.Unmarshal(raw, &in) != nil {
		return
	}
	if in.session() == "" && agent == "claude" {
		return
	}
	base := api.Req{Session: in.session(), Agent: agent, Cwd: in.cwd(), Via: "hook"}

	switch event {
	case "pre":
		cmd := in.command()
		if cmd == "" || (in.ToolName != "" && !shellTools[in.ToolName]) {
			return
		}
		req := base
		req.Cmd, req.Gate = cmd, true
		var resp api.TouchResp
		if api.CallTimeout(budget, "/touch", req, &resp) != nil || resp.Acquire == nil || resp.Acquire.Granted {
			return
		}
		o.deny("agentrm refused a " + resp.Kind + " lease: " + resp.Acquire.Reason +
			". If you hold something you no longer need, free it (agentrm release <lease>). " +
			"`agentrm status` shows who holds what. Do not try to get around this check.")

	case "post":
		req := base
		if cmd := in.command(); cmd != "" && (in.ToolName == "" || shellTools[in.ToolName]) {
			req.Cmd, req.RunInBackground = cmd, in.ToolInput.RunInBackground || in.ToolInput.IsBackground
		}
		_ = api.CallTimeout(budget, "/renew", req, nil)
		o.context("post", inbox(base))

	case "prompt":
		_ = api.CallTimeout(budget, "/renew", base, nil)
		o.context("prompt", inbox(base))

	case "end":
		// /end tells the collector the session is over; a daemon from before it has only /release.
		req := base
		req.Reason = in.Reason
		if err := api.CallTimeout(budget, "/end", req, nil); err != nil {
			_ = api.CallTimeout(budget, "/release", base, nil)
		}
	}
}
