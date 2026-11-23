// Package vault hands agents short-lived access to accounts: it checks what is asked against
// what an account allows, mints the credential (a GitHub installation token, an AWS STS
// session, a proxy token for an API key, a password), revokes it, and runs the local proxy that
// puts an API key on requests so the agent never holds it. It keeps no state: the manager owns
// grants, and the AgentRM app owns the secrets.
package vault

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"agentrm/internal/model"
)

const (
	DefaultMinutes = 30
	defaultMax     = 60
	githubMax      = 60 // an installation token lives an hour
	awsMin         = 15 // STS refuses less
)

var (
	nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,47}$`)
	permRE = regexp.MustCompile(`^[a-z_]{2,40}$`)
	envRE  = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)
	repoRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
)

var levels = map[string]int{"read": 1, "write": 2, "admin": 3}

// CheckAccount reports what is wrong with an account's settings, before it is saved.
func CheckAccount(a *model.VaultAccount) error {
	if !nameRE.MatchString(a.Name) {
		return fmt.Errorf("account name %q: use lowercase letters, digits, dot, dash or underscore (up to 48)", a.Name)
	}
	if a.MaxMinutes < 0 || a.MaxMinutes > 12*60 {
		return fmt.Errorf("%s: the longest grant must be between 1 minute and 12 hours", a.Name)
	}
	for _, e := range []string{a.KeyEnv, a.BaseEnv, a.UserEnv, a.PassEnv} {
		if e != "" && !envRE.MatchString(e) {
			return fmt.Errorf("%s: %q is not an environment variable name", a.Name, e)
		}
	}
	switch a.Kind {
	case model.VaultGitHub:
		if a.AppID == "" || a.InstallationID == "" {
			return fmt.Errorf("%s: a GitHub account needs the App ID and the installation ID", a.Name)
		}
		for p, l := range a.Permissions {
			if !permRE.MatchString(p) || levels[l] == 0 {
				return fmt.Errorf("%s: permission %s=%s: use read, write or admin", a.Name, p, l)
			}
		}
		for _, r := range a.Repos {
			if !repoRE.MatchString(repoName(r)) {
				return fmt.Errorf("%s: %q is not a repository name", a.Name, r)
			}
		}
	case model.VaultAPIKey:
		if !strings.HasPrefix(a.APIURL, "https://") && !strings.HasPrefix(a.APIURL, "http://127.0.0.1") && !strings.HasPrefix(a.APIURL, "http://localhost") {
			return fmt.Errorf("%s: the API address must start with https://", a.Name)
		}
		for _, s := range a.Scopes {
			if s != "full" && s != "read" {
				return fmt.Errorf("%s: an API key's scopes are full and read (GET only), not %q", a.Name, s)
			}
		}
	case model.VaultAWS:
		if len(a.Scopes) == 0 {
			return fmt.Errorf("%s: list at least one role ARN that may be assumed", a.Name)
		}
		for _, s := range a.Scopes {
			if !strings.HasPrefix(s, "arn:") || !strings.Contains(s, ":role/") {
				return fmt.Errorf("%s: %q is not a role ARN (arn:aws:iam::<account>:role/<name>)", a.Name, s)
			}
		}
	case model.VaultPassword:
	default:
		return fmt.Errorf("%s: unknown kind %q (github, apikey, aws or password)", a.Name, a.Kind)
	}
	return nil
}

// MaxMinutes is the longest grant an account allows.
func MaxMinutes(a *model.VaultAccount) int {
	m := a.MaxMinutes
	if m <= 0 {
		m = defaultMax
	}
	if a.Kind == model.VaultGitHub {
		m = min(m, githubMax)
	}
	return m
}

// Normalize fills in the defaults of an ask and checks it against what the account allows. It
// rewrites Scope and Repos into one canonical form, so that two asks for the same thing compare
// equal.
func Normalize(a *model.VaultAccount, ask *model.VaultAsk) error {
	ask.Account, ask.Kind = a.Name, a.Kind
	if strings.TrimSpace(ask.Purpose) == "" {
		return fmt.Errorf("say why you need %s (--purpose): it is what you will be asked to approve", a.Name)
	}
	if ask.Minutes <= 0 {
		ask.Minutes = DefaultMinutes
	}
	ask.Minutes = min(ask.Minutes, MaxMinutes(a))
	if a.Kind == model.VaultAWS {
		ask.Minutes = max(ask.Minutes, awsMin)
	}
	switch a.Kind {
	case model.VaultGitHub:
		return normalizeGitHub(a, ask)
	case model.VaultAWS:
		ask.Repos = nil
		if ask.Scope == "" {
			ask.Scope = a.Scopes[0]
		}
		for _, role := range a.Scopes {
			if ask.Scope == role || strings.HasSuffix(role, ":role/"+ask.Scope) {
				ask.Scope = role
				return nil
			}
		}
		return fmt.Errorf("%s may assume %s, not %q", a.Name, strings.Join(a.Scopes, ", "), ask.Scope)
	default:
		ask.Repos = nil
		allowed := a.Scopes
		if len(allowed) == 0 {
			allowed = []string{map[string]string{model.VaultAPIKey: "full", model.VaultPassword: "use"}[a.Kind]}
		}
		if ask.Scope == "" {
			ask.Scope = allowed[0]
		}
		if slices.Contains(allowed, ask.Scope) || (ask.Scope == "read" && slices.Contains(allowed, "full")) {
			return nil
		}
		return fmt.Errorf("%s allows %s, not %q", a.Name, strings.Join(allowed, " or "), ask.Scope)
	}
}

func normalizeGitHub(a *model.VaultAccount, ask *model.VaultAsk) error {
	if ask.Scope == "" {
		ask.Scope = "contents:read"
	}
	perms, err := ParsePermissions(ask.Scope)
	if err != nil {
		return err
	}
	if len(a.Permissions) > 0 {
		for p, l := range perms {
			if levels[l] > levels[a.Permissions[p]] {
				most := a.Permissions[p]
				if most == "" {
					return fmt.Errorf("%s does not allow %s at all (allowed: %s)", a.Name, p, FormatPermissions(a.Permissions))
				}
				return fmt.Errorf("%s allows %s:%s at most, not %s", a.Name, p, most, l)
			}
		}
	}
	ask.Scope = FormatPermissions(perms)
	var repos []string
	for _, r := range ask.Repos {
		r = repoName(strings.TrimSpace(r))
		if r == "" {
			continue
		}
		if !repoRE.MatchString(r) {
			return fmt.Errorf("%q is not a repository name", r)
		}
		if len(a.Repos) > 0 && !slices.ContainsFunc(a.Repos, func(x string) bool { return strings.EqualFold(repoName(x), r) }) {
			return fmt.Errorf("%s does not include the repository %s (it has: %s)", a.Name, r, strings.Join(a.Repos, ", "))
		}
		repos = append(repos, r)
	}
	if len(repos) == 0 {
		for _, r := range a.Repos {
			repos = append(repos, repoName(r))
		}
	}
	sort.Strings(repos)
	ask.Repos = slices.Compact(repos)
	return nil
}

// ParsePermissions reads "contents:read,pull_requests:write" (or with =).
func ParsePermissions(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		p, l, ok := strings.Cut(part, ":")
		if !ok {
			p, l, ok = strings.Cut(part, "=")
		}
		p, l = strings.TrimSpace(strings.ToLower(p)), strings.TrimSpace(strings.ToLower(l))
		if !ok || !permRE.MatchString(p) || levels[l] == 0 {
			return nil, fmt.Errorf("permission %q: write it as name:level, level being read, write or admin (contents:read,pull_requests:write)", part)
		}
		if levels[l] > levels[out[p]] {
			out[p] = l
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no permissions asked (for example --scope contents:read)")
	}
	return out, nil
}

func FormatPermissions(p map[string]string) string {
	keys := make([]string, 0, len(p))
	for k := range p {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + ":" + p[k]
	}
	return strings.Join(parts, ",")
}

// repoName drops an "owner/" prefix: GitHub's token API takes names within the installation.
func repoName(r string) string {
	if i := strings.LastIndex(r, "/"); i >= 0 {
		return r[i+1:]
	}
	return r
}

// Covers reports whether grant g already gives what ask (normalized) asks for, so the same
// session is not asked to approve again. It does not check that g is live.
func Covers(g *model.VaultGrant, ask *model.VaultAsk) bool {
	if g.Account != ask.Account || g.Kind != ask.Kind || g.Session != ask.Session || (ask.Reveal && !g.Reveal) {
		return false
	}
	switch g.Kind {
	case model.VaultGitHub:
		have, err1 := ParsePermissions(g.Scope)
		want, err2 := ParsePermissions(ask.Scope)
		if err1 != nil || err2 != nil {
			return false
		}
		for p, l := range want {
			if levels[have[p]] < levels[l] {
				return false
			}
		}
		// No repos listed means all of the installation's.
		if len(g.Repos) == 0 {
			return true
		}
		if len(ask.Repos) == 0 {
			return false
		}
		for _, r := range ask.Repos {
			if !slices.Contains(g.Repos, r) {
				return false
			}
		}
		return true
	case model.VaultAPIKey:
		return g.Scope == ask.Scope || (g.Scope == "full" && ask.Scope == "read")
	default:
		return g.Scope == ask.Scope
	}
}

// Describe says in a line what an ask gives, for the approval and the record.
func Describe(ask *model.VaultAsk) string {
	var what string
	switch ask.Kind {
	case model.VaultGitHub:
		repos := "every repository the app can reach"
		if len(ask.Repos) > 0 {
			repos = strings.Join(ask.Repos, ", ")
		}
		what = ask.Scope + " on " + repos
	case model.VaultAPIKey:
		what = map[string]string{"read": "read-only (GET) use of the API", "full": "full use of the API"}[ask.Scope]
	case model.VaultAWS:
		what = "the role " + ask.Scope
	case model.VaultPassword:
		what = "the login"
	}
	s := fmt.Sprintf("%s: %s, for %d min", ask.Account, what, ask.Minutes)
	if ask.Reveal {
		s += ", shown to the agent"
	}
	return s
}
