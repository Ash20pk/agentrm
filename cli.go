package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"agentrm/internal/api"
	"agentrm/internal/manager"
	"agentrm/internal/model"
	"agentrm/internal/setup"
	"agentrm/internal/store"
	"agentrm/internal/trace"
)

const usage = `agentrm — a resource manager for coding agents on this Mac

Leases
  agentrm acquire --kind K --purpose "why" [--mem GB] [--ttl MIN] [--until MIN] [--wait]
  agentrm release [LEASE] | --all             release a lease, or all of yours
  agentrm claim RESOURCE --purpose "why" [--until MIN]   adopt something already running
  agentrm disown RESOURCE                      "not mine", answering an orphan question
  agentrm yield LEASE --cost none|low|high [--restart-seconds N]
                                               what giving up a lease would cost you now (answers AgentRM)
  agentrm inbox                                questions waiting for you
  agentrm whoami                               who the daemon thinks you are

Looking
  agentrm status [--json] [--stopped] | top    memory, leases, what is running
  agentrm review [--all] [--json]              pause/stop decisions to mark right or wrong
  agentrm review mark ID right|wrong [--note "..."]
  agentrm receipt [--days 7] [--json]          what AgentRM did: freed, found, waited, restored
  agentrm collect                              stop now what ended sessions left (asks for approval)

Measuring policies (see docs/replay.md)
  agentrm record on|off                        keep a trace of what the daemon sees (~/.agentrm/traces)
  agentrm replay [TRACE] [--policy agentrm|reaper] [--config FILE] [--json]
                                               run a policy over a trace: wrong stops, memory freed, waits
  agentrm compare [TRACE] [--config FILE]      AgentRM and a parent-PID reaper on the same trace

Acting (pinning, settings and overriding others ask for approval in the app)
  agentrm bypass [MINUTES|off]                 let everything through for a while (default 60 min)
  agentrm stop RESOURCE                        stop something now (it can be restored)
  agentrm resume RESOURCE | --all              thaw something AgentRM paused
  agentrm restore RESOURCE | --last            bring back something AgentRM stopped
  agentrm leak accept|dismiss LEAK|PID         restart (or stop) a process flagged as leaking, or leave it
  agentrm pin RESOURCE | unpin RESOURCE        never / again pause or stop it
  agentrm priority SESSION urgent|normal       an urgent session goes first in the queue
  agentrm config [--dry-run=true|false] [--grace MIN] [--headroom GB] [--idle MIN] [--pause MIN]

Vault (short-lived access to accounts; see agentrm vault)
  agentrm vault list                           accounts you may ask for, and your grants
  agentrm vault run ACCOUNT --purpose "why" -- COMMAND   run with access, approved with Touch ID
  agentrm vault revoke GRANT | --all           end access early

Setup
  agentrm setup claude|codex|gemini|cursor|path|shim [--uninstall]
                                               an agent's hooks, the CLI link, or the PATH shim
  agentrm setup preview claude|codex|gemini|cursor   what installing the hooks would change
  agentrm setup check [--json]                 what is installed, and what needs repair
  agentrm setup repair                         point everything at this copy of AgentRM
  agentrm setup uninstall                      remove everything AgentRM installed
  agentrm setup mcp                            how to add AgentRM to an MCP client

  agentrm daemon | hook EVENT | mcp | version

Kinds: ` + "cluster compose docker-build cargo-build gradle xcode-build simulator emulator vm model loadtest server browser" + `
`

func runCLI(cmd string, args []string) int {
	var pos []string
	for len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		pos, args = append(pos, args[0]), args[1:]
	}
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	kind := fs.String("kind", "", "lease kind")
	purpose := fs.String("purpose", "", "why you need it")
	mem := fs.Float64("mem", 0, "memory in GB (default: AgentRM's estimate)")
	ttl := fs.Int("ttl", 0, "lease lifetime in minutes")
	until := fs.Int("until", 0, "you need it for this many minutes; the lease ends then")
	wait := fs.Bool("wait", false, "if queued, stay until the lease is granted (max 30 min)")
	all := fs.Bool("all", false, "all of yours / everything")
	last := fs.Bool("last", false, "the most recent one")
	asJSON := fs.Bool("json", false, "machine-readable output")
	stopped := fs.Bool("stopped", false, "also list what AgentRM stopped")
	uninstall := fs.Bool("uninstall", false, "remove instead of install")
	dryRun := fs.String("dry-run", "", "true or false")
	grace := fs.Int("grace", 0, "minutes an orphan may run unclaimed")
	headroom := fs.Float64("headroom", 0, "GB kept free after any grant")
	idle := fs.Int("idle", 0, "minutes unused before a holder is asked")
	pause := fs.Int("pause", -1, "minutes frozen before an unclaimed resource is stopped (0 = stop directly)")
	note := fs.String("note", "", "a note for a review mark")
	policy := fs.String("policy", "agentrm", "replay: agentrm or reaper")
	cost := fs.String("cost", "", "yield: none, low or high")
	days := fs.Int("days", 7, "receipt: how many days back")
	autoStop := fs.String("auto-stop", "", "config: level=on|off (owner_gone, unclaimed, idle_lease, engine_restart, xcode_helpers)")
	restartSecs := fs.Int("restart-seconds", 0, "yield: how long bringing it back would take")
	configFile := fs.String("config", "", "replay: a config.json to replay with (default: this Mac's)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	pos = append(pos, fs.Args()...)
	arg := func(i int) string {
		if i < len(pos) {
			return pos[i]
		}
		return ""
	}
	req := api.Req{Via: "cli"}
	req.Cwd, _ = os.Getwd()

	var err error
	switch cmd {
	case "version", "--version", "-v":
		fmt.Println("agentrm", version)
	case "help", "--help", "-h":
		fmt.Print(usage)
	case "acquire":
		if *kind == "" || *purpose == "" {
			return fail(fmt.Errorf("--kind and --purpose are required"))
		}
		req.Kind, req.Purpose, req.MemGB, req.TTLMinutes, req.UntilMinutes = *kind, *purpose, *mem, *ttl, *until
		deadline := time.Now().Add(30 * time.Minute)
		for {
			var r api.AcquireResp
			asked, held := time.Now(), req.WaitSeconds > 0
			// With WaitSeconds the daemon keeps a queued request connected and answers when the
			// lease is granted, so there is nothing to poll.
			if err = api.CallTimeout(time.Duration(req.WaitSeconds)*time.Second+5*time.Second, "/acquire", req, &r); err != nil {
				break
			}
			if r.Granted {
				fmt.Printf("granted lease %s: %s, %.1f GB%s\n", r.Lease.ID, r.Lease.Kind, r.Lease.MemGB,
					map[bool]string{true: " (reused)"}[r.Reused])
				fmt.Printf("release it when done: agentrm release %s\n", r.Lease.ID)
				break
			}
			left := time.Until(deadline)
			if !*wait || left <= 0 {
				fmt.Fprintln(os.Stderr, "refused:", r.Reason)
				return 2
			}
			if req.WaitSeconds == 0 {
				fmt.Fprintf(os.Stderr, "waiting (number %d in the queue): %s\n", r.Queue, trunc(r.Reason, 160))
			}
			req.WaitSeconds = max(1, int(left.Seconds()))
			// Not queued (a full disk, a busy CPU), or a daemon that does not hold requests:
			// it answered at once, so pace the retries here.
			if held && time.Since(asked) < 5*time.Second {
				time.Sleep(min(20*time.Second, left))
			}
		}
	case "release":
		req.LeaseID = arg(0)
		if req.LeaseID == "" && !*all {
			return fail(fmt.Errorf("give a lease id, or --all for every lease of yours"))
		}
		var r api.CountResp
		if err = callApproved("/release", req, &r); err == nil {
			fmt.Printf("released %d lease(s)\n", r.N)
		}
	case "renew":
		var r api.CountResp
		if err = api.Call("/renew", req, &r); err == nil {
			fmt.Printf("renewed %d lease(s)\n", r.N)
		}
	case "claim":
		req.ResourceID, req.Purpose, req.TTLMinutes, req.UntilMinutes = arg(0), *purpose, *ttl, *until
		var r api.ClaimResp
		if err = api.Call("/claim", req, &r); err == nil {
			v := "verified"
			if !r.Verified {
				v = "UNVERIFIED"
			}
			fmt.Printf("%s is held by lease %s (%s: %s)\n", req.ResourceID, r.Lease.ID, v, r.Evidence)
			fmt.Println("it stays held while you use it; unused for a while, you will be asked again")
		}
	case "yield":
		req.LeaseID, req.Cost, req.RestartSeconds = arg(0), *cost, *restartSecs
		var r api.CountResp
		if err = api.Call("/yield", req, &r); err == nil {
			fmt.Println(r.Note)
		}
	case "disown":
		req.ResourceID = arg(0)
		if err = api.Call("/disown", req, nil); err == nil {
			fmt.Println("noted: not yours")
		}
	case "whoami":
		var c api.Caller
		if err = api.Call("/whoami", req, &c); err == nil {
			b, _ := json.Marshal(c)
			fmt.Println(string(b))
		}
	case "inbox":
		req.All = true
		var r api.InboxResp
		if err = api.Call("/inbox", req, &r); err == nil {
			if len(r.Questions) == 0 {
				fmt.Println("no questions")
			}
			for _, q := range r.Questions {
				fmt.Println(q)
			}
		}
	case "pin", "unpin":
		on := cmd == "pin"
		req.ResourceID, req.Pinned = arg(0), &on
		if err = callApproved("/pin", req, nil); err == nil {
			fmt.Printf("%sned %s\n", cmd, req.ResourceID)
		}
	case "priority":
		p := map[string]int{"urgent": 1, "normal": 0}
		v, ok := p[arg(1)]
		if arg(0) == "" || !ok {
			return fail(fmt.Errorf("usage: agentrm priority SESSION urgent|normal"))
		}
		req.ResourceID, req.Priority = arg(0), &v
		if err = callApproved("/priority", req, nil); err == nil {
			fmt.Printf("%s is now %s\n", arg(0), arg(1))
		}
	case "status":
		if *asJSON {
			var raw json.RawMessage
			if err = api.Call("/status", req, &raw); err == nil {
				fmt.Println(string(raw))
			}
			break
		}
		err = printStatus(*stopped)
	case "top":
		for {
			fmt.Print("\033[H\033[2J")
			if err = printStatus(false); err != nil {
				break
			}
			time.Sleep(2 * time.Second)
		}
	case "stop":
		req.ResourceID = arg(0)
		var r api.StopResp
		if err = callApproved("/stop", req, &r); err == nil {
			fmt.Printf("stopping %s. Bring it back with: %s\n", req.ResourceID, r.Restart)
		}
	case "resume":
		req.ResourceID, req.All = arg(0), *all
		var r api.CountResp
		if err = api.Call("/resume", req, &r); err == nil {
			fmt.Printf("resumed %d\n", r.N)
		}
	case "restore":
		req.ResourceID, req.All = arg(0), *last
		var r api.AcquireResp
		if err = api.CallTimeout(30*time.Second, "/restore", req, &r); err == nil {
			if !r.Granted {
				fmt.Fprintln(os.Stderr, "refused:", r.Reason)
				return 2
			}
			fmt.Printf("restoring; it is held by lease %s\n", r.Lease.ID)
		}
	case "config":
		req.GraceMinutes, req.HeadroomGB, req.IdleMinutes = *grace, *headroom, *idle
		if *dryRun != "" {
			v, perr := parseBool(*dryRun)
			if perr != nil {
				return fail(fmt.Errorf("--dry-run: %w", perr))
			}
			req.DryRun = &v
		}
		if *pause >= 0 {
			req.PauseMinutes = pause
		}
		if *autoStop != "" {
			level, s, ok := strings.Cut(*autoStop, "=")
			v, perr := parseBool(s)
			if !ok || level == "" || perr != nil {
				return fail(fmt.Errorf("--auto-stop wants level=on|off, got %q", *autoStop))
			}
			req.AutoStop = map[string]bool{level: v}
		}
		var cfg model.Config
		if err = callApproved("/config", req, &cfg); err == nil {
			b, _ := json.MarshalIndent(cfg, "", "  ")
			fmt.Println(string(b))
		}
	case "review":
		if arg(0) == "mark" {
			req.DecisionID, req.Verdict, req.Note = arg(1), arg(2), *note
		}
		req.All = *all
		var r api.ReviewResp
		if err = api.Call("/review", req, &r); err == nil {
			if *asJSON {
				b, _ := json.Marshal(r)
				fmt.Println(string(b))
				break
			}
			printReview(&r)
		}
	case "leak":
		ok := arg(0) == "accept"
		if (!ok && arg(0) != "dismiss") || arg(1) == "" {
			return fail(fmt.Errorf("usage: agentrm leak accept|dismiss LEAK|PID (see agentrm status)"))
		}
		req.ResourceID, req.Approve = arg(1), &ok
		var r api.CountResp
		if err = callApproved("/leak", req, &r); err == nil {
			fmt.Println(r.Note)
		}
	case "approve", "deny":
		ok := cmd == "approve"
		req.ApprovalID, req.Approve = arg(0), &ok
		var r api.ApprovalResp
		if err = api.Call("/approve", req, &r); err == nil {
			fmt.Printf("%s: %s\n", r.Approval.ID, r.Approval.State)
		}
	case "setup":
		err = runSetup(arg(0), arg(1), *uninstall, *asJSON)
	case "bypass":
		// agentrm bypass [MINUTES|off]: let everything through for a while (default an hour).
		n := 60
		switch a := arg(0); a {
		case "", "on":
		case "off", "0":
			n = 0
		default:
			v, perr := strconv.Atoi(a)
			if perr != nil || v < 0 {
				return fail(fmt.Errorf("agentrm bypass [MINUTES|off]"))
			}
			n = v
		}
		req.UntilMinutes = n
		var r api.BypassResp
		if err = api.Call("/bypass", req, &r); err == nil {
			if r.Until.IsZero() {
				fmt.Println("AgentRM is managing memory again")
			} else {
				fmt.Printf("everything is let through until %s; nothing is paused or stopped automatically. End it early: agentrm bypass off\n",
					r.Until.Format("15:04"))
			}
		}
	case "collect":
		var r api.CountResp
		if err = callApproved("/collect", req, &r); err == nil {
			fmt.Printf("collecting %d thing(s) ended sessions left (%s)\n", r.N, r.Note)
		}
	case "receipt":
		req.Days = *days
		var r api.Receipt
		if err = api.Call("/receipt", req, &r); err == nil {
			if *asJSON {
				b, _ := json.Marshal(r)
				fmt.Println(string(b))
				break
			}
			fmt.Printf("%s to %s: %s\n", r.Since.Format("Jan 2"), r.Until.Format("Jan 2"), r.Text)
		}
	case "record":
		on := arg(0) == "on"
		if !on && arg(0) != "off" {
			return fail(fmt.Errorf("agentrm record on|off"))
		}
		req.RecordTrace = &on
		var r model.Config
		if err = callApproved("/config", req, &r); err == nil {
			fmt.Printf("recording: %v (traces in %s, kept %d days)\n", r.RecordTrace, trace.Dir(store.Dir()), r.TraceDays)
		}
	case "replay", "compare":
		err = runReplay(cmd, arg(0), *policy, *configFile, *asJSON)
	default:
		fmt.Print(usage)
		return 2
	}
	return fail(err)
}

// parseBool reads a yes/no flag value. Anything it does not recognise is an error, never a
// silent "false": `--dry-run=yes` once turned dry run off.
func parseBool(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "t", "yes", "y", "on", "1":
		return true, nil
	case "false", "f", "no", "n", "off", "0":
		return false, nil
	}
	return false, fmt.Errorf("%q is not true or false (also accepted: yes/no, on/off, 1/0)", s)
}

func fail(err error) int {
	if err == nil {
		return 0
	}
	switch {
	case errors.Is(err, api.ErrDaemonDown):
		fmt.Fprintln(os.Stderr, "agentrm: the daemon is not running. Open AgentRM, or check System Settings → General → Login Items.")
	default:
		fmt.Fprintln(os.Stderr, "agentrm:", err)
	}
	return 1
}

// callApproved makes a privileged call; from a terminal it waits for a click in the app.
func callApproved(path string, req api.Req, out any) error {
	err := api.Call(path, req, out)
	var na *api.NeedsApproval
	if !errors.As(err, &na) {
		return err
	}
	fmt.Fprintf(os.Stderr, "%s\nWaiting for your answer in AgentRM (2 minutes)…\n", na.Message)
	deadline := time.Now().Add(2*time.Minute + 5*time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		var r api.ApprovalResp
		if err := api.Call("/approval", api.Req{ApprovalID: na.ID}, &r); err != nil {
			return err
		}
		switch r.Approval.State {
		case "approved":
			if out != nil && r.Result != nil {
				b, _ := json.Marshal(r.Result)
				_ = json.Unmarshal(b, out)
			}
			return nil
		case "failed":
			return errors.New(r.Error)
		case "denied":
			return fmt.Errorf("denied in AgentRM")
		case "expired":
			return fmt.Errorf("not answered in time")
		}
	}
	return fmt.Errorf("not answered in time")
}

func runSetup(what, sub string, uninstall, asJSON bool) error {
	done := func(err error) error {
		if err == nil {
			fmt.Printf("setup %s: %s\n", what, map[bool]string{true: "removed", false: "installed"}[uninstall])
		}
		return err
	}
	switch what {
	case "claude":
		return done(setup.SetupClaude(uninstall))
	case "codex", "gemini", "cursor":
		if what == "codex" {
			if err := setup.SetupCodex(uninstall); err != nil { // the AGENTS.md note
				return err
			}
		}
		if !uninstall {
			fmt.Print(setup.PreviewAgentHooks(what))
		}
		return done(setup.SetupAgentHooks(what, uninstall))
	case "path":
		return done(setup.SetupPath(uninstall))
	case "shim":
		if err := done(setup.SetupShim(uninstall)); err != nil {
			return err
		}
		if !uninstall {
			fmt.Println("New shells put", setup.ShimDir(), "first on PATH. Agents started from now on are checked; restart running ones.")
		}
		return nil
	case "preview":
		if sub != "" && sub != "claude" {
			fmt.Print(setup.PreviewAgentHooks(sub))
			return nil
		}
		s, err := setup.PreviewClaude(uninstall)
		if err == nil {
			fmt.Print(s)
		}
		return err
	case "check":
		c := setup.RunCheck()
		if asJSON {
			b, _ := json.MarshalIndent(c, "", "  ")
			fmt.Println(string(b))
			return nil
		}
		yes := map[bool]string{true: "yes", false: "no"}
		fmt.Printf("binary          %s\ndaemon running  %s\nClaude hooks    %s (current: %s)\nCodex note      %s\nCLI link        %s\nPATH shim       %s\n",
			c.Binary, yes[c.DaemonRunning], yes[c.ClaudeHooks], yes[c.ClaudeHooksCurrent], yes[c.CodexNote], orNone(c.CLILink), yes[c.Shim])
		for _, p := range c.Problems {
			fmt.Println("PROBLEM:", p)
		}
		return nil
	case "repair":
		if err := setup.Repair(); err != nil {
			return err
		}
		fmt.Println("repaired: everything installed now points at", setup.SelfPath())
		return nil
	case "uninstall":
		r := setup.Uninstall()
		for _, d := range r.Done {
			fmt.Println(d)
		}
		for _, e := range r.Errs {
			fmt.Fprintln(os.Stderr, "agentrm:", e)
		}
		if len(r.Errs) > 0 {
			return fmt.Errorf("uninstall did not finish: %d problem(s) above; fix them and run agentrm setup uninstall again", len(r.Errs))
		}
		if len(r.Done) == 0 {
			fmt.Println("nothing of AgentRM's was installed.")
		}
		fmt.Println("To remove the app, quit it and move it to the Bin.")
		return nil
	case "mcp":
		bin := setup.SelfPath()
		fmt.Printf(`Add AgentRM as an MCP server (stdio) to any client:

  command: %s
  args:    ["mcp"]

Claude Code:   claude mcp add agentrm -- %q mcp
Cursor:        ~/.cursor/mcp.json   {"mcpServers": {"agentrm": {"command": %q, "args": ["mcp"]}}}
Gemini CLI:    ~/.gemini/settings.json   {"mcpServers": {"agentrm": {"command": %q, "args": ["mcp"]}}}

AgentRM does not edit other tools' MCP settings itself.
`, bin, bin, bin, bin)
		return nil
	}
	return fmt.Errorf("setup what? claude | codex | path | shim | preview | check | repair | uninstall | mcp")
}

func printReview(r *api.ReviewResp) {
	rate := "no reviews yet"
	if r.FalseRate >= 0 {
		rate = fmt.Sprintf("%d of %d reviewed decisions were wrong (%.1f%%)", r.Wrong, r.Reviewed, 100*r.FalseRate)
	}
	fmt.Println("REVIEW  " + rate)
	if len(r.Decisions) == 0 {
		fmt.Println("  nothing to review")
		return
	}
	for _, d := range r.Decisions {
		v := d.Verdict
		if v == "" {
			v = "-"
		}
		dry := ""
		if d.DryRun {
			dry = " (would)"
		}
		fmt.Printf("  %-8s %s %-7s%-8s %-26s %5.1f GB  %-5s %s\n", d.ID, d.Time.Format("Jan 2 15:04"), d.Action, dry,
			trunc(d.Resource, 26), d.MemMB/1024, v, trunc(d.Why, 60))
	}
	fmt.Println("\nmark one: agentrm review mark ID right|wrong [--note \"why\"]")
}

func printStatus(showStopped bool) error {
	var st api.Status
	if err := api.Call("/status", api.Req{}, &st); err != nil {
		return err
	}
	m := st.Mem
	pressure := map[int]string{1: "normal", 2: "WARNING", 4: "CRITICAL"}[m.PressureLevel]
	fmt.Printf("MEMORY   %.1f GB total · %.1f GB available · %.1f GB compressed · swap %.1f/%.1f GB · pressure %s · swapping out %.0f MB/min\n",
		m.TotalMB/1024, m.AvailableMB/1024, m.CompressedMB/1024, m.SwapUsedMB/1024, m.SwapTotalMB/1024, pressure, st.SwapRate)
	fmt.Printf("SYSTEM   disk %.0f GB free · load %.1f on %d cores\n", m.DiskFreeGB, m.Load1, m.Cores)
	fmt.Printf("HEADROOM %.1f GB can be granted now (%.1f GB available - %.1f GB promised - %.1f GB kept free)%s\n",
		max(0, st.HeadroomMB)/1024, m.AvailableMB/1024, st.PendingMB/1024, st.Config.HeadroomGB,
		map[bool]string{true: "   [DRY RUN]"}[st.Config.DryRun])
	swept := "not swept yet (starting)"
	if !st.LastSweep.IsZero() {
		swept = "swept " + time.Since(st.LastSweep).Round(time.Second).String() + " ago"
	}
	fmt.Printf("DAEMON   %s · %s · itself %.0f MB, %.1f%% CPU, last sweep %.0f ms\n", st.DaemonVersion,
		swept, st.Self.RSSMB, st.Self.CPUPercent, st.Self.SweepMS)
	if st.FailOpen != "" {
		fmt.Println("OPEN    ", st.FailOpen+": every request is granted, nothing is paused or stopped")
	}
	if line := manager.AccuracyLine(st.Accuracy); line != "" {
		fmt.Println("ACCURACY", line)
	}
	for src, why := range st.Sources {
		if why != "" {
			fmt.Printf("SOURCE   %s unavailable: %s (its resources are kept as last seen)\n", src, why)
		}
	}
	for _, n := range st.Notes {
		fmt.Println("NOTE    ", n)
	}
	for i, w := range st.Waiters {
		urgent := ""
		if w.Priority > 0 {
			urgent = " [urgent]"
		}
		fmt.Printf("WAITING  %d. %s wants %.1f GB for %s (since %s)%s\n", i+1, short(w.Session), w.NeedMB/1024, w.Kind, w.Since.Format("15:04:05"), urgent)
	}
	for _, a := range st.Approvals {
		if a.State == "pending" {
			fmt.Printf("APPROVE  %s: %s (from %s; answer in the AgentRM app)\n", a.ID, a.What, orNone(a.From))
		}
	}
	for _, l := range st.Leaks {
		ask := fmt.Sprintf("%s it: agentrm leak accept %s", l.Action, l.ID)
		if l.Dismissed {
			ask = "left alone"
		}
		fmt.Printf("LEAKING  %s (pid %d) %.1f → %.1f GB since %s, %+.0f MB/h · %s\n", l.Name, l.PID, l.StartMB/1024,
			l.NowMB/1024, l.Since.Format("15:04"), l.RateMBh, ask)
	}
	fmt.Println()

	fmt.Println("LEASES")
	ls := make([]*model.Lease, 0, len(st.Leases))
	for _, l := range st.Leases {
		ls = append(ls, l)
	}
	sort.Slice(ls, func(i, j int) bool { return ls[i].Created.Before(ls[j].Created) })
	if len(ls) == 0 {
		fmt.Println("  none")
	}
	for _, l := range ls {
		v := "✓"
		if !l.Verified {
			v = "?"
		}
		fmt.Printf("  %-8s %s %-20s %5.1f GB  %-7s %-14s used %-8s ago  %s\n", l.ID, v, l.Kind, l.MemGB, l.Agent,
			short(l.Session), time.Since(l.LastUsed).Round(time.Minute), trunc(l.Purpose, 50))
		var notes []string
		if l.ExpectedEnd != nil {
			notes = append(notes, "usually ends "+l.ExpectedEnd.Format("15:04"))
		}
		if l.LikelyDone {
			notes = append(notes, "probably finished")
		}
		if l.TaskDoneAt != nil {
			notes = append(notes, "task finished: "+l.TaskDoneWhy)
		}
		if l.Branch != "" {
			notes = append(notes, "branch "+l.Branch)
		}
		if l.Backfill {
			notes = append(notes, "backfilled")
		}
		if len(notes) > 0 {
			fmt.Printf("           %s\n", strings.Join(notes, " · "))
		}
	}
	if st.Prediction != "" {
		fmt.Printf("  (%s)\n", st.Prediction)
	}

	fmt.Println("\nRUNNING")
	rs := make([]*model.Resource, 0, len(st.Resources))
	for _, r := range st.Resources {
		rs = append(rs, r)
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].MemMB > rs[j].MemMB })
	if len(rs) == 0 {
		fmt.Println("  nothing tracked")
	}
	grace := time.Duration(st.Config.GraceMinutes) * time.Minute
	for _, r := range rs {
		owner := "lease " + r.LeaseID
		switch {
		case r.Unseen != nil:
			owner = fmt.Sprintf("UNSEEN %s (%s not answering; last view)", time.Since(*r.Unseen).Round(time.Minute), r.Source)
		case r.Pinned:
			owner = "pinned"
		case r.PausedAt != nil:
			owner = "PAUSED " + time.Since(*r.PausedAt).Round(time.Minute).String()
		case r.LeaseID == "" && r.OrphanSince != nil && st.Config.DryRun:
			owner = "ORPHAN (dry run)"
		case r.LeaseID == "" && r.OrphanSince != nil:
			owner = fmt.Sprintf("ORPHAN, pause in %s", max(0, time.Until(r.OrphanSince.Add(grace))).Round(time.Second))
		case r.LeaseID == "":
			owner = "unclaimed"
		}
		fmt.Printf("  %-26s %-16s %5.1f GB  %-24s %s\n", trunc(r.ID, 26), r.Kind, r.MemMB/1024, owner, trunc(r.Name, 40))
	}

	fmt.Println("\nSESSIONS")
	ss := make([]*model.Session, 0, len(st.Sessions))
	for _, s := range st.Sessions {
		ss = append(ss, s)
	}
	sort.Slice(ss, func(i, j int) bool { return ss[i].LastSeen.After(ss[j].LastSeen) })
	if len(ss) == 0 {
		fmt.Println("  none yet")
	}
	for _, s := range ss {
		u := ""
		if s.Priority > 0 {
			u = " [urgent]"
		}
		if s.EndedAt != nil {
			fmt.Printf("  %-14s %-8s ended %-8s ago  %s (%s)\n", short(s.ID), s.Agent,
				time.Since(*s.EndedAt).Round(time.Second), s.Cwd, trunc(s.EndReason, 50))
			continue
		}
		fmt.Printf("  %-14s %-8s using %5.1f GB  seen %-8s ago  %s%s\n", short(s.ID), s.Agent, s.FootprintMB/1024,
			time.Since(s.LastSeen).Round(time.Second), s.Cwd, u)
	}
	if showStopped {
		fmt.Println("\nSTOPPED BY AGENTRM (newest last; bring one back with agentrm restore ID)")
		if len(st.Stopped) == 0 {
			fmt.Println("  nothing")
		}
		for _, s := range st.Stopped {
			fmt.Printf("  %-26s %s  %s\n", trunc(s.Resource.ID, 26), s.StoppedAt.Format("Jan 2 15:04"), trunc(s.Why, 70))
		}
	}
	return nil
}

func short(s string) string { return trunc(s, 12) }

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
