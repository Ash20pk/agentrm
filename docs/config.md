# Configuration

`~/.agentrm/config.json`, created on first run with defaults sized to this Mac's memory. AgentRM
notices edits within a sweep and applies them (and tells you, in case it was not you). The app's
The Limits tab of the Activity window changes the common ones. Keys not listed are ignored.

## Memory and admission

| Key | Default (16 GB Mac) | Meaning |
|---|---|---|
| `headroom_gb` | 1 (0.75 on 8 GB, 2 on 32 GB, 3 on 64 GB) | Memory kept free after any grant |
| `max_swap_mb_per_min` | 300 (scaled to memory) | Swapping out faster than this is thrashing: nothing is granted |
| `max_session_share` | 0.6 | While others wait, one session may hold at most this share of usable memory |
| `heavy_growth_gb` | 1.5 (scaled) | A session's process tree growing this much, unexplained by a lease, becomes an inferred lease |
| `unleased_gb` | 2 (scaled) | Flag a session using this much with no lease at all |
| `min_disk_gb` | 10 | Refuse disk-heavy leases below this free space on `/` |
| `max_load_per_core` | 1.5 | Refuse builds above this 1-minute load per core |
| `kind_mem_gb` | per kind | Starting estimates, until AgentRM has learned real ones |

## Leases and claims

| Key | Default | Meaning |
|---|---|---|
| `default_ttl_minutes` | 120 | Lease lifetime, renewed while used |
| `build_ttl_minutes` | 30 | Lifetime of a build lease (not renewed; released when the command ends) |
| `idle_minutes` | 30 | A held resource unused this long gets its holder asked |
| `claim_window_minutes` | 60 | A claim is verified if the session used the resource this recently |
| `unverified_ttl_minutes` | 30 | Lifetime of a claim with no evidence |
| `pressure_grace_minutes` | 2 | Deadline for pressed holders while a request waits |
| `pressure_idle_minutes` | 5 | A lease unused this long can be reclaimed for a waiting request |
| `queue_minutes` | 10 | A queued request keeps its place this long after its session was last heard from |
| `grant_hold_minutes` | 3 | A lease granted from the queue is held this long for its session to take up, then goes to the next in line |

## Unclaimed resources

| Key | Default | Meaning |
|---|---|---|
| `dry_run` | true | Ask and report, but never pause or stop (except the levels turned on in `auto_stop`) |
| `auto_stop` | {} | Levels that pause and stop on their own while `dry_run` is on, in order: `owner_gone`, `unclaimed`, `idle_lease`. `agentrm config --auto-stop owner_gone=on` |
| `grace_minutes` | 10 | How long an orphan runs before it is paused |
| `pause_minutes` | 30 | How long it stays paused before it is stopped; 0 stops directly |
| `auto_thaw_on_connect` | true | Resume a paused server when something connects to it |
| `never_stop` | [] | Resource IDs or name substrings never paused or stopped (pinning does the same per resource) |
| `owner_gone_grace_minutes` | 2 | Unclaimed, and the agent that started or held it has exited: paused after this |
| `owner_gone_pause_minutes` | 10 | How long such a thing stays paused before it is stopped |
| `old_generation_hours` | 24 | Running this long with no known owner: three times the grace |
| `leftover_min_mb` | 40 | A process an exited agent left running is tracked from this size (or 2% CPU), by footprint |
| `learned_timing` | true | Choose the wait before pausing something unclaimed from how long things of its kind sit idle before reuse |
| `min_timing_samples` | 20 | Observations per kind before learned timing is used |
| `restart_weight_gb` | 4 | A minute spent restarting something is worth this many GB-minutes of memory held |

## The queue

| Key | Default | Meaning |
|---|---|---|
| `queue_minutes` | 10 | A queued request keeps its place this long after its session was last heard from |
| `grant_hold_minutes` | 3 | A lease granted from the queue is held this long for its session to take up |
| `backfill` | true | A short build may run ahead of a larger waiting request, in free memory |
| `backfill_max_minutes` | 5 | "Short": nine in ten of that kind's builds in this project finish within this |

## Traces

| Key | Default | Meaning |
|---|---|---|
| `record_trace` | false | Keep a trace for `agentrm replay` (`agentrm record on`) |
| `trace_days` | 14 | Days of traces kept |

## Recognising things

| Key | Default | Meaning |
|---|---|---|
| `extra_patterns` | {} | More commands per lease kind, matched as typed: `{"compose": ["^\\./stack\\.sh\\s+up"]}` |
| `server_names` | node, python, ruby, bun, deno, php, java, uvicorn, gunicorn, air, main | Listening processes treated as dev servers |
| `server_exclude` | mcp, claude, codex, language servers | Substrings that are never dev servers |

## Safety and operation

| Key | Default | Meaning |
|---|---|---|
| `require_app_approval` | true | Pinning, settings, priorities and overriding others from a terminal wait for a click in the app |
| `sweep_seconds` | 30 | Full discovery interval |
| `fast_sweep_seconds` | 3 | Memory and process-tree sampling while something needs it |
| `log_level` | info | `debug`, `info`, `warn`, `error` for `~/.agentrm/agentrm.log` |

## Files

| File | What |
|---|---|
| `config.json` | This configuration |
| `state.json` | Leases, sessions, learned estimates, decisions (versioned; migrated on upgrade) |
| `events.log`, `agentrm.log` | What AgentRM did and why (rotated at 5 MB, 3 kept) |
| `shims/` | The PATH shim links, if installed |
| `agentrm.sock` | The daemon's socket (only you can connect) |
