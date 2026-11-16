# AgentRM

A resource manager for coding agents on macOS: Activity Monitor for the agentic era.

When several Claude Code or Codex sessions share one laptop, each starts clusters, builds,
simulators and dev servers as if it had the machine to itself. Nobody stops them, memory runs
out, and everything slows to a crawl. AgentRM puts one rule in front of that: **ask first, say
why, and clean up after yourself.**

- **Leases.** Before a heavy job starts (a kind cluster, `docker compose up`, a Docker, cargo,
  Gradle or Xcode build, a simulator, a load test, a dev server), the agent gets a lease. AgentRM
  grants it only if the memory really is there and the Mac is not thrashing; otherwise it queues the request, starts reclaiming from whoever uses theirs least, and says who
  holds what.
- **Reporting back.** Leases renew while the session works. Builds release when the command
  returns; everything a session holds is released when it ends.
- **Orphans.** Every 30 seconds AgentRM lists what is actually running. Anything no lease
  covers is an orphan: each active agent session is asked whether it is using it and why. If no
  one claims it within the grace period (10 minutes by default), it is **paused**: frozen in
  place, losing nothing. Claiming it, pinning it, or running a command that uses it thaws it at
  once. Still unclaimed 30 minutes later, it is stopped: containers and simulators are stopped,
  never removed; processes get SIGTERM. The event log says how to start each one again. If a
  request is blocked waiting for memory, the pause is skipped, because a frozen process still
  holds its memory.
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
agentrm resume srv:4242                           # thaw something AgentRM paused (--all for everything)
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

## How it decides

AgentRM trusts what the machine shows, not what an agent says, and not its own bookkeeping.

### Admission: measured headroom, not a ledger

```
can grant = memory the kernel can hand out now
          - promises (the part of each granted lease not yet taken as real use)
          - a margin kept free (headroom_gb)
```

Memory used by anything, tracked or not, lowers the first term by itself, so an untracked app
or a script AgentRM did not recognise cannot make it over-grant. A lease only has to cover the
gap between the grant and the moment the memory is really taken. Whatever the headroom, nothing
is granted while the Mac is thrashing: the kernel reports memory pressure, or it is swapping out
faster than `max_swap_mb_per_min`. A full but quiet swap file is not thrashing.

- **Estimates** start from per-kind defaults and then follow what leases of that kind really
  used (mean plus twice the typical deviation). Only leases that ran something teach anything.
- **Declared sizes** (`--mem`) are accepted, but never below what the kind has really used, and
  a lease that takes more than 1.5x what it declared costs its session the right to declare
  for an hour.
- **Every session's process tree is measured.** A wrapper script whose name says nothing is
  still counted, taught from, and flagged when it is heavy and holds no lease.
- Disk-heavy work is refused below `min_disk_gb` free; builds above `max_load_per_core`.

### Contention: a queue and reclamation, not just "no"

A request that does not fit is queued, first come first served: a later, smaller request cannot
take memory an earlier one is waiting for, a waiter that stops retrying loses its place after
90 seconds, and a request that could never fit is refused outright. At the same time AgentRM
starts reclaiming, pressing whoever uses theirs least, and only as many as needed: unclaimed
resources first, then unverified claims, then leases idle for `pressure_idle_minutes` or more,
longest idle first. Their deadlines shrink to `pressure_grace_minutes`, and the holder is told
another agent is waiting. A lease in active use is never pressed.

### Identity and evidence

- **Who is asking.** The daemon reads the process ID on the other end of every request and walks
  up the process tree. Under a Claude Code or Codex process, the caller is that agent's session,
  whatever `--session` it passes. Under a terminal, an IDE or the AgentRM app with no agent
  above it, the caller is you. A process detached from whatever started it is nobody: it gets
  no session's identity and none of your rights. An agent can release only its own leases and
  stop only what it holds or what nobody holds; only you can pin things or change settings.
- **What each session uses.** The Claude Code hook reports every command before it runs. Only
  the parts that execute something count: `echo`, `grep`, `git commit -m` and the like, and
  text written through a heredoc, neither use a resource nor start a heavy job by mentioning
  one. What remains is matched against running resources: kube contexts (including the current
  one for a bare `kubectl`), container and Compose project names, ports, simulator IDs.
- **Who started what.** By process ancestry where there is a process: an agent's session, or
  you when it came from your terminal. Otherwise by the recent command that named it. If no
  agent ran anything in the last 10 minutes, something new can only be yours. With no evidence,
  AgentRM binds a new resource only when exactly one lease is still waiting for one like it.
  What you start is held in your name and idle-checked like anything else; you get the
  question as a notification.
- **Claims need evidence.** Verified if the session started the resource or used it in the
  last hour, or if you make the claim. Otherwise unverified: 30 minutes, and first in line
  for reclamation.
- **Leases live on use,** not on unrelated tool calls: commands of the holder that use the
  resource, or activity on it (open connections on its ports, CPU for Gradle). Unused for
  `idle_minutes`, the holder is asked; no answer releases it. A lease under which nothing was
  started within 15 minutes is dropped.

## Limits you should know

- **It is a referee, not a jail.** Agents run as you. One that sets out to cheat can kill the
  daemon, edit `~/.agentrm`, remove its own hooks, or drive a terminal app to act as you.
  AgentRM stops the sloppy and the lazy cases, and makes the rest visible: anything privileged
  done from a terminal rather than the app raises a notification. Real isolation would need a
  privileged daemon and is not attempted.
- **Codex is checked after the fact.** It has no hooks, so nothing stops a heavy command before
  it runs; its process tree is measured and its resources attributed once they exist.
- **Use is inferred from command text and connections.** A script that reaches a resource
  without naming it is missed until the resource shows activity; a browser tab holding a
  connection open keeps a dev server "in use".
- **Sampling is every 30 seconds.** A short spike between samples is not seen, and the swap
  rate is an average over that interval.
- **A paused server looks hung to whoever uses it without AgentRM knowing:** your browser, or
  a script that does not name it. The notification and the menu say how to resume. If the
  daemon is removed while something is paused, run `agentrm resume --all` first (or
  `kill -CONT <pid>` / `docker unpause <name>`). Simulators cannot be paused and are stopped.
- **Stopping is reversible, not free.** A stopped container keeps its data; a dev server loses
  what it held in memory.

## Configuration

`~/.agentrm/config.json` (created on first run):

| Key | Default | Meaning |
| --- | --- | --- |
| `headroom_gb` | 1 | Memory kept free after any grant |
| `max_swap_mb_per_min` | 300 | Refuse new leases while swapping out faster than this |
| `pressure_grace_minutes` | 2 | Deadline for pressed holders when a request is blocked |
| `pressure_idle_minutes` | 5 | A lease unused this long can be reclaimed for a blocked request |
| `pause_minutes` | 30 | How long an unclaimed resource stays frozen before it is stopped; 0 stops directly |
| `unleased_gb` | 2 | Flag a session whose process tree uses this much with no lease |
| `grace_minutes` | 10 | How long an orphan may run unclaimed |
| `dry_run` | true | Ask and notify, never stop |
| `never_stop` | [] | Resource IDs or name substrings to leave alone |
| `extra_patterns` | {} | More commands per lease kind, e.g. `{"compose": ["my-stack\\.sh\\s+start"]}` |
| `idle_minutes` | 30 | A held resource unused this long gets its holder asked |
| `claim_window_minutes` | 60 | A claim is verified if the session used the resource this recently |
| `unverified_ttl_minutes` | 30 | How long a claim without evidence lasts |
| `min_disk_gb` | 10 | Refuse disk-heavy leases below this free space |
| `max_load_per_core` | 1.5 | Refuse builds above this load |
| `server_names`, `server_exclude` | see file | Which listening processes count as dev servers |

State is in `~/.agentrm/state.json` and every decision is logged to `~/.agentrm/events.log`.

## Safety

- The hooks fail open. If the daemon is not running, agents work exactly as without AgentRM.
- Nothing is ever deleted. Pin anything you want left alone.
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
