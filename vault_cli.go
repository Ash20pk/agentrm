package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"syscall"
	"time"

	"agentrm/internal/api"
	"agentrm/internal/model"
	"agentrm/internal/sys"
)

const vaultUsage = `agentrm vault — short-lived access to accounts, approved by a person

  agentrm vault list [--json]                  the accounts you may ask for, and your grants
  agentrm vault run ACCOUNT --purpose "why" [--scope S] [--repo R] [--for MIN] [--wait SEC] -- COMMAND...
                                               run COMMAND with access to ACCOUNT; the person
                                               approves it with Touch ID the first time
  agentrm vault reveal ACCOUNT --purpose "why" [...]
                                               print the access as shell exports (approved
                                               separately: what is printed goes into your transcript)
  agentrm vault revoke GRANT | --all           end access before its time

Scopes: GitHub permissions (contents:read,pull_requests:write; default contents:read) with
--repo names; an API key "full" or "read" (GET only); AWS a role name or ARN; a login "use".
One approval covers the same session asking again for no more, until the grant expires.
`

func runVault(args []string) int {
	if len(args) == 0 {
		fmt.Print(vaultUsage)
		return 2
	}
	sub, args := args[0], args[1:]
	var command []string
	for i, a := range args {
		if a == "--" {
			args, command = args[:i], args[i+1:]
			break
		}
	}
	var pos []string
	for len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		pos, args = append(pos, args[0]), args[1:]
	}
	fs := flag.NewFlagSet("vault "+sub, flag.ContinueOnError)
	purpose := fs.String("purpose", "", "why you need it: the person approves what you write here")
	scope := fs.String("scope", "", "what you need (see agentrm vault list)")
	var repos multiFlag
	fs.Var(&repos, "repo", "a GitHub repository (repeat, or comma-separated)")
	minutes := fs.Int("for", 0, "minutes you need it (default 30; at most what the account allows)")
	wait := fs.Int("wait", 110, "seconds to wait for the person's answer")
	all := fs.Bool("all", false, "revoke: all your grants")
	asJSON := fs.Bool("json", false, "machine-readable output")
	approve := fs.Bool("approve", false, "answer: approve (the app only; the secret comes on stdin)")
	deny := fs.Bool("deny", false, "answer: deny")
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

	switch sub {
	case "list", "ls", "grants":
		return fail(vaultList(*asJSON))
	case "run", "reveal":
		reveal := sub == "reveal"
		if arg(0) == "" {
			return fail(fmt.Errorf("name an account (see agentrm vault list)"))
		}
		if !reveal && len(command) == 0 {
			return fail(fmt.Errorf("give the command after --: agentrm vault run %s --purpose \"...\" -- gh pr create", arg(0)))
		}
		req.Account, req.Scope, req.Repos, req.TTLMinutes, req.Purpose, req.Reveal = arg(0), *scope, repos, *minutes, *purpose, reveal
		g, err := vaultAsk(req, time.Duration(*wait)*time.Second)
		if err != nil {
			var w *stillWaiting
			if errors.As(err, &w) {
				fmt.Fprintln(os.Stderr, "agentrm vault:", err)
				return 75 // EX_TEMPFAIL: try again
			}
			return fail(err)
		}
		left := time.Until(g.Grant.Expires).Round(time.Minute)
		fmt.Fprintf(os.Stderr, "agentrm vault: %s (%s) for %s more, grant %s\n", g.Grant.Account, g.Grant.Scope, left, g.Grant.ID)
		if reveal {
			printExports(os.Stdout, g)
			return 0
		}
		return fail(execWith(command, g))
	case "revoke":
		req.GrantID, req.All = arg(0), *all
		var r api.CountResp
		if err := api.Call("/vault/revoke", req, &r); err != nil {
			return fail(err)
		}
		fmt.Printf("revoked %d grant(s)\n", r.N)
	case "answer": // the AgentRM app, after Touch ID; the secret comes on stdin
		if *approve == *deny {
			return fail(fmt.Errorf("--approve or --deny"))
		}
		req.ApprovalID, req.Approve, req.TTLMinutes = arg(0), approve, *minutes
		if *approve {
			var in struct {
				Material map[string]string  `json:"material"`
				Account  *model.VaultAccount `json:"account"` // as saved in the app
			}
			if err := json.NewDecoder(io.LimitReader(os.Stdin, 1<<20)).Decode(&in); err != nil {
				return fail(fmt.Errorf("the secret was not readable on stdin: %w", err))
			}
			req.Material = in.Material
			if in.Account != nil {
				req.VaultAccounts = []model.VaultAccount{*in.Account}
			}
		}
		var r api.ApprovalResp
		if err := api.CallDaemon(45*time.Second, "/vault/answer", req, &r); err != nil {
			return fail(err)
		}
		fmt.Println(r.Approval.State)
	case "accounts-set": // the AgentRM app
		var in []model.VaultAccount
		if err := json.NewDecoder(io.LimitReader(os.Stdin, 1<<20)).Decode(&in); err != nil {
			return fail(fmt.Errorf("accounts: %w", err))
		}
		req.VaultAccounts = in
		if req.VaultAccounts == nil {
			req.VaultAccounts = []model.VaultAccount{}
		}
		if err := api.CallDaemon(5*time.Second, "/vault/accounts", req, nil); err != nil {
			return fail(err)
		}
	default:
		fmt.Fprint(os.Stderr, vaultUsage)
		return 2
	}
	return 0
}

type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			*m = append(*m, s)
		}
	}
	return nil
}

type stillWaiting struct{ id string }

func (e *stillWaiting) Error() string {
	return "still waiting for the person to approve request " + e.id + " in AgentRM; run the same command again to keep waiting (it stays open 5 minutes)"
}

// vaultAsk asks, and if the person must approve, waits for the answer, then asks again for the
// grant the approval made.
func vaultAsk(req api.Req, wait time.Duration) (*api.VaultResp, error) {
	var g api.VaultResp
	err := api.Call("/vault/request", req, &g)
	var na *api.NeedsApproval
	if !errors.As(err, &na) {
		return &g, err
	}
	fmt.Fprintln(os.Stderr, "agentrm vault:", na.Message)
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		var r api.ApprovalResp
		if err := api.Call("/approval", api.Req{ApprovalID: na.ID}, &r); err != nil {
			return nil, err
		}
		switch r.Approval.State {
		case "approved":
			err := api.Call("/vault/request", req, &g)
			return &g, err
		case "failed":
			return nil, fmt.Errorf("approved, but the access could not be made: %s", r.Error)
		case "denied":
			return nil, fmt.Errorf("the person denied access to %s; ask them in the chat what to do instead", req.Account)
		case "expired":
			return nil, fmt.Errorf("nobody answered in AgentRM; ask the person in the chat to approve it, then try again")
		}
	}
	return nil, &stillWaiting{id: na.ID}
}

// execWith replaces this process with the command, its environment carrying the access.
func execWith(command []string, g *api.VaultResp) error {
	path, err := exec.LookPath(command[0])
	if err != nil {
		return err
	}
	drop := map[string]bool{}
	for _, k := range g.Unset {
		drop[k] = true
	}
	for k := range g.Env {
		drop[k] = true
	}
	var env []string
	drop[sys.VaultVarsEnv] = true
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); !drop[k] {
			env = append(env, kv)
		}
	}
	names := make([]string, 0, len(g.Env))
	for k, v := range g.Env {
		env = append(env, k+"="+v)
		names = append(names, k)
	}
	sort.Strings(names)
	// So AgentRM never keeps them when it records how to restart this command.
	env = append(env, sys.VaultVarsEnv+"="+strings.Join(names, ","))
	return syscall.Exec(path, command, env)
}

func printExports(w io.Writer, g *api.VaultResp) {
	keys := make([]string, 0, len(g.Env))
	for k := range g.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range g.Unset {
		fmt.Fprintf(w, "unset %s\n", k)
	}
	for _, k := range keys {
		fmt.Fprintf(w, "export %s='%s'\n", k, strings.ReplaceAll(g.Env[k], "'", `'\''`))
	}
	// As vault run does: so that AgentRM never keeps them when it records how to restart a
	// command started from this shell.
	if len(keys) > 0 {
		fmt.Fprintf(w, "export %s=\"${%s:+$%s,}%s\"\n", sys.VaultVarsEnv, sys.VaultVarsEnv, sys.VaultVarsEnv, strings.Join(keys, ","))
	}
}

func vaultList(asJSON bool) error {
	var r api.VaultListResp
	if err := api.Call("/vault/list", api.Req{Via: "cli"}, &r); err != nil {
		return err
	}
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(r)
	}
	if len(r.Accounts) == 0 {
		fmt.Println("The vault has no accounts. The person adds them in the AgentRM app, under Vault.")
	}
	for _, a := range r.Accounts {
		line := fmt.Sprintf("%-20s %-9s", a.Name, a.Kind)
		switch a.Kind {
		case model.VaultGitHub:
			perms := "any permission the app has"
			if len(a.Permissions) > 0 {
				var ps []string
				for p, l := range a.Permissions {
					ps = append(ps, p+":"+l)
				}
				sort.Strings(ps)
				perms = "up to " + strings.Join(ps, ",")
			}
			repos := "all repos"
			if len(a.Repos) > 0 {
				repos = strings.Join(a.Repos, ", ")
			}
			line += " " + perms + "; " + repos
		case model.VaultAPIKey:
			line += fmt.Sprintf(" %s via the proxy, scopes %s; sets %s and %s", a.APIURL, orDefault(strings.Join(a.Scopes, "/"), "full"),
				orDefault(a.KeyEnv, "AGENTRM_API_KEY"), orDefault(a.BaseEnv, "AGENTRM_API_BASE"))
		case model.VaultAWS:
			line += " roles " + strings.Join(a.Scopes, ", ")
		case model.VaultPassword:
			line += " " + strings.TrimSpace(a.Username+" "+a.URL)
		}
		if a.Note != "" {
			line += " — " + a.Note
		}
		fmt.Println(line)
	}
	now := time.Now()
	var live []*model.VaultGrant
	for _, g := range r.Grants {
		if g.Live(now) {
			live = append(live, g)
		}
	}
	if len(live) > 0 {
		fmt.Println("\nLive grants:")
		for _, g := range live {
			fmt.Printf("  %s  %s %s, session %s, until %s, used %d time(s)\n", g.ID, g.Account, g.Scope, g.Session,
				g.Expires.Local().Format("15:04"), g.Uses)
		}
	}
	return nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
