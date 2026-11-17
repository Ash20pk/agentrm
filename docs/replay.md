# Traces and replay

Every policy change in AgentRM can be measured on real days before it touches a real machine.

## Recording

```sh
agentrm record on      # asks for approval in the app, like any setting
agentrm record off
```

The daemon then writes what it sees to `~/.agentrm/traces/` (mode 0600), one gzipped file per
day and per daemon start: each sweep's process table (as a change from the previous one), memory,
what discovery found and established connections; each request (who asked, and what); each agent
exit; each Docker attribution; and each decision the daemon took. About 1.5 MB a day; files older
than `trace_days` (14) are removed. Command lines are kept with anything that looks like a secret
replaced (`--token=…`, `password=…`, bearer headers, long opaque strings); environments are never
recorded, only which agent session a process belongs to.

## Replaying

```sh
agentrm replay                                # this Mac's traces, this Mac's config
agentrm replay ~/.agentrm/traces/2026-11-17-090000.jsonl.gz --config candidate.json
agentrm replay --policy reaper                # the rule cleanup scripts use
agentrm compare                               # both, on the same traces
agentrm replay --json                         # every stop, with what happened after
```

Replay runs a fresh manager on the trace's own clock with auto-stop on, so every decision a
policy would take is taken (on nothing real). It reports:

- **Stops, and wrong stops.** A stop is wrong if the recording shows the thing used again within
  24 hours afterwards: a connection or traffic, CPU, a command naming it, a claim. The judgement
  comes from the recording, not from the policy, so every policy is scored the same way.
- **Stops while the agent that started it was still running.** For AgentRM this should be zero;
  for a parent-PID reaper it is every dev server started with `&`.
- **Memory freed**, in GB-hours, from each stop until the end of the trace or the next use.
- **Pauses**, and how many were used while paused (AgentRM thaws those: a cost, not a loss).
- **The queue**: requests that waited, and how long until granted.
- **Agreement with the live daemon**, for a trace recorded with the same config: the share of
  its recorded decisions the replay also took. A replay that cannot reproduce the live daemon
  cannot be trusted to judge a change.
- **The predictor**: next heavy command predicted, against the most-frequent-kind baseline.

## The reaper policy

`--policy reaper` is what cleanup scripts for agent leftovers do: on every sweep, kill anything
re-parented to launchd whose name looks like a dev process (`node`, `python`, `java`, `postgres`,
…). It is replayed on the same trace and scored the same way, as the baseline AgentRM has to
beat.
