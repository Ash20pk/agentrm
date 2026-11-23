# Agents, and the lease API

## Agents with hooks

AgentRM works best when an agent tells it about each shell command before running it. Four agents
can, through their hooks; one command installs AgentRM's (a backup of the file is kept, and your
own hooks and settings stay as they are):

| Agent | Install | File | Events used |
|---|---|---|---|
| Claude Code | `agentrm setup claude` | `~/.claude/settings.json` | `PreToolUse` (Bash), `PostToolUse`, `UserPromptSubmit`, `SessionEnd` |
| Codex | `agentrm setup codex` | `~/.codex/hooks.json` | `PreToolUse` (Bash), `PostToolUse` (Bash), `UserPromptSubmit`, `SessionEnd` |
| Gemini CLI | `agentrm setup gemini` | `~/.gemini/settings.json` | `BeforeTool` / `AfterTool` (`run_shell_command`), `BeforeAgent`, `SessionEnd` |
| Cursor | `agentrm setup cursor` | `~/.cursor/hooks.json` | `beforeShellExecution`, `postToolUse`, `beforeSubmitPrompt`, `sessionEnd` |

`agentrm setup preview <agent>` shows the change first; `--uninstall` removes it. Codex asks you to
trust new hooks: run `/hooks` in Codex once and approve them.

With hooks, a heavy command is checked before it starts (and refused in the agent's own terms when
memory is short, with the reason), questions reach the agent after its next tool call, and the
session's end ends its leases. Every hook fails open: if AgentRM is down or slow, the command runs
as if it were not there. (Cursor treats a silent permission hook as a refusal, so for Cursor the
hook always answers "allow" explicitly.)

Identity still comes from the process tree, not from what a hook says. One exception, for Cursor:
its agent runs inside Cursor.app, which by ancestry looks like you. A hook call from inside an app
that says it is an agent is taken as that agent, because that can only lower its rights: an agent
may do less than a person.

Agents without hooks (Aider, OpenCode, older versions) are covered by the PATH shim
(`agentrm setup shim`) and the MCP server (`agentrm mcp`).

## Accounts: the vault

An agent that needs GitHub, an API key, AWS or a login asks the vault instead of the person:
`agentrm vault list`, then `agentrm vault run <account> --purpose "<why>" -- <command>`. The
person approves with Touch ID; the command runs with short-lived access. Each session is told
once, in its inbox, which accounts exist. See [vault.md](vault.md).

## The lease API, for orchestrators

Tools that run several agents (Conductor, Claude Squad, a CI runner, your own scripts) can take
leases on their agents' behalf, or let each agent do it. The API is HTTP over the unix socket at
`~/.agentrm/agentrm.sock` (mode 0600), JSON in and out, one header:

```sh
curl --unix-socket ~/.agentrm/agentrm.sock -H 'X-AgentRM-API: 2' \
  -d '{"kind":"cluster","purpose":"integration tests for PR 41","mem_gb":3,"wait_seconds":600}' \
  http://agentrm/acquire
```

The caller is identified by its process (the socket's peer PID), walked up to the nearest agent
process or to a terminal or app. Run the request from inside the agent's process tree and it is
that agent's lease; from your own process, it is yours.

| Path | Body | What |
|---|---|---|
| `/acquire` | `kind`, `purpose`, `mem_gb`?, `ttl_minutes`?, `until_minutes`?, `wait_seconds`? | A lease, or the reason and a queue position. With `wait_seconds`, a queued request stays connected and is answered when it is granted: no polling. |
| `/release` | `lease_id`, or nothing for all of yours | Done with it |
| `/yield` | `lease_id`, `cost` (`none`/`low`/`high`), `restart_seconds`? | Answer "what would giving it up cost you now?" |
| `/claim` | `resource_id`, `purpose` | Adopt something already running |
| `/inbox` | | Questions for this session |
| `/renew` | | Keep this session's waiting leases alive |
| `/end` | `reason`? | The session is over: its leases are released and what it left is collected |
| `/status` | | Everything `agentrm status` shows, as JSON |

The CLI (`agentrm acquire --wait`, `agentrm yield`, …) is a thin client of the same API.

**Memory figures and receipts.** A request that sets `mem_gb` is charged exactly that; AgentRM's
own estimate is used only when it is left out. When a lease ends, AgentRM appends one line to
`~/.agentrm/receipts/<project>.txt`: what was asked, what the work really used (footprint of what
it started), and what to ask next time. Read it before asking. If the ask was far off (used under
60% or over 125%), the session also gets the figure in its inbox. macOS cannot cap a process's
memory, so a lease cannot stop an overrun: real use counts against what others are granted as it
runs, the session is told at once, and for an hour its requests are charged at least AgentRM's
estimate.

A sketch of an orchestrator loop: before starting an agent's heavy step, `acquire` with
`wait_seconds`; start the step when it returns granted; `release` when the step ends; answer
`yield` questions from `/inbox` if the orchestrator knows the step is idle; and `end` the session
when the agent is retired.
