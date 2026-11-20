# The vault

Agents keep needing accounts: a GitHub token to open a PR, an API key to call a model, AWS to
deploy, a login to check staging. The usual answer is to paste a long-lived secret into the chat
or an `.env`, where it stays, in the transcript and on disk, with every permission it has.

The vault inverts that. **An agent asks for an account and says why; you approve with Touch ID;
it gets access that expires by itself.** The secret stays in your Keychain.

```sh
agentrm vault list
agentrm vault run github-openbox --scope contents:write,pull_requests:write --repo agentrm \
  --purpose "push the fix for T214 and open its PR" -- gh pr create --fill
```

The first time, the agent waits (up to ~2 minutes; `vault run` exits 75 and can be re-run to
keep waiting) while AgentRM shows you who is asking, for what, for how long and why. Approve
with Touch ID and the command runs. The same session asking again for no more than it was given
gets it without asking you, until the grant expires (30 minutes by default; each account sets
its longest).

## What each kind of account gives

| Kind | What the agent gets | When it ends |
|---|---|---|
| **GitHub App** | A fresh installation token limited to the repos and permissions asked (`GH_TOKEN`, `GITHUB_TOKEN`, and git set up to push with it) | Expiry, revoke or session end: revoked at GitHub at once. GitHub's own limit is one hour |
| **API key** | A token for AgentRM's local proxy and the proxy's address (`OPENAI_API_KEY`, `OPENAI_BASE_URL`...). The proxy adds the real key to each request. `read` grants only allow GET | At once, everywhere: the proxy stops accepting the token. **The agent never holds the key** |
| **AWS** | An STS session for one of the account's roles (`AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`, `AWS_REGION`) | At its expiry (15 minutes at least). STS sessions cannot be cut short: a revoked grant still works until then, and the record says so |
| **Login** | The username and password (`AGENTRM_USERNAME`, `AGENTRM_PASSWORD`, `AGENTRM_URL`) | The grant ends, but a password cannot be taken back once seen. Prefer the other kinds |

Agents that use AgentRM over MCP have `agentrm_vault_list`, `agentrm_vault_request` and
`agentrm_vault_revoke`. Over MCP an API key's access is handed over as it is to a command (a
proxy token and base URL, never the key); any other kind is handed over only with `reveal`,
because a tool's answer goes into the transcript.

`agentrm vault run` puts the access in the command's environment only. `agentrm vault reveal`
prints it as shell exports: that is a separate approval, marked in red, because what an agent
prints goes into its transcript, and so to its model provider.

## Setting accounts up

Open AgentRM → Vault → Add account. Saving asks for Touch ID; secrets go to the Keychain, the
rest to the daemon. Editing an account ends its live grants.

**GitHub App.** Create one at github.com/settings/apps/new (or your organisation's), with no
webhook, and the most permissions you would ever let an agent have (for example Contents: write,
Pull requests: write, Metadata: read). Generate a private key, install the app on the repos
agents may touch, and note the App ID and the installation ID (the number at the end of
github.com/settings/installations/<id>). In AgentRM, the repos and permissions you list are the
most an agent may ask for; each grant is narrower if the agent asks for less (the default is
`contents:read`).

**API key.** Choose OpenAI, Anthropic or OpenRouter to fill in the address and variables, or
enter your own. The tool the agent runs must let its API address be set by a variable (most
SDKs and CLIs do: `OPENAI_BASE_URL`, `ANTHROPIC_BASE_URL`...). Tick read-only for keys agents
should only query with.

**AWS.** Keys of an IAM user whose only permission is `sts:AssumeRole` on the roles listed, and
the role ARNs. Each role's trust policy must allow that user. Agents ask for a role by name.

**Login.** A username, a URL and a password. Use it for staging dashboards and test accounts,
not for anything you could not change in a minute.

## Grants

The Vault tab lists live grants (with Revoke) and the last ones that ended, with why: expired,
revoked, its session ended, the account changed, AgentRM restarted. Agents see their own with
`agentrm vault list` and can end them with `agentrm vault revoke <grant>`.

Each session is told once, in its inbox, which accounts it may ask for. Codex is told in
`~/.codex/AGENTS.md`.

## What it does and does not protect

See [the threat model](threat-model.md#the-vault). In short: it keeps secrets out of chats,
transcripts and dotfiles, makes access short-lived and scoped, and leaves a record. It does not
stop an agent that has been given access from misusing it while the grant lasts, and an agent
can print what `vault run` gives it (only the API-key proxy keeps the secret itself away).
