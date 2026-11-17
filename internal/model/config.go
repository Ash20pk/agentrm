package model

type Config struct {
	HeadroomGB           float64 `json:"headroom_gb"`            // memory kept free after any grant
	MaxSwapMBPerMin      float64 `json:"max_swap_mb_per_min"`    // refuse new leases while swapping out faster than this
	PressureGraceMinutes int     `json:"pressure_grace_minutes"` // deadline for holders when another request is blocked
	PressureIdleMinutes  int     `json:"pressure_idle_minutes"`  // a lease unused this long can be reclaimed for a blocked request
	PauseMinutes         int     `json:"pause_minutes"`          // how long an unclaimed resource stays frozen before it is stopped; 0 stops directly
	UnleasedGB           float64 `json:"unleased_gb"`            // flag a session using this much with no lease
	HeavyGrowthGB        float64 `json:"heavy_growth_gb"`        // process-tree growth that counts as heavy work, whatever the command
	GraceMinutes         int     `json:"grace_minutes"`          // how long an orphan may run unclaimed
	DefaultTTLMinutes    int     `json:"default_ttl_minutes"`    // lease lifetime, renewed while used
	BuildTTLMinutes      int     `json:"build_ttl_minutes"`      // builds are not renewed
	SweepSeconds         int     `json:"sweep_seconds"`          // sampling interval when nothing is leased
	FastSweepSeconds     int     `json:"fast_sweep_seconds"`     // sampling interval while a lease is active or a request waits
	DryRun               bool    `json:"dry_run"`                // ask and notify, but never pause or stop anything
	// AutoStop turns pausing and stopping on per level while dry_run is on: owner_gone, then
	// unclaimed, then idle_lease (see manager/trust.go). dry_run false turns all of them on.
	AutoStop      map[string]bool    `json:"auto_stop,omitempty"`
	KindMemGB     map[string]float64 `json:"kind_mem_gb"`
	ServerNames   []string           `json:"server_names"`   // listening processes treated as dev servers
	ServerExclude []string           `json:"server_exclude"` // substrings of args that are never dev servers
	NeverStop     []string           `json:"never_stop"`     // resource IDs or name substrings left alone
	// ExtraPatterns adds command regexes per lease kind, e.g. {"compose": ["my-stack\\.sh\\s+start"]}.
	ExtraPatterns map[string][]string `json:"extra_patterns"`

	IdleMinutes          int     `json:"idle_minutes"`           // a held resource unused this long gets its holder asked
	ClaimWindowMinutes   int     `json:"claim_window_minutes"`   // a claim is verified if the session used the resource this recently
	UnverifiedTTLMinutes int     `json:"unverified_ttl_minutes"` // lifetime of a claim with no evidence behind it
	MinDiskGB            float64 `json:"min_disk_gb"`            // refuse disk-heavy leases below this free space on /
	MaxLoadPerCore       float64 `json:"max_load_per_core"`      // refuse builds above this 1-minute load per core
	MaxSessionShare      float64 `json:"max_session_share"`      // while others wait, one session may hold at most this share of usable memory
	RequireAppApproval   bool    `json:"require_app_approval"`   // privileged actions from a terminal need a click in the app
	AutoThawOnConnect    bool    `json:"auto_thaw_on_connect"`   // resume a paused server when something tries to connect to it
	LogLevel             string  `json:"log_level"`              // debug, info, warn, error

	// The collector (see docs/how-it-works.md, "Garbage collection").
	OwnerGoneGraceMinutes int     `json:"owner_gone_grace_minutes"` // unclaimed, and the agent that started or held it has exited
	OwnerGonePauseMinutes int     `json:"owner_gone_pause_minutes"` // how long such a thing stays paused before it is stopped
	OldGenerationHours    int     `json:"old_generation_hours"`     // running this long with no known owner: slower, and always asked first
	LeftoverMinMB         float64 `json:"leftover_min_mb"`          // a process an exited agent left running is tracked from this size

	// The queue (see docs/how-it-works.md, "Contention").
	QueueMinutes     int `json:"queue_minutes"`      // a queued request keeps its place this long after its session was last heard from
	GrantHoldMinutes int `json:"grant_hold_minutes"` // a lease granted from the queue is held this long for its session to take up

	// Learned stop timing (see manager/timing.go): the wait before pausing something unclaimed
	// is chosen from how long things of its kind sit idle before being used again.
	LearnedTiming    bool    `json:"learned_timing"`
	MinTimingSamples int     `json:"min_timing_samples"` // observations per kind before it is used
	RestartWeightGB  float64 `json:"restart_weight_gb"`  // a minute spent restarting is worth this many GB-minutes

	// Backfilling: a short build may run ahead of a larger waiting request if it fits in free
	// memory and is expected to finish within backfill_max_minutes.
	Backfill           bool `json:"backfill"`
	BackfillMaxMinutes int  `json:"backfill_max_minutes"`

	// Traces (see docs/replay.md): what the daemon sees, kept on this Mac for replaying policies.
	RecordTrace bool `json:"record_trace"` // off by default: traces hold command lines (secrets redacted)
	TraceDays   int  `json:"trace_days"`   // daily trace files kept
}

// DefaultConfig is sized for a 16 GB Mac; ScaleToMachine adjusts it to the real one.
func DefaultConfig() Config {
	return Config{
		HeadroomGB: 1, MaxSwapMBPerMin: 300, PressureGraceMinutes: 2, PressureIdleMinutes: 5, UnleasedGB: 2,
		HeavyGrowthGB: 1.5, PauseMinutes: 30, GraceMinutes: 10, DefaultTTLMinutes: 120, BuildTTLMinutes: 30,
		SweepSeconds: 30, FastSweepSeconds: 3, DryRun: true, ExtraPatterns: map[string][]string{},
		IdleMinutes: 30, ClaimWindowMinutes: 60, UnverifiedTTLMinutes: 30, MinDiskGB: 10, MaxLoadPerCore: 1.5,
		MaxSessionShare: 0.6, RequireAppApproval: true, AutoThawOnConnect: true, LogLevel: "info",
		OwnerGoneGraceMinutes: 2, OwnerGonePauseMinutes: 10, OldGenerationHours: 24, LeftoverMinMB: 40,
		QueueMinutes: 10, GrantHoldMinutes: 3, TraceDays: 14,
		LearnedTiming: true, MinTimingSamples: 20, RestartWeightGB: 4, Backfill: true, BackfillMaxMinutes: 5,
		KindMemGB: map[string]float64{
			KCluster: 3, KCompose: 2, KDockerBuild: 3, KCargoBuild: 2.5, KGradle: 3, KXcodeBuild: 3,
			KSimulator: 1.5, KEmulator: 2.5, KVM: 4, KModel: 5, KLoadtest: 2, KServer: 0.5, KBrowser: 0.8,
			KHeavy: 1.5,
		},
		ServerNames:   []string{"node", "python", "python3*", "ruby", "deno", "bun", "php", "uvicorn", "gunicorn", "java", "air", "main"},
		ServerExclude: []string{"mcp", "claude", "codex", "copilot", "language-server", "langserver", "tsserver", "gopls", "rust-analyzer"},
	}
}

// ScaleToMachine sizes the memory-related defaults to the RAM of this Mac. The 16 GB values
// are the reference; a bigger machine keeps proportionally more free and expects bigger jobs.
// It only touches values still at their defaults, so the user's own settings win.
func ScaleToMachine(c *Config, totalMB float64) {
	if totalMB <= 0 {
		return
	}
	ref := DefaultConfig()
	factor := totalMB / (16 * 1024)
	// Headroom grows with the machine but not linearly: 0.75 GB on 8 GB, 1 on 16, 2 on 32, 3 on 64.
	if c.HeadroomGB == ref.HeadroomGB {
		switch {
		case totalMB <= 8*1024:
			c.HeadroomGB = 0.75
		case totalMB <= 16*1024:
			c.HeadroomGB = 1
		case totalMB <= 32*1024:
			c.HeadroomGB = 2
		default:
			c.HeadroomGB = 3
		}
	}
	if c.UnleasedGB == ref.UnleasedGB {
		c.UnleasedGB = clamp(ref.UnleasedGB*factor, 1, 8)
	}
	if c.HeavyGrowthGB == ref.HeavyGrowthGB {
		c.HeavyGrowthGB = clamp(ref.HeavyGrowthGB*factor, 1, 6)
	}
	// A swap-out rate that means thrashing on 8 GB is noise on 64 GB, and vice versa.
	if c.MaxSwapMBPerMin == ref.MaxSwapMBPerMin {
		c.MaxSwapMBPerMin = clamp(ref.MaxSwapMBPerMin*factor, 150, 1200)
	}
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
