package vault

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"agentrm/internal/model"
)

// A GitHub App can mint installation tokens limited to some repositories and permissions,
// each living an hour at most and revocable at once: the right shape for an agent's access.
// See https://docs.github.com/en/rest/apps/apps#create-an-installation-access-token-for-an-app

func githubAPI(a model.VaultAccount) string {
	return strings.TrimRight(or(a.APIURL, "https://api.github.com"), "/")
}

func mintGitHub(ctx context.Context, a model.VaultAccount, m Material, ask model.VaultAsk, o Opts) (*Credential, error) {
	key, err := parseRSAKey(m["private_key"])
	if err != nil {
		return nil, fmt.Errorf("%s: the App's private key: %w", a.Name, err)
	}
	jwt, err := appJWT(a.AppID, key, o.Now)
	if err != nil {
		return nil, err
	}
	perms, err := ParsePermissions(ask.Scope)
	if err != nil {
		return nil, err
	}
	body := map[string]any{"permissions": perms}
	if len(ask.Repos) > 0 {
		body["repositories"] = ask.Repos
	}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		githubAPI(a)+"/app/installations/"+a.InstallationID+"/access_tokens", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+jwt)
	githubHeaders(req)
	resp, err := o.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("GitHub did not answer: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("GitHub refused the token (%s): %s", resp.Status, githubMessage(raw))
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Token == "" {
		return nil, fmt.Errorf("GitHub's answer had no token")
	}
	return &Credential{Env: gitEnv(githubAPI(a), out.Token), Expires: out.ExpiresAt, Token: out.Token}, nil
}

func revokeGitHub(ctx context.Context, a model.VaultAccount, token string, o Opts) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, githubAPI(a)+"/installation/token", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	githubHeaders(req)
	resp, err := o.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// 401: the token already expired or was revoked. Either way it no longer works.
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusUnauthorized {
		return fmt.Errorf("GitHub answered %s", resp.Status)
	}
	return nil
}

func githubHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "agentrm-vault")
}

func githubMessage(raw []byte) string {
	var e struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Message != "" {
		return e.Message
	}
	return strings.TrimSpace(string(raw))
}

// gitEnv makes gh, the GitHub API clients and git itself use the token. For git, the
// credential helpers you have (the Keychain's, with your own login) are cleared for GitHub
// first, for this command only, so a push goes as the App and not as you.
func gitEnv(api, token string) map[string]string {
	env := map[string]string{"GH_TOKEN": token, "GITHUB_TOKEN": token}
	host := "github.com"
	if api != "https://api.github.com" {
		host = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(api, "https://"), "http://"), "/api/v3")
		env["GH_HOST"] = host
	}
	key := "credential.https://" + host + ".helper"
	env["GIT_CONFIG_COUNT"] = "2"
	env["GIT_CONFIG_KEY_0"], env["GIT_CONFIG_VALUE_0"] = key, ""
	env["GIT_CONFIG_KEY_1"], env["GIT_CONFIG_VALUE_1"] = key, `!f() { echo username=x-access-token; echo "password=$GH_TOKEN"; }; f`
	return env
}

// appJWT is the App's own short credential (RS256, ten minutes at most), used only to ask for
// an installation token. iat is a minute back, as GitHub advises, for clock drift.
func appJWT(appID string, key *rsa.PrivateKey, now time.Time) (string, error) {
	enc := base64.RawURLEncoding
	head := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(9 * time.Minute).Unix(), "iss": appID})
	signing := head + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(nil, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

func parseRSAKey(s string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(s)))
	if block == nil {
		return nil, fmt.Errorf("not a PEM key (paste the whole .pem GitHub gave you)")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("not an RSA private key")
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("not an RSA private key")
	}
	return rk, nil
}
