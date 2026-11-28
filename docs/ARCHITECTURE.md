# Architecture

AgentRM is a daemon (`agentrm daemon`), a CLI and hook client (the same binary), and a menu-bar
app that runs the daemon as its child process (started when the app opens, stopped when it quits,
and exiting by itself if the app dies) and reads its state through the CLI.

```
Claude Code ──hook──┐
Codex ──shim/CLI────┤                 ┌── discovery (docker, simctl, ps, lsof)
MCP clients ──mcp───┼── unix socket ──┤── policy (admission, leases, claims, orphans)
AgentRM.app ──CLI───┘   (peer PID)    └── effects (pause, resume, stop) ── outside the lock
```

## Packages

| Package | Owns | Depends on |
|---|---|---|
| `internal/model` | Shared types (Lease, Resource, Session, Config, kinds) and their JSON form | — |
| `internal/sys` | Facts about the machine: processes, memory, pressure, disk, load, ports, peer PID, running commands with timeouts | — |
| `internal/shell` | What a command line runs: parsing, inert commands, heavy-command classification | — |
| `internal/discover` | Finding resources and acting on them (stop, pause, resume) | model, sys |
| `internal/evidence` | Which resources a command uses; whether a resource shows activity | model, shell |
| `internal/store` | State and config on disk: atomic, fsync'd, debounced, versioned with migrations; rotating logs | model |
| `internal/manager` | Policy: admission, queue, reclamation, leases, claims, orphans, attribution, identity mapping | all of the above |
| `internal/api` | Wire types, protocol version, client | model |
| `internal/server` | Unix-socket HTTP server: peer identity, routing, version check | api, manager, sys |
| `internal/hook` | Claude Code hook entry point (fails open) | api, shell |
| `internal/setup` | Installing and repairing hooks, the Codex note, the CLI link | api |

`main.go` and `cli.go` are the command line.

## Decisions

**D1. One lock, no I/O under it.** The manager keeps one mutex over its state. Nothing that can
block (exec, file writes, network) runs while it is held. Decisions that need an external action
return *effects*; the server runs them after unlocking and reports the results back under the
lock. A hung `docker` therefore delays one request, not every agent's hooks.

**D2. State stays a JSON file, not SQLite.** The state is small (tens of KB), has no query
pattern beyond "load all", and is already written atomically (temp file, fsync, rename), so a
crash cannot corrupt it. The real costs were rewriting it on every request and fsync-less
renames; both are fixed by debounced writes (at most once a second, and on shutdown) with fsync.
SQLite would add a C dependency or ~10 MB of pure-Go driver for no gain.

**D3. No cgo.** The daemon ships as one static universal binary. Kernel values (pressure level,
swap usage, load, memory size) are read with `sysctl` through `golang.org/x/sys/unix`, not by
running `sysctl`. `vm_stat` is still run once per sweep, because `host_statistics64` needs Mach
calls that would need cgo.

**D4. A failed source keeps its last view.** If `docker ps` fails or times out, the manager keeps
the containers it last saw from Docker instead of concluding they are gone. Losing them would
drop their leases and restart their orphan clocks. Each source has a timeout and a back-off.

**D5. Versioned protocol and state.** The API has a version, checked on every request; a
mismatch (an app update that has not restarted the daemon) gets a clear error, and hooks fail
open. The state file has a version and runs migrations on load. Hook commands carry a protocol
number so the app can see stale hooks and repair them.

**D6. Shell text is parsed, not split.** Command lines are parsed with `mvdan.cc/sh` into a
syntax tree; only commands that execute something count, and heredoc bodies, quoted strings and
comments are data. If parsing fails, the old splitter is the fallback.

**D7. The privileged helper is not built.** Running the decision-maker as root (so an agent
running as the user could not tamper with it) is the only real enforcement, but it needs an
admin prompt at install and changes the security model. It is designed in
[threat-model.md](threat-model.md) and waits for an explicit decision.
