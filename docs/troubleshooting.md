# Troubleshooting

Start with `agentrm setup check` and `agentrm status`. Both say what is wrong in plain words.

**"the daemon is not running"** — Open AgentRM: the app runs the daemon while it is open and stops
it when you quit. To have it running all the time, add AgentRM to System Settings → General →
Login Items. The daemon's log is `~/.agentrm/agentrm.log`; if it fails to start, its last error
is in `~/.agentrm/daemon.err`.

**"this client speaks API version N and the daemon speaks M"** — the app was updated but the
daemon was not restarted. The daemon exits within about 15 seconds of its binary being replaced,
and the app restarts it at launch when its version differs, so this should clear by itself; if
not, quit and reopen AgentRM, or run `launchctl kickstart -k gui/$(id -u)/dev.agentrm.daemon`.
Until then hooks fail open: nothing is checked.

**"another AgentRM daemon is already running"** — only one daemon runs per `~/.agentrm`; it holds
`~/.agentrm/daemon.lock`, which names its PID. Stop the other one (an `agentrm daemon` started
by hand, or a second copy of the app) rather than deleting the lock.

**An agent's command was refused** — the refusal says why (no memory, thrashing, full disk, busy
CPU, fair share) and what AgentRM is reclaiming. `agentrm status` shows who holds what. A request
that is queued does not need retrying: the lease is granted when it fits, and the agent is told on
its next tool call (or at once, if it ran `agentrm acquire --wait`). It then has
`grant_hold_minutes` to start. If it is urgent, mark the session:
`agentrm priority SESSION urgent` (you approve it in the app).

**Something I am using was paused** — anything that uses it resumes it: a command naming it, a
connection to its port, or `agentrm resume ID`. Claim it (`agentrm claim ID --purpose "..."`) or
pin it so it is left alone. Your browser holding a connection open counts as use.

**Something was stopped** — `agentrm status --stopped`, then `agentrm restore ID`. Mark the
decision wrong in Decisions in the Activity window (Open in the menu), so you know whether auto-stop is safe for you.

**A command was not recognised as heavy** — AgentRM still sees it if the agent's process tree
grows (an inferred "heavy" lease). To have it gated before it starts, add a pattern to
`extra_patterns` in `~/.agentrm/config.json`.

**A Docker or simulator source shows "unavailable"** — Docker or `xcrun` timed out. AgentRM keeps
the last view of their resources and backs off. Check that Docker Desktop is responsive.

**"The Claude Code hooks point at …"** — AgentRM was moved or updated. Activity window → Setup → Repair,
or `agentrm setup repair`. An installed app in /Applications repairs itself at launch.

**An approval appeared that I did not ask for** — deny it. Something ran `agentrm` from a terminal
app, possibly an agent driving it. `~/.agentrm/events.log` shows what and when.

**A "tampered" notification** — the daemon was killed, a file was edited behind its back, or the
hooks were removed. If it was not you, check what your agents ran around that time.

**Removing AgentRM** — Activity window → Setup → Uninstall, or `agentrm setup uninstall`, then
move the app to the Bin. It removes the Claude Code, Codex, Gemini CLI and Cursor hooks, the Codex
note, the CLI link and the PATH shim, and resumes anything paused (from `state.json` if the daemon
is not running). It lists what it removed, and if anything could not be undone it says what and
exits non-zero; run it again once that is fixed. Data stays in `~/.agentrm`.
