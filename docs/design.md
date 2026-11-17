# Garbage collection for what coding agents leave behind

Coding agents start things: a kind cluster for an integration test, a Compose stack, a dev
server with `&`, a Gradle daemon, an MCP server. Then the session ends, or moves on to the next
task, and the things keep running. With several agents on one laptop, memory runs out and the
machine swaps. The issue trackers of the agents are full of it: dev servers re-parented to launchd
holding gigabytes, dozens of orphaned MCP servers after a morning's sessions.

The usual fix is a reaper: periodically kill whatever hangs off launchd (PID 1) and is named like
a dev process. It is cheap and it is wrong in both directions. `npm run dev &` from a live
session is re-parented to launchd the moment its shell exits, so the reaper kills a server that
is in use. And a leftover named `esbuild` or `my-worker` is not on its list, so it survives.

AgentRM treats this as what it is: garbage collection, with ownership as the reference graph.

## Roots and edges

**Roots** are what keeps something alive: a running agent session; you (anything started from a
terminal or app, pinned, or on the never-stop list); and leases in recent use.

**Edges** are recorded as things start. A session takes a lease for heavy work and the resource
binds to it; Docker's event stream names the client that created each container; the agent's
process tree is sampled. The edge that makes the rest exact comes from the environment: every
command Claude Code runs carries `CLAUDE_CODE_SESSION_ID` and `CLAUDE_PID`, a process keeps its
environment for life, and the kernel will show it to the same user. So a process that detached
an hour ago, or was left by a session that ended before AgentRM ran, still names its owner.
Liveness of the owner is checked exactly too: by PID *and* start time (a reused PID is younger
than what it is named in), and with the kernel's exit notice rather than a polling interval.

A session stops being a root when it ends (its hook says so, or its process exits), and also,
for what it started for one task, when that task ends: a pull request opened, or the branch left.

## A reversible sweep

What is unreachable is not killed. It is asked about, then paused (SIGSTOP or `docker pause`:
nothing is lost, and the first connection or command naming it resumes it), then stopped, with
the command, directory and environment it ran with recorded so it can be started again. The
timing follows the evidence: minutes when its owner is known to be gone and nothing has used it
since; longer when nobody is known to own it; longer still for something that has run for a day.
Some things are never stopped automatically, only asked about: VMs, what you started, apps an
agent happened to launch.

## Agents are participants, not jobs

Classical schedulers treat their jobs as opaque. These jobs can talk. When a request is blocked,
AgentRM asks each holder what giving up its lease would cost it right now. An agent between test
runs says "nothing"; one in the middle of a migration says "a lot". The cheapest offers are taken
first. Because an agent could say anything, every answer is checked against what the machine
shows next — did it use the thing again? did the restart take as long as it said? — and each
session's later answers are weighed by its record.

The pre-tool hook also gives AgentRM something no OS scheduler has: every command, before it
runs. From that stream it learns, per project, which heavy work follows which and how long each
kind lives. That feeds an expected end for every lease, an early start on making room for a
likely request, backfilling of short builds past a large waiting request, and the timing of
stops, chosen to minimise expected cost (memory held against restarts needed) over the observed
record of idle gaps, rather than fixed.

## Measuring it

A policy that decides when to stop people's work has to be measured, not argued for. AgentRM can
record a trace of everything it sees and replay any configuration over it offline, scoring every
stop against the recording itself: a stop is wrong if the recording shows the thing used again.
The reaper rule is replayed on the same trace as the baseline.

On a synthetic hour (an agent starts a dev server with `&`, exits five minutes in, and someone
uses the server forty minutes later), the reaper kills the server on its first sweep, while its
agent is still running; AgentRM leaves it alone until the agent exits and collects it on the
owner-gone path. Both stops count as wrong in that variant, which is the point of scoring
against the recording. Numbers from real days are the next step and are not in this document
yet: they need traces recorded on real machines (`agentrm record on`).

## What it does not do

AgentRM runs as you, with no privileges, so it can act only on your processes. It cannot see the
environment of Apple's own binaries on macOS, so a detached `/bin/sleep` is owned only if the
tree was sampled first. It trusts an agent's word only where that lowers the agent's rights. And
it starts in dry run: it asks and reports, and stops nothing, until its record says it may.
