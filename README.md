# AgentRM

A resource manager for coding agents on macOS (and Linux): Activity Monitor for the agentic era.

When several Claude Code, Codex or other agent sessions share one laptop, each starts clusters,
builds, simulators and dev servers as if it had the machine to itself. Nobody stops them, memory
runs out, and everything slows to a crawl. AgentRM puts one rule in front of that: **ask first,
say why, and clean up after yourself.**

- **Leases.** Heavy work (a kind cluster, `docker compose up`, a Docker, cargo, Gradle or Xcode
  build, a simulator or emulator, a VM, a local model, a load test, a dev server) gets a lease
  first. It is granted only if the memory is really there and the Mac is not thrashing;
  otherwise the request is queued and AgentRM reclaims from whoever uses theirs least.
- **Evidence, not trust.** Who is asking comes from the process tree, not from what an agent
  says. Claims need recorded use. Leases stay alive on use, not on any activity of the session.
  Each agent's process tree is measured, so a wrapper script is counted too.
- **Ask, pause, stop.** Anything running with no lease is asked about, then paused (nothing is
  lost; using it resumes it), then stopped. Everything stopped can be restored.
- **You stay in charge.** Pinning, settings and overriding an agent from a terminal need a click
  in the app. Tampering (a killed daemon, edited files, removed hooks) is reported.
- **A vault for accounts.** An agent that needs GitHub, an API key, AWS or a login asks
  (`agentrm vault run <account> --purpose "why" -- <command>`); you approve with Touch ID; it
  gets access that expires by itself, scoped to what it asked. Secrets stay in your Keychain,
  and an API key never reaches the agent at all.
- **A menu-bar app** with memory, swap, headroom, leases, what is running, approvals, and a
  history of every decision for you to review.

New installs start in **dry run**: AgentRM asks and reports but never pauses or stops anything.
Mark its decisions right or wrong in the app, and turn on auto-stop when they look right.

## Install

Download `AgentRM-<version>.dmg` from [Releases](https://github.com/Ash20pk/agentrm/releases)
(or `brew trust ash20pk/tap && brew install --cask ash20pk/tap/agentrm`), drag AgentRM to Applications and open it. The
welcome window shows what is running and what connecting your agents would change, before it
changes anything. It updates itself (Sparkle, signed updates). Requires macOS 14 or later.

| Agent | How it is connected |
|---|---|
| Claude Code | Four hooks in `~/.claude/settings.json`: heavy commands are checked before they run |
| Codex, Gemini CLI, Aider, OpenCode, Cursor | A PATH shim in front of `docker`, `cargo`, `kind`…: checked before they run |
| Any MCP client | `agentrm mcp`, a stdio MCP server with acquire / release / claim / status tools |
| Codex | Also a short note in `~/.codex/AGENTS.md` |

## Use

Agents need nothing more. For you, or an agent asking explicitly:

```sh
agentrm status                     # memory, leases, what is running (or: agentrm top)
agentrm acquire --kind cluster --purpose "integration test for #123" [--until 60]
agentrm release <lease-id>
agentrm claim kind:dev --purpose "still debugging the ingress"
agentrm resume srv:4242            # thaw something AgentRM paused
agentrm restore kind:dev           # bring back something it stopped
agentrm review                     # mark its decisions right or wrong
agentrm collect                    # stop now what ended sessions left (restorable)
agentrm leak accept leak-1a2b3c4d  # restart (or stop) a process whose memory keeps climbing
agentrm receipt                    # the week: freed, found, waited, restored
agentrm compare                    # AgentRM against a parent-PID reaper, on your recorded traces
agentrm vault list                 # accounts agents may ask for, and live grants
agentrm vault run github-openbox --scope contents:write --purpose "push the fix" -- git push
```

## Docs

- [How it decides](docs/how-it-works.md): admission, the queue, evidence, pause and stop,
  garbage collection, cooperative preemption, learning from use
- [Agents and the lease API](docs/agents.md): Claude Code, Codex, Gemini CLI, Cursor; orchestrators
- [Traces and replay](docs/replay.md): measuring a policy on real days
- [Design](docs/design.md): garbage collection for what coding agents leave behind
- [The vault](docs/vault.md): short-lived, approved access to accounts for agents
- [Configuration](docs/config.md): every setting and file
- [Troubleshooting](docs/troubleshooting.md)
- [Threat model](docs/threat-model.md): what it defends against, and what it does not
- [Architecture](docs/ARCHITECTURE.md): packages and design decisions

## Build from source

```sh
go test ./... && go build -o agentrm .          # the daemon, CLI, hook, shim and MCP server
cd app && echo "DEVELOPMENT_TEAM = <your team id>" > Config/Local.xcconfig
xcodegen generate && open AgentRM.xcodeproj       # the menu-bar app (bundles the Go binary)
scripts/release.sh                                # notarized zip, DMG, appcast, cask (maintainers)
```

Requires Go 1.24+, Xcode 16+ and [XcodeGen](https://github.com/yonaskolb/XcodeGen).

## License

MIT
