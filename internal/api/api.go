// Package api is the contract between the daemon and its clients: the CLI, the Claude Code
// hook, the PATH shim, the MCP server and (through the CLI) the app.
package api

import (
	"time"

	"agentrm/internal/model"
	"agentrm/internal/sys"
)

// Version of the wire protocol. A client and a daemon with different versions refuse to talk
// (the daemon answers 409), so an app update that has not restarted the daemon gets a clear
// message instead of half-understood requests. Hooks fail open on it.
const Version = 2

const VersionHeader = "X-AgentRM-API"

// HookProto is the hook command protocol written into ~/.claude/settings.json. The app compares
// it with what is installed and offers to repair older hooks.
const HookProto = 2

// Req carries every request's fields; each endpoint reads the ones it needs. Session and Agent
// are what the client claims; the daemon replaces them with what it can prove (see Caller).
type Req struct {
	Session string `json:"session,omitempty"`
	Agent   string `json:"agent,omitempty"`
	Cwd     string `json:"cwd,omitempty"`
	Via     string `json:"via,omitempty"` // "hook", "shim", "mcp", "cli", "app"

	Kind            string  `json:"kind,omitempty"`
	MemGB           float64 `json:"mem_gb,omitempty"`
	TTLMinutes      int     `json:"ttl_minutes,omitempty"`
	UntilMinutes    int     `json:"until_minutes,omitempty"` // declared end: the lease ends then
	Purpose         string  `json:"purpose,omitempty"`
	Cmd             string  `json:"cmd,omitempty"`
	RunInBackground bool    `json:"run_in_background,omitempty"`
	// Gate asks /touch to classify Cmd and, if it is heavy, take a lease for it in the same call.
	Gate bool `json:"gate,omitempty"`

	LeaseID    string `json:"lease_id,omitempty"`
	ResourceID string `json:"resource_id,omitempty"`
	All        bool   `json:"all,omitempty"`
	Pinned     *bool  `json:"pinned,omitempty"`
	Priority   *int   `json:"priority,omitempty"`

	DryRun       *bool    `json:"dry_run,omitempty"`
	GraceMinutes int      `json:"grace_minutes,omitempty"`
	HeadroomGB   float64  `json:"headroom_gb,omitempty"`
	PauseMinutes *int     `json:"pause_minutes,omitempty"`
	IdleMinutes  int      `json:"idle_minutes,omitempty"`
	NeverStop    []string `json:"never_stop,omitempty"`

	DecisionID string `json:"decision_id,omitempty"`
	Verdict    string `json:"verdict,omitempty"` // right | wrong
	Note       string `json:"note,omitempty"`

	ApprovalID string `json:"approval_id,omitempty"`
	Approve    *bool  `json:"approve,omitempty"`
}

// Caller is who is really on the other end, as established from the peer PID.
type Caller struct {
	PID      int    `json:"pid"`
	AgentPID int    `json:"agent_pid,omitempty"`
	Agent    string `json:"agent"` // claude, codex, ..., "you", or "detached"
	Session  string `json:"session"`
	Human    bool   `json:"human"`
	ViaApp   bool   `json:"via_app"` // the request came from the AgentRM app itself
	App      string `json:"app,omitempty"`
}

type Error struct {
	Error string `json:"error"`
	// ApprovalID is set when the request needs a click in the app before it runs.
	ApprovalID string `json:"approval_id,omitempty"`
}

type AcquireResp struct {
	Granted bool         `json:"granted"`
	Reason  string       `json:"reason,omitempty"`
	Lease   *model.Lease `json:"lease,omitempty"`
	Reused  bool         `json:"reused,omitempty"`
	Queue   int          `json:"queue,omitempty"` // position when refused
}

type ClaimResp struct {
	Lease    *model.Lease `json:"lease"`
	Verified bool         `json:"verified"`
	Evidence string       `json:"evidence"`
	Note     string       `json:"note,omitempty"`
}

type CountResp struct {
	N    int    `json:"n"`
	Note string `json:"note,omitempty"`
}

type InboxResp struct {
	Questions []string `json:"questions"`
}

type TouchResp struct {
	Touched []string     `json:"touched"`
	Thawed  []string     `json:"thawed,omitempty"`
	Kind    string       `json:"kind,omitempty"`    // with Gate: the lease kind the command needs
	Acquire *AcquireResp `json:"acquire,omitempty"` // with Gate: the answer for that lease
}

type StopResp struct {
	Restart string `json:"restart"`
}

type ApprovalResp struct {
	Approval *model.Approval `json:"approval"`
	Result   any             `json:"result,omitempty"`
	Error    string          `json:"error,omitempty"`
}

type ReviewResp struct {
	Decisions []*model.Decision `json:"decisions"`
	Reviewed  int               `json:"reviewed"`
	Wrong     int               `json:"wrong"`
	// FalseRate is wrong / reviewed among pause and stop decisions, -1 before any review.
	FalseRate float64 `json:"false_rate"`
}

// Status is everything the CLI and the app show.
type Status struct {
	APIVersion    int                        `json:"api_version"`
	DaemonVersion string                     `json:"daemon_version"`
	Mem           sys.MemStats               `json:"mem"`
	HeadroomMB    float64                    `json:"headroom_mb"`
	PendingMB     float64                    `json:"pending_mb"`
	SwapRate      float64                    `json:"swap_rate_mb_min"`
	Waiters       []*model.Waiter            `json:"waiters"`
	Config        model.Config               `json:"config"`
	Leases        map[string]*model.Lease    `json:"leases"`
	Resources     map[string]*model.Resource `json:"resources"`
	Sessions      map[string]*model.Session  `json:"sessions"`
	Questions     map[string]*model.Question `json:"questions"`
	LastSweep     time.Time                  `json:"last_sweep"`
	Events        []model.Event              `json:"events"`
	Learned       map[string]float64         `json:"learned_mb"`
	Stopped       []*model.Stopped           `json:"stopped"`
	Approvals     map[string]*model.Approval `json:"approvals"`
	Sources       map[string]string          `json:"sources"` // source -> "" (ok) or why it failed
	Self          sys.SelfUsage              `json:"self"`
	StartedAt     time.Time                  `json:"started_at"`
	Notes         []string                   `json:"notes,omitempty"` // problems at startup
}
