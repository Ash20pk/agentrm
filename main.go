package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/user"
	"sort"
	"strings"
	"time"
)

var version = "dev"

var client = &http.Client{
	Timeout: 5 * time.Second,
	Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", sockPath())
	}},
}

func call(path string, req any, out any) error {
	b, _ := json.Marshal(req)
	resp, err := client.Post("http://agentrm"+path, "application/json", bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("agentrm daemon is not running (%v)", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		var e struct{ Error string }
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("%s", e.Error)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func defaultSession() string {
	if s := os.Getenv("AGENTRM_SESSION"); s != "" {
		return s
	}
	if s := os.Getenv("CLAUDE_SESSION_ID"); s != "" {
		return s
	}
	u, _ := user.Current()
	return "manual-" + u.Username
}

func defaultAgent() string {
	if a := os.Getenv("AGENTRM_AGENT"); a != "" {
		return a
	}
	return "manual"
}

const usage = `agentrm — a resource manager for coding agents on this Mac

  agentrm acquire --kind K --purpose "why" [--mem GB] [--ttl MIN] [--wait]
  agentrm release [LEASE_ID] [--all]          release a lease (or all of this session's)
  agentrm renew                                keep this session's leases alive
  agentrm claim RESOURCE --purpose "why" [--ttl MIN]   adopt something already running
  agentrm disown RESOURCE                      answer "not mine" to an orphan question
  agentrm pin RESOURCE | unpin RESOURCE        never / again auto-stop a resource
  agentrm inbox                                questions waiting for this session
  agentrm status [--json] | top                what is running, who holds it, memory
  agentrm stop RESOURCE                        stop something now (reversibly)
  agentrm config [--dry-run=true|false] [--grace MIN] [--reserve GB] [--min-free PCT]
  agentrm setup claude|codex|path [--uninstall]   install the Claude hooks / Codex note / CLI link
  agentrm setup check                          what is installed (JSON)
  agentrm daemon                               run the manager (the app or launchd does this)
  agentrm hook pre|post|prompt|end             Claude Code hook entry point

Kinds: cluster compose docker-build cargo-build gradle xcode-build simulator loadtest server
Session: --session, or $AGENTRM_SESSION (default manual-<user>); agent: --agent or $AGENTRM_AGENT.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(1)
	}
	cmd, args := os.Args[1], os.Args[2:]
	// Allow a positional argument before the flags (agentrm claim kind:x --purpose y).
	var pos string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		pos, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	session := fs.String("session", defaultSession(), "session id")
	agent := fs.String("agent", defaultAgent(), "agent name (claude, codex, manual)")
	kind := fs.String("kind", "", "lease kind")
	purpose := fs.String("purpose", "", "why you need it")
	mem := fs.Float64("mem", 0, "memory in GB (default: per-kind estimate)")
	ttl := fs.Int("ttl", 0, "lease lifetime in minutes")
	wait := fs.Bool("wait", false, "keep retrying until granted (max 30 min)")
	all := fs.Bool("all", false, "all of this session's leases")
	asJSON := fs.Bool("json", false, "machine-readable output")
	uninstall := fs.Bool("uninstall", false, "remove instead of install")
	dryRun := fs.String("dry-run", "", "true or false")
	grace := fs.Int("grace", 0, "minutes an orphan may run unclaimed")
	reserve := fs.Float64("reserve", 0, "GB kept back for macOS and apps")
	minFree := fs.Int("min-free", 0, "refuse leases below this free %")
	_ = fs.Parse(args)
	if pos == "" && fs.NArg() > 0 {
		pos = fs.Arg(0)
	}
	base := map[string]any{"session": *session, "agent": *agent}
	with := func(kv ...any) map[string]any {
		m := map[string]any{}
		for k, v := range base {
			m[k] = v
		}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}

	var err error
	switch cmd {
	case "version", "--version":
		fmt.Println("agentrm", version)
	case "daemon":
		err = runDaemon()
	case "hook":
		runHook(pos)
		return
	case "acquire":
		if *kind == "" || *purpose == "" {
			err = fmt.Errorf("--kind and --purpose are required")
			break
		}
		deadline := time.Now().Add(30 * time.Minute)
		for {
			var resp struct {
				Granted bool
				Reason  string
				Lease   *Lease
				Reused  bool
			}
			if err = call("/acquire", with("kind", *kind, "purpose", *purpose, "mem_gb", *mem, "ttl_minutes", *ttl), &resp); err != nil {
				break
			}
			if resp.Granted {
				fmt.Printf("granted lease %s: %s, %.1f GB, expires %s%s\n", resp.Lease.ID, resp.Lease.Kind,
					resp.Lease.MemGB, resp.Lease.Expires.Format("15:04"), map[bool]string{true: " (reused)"}[resp.Reused])
				fmt.Printf("release it when done: agentrm release %s\n", resp.Lease.ID)
				break
			}
			if !*wait || time.Now().After(deadline) {
				fmt.Fprintln(os.Stderr, "refused:", resp.Reason)
				os.Exit(2)
			}
			fmt.Fprintln(os.Stderr, "waiting:", trunc(resp.Reason, 160))
			time.Sleep(20 * time.Second)
		}
	case "release":
		var resp struct{ Released int }
		if pos != "" {
			err = call("/release", map[string]any{"lease_id": pos}, &resp)
		} else if *all {
			err = call("/release", map[string]any{"session": *session}, &resp)
		} else {
			err = fmt.Errorf("give a lease id, or --all for every lease of session %s", *session)
		}
		if err == nil {
			fmt.Printf("released %d lease(s)\n", resp.Released)
		}
	case "renew":
		var resp struct{ Renewed int }
		if err = call("/renew", base, &resp); err == nil {
			fmt.Printf("renewed %d lease(s)\n", resp.Renewed)
		}
	case "claim":
		var resp struct{ Lease *Lease }
		if err = call("/claim", with("resource_id", pos, "purpose", *purpose, "ttl_minutes", *ttl), &resp); err == nil {
			fmt.Printf("%s is now held by lease %s until %s (renewed while this session is active)\n",
				pos, resp.Lease.ID, resp.Lease.Expires.Format("15:04"))
		}
	case "disown":
		if err = call("/disown", with("resource_id", pos), nil); err == nil {
			fmt.Println("noted: not yours")
		}
	case "pin", "unpin":
		err = call("/pin", map[string]any{"resource_id": pos, "pinned": fmt.Sprint(cmd == "pin")}, nil)
		if err == nil {
			fmt.Printf("%sned %s\n", cmd, pos)
		}
	case "inbox":
		var resp struct{ Questions []string }
		if err = call("/inbox", with("all", "true"), &resp); err == nil {
			if len(resp.Questions) == 0 {
				fmt.Println("no questions")
			}
			for _, q := range resp.Questions {
				fmt.Println(q)
			}
		}
	case "status":
		if *asJSON {
			var raw json.RawMessage
			if err = call("/status", map[string]any{}, &raw); err == nil {
				fmt.Println(string(raw))
			}
			break
		}
		err = printStatus()
	case "stop":
		var resp struct{ Restart string }
		if err = call("/stop", map[string]any{"resource_id": pos}, &resp); err == nil {
			fmt.Printf("stopped %s. Restart with: %s\n", pos, resp.Restart)
		}
	case "config":
		req := map[string]any{"grace_minutes": *grace, "reserve_gb": *reserve, "min_free_percent": *minFree}
		if *dryRun != "" {
			req["dry_run"] = *dryRun == "true"
		}
		var raw json.RawMessage
		if err = call("/config", req, &raw); err == nil {
			fmt.Println(string(raw))
		}
	case "setup":
		err = runSetup(pos, *uninstall)
		if err == nil && pos != "check" {
			fmt.Printf("setup %s: %s\n", pos, map[bool]string{true: "removed", false: "installed"}[*uninstall])
		}
	case "top":
		for {
			fmt.Print("\033[H\033[2J")
			if err = printStatus(); err != nil {
				break
			}
			time.Sleep(2 * time.Second)
		}
	default:
		fmt.Print(usage)
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "agentrm:", err)
		os.Exit(1)
	}
}

func printStatus() error {
	var st struct {
		Mem         MemStats             `json:"mem"`
		CommittedMB float64              `json:"committed_mb"`
		BudgetMB    float64              `json:"budget_mb"`
		Leases      map[string]*Lease    `json:"leases"`
		Resources   map[string]*Resource `json:"resources"`
		Sessions    map[string]*Session  `json:"sessions"`
		LastSweep   time.Time            `json:"last_sweep"`
		Config      Config               `json:"config"`
	}
	if err := call("/status", map[string]any{}, &st); err != nil {
		return err
	}
	m := st.Mem
	fmt.Printf("MEMORY  %.1f GB total · %.1f GB available · %.1f GB compressed · swap %.1f/%.1f GB · %d%% free\n",
		m.TotalMB/1024, m.AvailableMB/1024, m.CompressedMB/1024, m.SwapUsedMB/1024, m.SwapTotalMB/1024, m.FreePercent)
	fmt.Printf("BUDGET  %.1f of %.1f GB committed (%.0f GB reserved for macOS)   swept %s ago%s\n\n",
		st.CommittedMB/1024, st.BudgetMB/1024, st.Config.ReserveGB, time.Since(st.LastSweep).Round(time.Second),
		map[bool]string{true: "   [DRY RUN]"}[st.Config.DryRun])

	fmt.Println("LEASES")
	ls := make([]*Lease, 0, len(st.Leases))
	for _, l := range st.Leases {
		ls = append(ls, l)
	}
	sort.Slice(ls, func(i, j int) bool { return ls[i].Created.Before(ls[j].Created) })
	if len(ls) == 0 {
		fmt.Println("  none")
	}
	for _, l := range ls {
		fmt.Printf("  %-8s %-20s %5.1f GB  %-7s %-14s expires %s  %s\n", l.ID, l.Kind, l.MemGB, l.Agent,
			short(l.Session), l.Expires.Format("15:04"), trunc(l.Purpose, 60))
	}

	fmt.Println("\nRUNNING")
	rs := make([]*Resource, 0, len(st.Resources))
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
		case r.Pinned:
			owner = "pinned"
		case r.LeaseID == "" && r.OrphanSince != nil:
			left := time.Until(r.OrphanSince.Add(grace)).Round(time.Second)
			if left < 0 {
				left = 0
			}
			owner = fmt.Sprintf("ORPHAN, stop in %s", left)
			if st.Config.DryRun {
				owner = "ORPHAN (dry run)"
			}
		case r.LeaseID == "":
			owner = "unclaimed"
		}
		fmt.Printf("  %-26s %-14s %5.1f GB  %-24s %s\n", trunc(r.ID, 26), r.Kind, r.MemMB/1024, owner, trunc(r.Name, 40))
	}

	fmt.Println("\nSESSIONS")
	ss := make([]*Session, 0, len(st.Sessions))
	for _, s := range st.Sessions {
		ss = append(ss, s)
	}
	sort.Slice(ss, func(i, j int) bool { return ss[i].LastSeen.After(ss[j].LastSeen) })
	if len(ss) == 0 {
		fmt.Println("  none yet")
	}
	for _, s := range ss {
		fmt.Printf("  %-14s %-7s seen %-8s ago  %s\n", short(s.ID), s.Agent,
			time.Since(s.LastSeen).Round(time.Second), s.Cwd)
	}
	return nil
}
