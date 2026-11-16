# AgentRM

A resource manager for coding agents on macOS: Activity Monitor for the agentic era.

When several Claude Code or Codex sessions share one laptop, each starts clusters, builds,
simulators and dev servers as if it had the machine to itself. Nobody stops them, memory runs
out, and everything slows to a crawl. AgentRM puts one rule in front of that: **ask first, say
why, and clean up after yourself.**

- **Leases.** Before a heavy job starts (a kind cluster, `docker compose up`, a Docker, cargo,
  Gradle or Xcode build, a simulator, a load test, a dev server), the agent gets a lease. AgentRM
  grants it only if the memory budget and macOS memory pressure allow, and otherwise says who
  holds what.
- **Reporting back.** Leases renew while the session works. Builds release when the command
  returns; everything a session holds is released when it ends.
- **Orphans.** Every 30 seconds AgentRM lists what is actually running. Anything no lease
  covers is an orphan: each active agent session is asked whether it is using it and why. If no
  one claims it within the grace period (10 minutes by default), it is stopped. Containers and
  simulators are stopped, never removed; processes get SIGTERM. The event log says how to start
  each one again.
- **A menu-bar view** of memory, swap, leases, running resources and orphans, with Claim, Pin
  and Stop.

New installs start in **dry run**: AgentRM asks and notifies but never stops anything, so you
can see what it would do before you turn auto-stop on.

## Install

Download `AgentRM-<version>.dmg` from [Releases](https://github.com/Ash20pk/agentrm/releases), drag AgentRM to Applications and open it. It
lives in the menu bar. Open **Settings → Setup** and turn on what you want:

| Item | What it does |
| --- | --- |
| Background daemon | Runs `agentrm daemon` at login (System Settings → Login Items). |
| Claude Code hooks | Adds four hooks to `~/.claude/settings.json` (a backup is kept; your other settings are untouched). |
| Codex instructions | Adds a marked section to `~/.codex/AGENTS.md`. Codex has no hooks, so this one is a convention, not enforced. |
| Command line | Links `~/.local/bin/agentrm` to the copy inside the app. |

Requires macOS 14 or later.

## How agents use it

Claude Code needs nothing more: the hooks request leases, renew them, release them, and deliver
orphan questions into the session. Any other agent, or you, can use the CLI:

```sh
agentrm acquire --kind cluster --purpose "integration test for #123"   # prints a lease id
agentrm release <lease-id>
agentrm inbox                                     # questions for this session
agentrm claim kind:dev --purpose "still debugging the ingress"
agentrm disown compose:old-stack
agentrm status        # or: agentrm top
```

Kinds and their default memory estimates: `cluster` 3 GB, `compose` 2, `docker-build` 3,
`cargo-build` 2.5, `gradle` 3, `xcode-build` 3, `simulator` 1.5, `loadtest` 2, `server` 0.5.

## What it watches

| Resource | Found by | Stopped with |
| --- | --- | --- |
| kind clusters | Docker label `io.x-k8s.kind.cluster` | `docker stop` on its nodes |
| Compose / Supabase stacks, other containers | `docker ps` labels | `docker stop` |
| iOS simulators | `xcrun simctl list devices booted` | `xcrun simctl shutdown` |
| Gradle and Kotlin daemons | process arguments | SIGTERM |
| Dev servers | listening TCP sockets owned by node, python, ruby, bun, deno, php, java, `go run` and similar | SIGTERM |

Apps under `/Applications` and `/System`, MCP servers, language servers and the agents
themselves are never treated as dev servers.

## Configuration

`~/.agentrm/config.json` (created on first run):

| Key | Default | Meaning |
| --- | --- | --- |
| `reserve_gb` | 4 | Memory kept back for macOS and your own apps |
| `min_free_percent` | 10 | Refuse leases when `memory_pressure` reports less free memory |
| `grace_minutes` | 10 | How long an orphan may run unclaimed |
| `dry_run` | true | Ask and notify, never stop |
| `never_stop` | [] | Resource IDs or name substrings to leave alone |
| `extra_patterns` | {} | More commands per lease kind, e.g. `{"compose": ["my-stack\\.sh\\s+start"]}` |
| `server_names`, `server_exclude` | see file | Which listening processes count as dev servers |

State is in `~/.agentrm/state.json` and every decision is logged to `~/.agentrm/events.log`.

## Safety

- The hooks fail open. If the daemon is not running, agents work exactly as without AgentRM.
- Nothing is ever deleted. Pin anything you want left alone.
- The admission check counts the larger of each lease's estimate and what its resources
  really use, plus everything unclaimed, against `total memory - reserve_gb`.
- Command matching is by pattern, so it errs toward asking: a command that merely mentions
  `kind create cluster` (a `grep`, say) also asks for a lease.

## Build from source

```sh
go test ./... && go build -o agentrm .        # the daemon and CLI
cd app && echo "DEVELOPMENT_TEAM = <your team id>" > Config/Local.xcconfig
xcodegen generate && open AgentRM.xcodeproj     # the menu-bar app (bundles the CLI)
scripts/release.sh                              # notarized zip + DMG (maintainers; needs an Apple ID in Xcode)
```

Requires Go 1.24+, Xcode 16+ and [XcodeGen](https://github.com/yonaskolb/XcodeGen).

## License

MIT
