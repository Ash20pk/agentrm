package mcp

import (
	"bufio"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agentrm/internal/api"
	"agentrm/internal/model"
)

func TestProtocol(t *testing.T) {
	t.Setenv("AGENTRM_SOCK", "/nonexistent/agentrm.sock") // no daemon: tools must say so, not crash
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"agentrm_status","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"nope"}`,
		`not json`,
	}, "\n") + "\n"
	var out strings.Builder
	if err := Serve(strings.NewReader(in), &out, "test"); err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(strings.NewReader(out.String()))
	var resps []map[string]any
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("bad output line %q", sc.Text())
		}
		resps = append(resps, m)
	}
	if len(resps) != 5 {
		t.Fatalf("want 5 responses (the notification gets none), got %d: %s", len(resps), out.String())
	}
	if v := resps[0]["result"].(map[string]any)["protocolVersion"]; v != "2025-06-18" {
		t.Errorf("protocol version not echoed: %v", v)
	}
	if n := len(resps[1]["result"].(map[string]any)["tools"].([]any)); n != len(tools) {
		t.Errorf("tools/list returned %d tools", n)
	}
	call := resps[2]["result"].(map[string]any)
	if call["isError"] != true || !strings.Contains(call["content"].([]any)[0].(map[string]any)["text"].(string), "not running") {
		t.Errorf("a tool call with no daemon should report it: %v", call)
	}
	if resps[3]["error"] == nil || resps[4]["error"] == nil {
		t.Error("unknown method and bad JSON should be errors")
	}
}

// fakeDaemon serves the vault's paths on a socket: one API-key account and one login; the
// first request needs approval, which is given on the second look.
func fakeDaemon(t *testing.T) (asked *[]api.Req) {
	t.Helper()
	dir, err := os.MkdirTemp("", "mcp") // short: a socket path has a 104-byte limit
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTRM_SOCK", sock)
	var mu sync.Mutex
	var reqs []api.Req
	looks := 0
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req api.Req
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/vault/list":
			_ = json.NewEncoder(w).Encode(api.VaultListResp{Accounts: []*model.VaultAccount{
				{Name: "openai", Kind: model.VaultAPIKey, APIURL: "https://api.openai.com/v1"},
				{Name: "staging", Kind: model.VaultPassword}}})
		case "/vault/request":
			reqs = append(reqs, req)
			if len(reqs) == 1 {
				w.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(w).Encode(api.Error{Error: "needs approval", ApprovalID: "apr-1"})
				return
			}
			_ = json.NewEncoder(w).Encode(api.VaultResp{
				Grant: &model.VaultGrant{ID: "vg-1", VaultAsk: model.VaultAsk{Account: req.Account, Scope: "full"}, Expires: time.Now().Add(30 * time.Minute)},
				Env:   map[string]string{"OPENAI_API_KEY": "agentrm_tok", "OPENAI_BASE_URL": "http://127.0.0.1:9/v1"}})
		case "/approval":
			looks++
			_ = json.NewEncoder(w).Encode(api.ApprovalResp{Approval: &model.Approval{ID: "apr-1", State: "approved"}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return &reqs
}

func TestVaultRequestWaitsForApproval(t *testing.T) {
	reqs := fakeDaemon(t)
	text, isErr := vaultRequest(map[string]any{"account": "openai", "purpose": "list models", "repos": "a, b"}, 10*time.Second)
	if isErr || !strings.Contains(text, "OPENAI_BASE_URL=http://127.0.0.1:9/v1") || !strings.Contains(text, "not the real key") {
		t.Fatalf("%v %s", isErr, text)
	}
	if len(*reqs) != 2 || (*reqs)[1].Purpose != "list models" || len((*reqs)[1].Repos) != 2 {
		t.Fatalf("asked %+v", *reqs)
	}
}

// A login is not handed over in the transcript unless reveal was asked for: no grant is made.
func TestVaultRequestKeepsSecretsOutOfTheTranscript(t *testing.T) {
	reqs := fakeDaemon(t)
	text, isErr := vaultRequest(map[string]any{"account": "staging", "purpose": "check the dashboard"}, time.Second)
	if !isErr || !strings.Contains(text, "agentrm vault run staging") || len(*reqs) != 0 {
		t.Fatalf("%v %s, asked %d", isErr, text, len(*reqs))
	}
}
