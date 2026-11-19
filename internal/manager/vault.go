package manager

// The vault: an agent asks for access to an account (a GitHub App, an API key, AWS, a login),
// says why, and waits; you approve in the app with Touch ID; the app reads the account's secret
// from the Keychain and hands it to the daemon, which mints a short-lived credential and keeps
// it in memory for as long as the grant lasts. The same session asking again for no more than
// it was given gets it without asking you. A grant ends when it expires, when it is revoked,
// when its session ends, or when the daemon stops: nothing secret is ever written to disk,
// logged, traced or put in an event.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"agentrm/internal/api"
	"agentrm/internal/model"
	"agentrm/internal/vault"
)

const (
	// A vault request waits longer than a privileged one: the agent is blocked on it, and you
	// may be reading what it asks.
	vaultApprovalTTL = 5 * time.Minute
	// After you deny a session an account, the same ask is refused for this long, so an agent
	// cannot keep the notification coming.
	vaultDeniedQuiet = 2 * time.Minute
	vaultKeepGrants  = 200
)

// vaultLive is a grant's credential, in memory only.
type vaultLive struct {
	cred *vault.Credential
	acct model.VaultAccount
}

type vaultRevoke struct {
	acct  model.VaultAccount
	cred  *vault.Credential
	grant string
}

func (m *Manager) vaultAccount(name string) *model.VaultAccount {
	return m.st.VaultAccounts[name]
}

func (m *Manager) vaultGrant(id string) *model.VaultGrant {
	for _, g := range m.st.VaultGrants {
		if g.ID == id {
			return g
		}
	}
	return nil
}

func (m *Manager) vaultAccountList() []*model.VaultAccount {
	out := make([]*model.VaultAccount, 0, len(m.st.VaultAccounts))
	for _, a := range m.st.VaultAccounts {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (m *Manager) vaultNames() string {
	var names []string
	for _, a := range m.vaultAccountList() {
		names = append(names, a.Name+" ("+a.Kind+")")
	}
	if len(names) == 0 {
		return "the vault has no accounts yet: they are added in the AgentRM app, under Vault"
	}
	return "accounts: " + strings.Join(names, ", ")
}

// vaultList: the accounts, and the grants of the session asking (all grants, for you).
func (m *Manager) vaultList(c api.Caller, r api.Req) (any, []Effect, error) {
	out := &api.VaultListResp{Accounts: m.vaultAccountList(), Grants: []*model.VaultGrant{}}
	for _, g := range m.st.VaultGrants {
		if c.Human || g.Session == c.Session {
			out.Grants = append(out.Grants, g)
		}
	}
	return out, nil, nil
}

// vaultRequest hands out a grant this session already has, or asks you for a new one.
func (m *Manager) vaultRequest(c api.Caller, r api.Req) (any, []Effect, error) {
	if c.Session == "" || c.Session == "detached" {
		return nil, nil, fmt.Errorf("a detached process cannot ask the vault: ask from the agent's own shell")
	}
	acct := m.vaultAccount(r.Account)
	if acct == nil {
		return nil, nil, fmt.Errorf("no vault account %q; %s", r.Account, m.vaultNames())
	}
	ask := model.VaultAsk{Scope: r.Scope, Repos: r.Repos, Minutes: r.TTLMinutes, Purpose: strings.TrimSpace(r.Purpose),
		Reveal: r.Reveal, Session: c.Session, Agent: c.Agent, Repo: r.Repo, Cwd: r.Cwd}
	if err := vault.Normalize(acct, &ask); err != nil {
		return nil, nil, err
	}
	now := m.now()
	for i := len(m.st.VaultGrants) - 1; i >= 0; i-- {
		g := m.st.VaultGrants[i]
		live := m.vaultLive[g.ID]
		if live == nil || !g.Live(now) || !vault.Covers(g, &ask) {
			continue
		}
		t := now
		g.Uses++
		g.LastUsed = &t
		m.dirty = true
		return &api.VaultResp{Grant: g, Env: live.cred.Env, Unset: live.cred.Unset}, nil, nil
	}
	for _, a := range m.st.Approvals {
		if a.Vault == nil || a.Vault.Session != ask.Session || a.Vault.Account != ask.Account {
			continue
		}
		same := a.Vault.Scope == ask.Scope && slices.Equal(a.Vault.Repos, ask.Repos) && a.Vault.Reveal == ask.Reveal
		switch {
		case a.State == "pending" && now.Before(a.Expires) && same:
			return nil, nil, &errApproval{what: a.What, a: a}
		case a.State == "denied" && now.Sub(a.Created) < vaultDeniedQuiet+vaultApprovalTTL && same:
			return nil, nil, fmt.Errorf("access to %s was denied in AgentRM a moment ago; ask the person in the chat before asking again", ask.Account)
		}
	}
	who := fmt.Sprintf("%s session %s", agentTitle(c.Agent), short(c.Session))
	if ask.Repo != "" {
		who += " in " + ask.Repo
	}
	a := &model.Approval{ID: newID(), What: who + " wants " + vault.Describe(&ask) + ". Why: " + trunc(ask.Purpose, 200),
		Created: now, Expires: now.Add(vaultApprovalTTL), Path: "/vault/request", From: c.App, State: "pending", Vault: &ask}
	if m.st.Approvals == nil {
		m.st.Approvals = map[string]*model.Approval{}
	}
	m.st.Approvals[a.ID] = a
	m.dirty = true
	m.event("APPROVAL %s requested by %s: %s. Approve it with Touch ID in AgentRM.", a.ID, who, vault.Describe(&ask))
	m.notify("Vault: "+who+" wants "+ask.Account, trunc(ask.Purpose, 120))
	return nil, nil, &errApproval{what: a.What, a: a}
}

// vaultAnswer is the app answering a vault request. Approving carries the account's secret,
// which is used to mint the credential outside the lock (it is a network call) and kept only
// in memory. Only the AgentRM app may answer: the secret is in its Keychain, behind Touch ID.
func (m *Manager) vaultAnswer(ctx context.Context, f facts, r api.Req) (any, error) {
	m.mu.Lock()
	c := m.resolve(f, &r)
	a, acct, ask, done, err := m.vaultAnswerCheck(c, r)
	if err != nil || done != nil {
		m.mu.Unlock()
		return done, err
	}
	a.State = "minting"
	m.dirty = true
	m.mu.Unlock()

	var base string
	if acct.Kind == model.VaultAPIKey {
		base, err = m.ensureProxy()
	}
	var cred *vault.Credential
	if err == nil {
		mctx, cancel := context.WithTimeout(ctx, 25*time.Second)
		cred, err = vault.Mint(mctx, acct, vault.Material(r.Material), ask, vault.Opts{Client: m.vaultClient, ProxyBase: base, Now: m.now()})
		cancel()
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if err != nil {
		a.State, a.Error = "failed", err.Error()
		m.event("VAULT could not give %s to session %s: %v", ask.Account, short(ask.Session), err)
		return nil, fmt.Errorf("approved, but %w", err)
	}
	expires := now.Add(minutes(ask.Minutes))
	if !cred.Expires.IsZero() && cred.Expires.Before(expires) {
		expires = cred.Expires
	}
	g := &model.VaultGrant{ID: "g" + newID(), VaultAsk: ask, Created: now, Expires: expires, Note: cred.Note}
	m.st.VaultGrants = append(m.st.VaultGrants, g)
	if n := len(m.st.VaultGrants); n > vaultKeepGrants {
		m.st.VaultGrants = m.st.VaultGrants[n-vaultKeepGrants:]
	}
	if m.vaultLive == nil {
		m.vaultLive, m.vaultTok = map[string]*vaultLive{}, map[string]string{}
	}
	m.vaultLive[g.ID] = &vaultLive{cred: cred, acct: acct}
	if cred.ProxyToken != "" {
		m.vaultTok[cred.ProxyToken] = g.ID
	}
	a.State = "approved"
	m.dirty = true
	m.event("VAULT granted %s to %s session %s until %s: %s", g.ID, agentTitle(ask.Agent), short(ask.Session),
		expires.Local().Format("15:04"), vault.Describe(&ask))
	// The session may have ended while the token was being made.
	if s := m.st.Sessions[ask.Session]; s != nil && s.EndedAt != nil {
		m.revokeLater([]*vaultRevoke{m.vaultEnd(g, "its session had ended", now)})
	}
	return &api.ApprovalResp{Approval: a, Result: g}, nil
}

// vaultAnswerCheck finds the approval being answered. done is the response when there is
// nothing to mint (denied, already answered). Must hold mu.
func (m *Manager) vaultAnswerCheck(c api.Caller, r api.Req) (a *model.Approval, acct model.VaultAccount, ask model.VaultAsk, done any, err error) {
	if !c.ViaApp {
		return nil, acct, ask, nil, fmt.Errorf("vault requests are answered only in the AgentRM app, with Touch ID")
	}
	a = m.st.Approvals[r.ApprovalID]
	if a == nil || a.Vault == nil {
		return nil, acct, ask, nil, fmt.Errorf("no vault request %s", r.ApprovalID)
	}
	if a.State != "pending" {
		return a, acct, ask, &api.ApprovalResp{Approval: a}, nil
	}
	if m.now().After(a.Expires) {
		a.State = "expired"
		return a, acct, ask, &api.ApprovalResp{Approval: a}, nil
	}
	if r.Approve == nil || !*r.Approve {
		a.State = "denied"
		m.dirty = true
		m.event("DENIED %s: %s", a.ID, a.What)
		return a, acct, ask, &api.ApprovalResp{Approval: a}, nil
	}
	p := m.vaultAccount(a.Vault.Account)
	if p == nil || p.Kind != a.Vault.Kind {
		a.State, a.Error = "failed", "the account was removed or changed"
		return a, acct, ask, nil, errors.New(a.Error)
	}
	if len(r.Material) == 0 {
		return a, acct, ask, nil, fmt.Errorf("no secret came with the approval of %s", a.Vault.Account)
	}
	ask = *a.Vault
	if r.TTLMinutes > 0 && r.TTLMinutes < ask.Minutes { // you may give less time than asked
		ask.Minutes = r.TTLMinutes
		if ask.Kind == model.VaultAWS {
			ask.Minutes = max(ask.Minutes, 15)
		}
	}
	return a, *p, ask, nil, nil
}

// vaultEnd ends a grant and forgets its credential. It returns what to revoke at the provider
// (nil when there is nothing), to run outside the lock. Must hold mu.
func (m *Manager) vaultEnd(g *model.VaultGrant, why string, now time.Time) *vaultRevoke {
	if g.Ended != nil {
		return nil
	}
	t := now
	g.Ended, g.EndReason = &t, why
	m.dirty = true
	live := m.vaultLive[g.ID]
	delete(m.vaultLive, g.ID)
	if live == nil {
		return nil
	}
	if live.cred.ProxyToken != "" {
		delete(m.vaultTok, live.cred.ProxyToken)
	}
	if why != "expired" {
		m.event("VAULT ended %s (%s, session %s): %s", g.ID, g.Account, short(g.Session), why)
	}
	if live.cred.Token == "" {
		return nil
	}
	return &vaultRevoke{acct: live.acct, cred: live.cred, grant: g.ID}
}

// revokeLater revokes at the providers, outside the lock. Callers may hold mu.
func (m *Manager) revokeLater(rs []*vaultRevoke) {
	rs = slices.DeleteFunc(rs, func(r *vaultRevoke) bool { return r == nil })
	if len(rs) == 0 {
		return
	}
	m.vaultWG.Add(1)
	go func() {
		defer m.vaultWG.Done()
		for _, r := range rs {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			err := vault.Revoke(ctx, r.acct, r.cred, vault.Opts{Client: m.vaultClient})
			cancel()
			if err != nil {
				m.mu.Lock()
				m.event("VAULT could not revoke %s at %s (%v); it stops working by itself at %s",
					r.grant, r.acct.Kind, err, r.cred.Expires.Local().Format("15:04"))
				m.mu.Unlock()
			}
		}
	}()
}

// vaultTick ends what expired. Called every second.
func (m *Manager) vaultTick() {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	var rs []*vaultRevoke
	for _, g := range m.st.VaultGrants {
		if g.Ended == nil && !now.Before(g.Expires) {
			rs = append(rs, m.vaultEnd(g, "expired", now))
		}
	}
	m.revokeLater(rs)
}

// vaultEndSession ends a session's grants. Must hold mu.
func (m *Manager) vaultEndSession(session, why string, now time.Time) {
	var rs []*vaultRevoke
	for _, g := range m.st.VaultGrants {
		if g.Session == session && g.Ended == nil {
			rs = append(rs, m.vaultEnd(g, why, now))
		}
	}
	m.revokeLater(rs)
}

// vaultRestarted ends what a previous daemon granted: its credentials were in its memory.
func (m *Manager) vaultRestarted(now time.Time) {
	n := 0
	for _, g := range m.st.VaultGrants {
		if g.Ended == nil {
			t := now
			g.Ended, g.EndReason = &t, "AgentRM restarted (grants live in its memory only)"
			n++
		}
	}
	if n > 0 {
		m.event("VAULT ended %d grant(s) on restart; a GitHub token among them works until its hour is up", n)
	}
}

// vaultRevokeReq: an agent may revoke its own grants, you any.
func (m *Manager) vaultRevokeReq(c api.Caller, r api.Req) (any, []Effect, error) {
	if r.GrantID == "" && !r.All {
		return nil, nil, fmt.Errorf("name a grant to revoke, or --all")
	}
	now := m.now()
	var rs []*vaultRevoke
	n := 0
	for _, g := range m.st.VaultGrants {
		if g.Ended != nil || (!r.All && g.ID != r.GrantID) {
			continue
		}
		if !c.Human && g.Session != c.Session {
			if !r.All {
				return nil, nil, fmt.Errorf("grant %s belongs to another session", g.ID)
			}
			continue
		}
		rs = append(rs, m.vaultEnd(g, "revoked by "+map[bool]string{true: "you", false: "its session"}[c.Human], now))
		n++
	}
	if n == 0 && !r.All {
		return nil, nil, fmt.Errorf("no live grant %s", r.GrantID)
	}
	m.revokeLater(rs)
	return &api.CountResp{N: n}, nil, nil
}

// vaultSetAccounts replaces the account list. Only the app may: it holds the secrets, so an
// account it does not know could never be granted anyway, and an agent must not be able to
// point an account's API address somewhere else. Grants of an account that changed end.
func (m *Manager) vaultSetAccounts(c api.Caller, r api.Req) (any, []Effect, error) {
	if !c.ViaApp {
		return nil, nil, fmt.Errorf("vault accounts are changed only in the AgentRM app")
	}
	next := map[string]*model.VaultAccount{}
	for i := range r.VaultAccounts {
		a := r.VaultAccounts[i]
		if err := vault.CheckAccount(&a); err != nil {
			return nil, nil, err
		}
		if next[a.Name] != nil {
			return nil, nil, fmt.Errorf("two accounts are named %s", a.Name)
		}
		next[a.Name] = &a
	}
	now := m.now()
	var rs []*vaultRevoke
	var changed []string
	for name, old := range m.st.VaultAccounts {
		if nw := next[name]; nw == nil || !reflect.DeepEqual(old, nw) {
			changed = append(changed, name)
			for _, g := range m.st.VaultGrants {
				if g.Account == name && g.Ended == nil {
					rs = append(rs, m.vaultEnd(g, "the account was changed or removed", now))
				}
			}
		}
	}
	for name := range next {
		if m.st.VaultAccounts[name] == nil {
			changed = append(changed, name)
		}
	}
	m.st.VaultAccounts = next
	m.vaultAnnounce(now)
	m.revokeLater(rs)
	if len(changed) > 0 {
		sort.Strings(changed)
		m.event("VAULT accounts changed: %s", strings.Join(changed, ", "))
	}
	return &api.VaultListResp{Accounts: m.vaultAccountList(), Grants: m.st.VaultGrants}, nil, nil
}

// vaultAnnounce tells each session once, in its inbox, which accounts it may ask for: an agent
// that does not know the vault exists asks the person for the secret instead. Must hold mu.
func (m *Manager) vaultAnnounce(now time.Time) {
	const key = "vault:accounts"
	if len(m.st.VaultAccounts) == 0 {
		delete(m.st.Questions, key)
		return
	}
	var names []string
	for _, a := range m.vaultAccountList() {
		names = append(names, a.Name+" ("+a.Kind+")")
	}
	if m.st.Questions == nil {
		m.st.Questions = map[string]*model.Question{}
	}
	m.st.Questions[key] = &model.Question{Created: now, Delivered: map[string]bool{}, Answers: map[string]string{},
		Text: "AgentRM's vault can give you short-lived access to: " + strings.Join(names, ", ") +
			". If your task needs one, run `agentrm vault run <account> --purpose \"<why>\" -- <command>` (see `agentrm vault list` for scopes); " +
			"the person approves it with Touch ID. Never ask the person to paste these secrets into the chat."}
}

// ensureProxy starts the API-key proxy on a loopback port, once.
func (m *Manager) ensureProxy() (string, error) {
	m.proxyOnce.Do(func() {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			m.proxyErr = fmt.Errorf("could not start the vault proxy: %w", err)
			return
		}
		m.proxyBase = "http://" + ln.Addr().String()
		srv := &http.Server{Handler: vault.NewProxy(m.proxyLookup), ReadHeaderTimeout: 10 * time.Second}
		go func() { _ = srv.Serve(ln) }()
	})
	return m.proxyBase, m.proxyErr
}

func (m *Manager) proxyLookup(tok string) (vault.Route, string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	g := m.vaultGrant(m.vaultTok[tok])
	live := m.vaultLive[m.vaultTok[tok]]
	if g == nil || live == nil {
		return vault.Route{}, "this token was not issued by this AgentRM, or the grant ended; ask again with agentrm vault run", false
	}
	now := m.now()
	if !g.Live(now) {
		return vault.Route{}, "the grant for " + g.Account + " ended; ask again with agentrm vault run", false
	}
	u, err := url.Parse(live.acct.APIURL)
	if err != nil {
		return vault.Route{}, "the account's API address is not valid", false
	}
	header, prefix := live.acct.Header, live.acct.Prefix
	if header == "" {
		header = "Authorization"
	}
	if prefix == "" && strings.EqualFold(header, "Authorization") {
		prefix = "Bearer "
	}
	t := now
	g.Uses++
	g.LastUsed = &t
	m.dirty = true
	return vault.Route{Upstream: u, Header: header, Prefix: prefix, Key: live.cred.ProxyKey, ReadOnly: g.Scope == "read"}, "", true
}
