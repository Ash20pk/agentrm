package model

import "time"

// Vault kinds: what an account is, and so how access to it is handed out.
const (
	VaultGitHub   = "github"   // a GitHub App: each grant is a fresh installation token (1 h at most)
	VaultAPIKey   = "apikey"   // a long-lived API key: the agent gets a proxy token, never the key
	VaultAWS      = "aws"      // AWS keys that may assume roles: each grant is an STS session
	VaultPassword = "password" // a login: handed to the command; cannot be taken back once seen
)

// VaultAccount is what the vault knows about an account, without its secret. The secret (a
// GitHub App's private key, an API key, AWS keys, a password) is in the macOS Keychain, held
// by the AgentRM app, and reaches the daemon only when you approve a request, for as long as
// the grant lasts, in memory.
type VaultAccount struct {
	Name string `json:"name"` // what agents ask for: "github-openbox", "stripe-test"
	Kind string `json:"kind"`
	Note string `json:"note,omitempty"` // shown to agents in `agentrm vault list`
	// MaxMinutes is the longest grant you allow for it (default 60).
	MaxMinutes int `json:"max_minutes,omitempty"`
	// Scopes are what may be asked, the first being the default: for an API key "full" and/or
	// "read" (GET and HEAD only); for AWS the role ARNs; for a password "use". GitHub uses
	// Permissions and Repos instead.
	Scopes []string `json:"scopes,omitempty"`

	// GitHub App
	AppID          string            `json:"app_id,omitempty"`
	InstallationID string            `json:"installation_id,omitempty"`
	Repos          []string          `json:"repos,omitempty"`       // the repos that may be asked for; empty: all the installation has
	Permissions    map[string]string `json:"permissions,omitempty"` // the most that may be asked: {"contents":"write"}; empty: the installation's
	// APIURL is GitHub's API (default https://api.github.com), or the API an API key is for
	// (https://api.openai.com/v1).
	APIURL string `json:"api_url,omitempty"`

	// API key, through the proxy
	Header string `json:"header,omitempty"` // the header the key goes in (default Authorization)
	Prefix string `json:"prefix,omitempty"` // before the key in it (default "Bearer " for Authorization)
	// RawKey: the key goes in Authorization as it is, with no "Bearer " (Linear, AssemblyAI).
	RawKey  bool   `json:"raw_key,omitempty"`
	KeyEnv  string `json:"key_env,omitempty"`  // the variable a tool reads the key from (OPENAI_API_KEY)
	BaseEnv string `json:"base_env,omitempty"` // the variable it reads the API's address from (OPENAI_BASE_URL)

	// AWS
	Region     string `json:"region,omitempty"`
	ExternalID string `json:"external_id,omitempty"`

	// Password
	Username string `json:"username,omitempty"`
	URL      string `json:"url,omitempty"`
	UserEnv  string `json:"user_env,omitempty"` // default AGENTRM_USERNAME
	PassEnv  string `json:"pass_env,omitempty"` // default AGENTRM_PASSWORD
}

// VaultAsk is what an agent asked of the vault: the part of an approval you are shown.
type VaultAsk struct {
	Account string   `json:"account"`
	Kind    string   `json:"kind"`
	Scope   string   `json:"scope"`
	Repos   []string `json:"repos,omitempty"`
	Minutes int      `json:"minutes"`
	Purpose string   `json:"purpose"`
	// Reveal: the agent asked to see the secret itself, not only to use it in a command. What
	// it is shown goes into its transcript.
	Reveal  bool   `json:"reveal,omitempty"`
	Session string `json:"session"`
	Agent   string `json:"agent"`
	// AgentPID is the agent process that asked. A grant is reused only by that same process:
	// a session's name is the agent's word (its hooks send it), and could be claimed by
	// another agent once the first is gone.
	AgentPID int `json:"agent_pid,omitempty"`
	// AgentStart is when that process started (sys.ProcStartID): a PID alone may since have
	// been given to another process.
	AgentStart uint64 `json:"agent_start,omitempty"`
	Repo       string `json:"repo,omitempty"` // the project it asked from
	Cwd        string `json:"cwd,omitempty"`
}

// VaultGrant is access you gave: kept as a record (the newest 200), never with the secret.
type VaultGrant struct {
	ID string `json:"id"`
	VaultAsk
	Created time.Time `json:"created"`
	Expires time.Time `json:"expires"`
	// Ended is set when the grant ended: expired, revoked, its session ended, AgentRM restarted.
	Ended     *time.Time `json:"ended,omitempty"`
	EndReason string     `json:"end_reason,omitempty"`
	Uses      int        `json:"uses"` // times it was handed out or, for an API key, requests through the proxy
	LastUsed  *time.Time `json:"last_used,omitempty"`
	// Note says what the grant cannot do: an AWS session cannot be cut short, a password
	// cannot be taken back.
	Note string `json:"note,omitempty"`
}

// Live reports whether the grant can still be used at now.
func (g *VaultGrant) Live(now time.Time) bool { return g.Ended == nil && now.Before(g.Expires) }
