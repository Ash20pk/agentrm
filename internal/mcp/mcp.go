// Package mcp serves AgentRM as an MCP server over stdio, so any agent that speaks MCP (Cursor,
// Gemini CLI, Claude, ...) can take and release leases and answer questions as tools. The
// daemon identifies the agent from the process tree: the MCP server runs under it.
package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"agentrm/internal/api"
)

type rpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func obj(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func num(desc string) map[string]any { return map[string]any{"type": "number", "description": desc} }

var tools = []tool{
	{"agentrm_acquire", "Ask AgentRM for a lease before starting something heavy (a cluster, a Compose stack, a build, a simulator or emulator, a VM, a local model, a load test, a dev server). If it is refused, do not start the job.",
		obj(map[string]any{
			"kind":          str("cluster | compose | docker-build | cargo-build | gradle | xcode-build | simulator | emulator | vm | model | loadtest | server | browser"),
			"purpose":       str("why you need it"),
			"mem_gb":        num("memory you expect to use, if you know better than AgentRM's estimate"),
			"until_minutes": num("how long you need it; the lease ends then unless you extend it"),
		}, "kind", "purpose")},
	{"agentrm_release", "Release a lease when the job is done (or all of yours if no lease_id).",
		obj(map[string]any{"lease_id": str("the lease to release")})},
	{"agentrm_claim", "Claim something already running that you are using, so it is not paused or stopped.",
		obj(map[string]any{"resource_id": str("e.g. kind:dev or srv:4242"), "purpose": str("why you need it")}, "resource_id", "purpose")},
	{"agentrm_yield", "Answer AgentRM's question about what giving up one of your leases would cost you right now. Answer honestly: AgentRM checks afterwards and weighs your later answers by it.",
		obj(map[string]any{
			"lease_id":        str("the lease"),
			"cost":            str("none (you are done with it: released now) | low (fine to stop; you can bring it back) | high (you are using it)"),
			"restart_seconds": num("with low: about how long bringing it back would take"),
		}, "lease_id", "cost")},
	{"agentrm_disown", "Say that something AgentRM asked about is not yours.",
		obj(map[string]any{"resource_id": str("the resource")}, "resource_id")},
	{"agentrm_inbox", "Questions AgentRM has for you. Answer each with agentrm_claim or agentrm_disown.", obj(map[string]any{})},
	{"agentrm_status", "Memory, what is running, and who holds what.", obj(map[string]any{})},
}

// Serve runs the MCP protocol on in/out until in is closed.
func Serve(in io.Reader, out io.Writer, version string) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64*1024), 8<<20)
	enc := json.NewEncoder(out)
	for sc.Scan() {
		var req rpcReq
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			_ = enc.Encode(rpcResp{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32700, "parse error"}})
			continue
		}
		if len(req.ID) == 0 {
			continue // a notification
		}
		resp := rpcResp{JSONRPC: "2.0", ID: req.ID}
		switch req.Method {
		case "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			_ = json.Unmarshal(req.Params, &p)
			if p.ProtocolVersion == "" {
				p.ProtocolVersion = "2025-06-18"
			}
			resp.Result = map[string]any{
				"protocolVersion": p.ProtocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "agentrm", "version": version},
				"instructions": "AgentRM shares this Mac's memory between coding agents. Call agentrm_acquire before " +
					"heavy work and agentrm_release after it; answer agentrm_inbox questions.",
			}
		case "ping":
			resp.Result = map[string]any{}
		case "tools/list":
			resp.Result = map[string]any{"tools": tools}
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			if err := json.Unmarshal(req.Params, &p); err != nil {
				resp.Error = &rpcError{-32602, "bad params"}
				break
			}
			text, isErr := call(p.Name, p.Arguments)
			resp.Result = map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": isErr}
		default:
			resp.Error = &rpcError{-32601, "method not found: " + req.Method}
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return sc.Err()
}

func s(a map[string]any, k string) string {
	v, _ := a[k].(string)
	return v
}

func n(a map[string]any, k string) float64 {
	v, _ := a[k].(float64)
	return v
}

func call(name string, a map[string]any) (string, bool) {
	req := api.Req{Via: "mcp"}
	switch name {
	case "agentrm_acquire":
		req.Kind, req.Purpose, req.MemGB, req.UntilMinutes = s(a, "kind"), s(a, "purpose"), n(a, "mem_gb"), int(n(a, "until_minutes"))
		var r api.AcquireResp
		if err := api.Call("/acquire", req, &r); err != nil {
			return err.Error(), true
		}
		if !r.Granted {
			return "Refused: " + r.Reason + " Do not start the job.", true
		}
		return fmt.Sprintf("Granted lease %s (%s, %.1f GB). Release it with agentrm_release when done.", r.Lease.ID, r.Lease.Kind, r.Lease.MemGB), false
	case "agentrm_yield":
		req.LeaseID, req.Cost, req.RestartSeconds = s(a, "lease_id"), s(a, "cost"), int(n(a, "restart_seconds"))
		var r api.CountResp
		if err := api.Call("/yield", req, &r); err != nil {
			return err.Error(), true
		}
		return "Noted: " + r.Note + ".", false
	case "agentrm_release":
		req.LeaseID = s(a, "lease_id")
		var r api.CountResp
		if err := api.Call("/release", req, &r); err != nil {
			return err.Error(), true
		}
		return fmt.Sprintf("Released %d lease(s).", r.N), false
	case "agentrm_claim":
		req.ResourceID, req.Purpose = s(a, "resource_id"), s(a, "purpose")
		var r api.ClaimResp
		if err := api.Call("/claim", req, &r); err != nil {
			return err.Error(), true
		}
		v := "verified"
		if !r.Verified {
			v = "unverified"
		}
		return fmt.Sprintf("Held by lease %s (%s: %s).", r.Lease.ID, v, r.Evidence), false
	case "agentrm_disown":
		req.ResourceID = s(a, "resource_id")
		if err := api.Call("/disown", req, nil); err != nil {
			return err.Error(), true
		}
		return "Noted: not yours.", false
	case "agentrm_inbox":
		req.All = true
		var r api.InboxResp
		if err := api.Call("/inbox", req, &r); err != nil {
			return err.Error(), true
		}
		if len(r.Questions) == 0 {
			return "No questions.", false
		}
		return strings.Join(r.Questions, "\n"), false
	case "agentrm_status":
		var st api.Status
		if err := api.Call("/status", req, &st); err != nil {
			return err.Error(), true
		}
		return Summary(&st), false
	}
	return "unknown tool " + name, true
}

// Summary is a short plain-text status for an agent.
func Summary(st *api.Status) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%.1f GB can be granted now (%.1f GB available, pressure %d). ", max(0, st.HeadroomMB)/1024, st.Mem.AvailableMB/1024, st.Mem.PressureLevel)
	var ls []string
	for _, l := range st.Leases {
		ls = append(ls, fmt.Sprintf("%s %s %.1f GB held by %s (used %s ago)", l.ID, l.Kind, l.MemGB, l.Session,
			time.Since(l.LastUsed).Round(time.Minute)))
	}
	sort.Strings(ls)
	fmt.Fprintf(&b, "Leases: %s.", strings.Join(append([]string{}, ls...), "; "))
	var un []string
	for _, r := range st.Resources {
		if r.LeaseID == "" {
			un = append(un, fmt.Sprintf("%s (%.1f GB)", r.ID, r.MemMB/1024))
		}
	}
	if len(un) > 0 {
		sort.Strings(un)
		fmt.Fprintf(&b, " Unclaimed: %s.", strings.Join(un, ", "))
	}
	return b.String()
}
