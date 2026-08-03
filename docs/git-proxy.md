# Git & GitHub proxy 🔐

**Audience:** operators who sandbox their agents and want them to keep doing
ordinary feature work — fetch, push, open a pull request — without handing them
an SSH key or a GitHub token.

## The problem this solves

A tclaude [sandbox profile](agent.md#sandbox-profiles) can deny `~/.ssh` and
`~/.config/gh` — both ship in the dashboard's common-rule catalog — and can deny
or filter the network. That is the posture you want for an agent you do not
fully trust.

It is also the posture that stops the agent doing its job. Most agents are
spawned to write code and open a pull request, and both halves of that need a
credential.

The usual escape is to widen the sandbox: reopen the credential directories,
open the network. That works, and it gives the agent the credentials.

`tclaude agentd` already runs unsandboxed on the host and already holds those
credentials. So it performs the network half on the agent's behalf. The agent
issues a *semantic* request — "push my branch", "open a pull request" — and
never sees a key, a token, or a command line.

```bash
# Inside a sandboxed agent that cannot read ~/.ssh:
tclaude agent git push -u
tclaude agent github pr create --title "Add the thing" --body-file pr.md
```

## The one invariant

**The proxy lends credentials, not filesystem reach.**

Every operation it performs is one the agent could already have performed on
files it can already write. The only thing it lacked was the secret. Concretely:
the repository is always the git work tree containing the agent's own
daemon-recorded launch directory. There is no `--repo` flag, no path parameter,
and nothing in the request an agent could use to aim the daemon's credentials at
a different checkout.

That directory comes from `sessions.resume_provenance` — the immutable physical
path captured at launch. It deliberately does **not** come from the
`agent_workdir` table, which the agent's own PostToolUse hook writes, and which
would therefore be agent-influenced.

## Enabling it

The proxy is **off** until you allow-list at least one remote. There is no
"allow everything" setting and no default that turns it on.

Add an `agent.git_proxy` block to `~/.tclaude/data/config.json`:

```json
{
  "agent": {
    "git_proxy": {
      "allowed_remotes": ["github.com/your-org"],
      "protected_refs": ["main", "master", "release/*"],
      "allow_force_push": false,
      "ssh_key": "",
      "github_token_file": ""
    }
  }
}
```

| Field | Meaning |
|---|---|
| `allowed_remotes` | Remotes the proxy may talk to, as `host/owner/repo` patterns. **Empty or absent disables the proxy entirely.** |
| `protected_refs` | Branches the proxy refuses to push to at all. Absent → `["main", "master"]`; an explicit `[]` turns the protection off. |
| `allow_force_push` | Permits `--force-with-lease` on non-protected refs. Default off. Plain `--force` is never available. |
| `ssh_key` | Pins one private key (`ssh -i … -o IdentitiesOnly=yes`). Empty uses the daemon's ambient SSH setup — normally an ssh-agent, which is the better posture. |
| `github_token_file` | A file whose contents become `GH_TOKEN`. Empty lets `gh` use the daemon's own authenticated configuration. |

### Allow-list patterns

Patterns are slash-separated and matched case-insensitively against the
remote's resolved `host/owner/repo`. `*` matches exactly one segment, and a
pattern with fewer segments matches as a prefix:

| Pattern | Matches |
|---|---|
| `github.com` | every repository on that host |
| `github.com/your-org` | every repository in that owner |
| `github.com/your-org/*` | the same, spelled explicitly |
| `github.com/your-org/one-repo` | exactly that repository |

Matching is **segment-wise**, so `github.com/tofu` does not authorize
`github.com/tofutools-evil`, and `github.com` does not authorize
`github.com.attacker.net`.

### Granting the permissions

Four slugs, none granted by default and none conferred by group ownership:

| Slug | Allows |
|---|---|
| `git.read` | `git remotes`, `git ls-remote`, `git fetch` |
| `git.push` | `git push` |
| `github.read` | `github pr ls/view/checks`, `github issue ls/view` |
| `github.write` | `github pr create/comment/ready`, `github issue comment` |

```bash
tclaude agent permissions grant <agent> git.read
tclaude agent permissions grant <agent> git.push
```

An agent without a grant can still ask for a one-off with `--ask-human 60s`,
which raises the ordinary [approval popup](agent.md#ad-hoc-human-approval-ask-human).

## What an agent can do

```bash
# Discovery — no network, no credential. Run this first.
tclaude agent git remotes

# Reads (git.read)
tclaude agent git ls-remote --heads
tclaude agent git fetch --prune
tclaude agent git pull                 # daemon fetch + LOCAL fast-forward

# Writes (git.push)
tclaude agent git push -u
tclaude agent git push --force-with-lease   # only if you enabled it

# GitHub (github.read / github.write)
tclaude agent github pr create --title "…" --body-file pr.md
tclaude agent github pr ls --state open
tclaude agent github pr view 42
tclaude agent github pr checks 42
tclaude agent github pr comment 42 --body-file reply.md
tclaude agent github pr ready 42
tclaude agent github issue ls
tclaude agent github issue view 7
tclaude agent github issue comment 7 --body-file note.md
```

`git remotes` is the command to point an agent at when something is refused: it
lists every remote with the allow-list verdict and the reason, so the agent can
tell you exactly what to add instead of guessing from a 403.

### Why there is no daemon-side `pull`

`git pull` updates the working tree, and updating the working tree runs
`.gitattributes` smudge filters — arbitrary programs named by a file inside the
agent's own repository. Running that in the daemon would put the agent's own
configuration in charge of what the daemon executes.

So `tclaude agent git pull` is split: the daemon fetches (the half that needs a
credential), and the fast-forward runs as an ordinary local `git merge --ff-only`
in the agent's own process, under its own sandbox, where it always was. A merge
that is not a fast-forward is reported and left for the agent to resolve; the
daemon does not merge.

## How it is hardened

The repository's own `.git/config`, `.git/hooks/*` and `.gitattributes` are all
files the agent can write, and every one of them can name a program for git to
run. The proxy therefore treats repo-local configuration as hostile.

| Vector | What the proxy does |
|---|---|
| `.git/hooks/pre-push`, `reference-transaction`, … | `core.hooksPath` is pinned to a daemon-owned empty directory. |
| `.gitattributes` filter programs | Only `fetch` / `push` / `ls-remote` run in the daemon; none updates the working tree. |
| `ext::<command>` remote URLs | Refused by the URL parser **and** by `protocol.ext.allow=never`. |
| `file://` and local-path remotes | Refused — they have no host to allow-list. |
| `http://` and `git://` remotes | Refused — no cleartext credentials, no unauthenticated transport. |
| `url.<base>.insteadOf` rewrites | Cannot be reset by a `-c` override, so instead each validated URL must be a **fixed point** of git's own rewriting. A repository that would redirect it is refused. |
| `remote.<name>.pushurl` | Validated separately from the fetch URL — otherwise push would be aimed somewhere unchecked. |
| `remote.<name>.uploadpack` / `receivepack` / `vcs` / `proxy` | **Refused outright** — a repository that sets one of these is refused rather than "neutralized". These keys select a *program*, and `uploadpack`/`receivepack` are read first-wins across config scopes, so a `-c` override does **not** displace a repo-local value. The stock programs are additionally passed as `--upload-pack` / `--receive-pack` flags, which do override config. |
| `core.sshCommand`, `core.alternateRefsCommand`, `core.fsmonitor`, `core.editor`, `core.pager`, `gpg.program`, `diff.external`, `http.proxy` | All pinned. |
| A repo-local `credential.helper` (an arbitrary command) | The helper list is reset, then repopulated from **global/system** configuration only, so your real helper keeps working. |
| Argument injection | Every parameter is charset-validated and refused if it begins with `-`. There is no passthrough flag and no `--` escape. |
| A hung transport | Every call is time-bounded and runs in a private process group that is killed after `Wait`, so an `ssh` child cannot outlive the request. |
| Secrets in `/proc` | Tokens travel in the child's environment or a `0600` file, never in argv. PR bodies and comments go through `--body-file`. |

Nothing here ever runs a shell.

For GitHub specifically, `gh` runs in a **neutral directory** — never the
agent's repository — and always with an explicit `--repo <owner>/<repo>` derived
from the validated remote. Otherwise `gh` would discover the repository by
reading `.git/config`, which would defeat the allow-list.

## What this is not

`agentd`'s permission layer is a coordination guardrail, not a security
boundary — see [Sandbox hardening](sandbox-hardening.md). **This feature does
not change that.** A same-uid agent that is not actually confined by the OS
sandbox can read `~/.ssh` directly and has no need of the proxy at all.

The proxy is what makes *denying* those paths survivable. It is not what
enforces the denial. If you have not set up the OS sandbox, adding this changes
nothing about what a hostile agent can reach.

Two consequences worth stating plainly:

- **Prefer SSH remotes.** With an ssh-agent, no secret enters the proxied child
  process at all. An HTTPS remote with a token puts that token in the child's
  environment, readable through `/proc/<pid>/environ` by any same-uid process
  for the life of the call.
- **Everything the GitHub half writes is attributed to your GitHub account.** A
  PR the agent opens is a PR you opened.

## Auditing

Every proxied call — including the reads, which is why they are POSTs — writes
an `audit_log` row visible in the dashboard: who ran what, against which remote
and ref, and with what exit code. Bodies, titles, tokens and subprocess output
are deliberately never recorded.

```bash
tclaude agent git push        # → audit verb "git.push"
tclaude agent github pr create # → audit verb "github.pr.create"
```

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| `503 git_proxy_disabled` | No `allowed_remotes` configured. Add the block above. |
| `403` naming a slug | The agent lacks `git.read` / `git.push` / `github.read` / `github.write`. Grant it, or the agent can retry with `--ask-human`. |
| `remote … is not on the operator's allow-list` | Run `tclaude agent git remotes` to see the resolved `host/owner/repo`, then add a matching pattern. |
| `protected_ref` | The branch is in `protected_refs`. Push a feature branch and open a PR. |
| `force_push_disabled` | Set `allow_force_push: true` if you want it. |
| `this repository rewrites its … URL (url.*.insteadOf)` | The repo has a rewrite rule that would redirect the validated URL. Remove it, or point the remote directly at the real URL. |
| `refusing an 'ext::' remote URL` | The remote names a command, not a server. Something has rewritten `.git/config`; inspect it. |
| `this repository sets remote.X.uploadpack …` | The repository configures a program-selecting key for that remote. Remove it with `git config --unset remote.X.uploadpack` (or `receivepack` / `vcs` / `proxy`). |
| `tool_missing` | `git` or `gh` is not installed on the host running agentd. |
| A push hangs then times out | Usually a passphrase-protected key that is not loaded into an ssh-agent. The proxy runs `ssh -o BatchMode=yes`, so it fails rather than prompting — load the key with `ssh-add`, or set `ssh_key`. |

## See also

- [Agent coordination](agent.md) — the permission model and the CLI surface.
- [Sandbox hardening](sandbox-hardening.md) — the OS sandbox this feature
  assumes you have set up.
- [How sandboxing works](sandboxing.md) — the operator mental model.
