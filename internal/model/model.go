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
	// RKLeftover is a process an agent started that kept running after the agent exited:
	// re-parented to launchd, with no known kind. Found by the collector, not by discovery.
	RKLeftover = "leftover-process"
	// RKStaleAgent is an agent process with no terminal or app above it that has not been
	// heard from in hours. Only ever asked about: it may be someone's tmux session.
	RKStaleAgent = "stale-agent"
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
	// Owner is the session that started or last held it, once that session is gone, and
	// OwnerGoneAt when it went: the collector's strongest evidence that nobody needs it.
	Owner       string     `json:"owner,omitempty"`
	OwnerGoneAt *time.Time `json:"owner_gone_at,omitempty"`
	// AskOnly: never paused or stopped by AgentRM, only asked about (an app an agent happened
	// to launch, such as Docker Desktop; an agent process).
	AskOnly bool `json:"ask_only,omitempty"`
	// ReleasedIdle: its lease was released because its holder let it sit idle and did not
	// answer (the idle_lease auto-stop level).
	ReleasedIdle bool `json:"released_idle,omitempty"`
	// RestartArgv, RestartEnv and Cwd are how a stopped leftover is started again: exactly
	// the command, environment (without secrets) and directory it was running with.
	RestartArgv []string `json:"restart_argv,omitempty"`
	RestartEnv  []string `json:"restart_env,omitempty"`
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
	// Offered is when the lease was granted from the queue, until its session takes it up. A
	// lease nobody takes up goes to the next in the queue.
	Offered *time.Time `json:"offered,omitempty"`
	// The task it was granted for: the repository and branch its session was on. When the
	// task looks finished (a pull request opened, the branch left), TaskDoneAt is set: the
	// holder is asked, and unless the lease is used again it is released.
	Repo        string     `json:"repo,omitempty"`
	Branch      string     `json:"branch,omitempty"`
	TaskDoneAt  *time.Time `json:"task_done_at,omitempty"`
	TaskDoneWhy string     `json:"task_done_why,omitempty"`
	// ExpectedEnd is when leases of this kind in this project usually end (learned);
	// LikelyDone is set once it has outlived nine in ten of them and is idle.
	ExpectedEnd *time.Time `json:"expected_end,omitempty"`
	LikelyDone  bool       `json:"likely_done,omitempty"`
	// Backfill: granted ahead of a larger waiting request, because it is expected to finish
	// before that request could start.
	Backfill bool `json:"backfill,omitempty"`
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
	// AgentStart is when the agent process started: with AgentPID it tells a live agent from
	// a reused PID.
	AgentStart time.Time `json:"agent_start,omitempty"`
	// EndedAt is when the session ended: it said so (SessionEnd), or its agent process exited.
	// An ended session is no longer a root: nothing it held or started is kept alive by it.
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	EndReason string     `json:"end_reason,omitempty"`
	// Yields this session offered whose outcome is known, and how many of those it got wrong
	// (said it was done, then used it again). Its later offers are weighted by the record.
	YieldReports    int `json:"yield_reports,omitempty"`
	YieldMisreports int `json:"yield_misreports,omitempty"`
	// Where it is working now.
	Repo   string `json:"repo,omitempty"`
	Branch string `json:"branch,omitempty"`
}

// Honesty is how far a session's yield offers can be believed: Laplace-smoothed share of its
// offers that turned out true (2/3 with no record, so one lie halves it).
func (s *Session) Honesty() float64 {
	return float64(s.YieldReports-s.YieldMisreports+2) / float64(s.YieldReports+3)
}

// Yield is a holder's answer to "what would giving up this lease cost you right now?", and what
// happened after: whether it used the thing again, and how long bringing it back really took.
type Yield struct {
	ID             string     `json:"id"`
	Lease          string     `json:"lease"`
	Session        string     `json:"session"`
	Kind           string     `json:"kind"`
	Resources      []string   `json:"resources,omitempty"`
	Cost           string     `json:"cost"` // none, low, high
	RestartSeconds int        `json:"restart_seconds,omitempty"`
	At             time.Time  `json:"at"`
	Taken          *time.Time `json:"taken,omitempty"` // its lease was released or its resource stopped
	UsedAgainAt    *time.Time `json:"used_again_at,omitempty"`
	RestartBegan   *time.Time `json:"restart_began,omitempty"`
	RestartedAt    *time.Time `json:"restarted_at,omitempty"`
	Verdict        string     `json:"verdict,omitempty"` // true, false
}

// Prediction is the command-stream model (see manager/predict.go).
type Prediction struct {
	// Next[project][previous kind][next kind] counts transitions ("" is a session's start).
	Next map[string]map[string]map[string]int `json:"next"`
	// Lifetimes[kind@project] and [kind]: recent lease lifetimes, in minutes.
	Lifetimes map[string][]float64 `json:"lifetimes"`
	// Last is each session's last heavy kind.
	Last map[string]string `json:"last"`
	// Score: predictions made, right, and how the most-frequent-kind baseline did on the same.
	Total    int `json:"total"`
	Hits     int `json:"hits"`
	BaseHits int `json:"base_hits"`
}

// Leftover is a process an ended session started that is still running.
type Leftover struct {
	PID     int       `json:"pid"`
	Start   time.Time `json:"start"`
	Session string    `json:"session"`
	Agent   string    `json:"agent"`
	Found   time.Time `json:"found"`
	// How it was found: "tree" (seen under the agent) or "env" (the agent's session in its
	// environment, for processes that detached before they were seen, or ended before AgentRM ran).
	Via     string   `json:"via,omitempty"`
	AskOnly bool     `json:"ask_only,omitempty"`
	Argv    []string `json:"argv,omitempty"`
	Env     []string `json:"env,omitempty"`
	Cwd     string   `json:"cwd,omitempty"`
}

// TreeProc is one process an agent started: which process (PID and start) and the most memory
// it was seen using. The peak matters because macOS compresses an idle process's memory, so a
// leftover that sits idle soon shows a fraction of what it holds.
type TreeProc struct {
	Start  time.Time `json:"start"`
	PeakMB float64   `json:"peak_mb,omitempty"`
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
	// Leftovers are processes ended sessions left running, keyed by PID.
	Leftovers map[string]*Leftover `json:"leftovers,omitempty"`
	// Trees is, per live session, every process its agent started, so a daemon restart does
	// not forget what has detached.
	Trees map[string]map[int]TreeProc `json:"trees,omitempty"`
	// Yields are recent answers to "what would giving it up cost you?" (the newest 200).
	Yields []*Yield `json:"yields,omitempty"`
	// RestartSeconds is, per resource kind, how long bringing one back took, as measured.
	RestartSeconds map[string]float64 `json:"restart_seconds,omitempty"`
	// Predict is what the command stream has taught: which heavy kind follows which, per
	// project, and how long leases live.
	Predict *Prediction `json:"predict,omitempty"`
	// IdleGaps is, per resource kind, how long things sat idle before being used again, in
	// minutes (-1: never used again). Learned stop timing works from it.
	IdleGaps map[string][]float64 `json:"idle_gaps,omitempty"`
	// LastReceipt is when the weekly receipt was last sent.
	LastReceipt time.Time `json:"last_receipt,omitempty"`
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
	// Category is the auto-stop level it belongs to: owner_gone, unclaimed, idle_lease.
	Category string `json:"category,omitempty"`
	Verdict  string `json:"verdict,omitempty"` // right, wrong, or empty when not reviewed
	Note     string `json:"note,omitempty"`
}

func NewState() State {
	return State{Version: StateVersion, Leases: map[string]*Lease{}, Sessions: map[string]*Session{},
		Resources: map[string]*Resource{}, Questions: map[string]*Question{}, Pinned: map[string]bool{},
		Touches: map[string]map[string]time.Time{}, Learned: map[string]float64{}, LearnedDev: map[string]float64{},
		AgentSessions: map[string]string{}, Approvals: map[string]*Approval{}, Leftovers: map[string]*Leftover{},
		Trees: map[string]map[int]TreeProc{}}
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
	// What was asked for, so the lease can be granted when it fits without being asked again.
	Agent        string `json:"agent,omitempty"`
	Purpose      string `json:"purpose,omitempty"`
	Cmd          string `json:"cmd,omitempty"`
	Cwd          string `json:"cwd,omitempty"`
	Via          string `json:"via,omitempty"`
	TTLMinutes   int    `json:"ttl_minutes,omitempty"`
	UntilMinutes int    `json:"until_minutes,omitempty"`
	Declared     bool   `json:"declared,omitempty"`
	// Retry marks a request only its own retry can complete (a restore has to start the thing
	// as well), so no lease is granted for it from the queue.
	Retry bool `json:"retry,omitempty"`
	// Pressed is when holders were last pressed to make room for it.
	Pressed time.Time `json:"-"`
}
