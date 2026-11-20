package manager

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"agentrm/internal/api"
	"agentrm/internal/model"
)

var appFacts = facts{pid: 4242, gui: true, viaApp: true, app: "AgentRM.app"}

func vaultTest(t *testing.T, accts ...model.VaultAccount) (*Manager, *clock) {
	t.Helper()
	m, c := newTest(t, 8)
	if _, _, err := m.vaultSetAccounts(you, api.Req{VaultAccounts: accts}); err != nil {
		t.Fatal(err)
	}
	return m, c
}

var loginAcct = model.VaultAccount{Name: "staging-admin", Kind: model.VaultPassword, Username: "ash", MaxMinutes: 60}

// ask makes a vault request and returns the approval it is waiting on, or the grant.
func ask(t *testing.T, m *Manager, c api.Caller, r api.Req) (*model.Approval, *api.VaultResp) {
	t.Helper()
	if r.Purpose == "" {
		r.Purpose = "log in to staging to check the fix"
	}
	out, _, err := m.vaultRequest(c, r)
	var ea *errApproval
	if errors.As(err, &ea) {
		return ea.a, nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return nil, out.(*api.VaultResp)
}

func answer(t *testing.T, m *Manager, a *model.Approval, approve bool, material map[string]string) error {
	t.Helper()
	_, err := m.vaultAnswer(context.Background(), appFacts, api.Req{ApprovalID: a.ID, Approve: &approve, Material: material})
	return err
}

func TestVaultAskApproveReuse(t *testing.T) {
	m, c := vaultTest(t, loginAcct)
	s1 := agent("s1")
	a, _ := ask(t, m, s1, api.Req{Account: "staging-admin"})
	if a == nil || a.Vault == nil || a.State != "pending" || !strings.Contains(a.What, "the login") {
		t.Fatalf("no approval asked: %+v", a)
	}
	// Asking again while it waits does not ask you twice.
	if again, _ := ask(t, m, s1, api.Req{Account: "staging-admin"}); again == nil || again.ID != a.ID {
		t.Fatal("a second approval for the same ask")
	}
	// Only the app answers, and the generic approve cannot (it has no secret).
	yes := true
	if _, err := m.vaultAnswer(context.Background(), facts{pid: 1, gui: true, app: "Warp.app"}, api.Req{ApprovalID: a.ID, Approve: &yes, Material: map[string]string{"password": "x"}}); err == nil {
		t.Fatal("a terminal answered a vault request")
	}
	if _, _, err := m.approve(you, api.Req{ApprovalID: a.ID, Approve: &yes}); err == nil {
		t.Fatal("the generic approve granted a vault request without its secret")
	}
	if err := answer(t, m, a, true, map[string]string{"password": "hunter2"}); err != nil {
		t.Fatal(err)
	}
	if a.State != "approved" {
		t.Fatalf("state %s", a.State)
	}
	_, got := ask(t, m, s1, api.Req{Account: "staging-admin"})
	if got == nil || got.Env["AGENTRM_PASSWORD"] != "hunter2" || got.Env["AGENTRM_USERNAME"] != "ash" {
		t.Fatalf("the same session asking again: %+v", got)
	}
	if got.Grant.Uses != 1 || got.Grant.Note == "" {
		t.Fatalf("grant record %+v", got.Grant)
	}
	// Another session is asked about separately.
	if other, _ := ask(t, m, agent("s2"), api.Req{Account: "staging-admin"}); other == nil {
		t.Fatal("another session got this session's grant")
	}
	// Showing the secret is a separate ask.
	if rev, _ := ask(t, m, s1, api.Req{Account: "staging-admin", Reveal: true}); rev == nil || !strings.Contains(rev.What, "shown to the agent") {
		t.Fatal("a grant to use covered a request to see")
	}
	// It expires.
	c.t = c.t.Add(31 * time.Minute)
	m.vaultTick()
	if g := m.st.VaultGrants[0]; g.Ended == nil || g.EndReason != "expired" || len(m.vaultLive) != 0 {
		t.Fatalf("not expired: %+v", g)
	}
	if a, _ := ask(t, m, s1, api.Req{Account: "staging-admin"}); a == nil {
		t.Fatal("an expired grant was handed out")
	}
}

// Nothing secret reaches the state file, events, approvals or the status.
func TestVaultSecretNeverStored(t *testing.T) {
	m, _ := vaultTest(t, loginAcct)
	a, _ := ask(t, m, agent("s1"), api.Req{Account: "staging-admin"})
	if err := answer(t, m, a, true, map[string]string{"password": "s3cr3t-value"}); err != nil {
		t.Fatal(err)
	}
	st, _ := json.Marshal(m.st)
	ev, _ := json.Marshal(m.events)
	status, _, _ := m.status(you, api.Req{})
	sj, _ := json.Marshal(status)
	for what, b := range map[string][]byte{"state": st, "events": ev, "status": sj} {
		if strings.Contains(string(b), "s3cr3t-value") {
			t.Fatalf("the secret is in the %s", what)
		}
	}
}

func TestVaultDenyQuiet(t *testing.T) {
	m, c := vaultTest(t, loginAcct)
	a, _ := ask(t, m, agent("s1"), api.Req{Account: "staging-admin"})
	if err := answer(t, m, a, false, nil); err != nil || a.State != "denied" {
		t.Fatalf("%v %s", err, a.State)
	}
	if _, _, err := m.vaultRequest(agent("s1"), api.Req{Account: "staging-admin", Purpose: "again"}); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("asked you again right after a denial: %v", err)
	}
	c.t = c.t.Add(10 * time.Minute)
	if a2, _ := ask(t, m, agent("s1"), api.Req{Account: "staging-admin"}); a2 == nil {
		t.Fatal("never allowed to ask again")
	}
}

func TestVaultRefusals(t *testing.T) {
	m, _ := vaultTest(t, loginAcct)
	if _, _, err := m.vaultRequest(api.Caller{Agent: "detached", Session: "detached"}, api.Req{Account: "staging-admin", Purpose: "x"}); err == nil {
		t.Fatal("a detached process asked the vault")
	}
	if _, _, err := m.vaultRequest(agent("s1"), api.Req{Account: "nope", Purpose: "x"}); err == nil || !strings.Contains(err.Error(), "staging-admin") {
		t.Fatalf("unknown account: %v", err)
	}
	if _, _, err := m.vaultRequest(agent("s1"), api.Req{Account: "staging-admin"}); err == nil {
		t.Fatal("asked without saying why")
	}
	if _, _, err := m.vaultSetAccounts(terminal, api.Req{}); err == nil {
		t.Fatal("accounts changed from a terminal")
	}
	if _, _, err := m.vaultSetAccounts(agent("s1"), api.Req{}); err == nil {
		t.Fatal("accounts changed by an agent")
	}
}

func grantFor(t *testing.T, m *Manager, c api.Caller, r api.Req, material map[string]string) *api.VaultResp {
	t.Helper()
	a, _ := ask(t, m, c, r)
	if a == nil {
		t.Fatal("no approval")
	}
	if err := answer(t, m, a, true, material); err != nil {
		t.Fatal(err)
	}
	_, g := ask(t, m, c, r)
	if g == nil {
		t.Fatal("not granted after approval")
	}
	return g
}

func TestVaultEndsWithSessionAndRevoke(t *testing.T) {
	m, c := vaultTest(t, loginAcct)
	g1 := grantFor(t, m, agent("s1"), api.Req{Account: "staging-admin"}, map[string]string{"password": "p"})
	g2 := grantFor(t, m, agent("s2"), api.Req{Account: "staging-admin"}, map[string]string{"password": "p"})
	// An agent cannot revoke another session's grant; you can.
	if _, _, err := m.vaultRevokeReq(agent("s2"), api.Req{GrantID: g1.Grant.ID}); err == nil {
		t.Fatal("an agent revoked another session's grant")
	}
	if _, _, err := m.vaultRevokeReq(terminal, api.Req{GrantID: g2.Grant.ID}); err != nil {
		t.Fatal(err)
	}
	if g := m.vaultGrant(g2.Grant.ID); g.Ended == nil || !strings.Contains(g.EndReason, "you") {
		t.Fatalf("not revoked: %+v", g)
	}
	m.st.Sessions["s1"] = &model.Session{ID: "s1", Agent: "claude"}
	m.endSession(m.st.Sessions["s1"], "exited", c.Now())
	if g := m.vaultGrant(g1.Grant.ID); g.Ended == nil || !strings.Contains(g.EndReason, "session ended") {
		t.Fatalf("outlived its session: %+v", g)
	}
	if len(m.vaultLive) != 0 {
		t.Fatal("a credential is still in memory")
	}
}

func TestVaultAccountChangeEndsGrants(t *testing.T) {
	m, _ := vaultTest(t, loginAcct)
	g := grantFor(t, m, agent("s1"), api.Req{Account: "staging-admin"}, map[string]string{"password": "p"})
	changed := loginAcct
	changed.Username = "someone-else"
	if _, _, err := m.vaultSetAccounts(you, api.Req{VaultAccounts: []model.VaultAccount{changed}}); err != nil {
		t.Fatal(err)
	}
	if m.vaultGrant(g.Grant.ID).Ended == nil {
		t.Fatal("a grant outlived a change to its account")
	}
}

// An API key: the agent gets a proxy token and the proxy's address; requests through it reach
// the API with the real key, and stop working when the grant ends.
func TestVaultAPIKeyThroughProxy(t *testing.T) {
	var gotKey string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	defer up.Close()
	acct := model.VaultAccount{Name: "openai", Kind: model.VaultAPIKey, APIURL: up.URL + "/v1",
		KeyEnv: "OPENAI_API_KEY", BaseEnv: "OPENAI_BASE_URL", Scopes: []string{"read", "full"}}
	m, c := vaultTest(t, acct)
	g := grantFor(t, m, agent("s1"), api.Req{Account: "openai", Scope: "read"}, map[string]string{"key": "sk-real-key"})
	tok, base := g.Env["OPENAI_API_KEY"], g.Env["OPENAI_BASE_URL"]
	if strings.Contains(tok, "sk-real") || !strings.HasSuffix(base, "/v1") {
		t.Fatalf("env %v", g.Env)
	}
	call := func(method string) int {
		req, _ := http.NewRequest(method, base+"/models", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := call("GET"); code != 200 || gotKey != "Bearer sk-real-key" {
		t.Fatalf("GET through the proxy: %d, key %q", code, gotKey)
	}
	if code := call("POST"); code != 403 {
		t.Fatalf("a read grant could POST: %d", code)
	}
	if u := m.vaultGrant(g.Grant.ID).Uses; u < 3 {
		t.Fatalf("uses %d", u)
	}
	c.t = c.t.Add(time.Hour)
	m.vaultTick()
	if code := call("GET"); code != 401 {
		t.Fatalf("an expired grant still works through the proxy: %d", code)
	}
}

// GitHub: the token is minted when you approve, and revoked at GitHub when the grant ends
// before its hour is up.
func TestVaultGitHubRevokedOnEnd(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	var mu sync.Mutex
	revoked := 0
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"token":"ghs_live","expires_at":"`+time.Now().Add(time.Hour).UTC().Format(time.RFC3339)+`"}`)
		case http.MethodDelete:
			mu.Lock()
			revoked++
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer gh.Close()
	acct := model.VaultAccount{Name: "gh", Kind: model.VaultGitHub, AppID: "1", InstallationID: "2", APIURL: gh.URL}
	m, _ := vaultTest(t, acct)
	g := grantFor(t, m, agent("s1"), api.Req{Account: "gh", Scope: "contents:write", Repos: []string{"agentrm"}}, map[string]string{"private_key": pemKey})
	if g.Env["GH_TOKEN"] != "ghs_live" {
		t.Fatalf("env %v", g.Env)
	}
	// A narrower ask from the same session is covered; a wider one asks you.
	if _, again := ask(t, m, agent("s1"), api.Req{Account: "gh", Scope: "contents:read", Repos: []string{"agentrm"}}); again == nil {
		t.Fatal("a narrower ask was not covered")
	}
	if wider, _ := ask(t, m, agent("s1"), api.Req{Account: "gh", Scope: "contents:write,pull_requests:write", Repos: []string{"agentrm"}}); wider == nil {
		t.Fatal("a wider ask was covered")
	}
	if _, _, err := m.vaultRevokeReq(agent("s1"), api.Req{GrantID: g.Grant.ID}); err != nil {
		t.Fatal(err)
	}
	m.vaultWG.Wait()
	mu.Lock()
	defer mu.Unlock()
	if revoked != 1 {
		t.Fatalf("revoked at GitHub %d times", revoked)
	}
}

func TestVaultMintFailureIsReported(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"message":"There is at least one repository that does not exist"}`)
	}))
	defer gh.Close()
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	m, _ := vaultTest(t, model.VaultAccount{Name: "gh", Kind: model.VaultGitHub, AppID: "1", InstallationID: "2", APIURL: gh.URL})
	a, _ := ask(t, m, agent("s1"), api.Req{Account: "gh", Repos: []string{"nope"}})
	err := answer(t, m, a, true, map[string]string{"private_key": pemKey})
	if err == nil || !strings.Contains(err.Error(), "does not exist") || a.State != "failed" {
		t.Fatalf("%v, state %s", err, a.State)
	}
	st, _, _ := m.approvalStatus(agent("s1"), api.Req{ApprovalID: a.ID})
	if st.(*api.ApprovalResp).Error == "" {
		t.Fatal("the waiting agent is not told why")
	}
}

// Each session is told once which accounts it may ask for.
func TestVaultAnnouncedOnce(t *testing.T) {
	m, _ := vaultTest(t, loginAcct)
	inbox := func(s string) []string {
		out, _, _ := m.inbox(agent(s), api.Req{})
		return out.(*api.InboxResp).Questions
	}
	if q := inbox("s1"); len(q) != 1 || !strings.Contains(q[0], "staging-admin (password)") {
		t.Fatalf("inbox %v", q)
	}
	if q := inbox("s1"); len(q) != 0 {
		t.Fatal("told twice")
	}
	if _, _, err := m.vaultSetAccounts(you, api.Req{}); err != nil {
		t.Fatal(err)
	}
	if q := inbox("s2"); len(q) != 0 {
		t.Fatalf("an empty vault announced: %v", q)
	}
}

// An account narrowed while a request waits: approving the old ask must fail.
func TestVaultNarrowedAccountFailsPending(t *testing.T) {
	acct := model.VaultAccount{Name: "aws", Kind: model.VaultAWS, Scopes: []string{"arn:aws:iam::1:role/admin", "arn:aws:iam::1:role/read"}}
	m, _ := vaultTest(t, acct)
	a, _ := ask(t, m, agent("s1"), api.Req{Account: "aws", Scope: "admin"})
	acct.Scopes = []string{"arn:aws:iam::1:role/read"}
	if _, _, err := m.vaultSetAccounts(you, api.Req{VaultAccounts: []model.VaultAccount{acct}}); err != nil {
		t.Fatal(err)
	}
	if a.State != "failed" {
		t.Fatalf("a request for a removed role still waits: %s", a.State)
	}
	if err := answer(t, m, a, true, map[string]string{"access_key_id": "x", "secret_access_key": "y"}); err != nil {
		t.Fatal(err) // answering a failed request does nothing
	}
	if len(m.st.VaultGrants) != 0 {
		t.Fatal("granted a role the account no longer allows")
	}
}

// After a denial the session cannot ask again for that account in another form either, and
// cannot keep many requests open.
func TestVaultAskingLimits(t *testing.T) {
	gh := model.VaultAccount{Name: "gh", Kind: model.VaultGitHub, AppID: "1", InstallationID: "2"}
	m, _ := vaultTest(t, loginAcct, gh)
	a, _ := ask(t, m, agent("s1"), api.Req{Account: "gh", Scope: "contents:read"})
	_ = answer(t, m, a, false, nil)
	if _, _, err := m.vaultRequest(agent("s1"), api.Req{Account: "gh", Scope: "issues:read", Purpose: "x"}); err == nil {
		t.Fatal("asked again for a denied account in another form")
	}
	for i, scope := range []string{"contents:read", "issues:read", "pull_requests:read"} {
		_, _, err := m.vaultRequest(agent("s2"), api.Req{Account: "gh", Scope: scope, Purpose: "x"})
		var ea *errApproval
		if !errors.As(err, &ea) {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	if _, _, err := m.vaultRequest(agent("s2"), api.Req{Account: "staging-admin", Purpose: "x"}); err == nil || !strings.Contains(err.Error(), "waiting") {
		t.Fatalf("a fourth open request: %v", err)
	}
}

// What you were granted is not handed to the next caller that passes for you.
func TestVaultYouAskedEveryTime(t *testing.T) {
	m, _ := vaultTest(t, loginAcct)
	a, _ := ask(t, m, terminal, api.Req{Account: "staging-admin"})
	if err := answer(t, m, a, true, map[string]string{"password": "p"}); err != nil {
		t.Fatal(err)
	}
	if again, got := ask(t, m, terminal, api.Req{Account: "staging-admin"}); again == nil || got != nil {
		t.Fatal("your grant was handed out again without asking")
	}
}

func TestVaultRestartEndsGrants(t *testing.T) {
	m, c := vaultTest(t, loginAcct)
	g := grantFor(t, m, agent("s1"), api.Req{Account: "staging-admin"}, map[string]string{"password": "p"})
	m.vaultRestarted(c.Now())
	if m.vaultGrant(g.Grant.ID).Ended == nil {
		t.Fatal("a grant survived a restart in the record")
	}
}

// A stream already flowing through the proxy is cut when its grant ends, not left to finish.
func TestVaultRevokeCutsAStream(t *testing.T) {
	upstreamDone := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(upstreamDone)
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		select { // a long answer: it would go on for minutes
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
			_, _ = io.WriteString(w, "data: after-revoke\n\n")
		}
	}))
	defer up.Close()
	acct := model.VaultAccount{Name: "openai", Kind: model.VaultAPIKey, APIURL: up.URL + "/v1"}
	m, _ := vaultTest(t, acct)
	g := grantFor(t, m, agent("s1"), api.Req{Account: "openai"}, map[string]string{"key": "sk-real-key"})
	req, _ := http.NewRequest("GET", g.Env["AGENTRM_API_BASE"]+"/stream", nil)
	req.Header.Set("Authorization", "Bearer "+g.Env["AGENTRM_API_KEY"])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	first := make([]byte, len("data: first\n\n"))
	if _, err := io.ReadFull(resp.Body, first); err != nil {
		t.Fatalf("first event: %v", err)
	}
	m.mu.Lock()
	m.revokeLater([]*vaultRevoke{m.vaultEnd(m.vaultGrant(g.Grant.ID), "revoked", m.now())})
	m.mu.Unlock()
	rest := make(chan []byte, 1)
	go func() { b, _ := io.ReadAll(resp.Body); rest <- b }()
	select {
	case b := <-rest:
		if strings.Contains(string(b), "after-revoke") {
			t.Fatalf("the stream went on after the revoke: %q", b)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stream was not cut when the grant was revoked")
	}
	select {
	case <-upstreamDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the request to the API was left open")
	}
}
