# How AgentRM decides

AgentRM trusts what the machine shows, not what an agent says, and not its own bookkeeping.

## Admission: measured headroom

```
can grant = memory the kernel can hand out now
          - promises (the part of each granted lease not yet taken as real use)
          - a margin kept free (headroom_gb)
```

Memory used by anything, tracked or not, lowers the first term by itself, so an untracked app
or a script AgentRM did not recognise cannot make it over-grant. A lease only covers the gap
between the grant and the moment the memory is really taken.

Nothing is granted while the Mac is **thrashing**: critical memory pressure; swapping out faster
than `max_swap_mb_per_min`; or the kernel's "warning" level while swapping out at a third of
that. Warning alone is not enough: macOS sits there for hours while compression copes.

- **Estimates** start from per-kind defaults, then follow what leases of that kind really used
  in this project (or anywhere, for a new project): the mean plus twice the typical deviation.
  Only leases that ran something teach anything.
- **Declared sizes** (`--mem`) are accepted, but never below what the kind has really used; a
  lease that takes more than 1.5× its declaration costs its session the right to declare for an
  hour.
- **Process trees are measured.** Each agent's whole process tree is sampled; growth of more than
  `heavy_growth_gb` that no lease explains becomes a lease of its own ("heavy", inferred), so a
  wrapper script whose name says nothing is still counted, held and learned from.
- Disk-heavy work is refused below `min_disk_gb` free; builds above `max_load_per_core`.

## Contention: a queue, fairness and reclamation

A request that does not fit is queued: urgent sessions first (you mark them), then first come.
A later request cannot take memory an earlier one is waiting for; a request that could never fit
is refused outright.

Nobody polls the queue. It remembers what was asked for, and the moment a request fits (on a
release, or on the next memory sample, every `fast_sweep_seconds` while anyone waits) the lease
is granted to it there and then. The session hears about it in one of two ways:

- `agentrm acquire --wait` stays connected and returns when the lease is granted. Run in the
  background by an agent, its exit is what wakes the agent. A waiting request holds no lock and
  costs the daemon one idle connection.
- Otherwise the news is in the session's inbox, which Claude Code's hooks deliver on its next
  tool call or prompt. Running the refused command again then takes the lease up.

A lease granted from the queue is an offer: if nothing is started under it and its session does
not come for it within `grant_hold_minutes` (3), it is released and the next in line gets the
memory. So an agent that changed its mind costs the queue three minutes, not a slot.

A queued request keeps its place while a client is connected for it, or while its session is
still talking (any tool call counts) and for `queue_minutes` (10) after it was last heard
from. A session that ends, or whose agent process exits, leaves the queue at once. A full disk
or a saturated CPU is a refusal, not a place in the queue: those are retried. So is a restore,
which has to start the thing as well as hold the lease.

While anyone waits, one session may hold at most `max_session_share` of usable memory.

At the same time AgentRM reclaims, pressing those who use theirs least and only as many as
needed: unclaimed resources, then sessions over their share, then unverified claims, then leases
idle for `pressure_idle_minutes` or more, longest idle first. Their deadlines shrink to
`pressure_grace_minutes` and the holder is told someone is waiting. A lease in active use is
never pressed, and an urgent session is never pressed for a normal request.

## Identity and evidence

- **Who is asking**: the process ID on the other end of the socket, walked up to the nearest agent
  process (its session) or to a terminal, IDE or the app (you). Detached processes are nobody.
- **What each session uses**: every command an agent runs is parsed (a real shell parser); only
  commands that execute something count. They are matched against running resources: kube
  contexts (including the current one for a bare `kubectl`), container and project names, ports,
  simulator and emulator IDs, database clients, model clients.
- **Who started what**: Docker's event stream names the client that created each container, at
  the moment it is created. Processes are attributed by ancestry. Otherwise, the recent command
  that named it. If no agent ran anything for 10 minutes, something new can only be yours.
- **Claims need evidence**: verified if the session started the resource or used it within the
  last hour, or if you make the claim. Otherwise unverified: 30 minutes, first to be reclaimed.
- **Leases live on use**: commands of the holder that use the resource, or activity on it (open
  connections; network traffic well above its own background, which is learned; Gradle CPU).
  Unused for `idle_minutes`, the holder is asked; no answer within the grace period releases it.
  A declared end time (`--until`) is held to. A lease under which nothing starts within 15 minutes
  is dropped.

## Unclaimed resources: ask, pause, stop

Something running with no lease is an orphan. Every active session is asked about it at once,
and you get a notification. After `grace_minutes` it is **paused** (SIGSTOP or `docker pause`):
nothing is lost, and claiming it, pinning it, running a command that uses it, or connecting to
its port resumes it instantly. After `pause_minutes` more it is **stopped**: containers are
stopped (not removed), simulators shut down, Homebrew services stopped through `brew`, processes
get SIGTERM, Ollama models are unloaded. Everything stopped is listed and can be restored with
`agentrm restore`, through admission like any request.

If a request is waiting for memory the pause is skipped, because a paused process still holds
its memory. VMs are never stopped automatically (for QEMU that is a hard power-off); they are
paused where possible and you are asked.

Every pause and stop decision, taken or (in dry run) not, is kept for `agentrm review`, where you
mark it right or wrong. That is the evidence for turning auto-stop on.

## Garbage collection: what an agent leaves behind

The orphan rules above apply to anything nobody claims. The collector adds what AgentRM knows
about who started things, so that what an agent leaves behind goes quickly and safely.

It is a mark and sweep over ownership. **Roots** keep things running: a live agent session, you
(anything started from a GUI app, pinned, or on the never-stop list), and leases with recent use.
**Edges** are recorded as things start: session → lease → resource, and session → every process
in the agent's tree, and the containers Docker's event stream credits to it.

Processes that detach (`npm run dev &`, `nohup`, daemons) re-parent to launchd, and the tree no
longer shows who started them. Their **environment** still does: every command Claude Code runs
carries `CLAUDE_CODE_SESSION_ID` and `CLAUDE_PID`, and a process keeps its environment for life.
AgentRM reads it from the kernel once per process (`kern.procargs2` on macOS, `/proc/<pid>/environ`
on Linux; only the agent markers are kept, never the rest). So a detached process belongs to its
session exactly, whenever it detached, and processes left by sessions that ended before AgentRM
was even running are found too. macOS withholds the environment of Apple's own binaries (a
detached `/bin/sleep`), so those still rely on the tree; Linux has no such exception. An app an
agent happened to launch (Docker Desktop, say) is tracked but **only ever asked about**.

A session stops being a root when it says so (Claude Code's SessionEnd hook), or the moment its
agent process exits: the kernel says so (kqueue on macOS, pidfd on Linux). The sweep checks too,
by PID *and* start time, so a reused PID never keeps a dead session alive, and only after two
tables in a row, so a failed `ps` ends nothing (a `ps` that fails or prints part of the table is
discarded, not believed). Claude Code's `/clear` does not end a session: the next session of the
same agent process takes over its leases and what it started. Then:

- its leases are released at once (not after the idle question or the TTL);
- everything it started or held, and nobody else holds, is marked owner-gone;
- processes from its tree that are still running and under no agent are tracked as
  **leftovers**, even when they are of no kind AgentRM knows (`tsc --watch`, `esbuild`, a Python
  worker): the top of each leftover tree, sized with what is under it, from `leftover_min_mb`.
  A leftover discovery already knows (a Gradle daemon, a dev server) is credited to its
  session instead of being listed twice. Leftovers are sized by their footprint (what Activity
  Monitor shows, compressed memory included), not by `ps`'s resident size, which an idle
  process macOS has compressed makes look small. AgentRM records each one's command line,
  directory and environment (secrets removed), so `agentrm restore left:<pid>` starts it again.

Sweeping is the usual staged, reversible path, timed by the evidence:

| Evidence | Paused after | Stopped after |
|---|---|---|
| Its owner ended and nothing has used it since | `owner_gone_grace_minutes` (2) | `owner_gone_pause_minutes` (10) |
| Its task finished (a PR opened, the branch left) and nothing has used it since | `owner_gone_grace_minutes` after the holder is asked | as above |
| No known owner | learned per kind (below), else `grace_minutes` (10), halved under memory pressure | `pause_minutes` (30) |
| No known owner, running for `old_generation_hours` (24) or more | three times `grace_minutes` | `pause_minutes` |

Use after the owner left (network traffic, CPU, another session's command naming it) takes a
resource off the fast path: someone may have taken it over, so it waits like anything else.
A session that talks again (resumed) is a root once more, and what it left is no longer
collected as left behind. What is never paused or stopped automatically: VMs, things you
started, apps an agent launched, pinned and never-stop items; AgentRM asks about them instead. The Activity window's
Collected tab lists each ended session with what it left and what happened to it.

## Liveness by task

An agent outlives its tasks: the dev server started for one branch is still running, unused,
while the agent works on the next. So a lease also belongs to a task: the repository and branch
its session was on (read from `.git/HEAD`, worktrees included). When the task looks finished (a
pull request opened from it, or the session switched to another branch) the holder is asked at
once, and unless the lease is used again within `owner_gone_grace_minutes` it is released and
collected like what an ended session left. Any use cancels that.

## Cooperative preemption

When a request is blocked, holders are not only chosen by rule. Each is asked what giving up its
lease would cost it right now, and answers `agentrm yield <lease> --cost none|low|high
[--restart-seconds N]`: `none` releases it at once, an honest `low` is taken at once when memory
is needed, `high` is taken last, and holders that do not answer are pressed in the usual order
(idle longest first). Answers are checked against what happens next: a holder that said `none`
and used the thing again within half an hour, or whose restart took far longer than it said,
misreported. Each session's later answers are weighed by its record; one misreport is enough for
its offers to count as no answer until honest ones outweigh it, and a poor record (below 40%)
also stops its memory figures from being believed for a day. Measured restart times feed the
learned stop timing below.

## Learning from use

- **What comes next.** Per project, AgentRM learns which kind of heavy work follows which (a
  first-order Markov chain over the kinds commands classify to) and how long each kind of lease
  lives. Every lease gets an expected end; one that has outlived nine in ten of its kind and is
  idle is marked probably finished. When a session's likely next request would not fit,
  holders are asked what yielding would cost before it arrives (asked only: nothing is taken on
  a guess). `agentrm status` shows how the predictor does against the obvious baseline, the
  project's most frequent kind.
- **When to stop.** Waiting costs the memory held for as long as it is held; stopping something
  then needed costs its restart time. Per resource kind, AgentRM records how long things sat
  idle before being used again (and which never were), and picks the wait that minimises the
  expected cost over that record, with memory weighted up under pressure and restarts at
  `restart_weight_gb` per minute. Without enough record the rule falls back to the fixed
  `grace_minutes`; with one it stays between a minute and three times `grace_minutes`.
- **Backfilling.** A large request at the head of the queue would block a two-minute build behind
  it. A build kind whose learned lifetime is short (nine times in ten within
  `backfill_max_minutes`) may run first, in memory that is free now; each session at most once
  per waiting request.

## Earning auto-stop, level by level

New installs only ask and report. Auto-stop is then turned on level by level, in order of how
strong the evidence behind each kind of decision is: first what ended sessions and finished
tasks left, then unclaimed things, then things whose holder let them sit idle. Each level shows
its own record (decisions, how many you reviewed, how many were wrong) and is ready once at least
5 have been reviewed over at least 7 days with under 2% wrong. A later level cannot be on while
an earlier one is off, and a stop you mark wrong turns its level off again by itself.

`agentrm collect` (or the first-run window's Collect button) stops, at your request, what ended
sessions left; `agentrm receipt` sums up a week: what was freed, found, made to wait and
restored, from the decision and events logs. It also arrives as one notification a week.

## Proving changes: traces and replay

`agentrm record on` keeps a trace of what the daemon sees (each sweep's process table, memory
and discovery, each request, each exit), delta-encoded, about 1.5 MB a day, in
`~/.agentrm/traces` (command lines with secrets redacted; no environments). `agentrm replay` runs
any configuration over a trace on its own clock and scores it against the trace itself: a stop is
wrong if the recording shows the thing used again afterwards. `agentrm compare` runs AgentRM and
the rule cleanup scripts use (kill what hangs off launchd and is named like a dev process) on the
same trace. See [replay](replay.md).

## Sampling

A full sweep (discovery, attribution, leases, orphans) runs every `sweep_seconds` (30), and at
once when a lease is granted. While a build or inferred heavy job runs, a request waits, or a
paused server could be reached, a fast tick runs every `fast_sweep_seconds` (3): memory,
pressure, process trees and new connections. The daemon reports its own cost in `agentrm status`.

If a discovery source fails (Docker hangs, say), its resources are kept as last seen rather than
dropped, and a source that keeps timing out is backed off.
