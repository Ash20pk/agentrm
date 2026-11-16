# Troubleshooting

Start with `agentrm setup check` and `agentrm status`. Both say what is wrong in plain words.

**"the daemon is not running"** — Open AgentRM. If the menu says macOS needs approval, allow
AgentRM in System Settings → General → Login Items. The daemon's log is
`~/.agentrm/agentrm.log`.

**"this client speaks API version N and the daemon speaks M"** — the app was updated but the
daemon was not restarted. Quit and reopen AgentRM, or run
`launchctl kickstart -k gui/$(id -u)/dev.agentrm.daemon`. Hooks keep working (they fail open).

**An agent's command was refused** — the refusal says why (no memory, thrashing, full disk, busy
CPU, fair share) and what AgentRM is reclaiming. `agentrm status` shows who holds what. The agent
keeps its place in the queue for 90 seconds between tries. If it is urgent, mark the session:
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
move the app to the Bin. Data stays in `~/.agentrm`.
