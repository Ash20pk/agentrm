package mcp

import (
	"bufio"
	"encoding/json"
	"strings"
	"testing"
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
