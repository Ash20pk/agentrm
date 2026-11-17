package hook

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentrm/internal/api"
)

// fakeDaemon answers on a unix socket: /touch refuses when refuse is set, /inbox has a question.
func fakeDaemon(t *testing.T, refuse bool) *[]api.Req {
	t.Helper()
	sock := filepath.Join("/tmp", "agentrm-hook-test-"+strings.ReplaceAll(t.Name(), "/", "_")+".sock")
	os.Remove(sock)
	t.Setenv("AGENTRM_SOCK", sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var got []api.Req
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req api.Req
		_ = json.NewDecoder(r.Body).Decode(&req)
		req.Cwd = r.URL.Path + " " + req.Cwd // remember which endpoint
		got = append(got, req)
		switch r.URL.Path {
		case "/touch":
			resp := api.TouchResp{Kind: "cluster", Acquire: &api.AcquireResp{Granted: !refuse, Reason: "not enough memory"}}
			_ = json.NewEncoder(w).Encode(resp)
		case "/inbox":
			_ = json.NewEncoder(w).Encode(api.InboxResp{Questions: []string{"[agentrm] still need srv:3000?"}})
		default:
			_, _ = w.Write([]byte("{}"))
		}
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close(); os.Remove(sock) })
	return &got
}

// run feeds stdin to the hook and returns what it printed.
func run(t *testing.T, event, agent, stdin string) string {
	t.Helper()
	inR, inW, _ := os.Pipe()
	outR, outW, _ := os.Pipe()
	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = inR, outW
	go func() { inW.Write([]byte(stdin)); inW.Close() }()
	Run(event, agent)
	outW.Close()
	os.Stdin, os.Stdout = oldIn, oldOut
	b, _ := io.ReadAll(outR)
	return strings.TrimSpace(string(b))
}

func TestEachAgentIsDeniedInItsOwnFormat(t *testing.T) {
	cases := []struct{ agent, stdin, want string }{
		{"claude", `{"session_id":"s","cwd":"/x","tool_name":"Bash","tool_input":{"command":"kind create cluster"}}`, `"permissionDecision":"deny"`},
		{"codex", `{"session_id":"s","cwd":"/x","tool_name":"Bash","tool_input":{"command":"kind create cluster"}}`, `"permissionDecision":"deny"`},
		{"gemini", `{"session_id":"s","cwd":"/x","tool_name":"run_shell_command","tool_input":{"command":"kind create cluster"}}`, `"decision":"deny"`},
		{"cursor", `{"conversation_id":"c","workspace_roots":["/x"],"command":"kind create cluster"}`, `"permission":"deny"`},
	}
	for _, c := range cases {
		t.Run(c.agent, func(t *testing.T) {
			got := fakeDaemon(t, true)
			out := run(t, "pre", c.agent, c.stdin)
			if !strings.Contains(out, c.want) || !strings.Contains(out, "not enough memory") {
				t.Fatalf("output %q", out)
			}
			if len(*got) == 0 || (*got)[0].Cmd != "kind create cluster" || (*got)[0].Agent != c.agent || (*got)[0].Session == "" {
				t.Fatalf("daemon was sent %+v", *got)
			}
		})
	}
}

func TestCursorIsAlwaysAnswered(t *testing.T) {
	fakeDaemon(t, false)
	if out := run(t, "pre", "cursor", `{"conversation_id":"c","command":"ls"}`); out != `{"continue":true,"permission":"allow"}` {
		t.Fatalf("an allowed command must still be answered for Cursor: %q", out)
	}
	t.Setenv("AGENTRM_SOCK", "/nonexistent.sock") // daemon down: fail open
	if out := run(t, "pre", "cursor", `{"conversation_id":"c","command":"kind create cluster"}`); !strings.Contains(out, `"allow"`) {
		t.Fatalf("with no daemon Cursor's command must be allowed: %q", out)
	}
	if out := run(t, "pre", "cursor", `not json`); !strings.Contains(out, `"allow"`) {
		t.Fatalf("bad input must fail open for Cursor: %q", out)
	}
	if out := run(t, "pre", "claude", `not json`); out != "" {
		t.Fatalf("Claude Code needs no answer to allow: %q", out)
	}
}

func TestQuestionsReachTheModel(t *testing.T) {
	for agent, want := range map[string]string{
		"claude": `"additionalContext"`, "codex": `"additionalContext"`, "gemini": `"hookEventName":"AfterTool"`, "cursor": `"additional_context"`,
	} {
		fakeDaemon(t, false)
		out := run(t, "post", agent, `{"session_id":"s","conversation_id":"s","tool_name":"Bash","tool_input":{"command":"ls"}}`)
		if !strings.Contains(out, want) || !strings.Contains(out, "still need srv:3000") {
			t.Fatalf("%s: %q", agent, out)
		}
	}
	fakeDaemon(t, false)
	if out := run(t, "prompt", "cursor", `{"conversation_id":"s"}`); out != `{"continue":true}` {
		t.Fatalf("Cursor's prompt hook: %q", out)
	}
}

func TestOtherToolsAreNotGated(t *testing.T) {
	got := fakeDaemon(t, true)
	if out := run(t, "pre", "gemini", `{"session_id":"s","tool_name":"write_file","tool_input":{"command":"kind create cluster"}}`); out != "" {
		t.Fatalf("a non-shell tool was gated: %q", out)
	}
	if len(*got) != 0 {
		t.Fatal("the daemon was asked about a non-shell tool")
	}
}
