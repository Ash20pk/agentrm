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
A later request cannot take memory an earlier one is waiting for; a waiter that stops retrying
loses its place after 90 seconds; a request that could never fit is refused outright.

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
in the agent's tree, including the ones that detach and re-parent to launchd (daemons, `nohup`,
`&`), and the containers Docker's event stream credits to it.

A session stops being a root when it says so (Claude Code's SessionEnd hook) or when its agent
process is gone. That is checked every sweep by PID *and* start time, so a reused PID never keeps
a dead session alive, and only after two sweeps in a row, so a failed `ps` ends nothing. Then:

- its leases are released at once (not after the idle question or the TTL);
- everything it started or held, and nobody else holds, is marked owner-gone;
- processes from its tree that are still running and under no agent are tracked as
  **leftovers**, even when they are of no kind AgentRM knows (`tsc --watch`, `esbuild`, a Python
  worker): the top of each leftover tree, sized with what is under it, from `leftover_min_mb`.
  A leftover discovery already knows (a Gradle daemon, a dev server) is credited to its
  session instead of being listed twice.

Sweeping is the usual staged, reversible path, timed by the evidence:

| Evidence | Paused after | Stopped after |
|---|---|---|
| Its owner ended and nothing has used it since | `owner_gone_grace_minutes` (2) | `owner_gone_pause_minutes` (10) |
| No known owner | `grace_minutes` (10), halved under memory pressure | `pause_minutes` (30) |
| No known owner, running for `old_generation_hours` (24) or more | three times `grace_minutes` | `pause_minutes` |

Use after the owner left (network traffic, CPU, another session's command naming it) takes a
resource off the fast path: someone may have taken it over, so it waits like anything else.
A session that talks again (resumed) is a root once more. What is never collected automatically
is unchanged: VMs, things you started, pinned and never-stop items. The Activity window's
Collected tab lists each ended session with what it left and what happened to it.

## Sampling

A full sweep (discovery, attribution, leases, orphans) runs every `sweep_seconds` (30), and at
once when a lease is granted. While a build or inferred heavy job runs, a request waits, or a
paused server could be reached, a fast tick runs every `fast_sweep_seconds` (3): memory,
pressure, process trees and new connections. The daemon reports its own cost in `agentrm status`.

If a discovery source fails (Docker hangs, say), its resources are kept as last seen rather than
dropped, and a source that keeps timing out is backed off.
