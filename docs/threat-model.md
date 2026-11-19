# Threat model

AgentRM decides which coding agents may use this Mac's memory, and stops what nobody claims.
This page says what it defends against, what it does not, and why.

## Who acts

| Actor | Runs as | Can AgentRM tell it apart? |
|---|---|---|
| You | your user | Yes: a caller under a terminal or an IDE, with no agent process above it; the app by its bundled helper (below) |
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
- claim to be another session: identity comes from the socket's peer PID, not from what it says.
  A hook's session ID is believed only for the agent process it runs under, and never when that
  ID already belongs to another live agent process (session IDs are visible in
  `agentrm status --json`): the hook keeps its own session and a `TAMPER` event is raised once.
  A session whose agent has exited can be taken up (a resumed session). An agent inside an app
  (Cursor's) has its session ID kept in a name of its own (`cursor:<app pid>:<id>`), so it
  cannot name another agent's session;
- claim to be you: only a caller with a GUI ancestor and no agent ancestor is a person. Apps are
  recognised by the real executable path the kernel reports, never by argv, and the walk
  starts at the caller's parent: a process that set its argv[0] to an app's path, or that
  hangs off launchd, is detached;
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

## How the app is recognised

Rights that need a click in the app (`ViaApp`) are given only to a caller that is the app. The
app talks to the daemon through the `agentrm` helper bundled in it, which it runs as a child,
so a caller is the app when, as the kernel reports it at the time of the request:

- its executable (`proc_pidpath`, read with the `proc_info` system call: no cgo) is
  `<bundle>/Contents/Helpers/agentrm`. This is the executable's vnode path: the process cannot
  choose it, unlike argv[0] or the exec path in `kern.procargs2`, which live in its own memory;
- its parent's executable is `<bundle>/Contents/MacOS/AgentRM` of the same bundle, so the
  real helper run detached or from a shell is not the app;
- the bundle is `/Applications/AgentRM.app` or the bundle the daemon itself runs from;
- both executables pass `codesign --verify --strict` with a requirement that they are signed
  by the same team as the daemon. The result is kept while the file stays the same (device,
  inode, size, mtime, ctime), so `codesign` runs once per install, not per request.

What this does not guarantee:

- **It checks the files, not the running code.** A process that can write into the bundle and
  win a race between the check and the request could run other code under the right path.
  Checking the running process (`SecCodeCopyGuestWithAttributes`) needs the Security framework
  and so cgo; it is not done. An agent that can write into the bundle can also replace the
  daemon, which runs from it: the bundle is as trusted as the daemon.
- **An unsigned daemon (a development build) checks paths only**: it has no team to compare.
- **Driving the real app** (UI scripting with Accessibility permission) still works, as above.
- **Being "you" is weaker than being the app.** A person is anyone under a terminal or an IDE
  with no agent above it, and an agent can still get there: by asking Terminal to run a
  command (AppleScript), or by copying a shell into a bundle of its own
  (`~/x/Foo.app/Contents/MacOS/sh`) and running commands under it. That is why pinning,
  settings, priorities and overrides wait for the app (`require_app_approval`).
- **On Linux there is no app**: no caller is the app. Terminals and editors are recognised by
  the base name of `/proc/<pid>/exe`, which an agent can copy under any name.

## The vault

The vault ([vault.md](vault.md)) changes what AgentRM holds: secrets. Where they are, and who
can reach them:

- **At rest, in the login Keychain, in items the AgentRM app created.** The Keychain trusts the
  app that made an item, so the app reads them without a prompt and anything else, including an
  agent running `security find-generic-password` or a copy of the `agentrm` binary, makes macOS
  ask you. This is the one boundary in AgentRM that holds against a process running as you.
- **On approval, only.** The app reads an account's secret after Touch ID (or your password),
  for one request it shows you, and passes it to the daemon on the helper's stdin (never argv,
  which other processes can read). The daemon accepts it only from the app (`ViaApp`, above).
- **In the daemon's memory, while a grant lasts.** It mints the short-lived credential, keeps
  it, and forgets it when the grant ends. Nothing secret is written to `state.json`, traces,
  events, the log or `agentrm status`; tests check this. A daemon restart ends every grant.
- **In the agent's command.** `vault run` hands GitHub and AWS credentials and passwords to the
  command it runs, so the agent can print them (`vault run x -- env`). That is the point of
  making them short-lived and narrow: a GitHub token is one hour and some repos at most, an
  AWS session is one role. Only API keys stay out of reach: the agent gets a proxy token, and
  the proxy on `127.0.0.1` puts the key on requests. Any local process with a live proxy token
  can use it until the grant ends.

What approving protects against, and what it does not:

- An agent cannot approve its own request: answering needs the app, and the app needs Touch ID.
  Editing or adding an account needs Touch ID too, so a driven UI cannot point an API key's
  account at another server and then ask for it.
- One session's grant is not another's: grants are bound to the session the kernel says is
  asking (see "Who acts").
- A denied request cannot be repeated for a few minutes, so an agent cannot keep asking until
  you click the wrong button.
- **Approving is trusting the purpose the agent wrote.** The approval shows what it will get and
  where the request comes from; it cannot show what the agent will do with it. Keep each
  account's own limits tight (the GitHub App's permissions, the IAM user's roles): they are the
  ceiling no approval can raise.
- **AWS sessions and seen passwords cannot be taken back.** Revoking ends the grant in AgentRM;
  an AWS session works until it expires, and a password until you change it.

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
