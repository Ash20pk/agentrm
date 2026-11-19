package vault

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"agentrm/internal/model"
)

// Material is an account's secret, as the app read it from the Keychain: private_key (GitHub),
// key (API key), access_key_id, secret_access_key and session_token (AWS), password.
type Material map[string]string

// Credential is what one grant hands out. It lives in the daemon's memory only, for as long as
// the grant does.
type Credential struct {
	// Env is what a command run with the grant gets.
	Env map[string]string
	// Unset is what a command run with the grant must not inherit.
	Unset []string
	// Expires is when the provider ends the credential itself (zero: never).
	Expires time.Time
	// Token is what to revoke when the grant ends early (a GitHub installation token).
	Token string
	// ProxyToken and ProxyKey: for an API key, the token the agent holds and the real key the
	// proxy puts in its place.
	ProxyToken, ProxyKey string
	// Note says what cannot be undone.
	Note string
}

// Opts carries what minting needs besides the account.
type Opts struct {
	Client    *http.Client // nil: a 20-second default
	ProxyBase string       // http://127.0.0.1:<port>, for API keys
	Now       time.Time
}

func (o Opts) client() *http.Client {
	if o.Client != nil {
		return o.Client
	}
	return &http.Client{Timeout: 20 * time.Second}
}

// Mint makes the credential for an approved ask.
func Mint(ctx context.Context, a model.VaultAccount, m Material, ask model.VaultAsk, o Opts) (*Credential, error) {
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	switch a.Kind {
	case model.VaultGitHub:
		return mintGitHub(ctx, a, m, ask, o)
	case model.VaultAWS:
		return mintAWS(ctx, a, m, ask, o)
	case model.VaultAPIKey:
		return mintAPIKey(a, m, o)
	case model.VaultPassword:
		return mintPassword(a, m)
	}
	return nil, fmt.Errorf("unknown kind %q", a.Kind)
}

// Revoke ends a credential before its time, where the provider allows it.
func Revoke(ctx context.Context, a model.VaultAccount, c *Credential, o Opts) error {
	if a.Kind == model.VaultGitHub && c.Token != "" {
		return revokeGitHub(ctx, a, c.Token, o)
	}
	return nil
}

func mintAPIKey(a model.VaultAccount, m Material, o Opts) (*Credential, error) {
	key := strings.TrimSpace(m["key"])
	if key == "" {
		return nil, fmt.Errorf("%s has no key saved in AgentRM", a.Name)
	}
	if o.ProxyBase == "" {
		return nil, fmt.Errorf("the vault proxy is not running")
	}
	u, err := url.Parse(a.APIURL)
	if err != nil {
		return nil, fmt.Errorf("%s: bad API address: %w", a.Name, err)
	}
	tok := NewProxyToken()
	keyEnv, baseEnv := or(a.KeyEnv, "AGENTRM_API_KEY"), or(a.BaseEnv, "AGENTRM_API_BASE")
	return &Credential{
		Env:        map[string]string{keyEnv: tok, baseEnv: strings.TrimRight(o.ProxyBase+u.Path, "/")},
		ProxyToken: tok, ProxyKey: key,
	}, nil
}

func mintPassword(a model.VaultAccount, m Material) (*Credential, error) {
	pw := m["password"]
	if pw == "" {
		return nil, fmt.Errorf("%s has no password saved in AgentRM", a.Name)
	}
	env := map[string]string{or(a.PassEnv, "AGENTRM_PASSWORD"): pw}
	if a.Username != "" {
		env[or(a.UserEnv, "AGENTRM_USERNAME")] = a.Username
	}
	if a.URL != "" {
		env["AGENTRM_URL"] = a.URL
	}
	return &Credential{Env: env, Note: "a password cannot be taken back once used: change it if this grant worries you"}, nil
}

// ProxyTokenPrefix starts every token the proxy accepts, so it can find one in any header.
const ProxyTokenPrefix = "agentrm_"

func NewProxyToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return ProxyTokenPrefix + hex.EncodeToString(b)
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
