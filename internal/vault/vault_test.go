package vault

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"agentrm/internal/model"
)

// AWS's own worked example of Signature Version 4 (IAM ListUsers), from the documentation:
// the signature must come out exactly as published.
func TestSignV4MatchesAWSExample(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://iam.amazonaws.com/?Action=ListUsers&Version=2010-05-08", nil)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	at := time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
	signV4(req, nil, "AKIDEXAMPLE", "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", "us-east-1", "iam", at)
	want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/iam/aws4_request, " +
		"SignedHeaders=content-type;host;x-amz-date, Signature=5d672d79c15b13162d9279b0855cfba6789a8edb4c82c400e06b5924a6f2b5d7"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("signature differs from AWS's example:\n got %s\nwant %s", got, want)
	}
}

func testKey(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
	return k, string(p)
}

func TestGitHubMintAndRevoke(t *testing.T) {
	key, pemKey := testKey(t)
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	revoked := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/42/access_tokens":
			jwt := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			parts := strings.Split(jwt, ".")
			if len(parts) != 3 {
				t.Errorf("not a JWT: %q", jwt)
			}
			sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
			sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
			if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
				t.Errorf("JWT signature does not verify: %v", err)
			}
			claims, _ := base64.RawURLEncoding.DecodeString(parts[1])
			var c map[string]any
			_ = json.Unmarshal(claims, &c)
			if c["iss"] != "123" || int64(c["exp"].(float64)) > now.Add(10*time.Minute).Unix() {
				t.Errorf("claims: %v", c)
			}
			var body struct {
				Repositories []string          `json:"repositories"`
				Permissions  map[string]string `json:"permissions"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if strings.Join(body.Repositories, ",") != "agentrm" || body.Permissions["contents"] != "read" || body.Permissions["pull_requests"] != "write" {
				t.Errorf("asked for %+v", body)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"ghs_abc","expires_at":"2026-11-19T11:00:00Z"}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/installation/token":
			revoked = r.Header.Get("Authorization")
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	a := model.VaultAccount{Name: "gh", Kind: model.VaultGitHub, AppID: "123", InstallationID: "42", APIURL: srv.URL,
		Permissions: map[string]string{"contents": "write", "pull_requests": "write"}}
	ask := model.VaultAsk{Scope: "pull_requests:write, contents:read", Repos: []string{"Ash20pk/agentrm"}, Purpose: "open a PR", Session: "s1"}
	if err := Normalize(&a, &ask); err != nil {
		t.Fatal(err)
	}
	if ask.Scope != "contents:read,pull_requests:write" || ask.Minutes != DefaultMinutes {
		t.Fatalf("normalized to %+v", ask)
	}
	c, err := Mint(context.Background(), a, Material{"private_key": pemKey}, ask, Opts{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if c.Env["GH_TOKEN"] != "ghs_abc" || c.Token != "ghs_abc" || !c.Expires.Equal(now.Add(time.Hour)) {
		t.Fatalf("credential %+v", c)
	}
	if c.Env["GIT_CONFIG_VALUE_0"] != "" || c.Env["GIT_CONFIG_KEY_1"] != "credential.https://"+strings.TrimPrefix(srv.URL, "http://")+".helper" {
		t.Fatalf("git is not told to use the token: %v", c.Env)
	}
	if env := gitEnv("https://api.github.com", "t"); env["GIT_CONFIG_KEY_1"] != "credential.https://github.com.helper" || env["GH_HOST"] != "" {
		t.Fatalf("github.com: %v", env)
	}
	if err := Revoke(context.Background(), a, c, Opts{}); err != nil || revoked != "Bearer ghs_abc" {
		t.Fatalf("revoke: %v, %q", err, revoked)
	}
}

func TestGitHubAskLimits(t *testing.T) {
	a := model.VaultAccount{Name: "gh", Kind: model.VaultGitHub, AppID: "1", InstallationID: "2",
		Repos: []string{"org/a", "org/b"}, Permissions: map[string]string{"contents": "read"}}
	for _, bad := range []model.VaultAsk{
		{Scope: "contents:write", Purpose: "x"},
		{Scope: "issues:read", Purpose: "x"},
		{Scope: "contents:read", Repos: []string{"c"}, Purpose: "x"},
		{Scope: "contents:read"}, // no purpose
		{Scope: "contents", Purpose: "x"},
	} {
		if err := Normalize(&a, &bad); err == nil {
			t.Errorf("allowed %+v", bad)
		}
	}
	ask := model.VaultAsk{Purpose: "x", Minutes: 600}
	if err := Normalize(&a, &ask); err != nil {
		t.Fatal(err)
	}
	if ask.Scope != "contents:read" || strings.Join(ask.Repos, ",") != "a,b" || ask.Minutes != 60 {
		t.Fatalf("defaults: %+v", ask)
	}
}

func TestCovers(t *testing.T) {
	g := &model.VaultGrant{VaultAsk: model.VaultAsk{Account: "gh", Kind: model.VaultGitHub, Session: "s1",
		Scope: "contents:write,pull_requests:read", Repos: []string{"a", "b"}}}
	yes := []model.VaultAsk{
		{Scope: "contents:read", Repos: []string{"a"}},
		{Scope: "contents:write,pull_requests:read", Repos: []string{"a", "b"}},
	}
	no := []model.VaultAsk{
		{Scope: "pull_requests:write", Repos: []string{"a"}},
		{Scope: "contents:read", Repos: []string{"c"}},
		{Scope: "contents:read"}, // all repos
		{Scope: "contents:read", Repos: []string{"a"}, Reveal: true},
	}
	for _, a := range yes {
		a.Account, a.Kind, a.Session = "gh", model.VaultGitHub, "s1"
		if !Covers(g, &a) {
			t.Errorf("should cover %+v", a)
		}
	}
	for _, a := range no {
		a.Account, a.Kind, a.Session = "gh", model.VaultGitHub, "s1"
		if Covers(g, &a) {
			t.Errorf("should not cover %+v", a)
		}
	}
	other := model.VaultAsk{Account: "gh", Kind: model.VaultGitHub, Session: "s2", Scope: "contents:read", Repos: []string{"a"}}
	if Covers(g, &other) {
		t.Error("a grant covered another session")
	}
	k := &model.VaultGrant{VaultAsk: model.VaultAsk{Account: "k", Kind: model.VaultAPIKey, Session: "s1", Scope: "full"}}
	if !Covers(k, &model.VaultAsk{Account: "k", Kind: model.VaultAPIKey, Session: "s1", Scope: "read"}) {
		t.Error("full should cover read")
	}
	k.Scope = "read"
	if Covers(k, &model.VaultAsk{Account: "k", Kind: model.VaultAPIKey, Session: "s1", Scope: "full"}) {
		t.Error("read covered full")
	}
}

func TestAWSMint(t *testing.T) {
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(b))
		if form.Get("Action") != "AssumeRole" || form.Get("RoleArn") != "arn:aws:iam::111:role/deploy" ||
			form.Get("DurationSeconds") != "900" || form.Get("RoleSessionName") != "agentrm-s1" {
			t.Errorf("form %v", form)
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=AKID/20261119/eu-west-1/sts/aws4_request") {
			t.Errorf("auth %q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`<AssumeRoleResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><AssumeRoleResult>
<Credentials><AccessKeyId>ASIA1</AccessKeyId><SecretAccessKey>sec</SecretAccessKey><SessionToken>tok</SessionToken>
<Expiration>2026-11-19T10:15:00Z</Expiration></Credentials></AssumeRoleResult></AssumeRoleResponse>`))
	}))
	defer srv.Close()
	a := model.VaultAccount{Name: "aws", Kind: model.VaultAWS, Region: "eu-west-1", APIURL: srv.URL,
		Scopes: []string{"arn:aws:iam::111:role/deploy", "arn:aws:iam::111:role/read"}}
	ask := model.VaultAsk{Scope: "deploy", Minutes: 5, Purpose: "deploy", Session: "s1"}
	if err := Normalize(&a, &ask); err != nil {
		t.Fatal(err)
	}
	if ask.Minutes != 15 || ask.Scope != "arn:aws:iam::111:role/deploy" {
		t.Fatalf("normalized %+v", ask)
	}
	c, err := Mint(context.Background(), a, Material{"access_key_id": "AKID", "secret_access_key": "x"}, ask, Opts{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if c.Env["AWS_ACCESS_KEY_ID"] != "ASIA1" || c.Env["AWS_SESSION_TOKEN"] != "tok" || c.Env["AWS_REGION"] != "eu-west-1" || len(c.Unset) == 0 {
		t.Fatalf("credential %+v", c)
	}
}

func TestProxyPutsTheKeyOn(t *testing.T) {
	var gotAuth, gotPath, gotXAPI string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath, gotXAPI = r.Header.Get("Authorization"), r.URL.Path, r.Header.Get("X-Api-Key")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	tok := NewProxyToken()
	uses := 0
	readOnly := false
	p := httptest.NewServer(NewProxy(func(t string) (Route, string, bool) {
		if t != tok {
			return Route{}, "unknown token", false
		}
		uses++
		return Route{Upstream: u, Header: "Authorization", Prefix: "Bearer ", Key: "sk-real", ReadOnly: readOnly}, "", true
	}))
	defer p.Close()

	do := func(method, header, value string) int {
		req, _ := http.NewRequest(method, p.URL+"/v1/models", strings.NewReader("{}"))
		if header != "" {
			req.Header.Set(header, value)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := do("POST", "Authorization", "Bearer "+tok); code != 200 || gotAuth != "Bearer sk-real" || gotPath != "/v1/models" {
		t.Fatalf("bearer: %d %q %q", code, gotAuth, gotPath)
	}
	// Anthropic-style header: the token is removed and the key goes where the account says.
	if code := do("POST", "X-Api-Key", tok); code != 200 || gotXAPI != "" || gotAuth != "Bearer sk-real" {
		t.Fatalf("x-api-key: %d %q %q", code, gotXAPI, gotAuth)
	}
	basic := base64.StdEncoding.EncodeToString([]byte(tok + ":"))
	if code := do("GET", "Authorization", "Basic "+basic); code != 200 || gotAuth != "Bearer sk-real" {
		t.Fatalf("basic: %d %q", code, gotAuth)
	}
	if code := do("GET", "Authorization", "Bearer "+NewProxyToken()); code != 401 {
		t.Fatalf("an unknown token got %d", code)
	}
	if code := do("GET", "", ""); code != 401 {
		t.Fatalf("no token got %d", code)
	}
	// Two copies of the token: neither reaches the API.
	req, _ := http.NewRequest("GET", p.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("X-Api-Key", tok)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 200 || gotXAPI != "" {
		t.Fatalf("second copy: %v %q", err, gotXAPI)
	}
	readOnly = true
	if code := do("POST", "Authorization", "Bearer "+tok); code != 403 {
		t.Fatalf("a read-only grant could POST: %d", code)
	}
	if uses != 5 {
		t.Fatalf("uses %d", uses)
	}
}

func TestCheckAccount(t *testing.T) {
	bad := []model.VaultAccount{
		{Name: "Bad Name", Kind: model.VaultPassword},
		{Name: "gh", Kind: model.VaultGitHub},
		{Name: "k", Kind: model.VaultAPIKey, APIURL: "http://api.example.com"},
		{Name: "k", Kind: model.VaultAPIKey, APIURL: "https://x", Scopes: []string{"write"}},
		{Name: "aws", Kind: model.VaultAWS},
		{Name: "aws", Kind: model.VaultAWS, Scopes: []string{"deploy"}},
		{Name: "p", Kind: model.VaultPassword, PassEnv: "lower"},
		{Name: "x", Kind: "ssh"},
	}
	for _, a := range bad {
		if CheckAccount(&a) == nil {
			t.Errorf("accepted %+v", a)
		}
	}
	good := model.VaultAccount{Name: "openai", Kind: model.VaultAPIKey, APIURL: "https://api.openai.com/v1", KeyEnv: "OPENAI_API_KEY", BaseEnv: "OPENAI_BASE_URL"}
	if err := CheckAccount(&good); err != nil {
		t.Fatal(err)
	}
	c, err := Mint(context.Background(), good, Material{"key": "sk"}, model.VaultAsk{}, Opts{ProxyBase: "http://127.0.0.1:9"})
	if err != nil || c.Env["OPENAI_BASE_URL"] != "http://127.0.0.1:9/v1" || !strings.HasPrefix(c.Env["OPENAI_API_KEY"], ProxyTokenPrefix) || c.ProxyKey != "sk" {
		t.Fatalf("%v %+v", err, c)
	}
}

// A grant for an API under a path reaches only that path.
func TestProxyStaysUnderTheBasePath(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	u, _ := url.Parse(up.URL + "/v1")
	tok := NewProxyToken()
	p := httptest.NewServer(NewProxy(func(string) (Route, string, bool) {
		return Route{Upstream: u, Key: "k"}, "", true
	}))
	defer p.Close()
	for path, want := range map[string]int{"/v1/models": 200, "/v1": 200, "/admin/keys": 403, "/v10/x": 403,
		// The API would resolve these to /admin/keys.
		"/v1/../admin/keys": 400, "/v1/%2e%2e/admin/keys": 400, "/v1/x/../../admin": 400, "/v1/..%5cadmin": 400, "/v1/./models": 400} {
		req, _ := http.NewRequest("GET", p.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("%s: %d, want %d", path, resp.StatusCode, want)
		}
	}
}
