//go:build live

// Checks against the real services, with keys that are not valid anywhere: each service must
// answer with its own "bad credentials" error, which shows the request reached it in the form
// it expects. Run with: go test -tags live -run Live -v ./internal/vault/
package vault

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"agentrm/internal/model"
)

func liveProxy(t *testing.T, upstream, header, prefix, key string, readOnly bool) (base, tok string) {
	t.Helper()
	u, _ := url.Parse(upstream)
	tok = NewProxyToken()
	srv := httptest.NewServer(NewProxy(func(got string) (Route, string, bool) {
		if got != tok {
			return Route{}, "unknown token", false
		}
		return Route{Upstream: u, Header: header, Prefix: prefix, Key: key, ReadOnly: readOnly}, "", true
	}))
	t.Cleanup(srv.Close)
	return srv.URL + u.Path, tok
}

func liveDo(t *testing.T, method, url string, h map[string]string, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	for k, v := range h {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// What the API received, echoed back: the key in its place, the token nowhere.
func TestLiveProxyEcho(t *testing.T) {
	base, tok := liveProxy(t, "https://httpbin.org/anything/v1", "Authorization", "Bearer ", "sk-livetest-key", false)
	code, body := liveDo(t, "POST", base+"/chat?x=1", map[string]string{
		"Authorization": "Bearer " + tok, "X-Api-Key": tok, "Content-Type": "application/json"}, `{"hi":1}`)
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	var echo struct {
		Headers map[string]string
		URL     string
		Data    string
	}
	_ = json.Unmarshal([]byte(body), &echo)
	t.Logf("upstream saw url=%s auth=%q", echo.URL, echo.Headers["Authorization"])
	if echo.Headers["Authorization"] != "Bearer sk-livetest-key" || strings.Contains(body, ProxyTokenPrefix) ||
		echo.Data != `{"hi":1}` || !strings.HasSuffix(echo.URL, "/anything/v1/chat?x=1") {
		t.Fatalf("upstream saw: %s", body)
	}
	if code, _ := liveDo(t, "GET", base+"/../../get", map[string]string{"Authorization": "Bearer " + tok}, ""); code == 200 {
		t.Fatal("a path outside the base got through")
	}
}

// OpenAI and Anthropic answer a key that is not theirs with their own 401: the request reached
// them with the key in the header they read.
func TestLiveProxyOpenAIAnthropic(t *testing.T) {
	base, tok := liveProxy(t, "https://api.openai.com/v1", "Authorization", "Bearer ", "sk-agentrmlivetest0000000000000000", false)
	code, body := liveDo(t, "GET", base+"/models", map[string]string{"Authorization": "Bearer " + tok}, "")
	t.Logf("openai: %d %s", code, body)
	if code != 401 || !strings.Contains(body, "invalid_api_key") || strings.Contains(body, ProxyTokenPrefix) {
		t.Fatalf("openai: %d %s", code, body)
	}

	base, tok = liveProxy(t, "https://api.anthropic.com", "x-api-key", "", "sk-ant-agentrmlivetest", false)
	code, body = liveDo(t, "POST", base+"/v1/messages", map[string]string{"x-api-key": tok,
		"anthropic-version": "2023-06-01", "content-type": "application/json"},
		`{"model":"claude-haiku-5-5","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`)
	t.Logf("anthropic: %d %s", code, body)
	if code != 401 || !strings.Contains(body, "authentication_error") {
		t.Fatalf("anthropic: %d %s", code, body)
	}

	// read-only refuses a POST before it leaves the machine.
	base, tok = liveProxy(t, "https://api.openai.com/v1", "Authorization", "Bearer ", "sk-x", true)
	if code, body := liveDo(t, "POST", base+"/chat/completions", map[string]string{"Authorization": "Bearer " + tok}, "{}"); code != 403 {
		t.Fatalf("read-only POST: %d %s", code, body)
	}
}

// GitHub decodes the app JWT we sign and looks the app up: there is no app 1, so it answers
// "Integration not found" (a malformed JWT gets "could not be decoded").
func TestLiveGitHubMint(t *testing.T) {
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	p := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
	a := model.VaultAccount{Name: "gh", Kind: model.VaultGitHub, AppID: "1", InstallationID: "1"}
	ask := model.VaultAsk{Account: "gh", Kind: model.VaultGitHub, Scope: "contents:read", Minutes: 10, Purpose: "live test"}
	_, err := Mint(context.Background(), a, Material{"private_key": string(p)}, ask, Opts{})
	t.Logf("github: %v", err)
	if err == nil || !strings.Contains(err.Error(), "Integration not found") {
		t.Fatalf("github: %v", err)
	}
}

// git uses the grant's token, not the one in the macOS Keychain: with a bad token, a push
// (dry run: nothing is sent) is refused. Reading a public repo needs no token, so it is a push.
func TestLiveGitUsesGrant(t *testing.T) {
	env := os.Environ()
	for k, v := range gitEnv("https://api.github.com", "ghs_agentrmlivetestnotatoken000000000000") {
		env = append(env, k+"="+v)
	}
	cmd := exec.Command("git", "push", "--dry-run", "https://github.com/Ash20pk/agentrm.git", "HEAD:refs/heads/agentrm-live-test-never")
	cmd.Env = append(env, "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	t.Logf("git: %v %s", err, out)
	if err == nil || !strings.Contains(string(out), "Invalid username or token") && !strings.Contains(string(out), "Authentication failed") {
		t.Fatal("git did not use the grant's token")
	}
}

// STS parses our SigV4 request and rejects the made-up key ID.
func TestLiveAWSMint(t *testing.T) {
	a := model.VaultAccount{Name: "aws", Kind: model.VaultAWS, Scopes: []string{"arn:aws:iam::123456789012:role/agentrm-test"}, Region: "us-east-1"}
	ask := model.VaultAsk{Account: "aws", Kind: model.VaultAWS, Scope: "agentrm-test", Minutes: 15, Purpose: "live test"}
	_, err := Mint(context.Background(), a, Material{"access_key_id": "AKIAAGENTRMLIVETEST0", "secret_access_key": "x"}, ask, Opts{Now: time.Now()})
	t.Logf("aws: %v", err)
	if err == nil || !strings.Contains(err.Error(), "InvalidClientTokenId") {
		t.Fatalf("aws: %v", err)
	}
}
