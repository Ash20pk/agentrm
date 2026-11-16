// Package model holds the types shared by the daemon, the CLI and the app. Their JSON form is
// the wire format and the on-disk format, so field names are stable.
package model

import "time"

// Resource kinds the sweeper knows how to find and act on.
const (
	RKCluster    = "kind-cluster"
	RKContainers = "containers"
	RKSimulator  = "simulator"
	RKGradle     = "gradle-daemon"
	RKServer     = "server"
	RKEmulator   = "android-emulator"
	RKVM         = "vm"
	RKModel      = "model-server"
	RKDatabase   = "database"
	RKBrowser    = "headless-browser"
)

// Lease kinds.
const (
	KCluster     = "cluster"
	KCompose     = "compose"
	KDockerBuild = "docker-build"
	KCargoBuild  = "cargo-build"
	KGradle      = "gradle"
	KXcodeBuild  = "xcode-build"
	KSimulator   = "simulator"
	KEmulator    = "emulator"
	KVM          = "vm"
	KModel       = "model"
	KLoadtest    = "loadtest"
	KServer      = "server"
	KBrowser     = "browser"
	KHeavy       = "heavy" // inferred from process-tree growth, not from the command text
)

// BuildKinds are leases for one command: released when the tool call returns.
var BuildKinds = map[string]bool{KDockerBuild: true, KCargoBuild: true, KGradle: true, KXcodeBuild: true, KLoadtest: true, KHeavy: true}

// DiskKinds write images, layers or build output; they need free disk.
var DiskKinds = map[string]bool{KCluster: true, KCompose: true, KDockerBuild: true, KCargoBuild: true, KGradle: true,
	KXcodeBuild: true, KEmulator: true, KVM: true, KModel: true}

// CPUKinds are refused while the machine is already saturated.
var CPUKinds = map[string]bool{KDockerBuild: true, KCargoBuild: true, KGradle: true, KXcodeBuild: true, KLoadtest: true, KHeavy: true}

// LeaseToResource says which discovered resource kind a lease kind produces.
var LeaseToResource = map[string]string{
	KCluster: RKCluster, KCompose: RKContainers, KGradle: RKGradle, KSimulator: RKSimulator, KServer: RKServer,
	KEmulator: RKEmulator, KVM: RKVM, KModel: RKModel, KBrowser: RKBrowser,
}

// ResourceToLease is the lease kind for an unleased resource attributed to a session.
var ResourceToLease = map[string]string{
	RKCluster: KCluster, RKContainers: KCompose, RKGradle: KGradle, RKSimulator: KSimulator, RKServer: KServer,
	RKEmulator: KEmulator, RKVM: KVM, RKModel: KModel, RKDatabase: KServer, RKBrowser: KBrowser,
}

// AllKinds lists the lease kinds an agent may ask for, for help text and validation.
var AllKinds = []string{KCluster, KCompose, KDockerBuild, KCargoBuild, KGradle, KXcodeBuild, KSimulator, KEmulator,
	KVM, KModel, KLoadtest, KServer, KBrowser}

type Resource struct {
	ID          string     `json:"id"`
	Kind        string     `json:"kind"`
	Source      string     `json:"source"` // which discovery source found it: docker, simctl, procs
	Name        string     `json:"name"`
	Detail      string     `json:"detail"`
	MemMB       float64    `json:"mem_mb"`
	Containers  []string   `json:"containers,omitempty"`
	PID         int        `json:"pid,omitempty"`
	UDID        string     `json:"udid,omitempty"`
	Cwd         string     `json:"cwd,omitempty"` // where its process runs, when known
	FirstSeen   time.Time  `json:"first_seen"`
	LeaseID     string     `json:"lease_id,omitempty"`
	OrphanSince *time.Time `json:"orphan_since,omitempty"`
	Pinned      bool       `json:"pinned,omitempty"`
	Ports       []string   `json:"ports,omitempty"`       // host TCP ports it listens on or publishes
	CPU         float64    `json:"cpu,omitempty"`         // percent, where measured
	NetBytes    float64    `json:"net_bytes,omitempty"`   // cumulative network bytes, for containers
	LastActive  time.Time  `json:"last_active,omitempty"` // last sign of use seen by the sweeper
	StartedBy   string     `json:"started_by,omitempty"`  // session that started it, when known
	PausedAt    *time.Time `json:"paused_at,omitempty"`   // frozen since then, not yet stopped
	Urgent      bool       `json:"urgent,omitempty"`      // a blocked request needs its memory: stop, do not freeze
	Restart     string     `json:"restart,omitempty"`     // how to bring it back after a stop
}

type Lease struct {
	ID        string    `json:"id"`
	Session   string    `json:"session"`
	Agent     string    `json:"agent"`
	Kind      string    `json:"kind"`
	MemGB     float64   `json:"mem_gb"`
	Purpose   string    `json:"purpose"`
	Cmd       string    `json:"cmd,omitempty"`
	Cwd       string    `json:"cwd,omitempty"`
	TTL       int       `json:"ttl_minutes"`
	Created   time.Time `json:"created"`
	Expires   time.Time `json:"expires"`
	Resources []string  `json:"resources,omitempty"`

	Verified  bool       `json:"verified"`
	Evidence  string     `json:"evidence,omitempty"`
	LastUsed  time.Time  `json:"last_used"`
	IdleAsked *time.Time `json:"idle_asked,omitempty"`
	PeakMB    float64    `json:"peak_mb,omitempty"`
	// BaseFootprintMB is the session's process-tree memory when the lease was granted.
	BaseFootprintMB float64 `json:"base_footprint_mb,omitempty"`
	Declared        bool    `json:"declared,omitempty"` // the holder chose MemGB itself
	HadResources    bool    `json:"had_resources,omitempty"`
	Overran         bool    `json:"overran,omitempty"`
	// Until is an end time the holder declared; past it the lease is released, not renewed.
	Until *time.Time `json:"until,omitempty"`
}

type Session struct {
	ID       string    `json:"id"`
	Agent    string    `json:"agent"`
	Cwd      string    `json:"cwd"`
	LastSeen time.Time `json:"last_seen"`
	// FootprintMB is the resident memory of everything running under the agent's process.
	FootprintMB float64   `json:"footprint_mb"`
	AgentPID    int       `json:"agent_pid,omitempty"`
	FlaggedAt   time.Time `json:"flagged_at,omitempty"`
	// DistrustUntil is set when a lease of this session used far more than it declared:
	// until then its own memory figures are ignored.
	DistrustUntil time.Time `json:"distrust_until,omitempty"`
	// Priority: 0 normal, 1 urgent (set by the user). Urgent sessions go ahead in the queue.
	Priority int `json:"priority,omitempty"`
}

type Question struct {
	ResourceID string            `json:"resource_id"`
	Text       string            `json:"text"`
	Created    time.Time         `json:"created"`
	Delivered  map[string]bool   `json:"delivered"`
	Answers    map[string]string `json:"answers"`       // session -> "not mine" etc.
	For        string            `json:"for,omitempty"` // only this session is asked
}

// Stopped remembers what AgentRM stopped, so it can be restored.
type Stopped struct {
	Resource  Resource  `json:"resource"`
	StoppedAt time.Time `json:"stopped_at"`
	Why       string    `json:"why"`
	By        string    `json:"by"` // "agentrm" or the session that asked
}

// Approval is a privileged request from a terminal waiting for a click in the app.
type Approval struct {
	ID      string         `json:"id"`
	What    string         `json:"what"` // human-readable
	Created time.Time      `json:"created"`
	Expires time.Time      `json:"expires"`
	Request map[string]any `json:"request"`
	Path    string         `json:"path"`
	From    string         `json:"from"` // the terminal app it came from
	State   string         `json:"state"`
}

// StateVersion is the current on-disk state format. Bump it and add a migration in store.
const StateVersion = 3

type State struct {
	Version   int                  `json:"version"`
	Leases    map[string]*Lease    `json:"leases"`
	Sessions  map[string]*Session  `json:"sessions"`
	Resources map[string]*Resource `json:"resources"`
	Questions map[string]*Question `json:"questions"`
	Pinned    map[string]bool      `json:"pinned"`
	// Touches records, per resource, when each session last ran a command that used it.
	Touches map[string]map[string]time.Time `json:"touches"`
	// Learned is the observed peak memory per estimate key (kind, or kind@project), in MB.
	Learned map[string]float64 `json:"learned_mb"`
	// LearnedDev is the typical deviation of that peak, so estimates cover the bad runs too.
	LearnedDev map[string]float64 `json:"learned_dev_mb"`
	// AgentSessions remembers which agent process is which session across daemon restarts.
	AgentSessions map[string]string `json:"agent_sessions"`
	// Stopped is what AgentRM stopped, newest last, kept for restoring (bounded).
	Stopped []*Stopped `json:"stopped"`
	// Approvals waiting for the user.
	Approvals map[string]*Approval `json:"approvals"`
	// Decisions records each would-pause/would-stop decision for review.
	Decisions []*Decision `json:"decisions"`
}

// Decision is one pause or stop decision (taken, or that would have been taken in dry run),
// kept so the user can review whether it was right.
type Decision struct {
	ID       string    `json:"id"`
	Time     time.Time `json:"time"`
	Action   string    `json:"action"` // pause, stop, reclaim
	Resource string    `json:"resource"`
	Name     string    `json:"name"`
	MemMB    float64   `json:"mem_mb"`
	Why      string    `json:"why"`
	DryRun   bool      `json:"dry_run"`
	Verdict  string    `json:"verdict,omitempty"` // right, wrong, or empty when not reviewed
	Note     string    `json:"note,omitempty"`
}

func NewState() State {
	return State{Version: StateVersion, Leases: map[string]*Lease{}, Sessions: map[string]*Session{},
		Resources: map[string]*Resource{}, Questions: map[string]*Question{}, Pinned: map[string]bool{},
		Touches: map[string]map[string]time.Time{}, Learned: map[string]float64{}, LearnedDev: map[string]float64{},
		AgentSessions: map[string]string{}, Approvals: map[string]*Approval{}}
}

type Event struct {
	Seq  int       `json:"seq"`
	Time time.Time `json:"time"`
	Kind string    `json:"kind"` // first word: GRANT, ORPHAN, STOPPED, ...
	Text string    `json:"text"`
}

type Waiter struct {
	Session  string    `json:"session"`
	Kind     string    `json:"kind"`
	NeedMB   float64   `json:"need_mb"`
	Since    time.Time `json:"since"`
	LastSeen time.Time `json:"last_seen"`
	Priority int       `json:"priority,omitempty"`
}
