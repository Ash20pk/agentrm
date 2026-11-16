# Threat model

AgentRM decides which coding agents may use this Mac's memory, and stops what nobody claims.
This page says what it defends against, what it does not, and why.

## Who acts

| Actor | Runs as | Can AgentRM tell it apart? |
|---|---|---|
| You | your user | Yes: a caller under a terminal, an IDE or the AgentRM app, with no agent process above it |
| A hooked agent (Claude Code) | your user | Yes: a caller under the `claude` process; its session id comes from its hooks |
| An unhooked agent (Codex, Gemini CLI, Aider, OpenCode, Cursor) | your user | Yes, by process ancestry; gated through the PATH shim when installed |
| A detached process (`nohup`, `setsid`, a daemon an agent started) | your user | It hangs off launchd with no terminal above it, so it gets no identity and no rights |

Everything runs as you. That is the root fact: AgentRM is a referee among cooperating agents, not
a sandbox around hostile ones.

## What it defends against

**Sloppy agents** (the main case): forgetting to stop clusters, servers, simulators, Gradle
daemons; starting heavy work on a machine that has no room. Leases, idle questions, pause and
stop handle this.

**Agents that say what is convenient.** An agent cannot:
- claim to be another session: identity comes from the socket's peer PID, not from what it says;
- claim to be you: only a caller with a GUI ancestor and no agent ancestor is a person;
- verify a claim by mentioning a resource: only executed commands count, not `echo`, `grep`,
  quoted strings or heredoc bodies;
- get a small charge by declaring a small size: declarations cannot go below what the kind has
  really used, and a session that overruns its declaration loses the right to declare;
- release another session's lease, stop another session's resource, pin, change settings or
  set priorities.

**An agent driving a terminal app to act as you.** Pinning, settings, priorities and overriding
another session from a terminal wait for a click in the AgentRM app (`require_app_approval`,
on by default). The notification names the terminal it came from and says to deny it if it was
not you.

**Quiet tampering.** These raise a notification (`TAMPER` or `OVERRIDE` events):
- the daemon was killed or crashed (no clean-shutdown marker on next start);
- `config.json` was edited outside AgentRM (the new settings are applied, and you are told);
- `state.json` was edited outside AgentRM (the daemon's copy is written back);
- the Claude Code hooks were removed other than through AgentRM;
- the hooks point at a binary that is not this copy, or not signed by the same developer
  (`agentrm setup check` and the app report it; Repair re-points them).

## What it does not defend against

- **An agent determined to cheat.** It can kill the daemon (launchd restarts it, and you are
  told), remove its hooks (you are told), delete `~/.agentrm` (you are told on restart), run a
  heavy job through a path AgentRM does not see, or use macOS UI scripting to click Approve if
  you have given it Accessibility permission. AgentRM makes these visible; it cannot prevent
  them while it runs as you.
- **Codex and other unhooked agents without the shim.** They are measured and attributed after
  the fact, not stopped before.
- **Commands that use a resource without naming it** (a script that reads a port from a file).
  They are caught by activity on the resource, not by the command.

## The privileged helper (decision D7, not built)

The only real enforcement is to take the decisions out of the user's reach: a root-owned helper
(`SMAppService.daemon`) that owns the state and the stop decisions, with the user-level daemon
reduced to reporting.

| | User daemon only (today) | With a privileged helper |
|---|---|---|
| Install | No prompt | An admin prompt at install, and macOS asks again to allow a background item |
| An agent kills AgentRM | Restarted by launchd; you are told | Cannot: it is root's process |
| An agent edits the state or config | You are told; the daemon's state wins | Cannot: root-owned files |
| An agent removes the hooks | You are told | Still possible (the hooks are in your settings); you are told |
| Stopping your processes | Allowed (same user) | Root can stop anything: the helper must re-check every target is yours |
| Attack surface | A user process | A root process reachable from user processes: it must treat every request as hostile |

Recommendation: do not build it until there is a real report of an agent defeating AgentRM on
purpose. The tampering notifications cover the cases seen so far, and a root helper is a
larger security responsibility than the problem it solves today.
