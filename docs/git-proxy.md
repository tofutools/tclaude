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
tclaude proxy git push -u
tclaude proxy github pr create --title "Add the thing" --body-file pr.md
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

Install the agent-facing proxy skills explicitly:

```bash
tclaude setup --install-proxy-skills
```

They are not installed by `--install-agent-skills` or `--install-all`, because
operators who have not configured a credential proxy should not expose those
capabilities to their agents.

The proxy becomes available to an agent when it has a remote-scoped grant or
when the legacy operator-global `allowed_remotes` list is configured. Remote
access is authorized by the grant scope, the legacy list, or both. There is no
"allow everything" setting.

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
| `allowed_remotes` | **Legacy global policy.** Remotes the proxy may talk to, as `host/owner/repo` patterns. An unscoped grant still requires a non-empty list. A remote-scoped grant can operate with this empty; when both are configured, both must match. |
| `protected_refs` | Branches the proxy refuses to push to at all. Absent → `["main", "master"]`; an explicit `[]` turns the protection off. |
| `allow_force_push` | Permits `--force-with-lease` on non-protected refs. Default off. Plain `--force` is never available. |
| `ssh_key` | Pins one private key (`ssh -i … -o IdentitiesOnly=yes`). Empty uses the daemon's ambient SSH setup — normally an ssh-agent, which is the better posture. |
| `github_token_file` | A file whose contents are the GitHub token the daemon sends. Empty delegates to `gh auth token` — see [Where the GitHub token comes from](#where-the-github-token-comes-from). This feeds the **GitHub** half only; `git` itself authenticates over SSH or through the operator's own credential helper. |

Both path fields accept `~/…`, which expands to the home directory of the user
account under which `agentd` runs — that account must be able to read the file.
Shell variables are **not** expanded — a config file is not a shell, so
`"${HOME}/token.txt"` is taken literally. Use `~/` or an absolute path.

### Where the GitHub token comes from

The daemon calls GitHub's API directly, so it needs a token of its own. There
are exactly two places it looks:

| Source | When |
|---|---|
| `agent.git_proxy.github_token_file` | Whenever it is configured. An explicit choice of identity wins, and `gh` is not consulted at all — this is how you run the proxy on a host with no `gh` installed. A file that cannot be read, is empty, or contains a control character is an **error**, not a reason to fall back: quietly spending a different credential because the configured one was unreadable is the worst of both answers. |
| `gh auth token` | Otherwise. |

**That is the whole chain.** The daemon does not read `hosts.yml`, the OS
keyring, or `GH_TOKEN`/`GITHUB_TOKEN`, and it does not try to work out which
account you are logged in as. gh already answers all of that, differently on
different hosts, and a second implementation living here would be a copy that
drifts — with bugs that present as authentication failures.

So an operator who has run `gh auth login` configures nothing and it works;
one who would rather not have `gh` on the host sets `github_token_file`. With
neither, every GitHub verb fails with `token_missing`, carrying gh's own
message.

`gh auth token` runs once per request, not once per API call — fewer
invocations than the proxy made when every verb was a `gh` command.

The token is sent to GitHub as an `Authorization` header: it reaches the API
through neither a child process's arguments nor its environment, so unlike the
`GH_TOKEN` this proxy used to hand `gh` there is no window in which
`/proc/<pid>/environ` exposes it to a same-uid process. That is a statement
about the **request**; obtaining the token can still run gh, and on that path
the token is gh's to hold for the length of one short-lived process.

### Remote patterns

The legacy allow-list and the `remote` grant scope use the same
case-insensitive, slash-segmented matcher against the remote's normalized
`host/owner/repo`. `*` matches exactly one segment, and a pattern with fewer
segments matches as a prefix:

| Pattern | Matches |
|---|---|
| `github.com` | every repository on that host |
| `github.com/your-org` | every repository in that owner |
| `github.com/your-org/*` | the same, spelled explicitly |
| `github.com/your-org/one-repo` | exactly that repository |

Matching is **segment-wise**, so `github.com/tofu` does not authorize
`github.com/tofutools-evil`, and `github.com` does not authorize
`github.com.attacker.net`.

The normalized key lower-cases the complete host/owner/repository key and
strips the URL scheme, credentials and a trailing `.git`.
SSH and HTTPS URLs for the same repository therefore produce the same key:
`git@github.com:your-org/repo.git` and
`https://github.com/your-org/repo.git` both become
`github.com/your-org/repo`.

### Granting the permissions

Four slugs, none granted by default and none conferred by group ownership:

| Slug | Allows |
|---|---|
| `proxy.git.read` | `git remotes`, `git ls-remote`, `git fetch` |
| `proxy.git.push` | `git push` |
| `proxy.github.read` | `github pr ls/view/checks/comments`, `github issue ls/view`, `github run ls/log-failed/artifacts/download` |
| `proxy.github.write` | `github pr create/edit/comment/ready`, `github issue comment` |

```bash
tclaude agent permissions grant <agent> proxy.git.read
tclaude agent permissions grant <agent> proxy.git.push

# Preferred: constrain each credential grant to the remotes it needs.
tclaude agent permissions grant <agent> proxy.git.read \
  --scope 'remote=github.com/your-org/*'
tclaude agent permissions grant <agent> proxy.git.push \
  --scope 'remote=github.com/your-org/*'
```

A scoped grant is opt-in: existing unscoped grants behave as before. When a
global `allowed_remotes` list is also present, authorization is the
intersection — a scoped grant cannot widen beyond the global list.

To migrate from the legacy global policy, copy its patterns into the relevant
per-agent or group grants as `remote` scopes. After verifying those grants,
empty `allowed_remotes` on the operator's schedule. Keep the `agent.git_proxy`
block only if it still carries credential or protected-ref policy. No automatic
migration or runtime warning is emitted.

An agent without a grant can still ask for a one-off with `--ask-human 60s`,
which raises the ordinary [approval popup](agent.md#ad-hoc-human-approval-ask-human).

## What an agent can do

```bash
# Discovery — no network, no credential. Run this first. With a scoped
# proxy.git.read grant, each remote's verdict combines the global list and its scope.
tclaude proxy git remotes

# Reads (proxy.git.read)
tclaude proxy git ls-remote --heads
tclaude proxy git fetch --prune
tclaude proxy git pull                 # daemon fetch + LOCAL fast-forward

# Writes (proxy.git.push)
tclaude proxy git push -u
tclaude proxy git push --force-with-lease   # only if you enabled it

# GitHub (proxy.github.read / proxy.github.write)
tclaude proxy github pr create --title "…" --body-file pr.md
tclaude proxy github pr ls --state open
tclaude proxy github pr ls --head feat/x --state all   # the PR for a branch
tclaude proxy github pr view 42
tclaude proxy github pr checks 42
tclaude proxy github pr comments 42            # read ALL review feedback
tclaude proxy github pr comment 42 --body-file reply.md
tclaude proxy github pr edit 42 --body-file new-description.md
tclaude proxy github pr ready 42
tclaude proxy github issue ls
tclaude proxy github issue view 7
tclaude proxy github issue comment 7 --body-file note.md
tclaude proxy github run ls --branch feat/x --status failure
tclaude proxy github run log-failed 18234567890   # why that check went red
tclaude proxy github run artifacts 18234567890    # what it uploaded, and how big
tclaude proxy github run download 18234567890 --name coverage
```

### Reading review feedback and CI failures

`pr checks` names the job that went red; `run log-failed` says why; `run ls`
finds the run id in between. An agent can walk from "CI is red" to the failing
assertion without a token and without a browser:

```bash
tclaude proxy github run ls --branch feat/x --status failure --limit 5
tclaude proxy github run log-failed <databaseId from that listing>
```

The id is also recoverable from the `detailsUrl` in a `pr checks` rollup
(`…/actions/runs/<run-id>/job/<job-id>`), which is worth knowing but is the
long way round.

`run ls` additionally reaches runs `pr checks` cannot show at all: a
`statusCheckRollup` is scoped to the pull request's **head commit**, so a
force-push or an amend takes every run against the superseded commit out of
`pr checks`, while `run ls --branch` still lists them. Compare `headSha`
against the commit you care about.

Re-runs are **not** such a case, and it is worth being explicit because the
intuition points the wrong way: re-running a workflow does not create a new
run, it adds an *attempt* to the same run id. A failure that was re-run green
therefore reports as green in `pr checks` and in `run ls` alike, and
`run log-failed` reads the latest attempt. The `attempt` field shows that a run
has been re-run; reading an earlier attempt's log is not something the proxy
offers.

`pr comments` returns everything said on the pull request, in two sections:
the **conversation** (issue comments and the body of each review submission,
interleaved oldest-first) and the **inline review comments** (the line-level
notes inside each review's diff threads, with file, line and permalink). Both
are needed for a review bot: CodeRabbit posts its summary as a review body and
every actionable finding as an inline comment, so the conversation alone tells
you the PR was reviewed and not what the review said.

`pr comments` and `run log-failed` are the only verbs that return **text rather
than JSON** — every other read, `pr checks` included, answers with a JSON
document in the field vocabulary `gh --json` used. These two are different
because their output is the payload rather than a diagnosis: each section keeps
a 256 KiB tail, so a long conversation cannot squeeze out the inline findings,
and `run log-failed` is allowed 180s because it downloads the run's whole log
archive rather than calling one endpoint. Only the *failed* steps are ever
available; there is no whole-run equivalent, because the full log of a green
matrix build is megabytes that say nothing the check rollup did not.

Two answers that look like nothing going wrong, and are not:

- `run log-failed` on a run with **no failed steps** prints nothing and exits 0.
  Silence means the run is green, not that the read failed.
- `run log-failed` on a run **still in progress** exits non-zero and says so.
  Wait and retry rather than treating it as a broken command.

The inline section is a projection rather than raw passthrough — GitHub returns
a `diff_hunk` per comment that repeats the surrounding diff and is routinely
larger than the comments themselves, which is a poor trade for an agent reading
under a context budget. The projection lives in `githubproxy_handlers.go`.

> **These two verbs carry third-party prose into an agent's context.** A PR
> comment can be written by anyone who can comment on the repository, and a CI
> log echoes branch and PR titles. Every other proxy read returns structured
> JSON; these return free text that an agent will read as part of its
> instructions unless it has been told otherwise. The bundled `proxy-git` skill
> says so; keep it in mind if you write your own guidance.

### Downloading a run's artifacts

Some CI jobs put what you need in an artifact rather than in the log: a coverage
profile, a JUnit report, a failing test's captured output, a built binary.
`run log-failed` cannot reach any of it.

```bash
tclaude proxy github run artifacts 18234567890            # names, sizes, expiry
tclaude proxy github run download 18234567890 --name coverage
```

**The destination is not a parameter, and there is no flag for it.** Everything
lands in `.tclaude-artifacts/run-<run-id>/` at the root of the agent's own work
tree, and the command prints that path plus a listing of what arrived. This is
the same rule as the repository: agentd runs unsandboxed, so a path it accepted
from an agent would be a path it would write to *as the operator*, which is
precisely the reach the proxy exists not to lend.

The directory is emptied before each download of the same run, so a listing is
never a mix of two downloads — and so repeated downloads cannot pile up. It also
contains a `.gitignore` of `*`, which makes it invisible to `git status` — an
agent that pulls an artifact mid-branch does not then commit it by reflex, and
you do not have to edit `.gitignore`. Older runs are pruned; see the limits
below.

Without `--name` every live artifact is fetched, each into a subdirectory named
after it; with `--name` that one artifact is unzipped directly into the
destination.

Sizes are checked **before** anything is fetched, from the same manifest
`run artifacts` returns, and a request totalling more than **512 MiB** is
refused. Artifacts are routinely that large — a job that uploads a build tree
does not think of itself as unusual — and this is the one verb where an agent's
mistake costs disk rather than context.

That figure is the **zip size**, the only one GitHub reports, so it is not by
itself a bound on disk. Two further limits make it one:

| Limit | Value | Why |
|---|---|---|
| Unpacked size, per download | 2 GiB | Deflate on repetitive content reaches ratios in the hundreds, so an artifact far under the zip cap can unpack to far more than a disk holds. On a public repository a fork's pull request can upload exactly that. Enforced **as the archive expands**; the moment the budget is spent the unpack stops, what it wrote is **deleted**, and the download is refused. |
| Run directories kept | 3 | Each run id gets its own directory. Without a cap, per-download limits bound nothing in aggregate — a caller with an endless supply of run ids fills a disk one legal download at a time. Least recently touched are pruned first; three so that comparing a red run against a green one still works. |

So the proxy never leaves more than **3 × 2 GiB** behind, however many times it
is asked. Downloading the *same* run repeatedly cannot accumulate at all,
because each download clears its own directory before it starts — which is also
why a failed or timed-out download is cleaned up rather than left as a partial
tree.

The daemon unzips the download itself, which is what makes that cap enforceable
during the unpack rather than after it: a zip bomb never reaches the disk it was
meant to fill. What is still fetched in full is the compressed archive, bounded
by the 512 MiB zip cap.

A zip entry naming `../../../.ssh/authorized_keys` is a real thing an untrusted
CI job can upload, and the daemon unpacks as the operator. Every entry is
written through an `os.Root` anchored at the download directory, so a traversing
name lands inside it rather than escaping.

The preflight also tells apart three failures that look identical from the
outside: "no artifact by that name" (it lists the live ones), "that artifact
expired" (the entry outlives the bytes, and retrying will not help), and "this
run uploaded nothing". Because it moves bulk bytes, `run download` gets the
longest bound in the proxy: 300s, shared across the manifest read and the
transfers.

`run artifacts` returns `{"total": N, "artifacts": [...]}`, projected down to
the fields that decide anything (`name`, `size_in_bytes`, `expired`, the
timestamps) — the raw entries embed a complete copy of the workflow-run object
each. At most 100 artifacts are listed, which no ordinary run approaches;
`total` is the run's real count, so a larger `total` than array means you are
looking at a page. A `run download` **without** `--name` fetches every artifact
in the run rather than every artifact on that page, so in exactly that case it
is refused rather than sized against a fraction of what it would pull — name the
one you want.

`git remotes` is the command to point an agent at when something is refused: it
lists every remote with the allow-list verdict and the reason, so the agent can
tell you exactly what to add instead of guessing from a 403.

### Why there is no daemon-side `pull`

`git pull` updates the working tree, and updating the working tree runs
`.gitattributes` smudge filters — arbitrary programs named by a file inside the
agent's own repository. Running that in the daemon would put the agent's own
configuration in charge of what the daemon executes.

So `tclaude proxy git pull` is split: the daemon fetches (the half that needs a
credential), and the fast-forward runs as an ordinary local `git merge --ff-only`
in the agent's own process, under its own sandbox, where it always was. A merge
that is not a fast-forward is reported and left for the agent to resolve; the
daemon does not merge.

## How it is hardened

The repository's own `.git/config`, `.git/hooks/*` and `.gitattributes` are all
files the agent can write, and every one of them can name a program for git to
run. The proxy therefore treats repo-local configuration as hostile.

There are two mechanisms, and it matters which one is doing the work.
**Pinning** (`git -c key=value`) is used where it is known to be effective.
**Refusal** — rejecting the whole operation — is used everywhere else, because
pinning turns out not to be uniformly reliable: `remote.<n>.uploadpack` is read
first-wins across config scopes so a `-c` override never displaces a repo-local
value, a URL-scoped `http.<url>.proxy` outranks a generic `-c http.proxy=`, and
`url.*.insteadOf` has no reset form at all. Which keys `-c` wins is a property
of git's config reader that varies per key and can change between versions, so
the load-bearing measures do not depend on it.

| Vector | Mechanism | What the proxy does |
|---|---|---|
| `.git/hooks/pre-push`, `reference-transaction`, … | pin | `core.hooksPath` → a daemon-owned empty directory. |
| `core.askPass` — a program run to obtain a credential | pin | Pinned empty. Git consults askpass **before** the terminal, so `GIT_TERMINAL_PROMPT=0` does not close this and clearing `GIT_ASKPASS` only removes the env-var route. |
| `core.sshCommand`, `core.alternateRefsCommand`, `core.fsmonitor`, `core.gitProxy`, `core.editor`, `core.pager`, `gpg.program`, `diff.external` | pin | All pinned. |
| `.gitattributes` filter programs | design | Only `fetch` / `push` / `ls-remote` run in the daemon; none updates the working tree. |
| `ext::<command>` remote URLs | refuse + pin | Refused by the URL parser **and** by `protocol.ext.allow=never`. |
| `file://`, local paths, `http://`, `git://` | refuse | No host to allow-list, cleartext credentials, or unauthenticated transport. |
| `url.<base>.insteadOf` rewrites | refuse | Each validated URL must be a **fixed point** of git's own rewriting; a repository that would redirect it is refused. |
| **Several `remote.<n>.url` values** | refuse | `git remote get-url` reports only the first, but `git push` contacts **every** one. All are read with `--all` and each must pass the allow-list. |
| `remote.<name>.pushurl` | refuse | Validated separately from the fetch URL. |
| `remote.<n>.uploadpack` / `receivepack` / `vcs` / `proxy` | refuse (+ flag) | A repository that sets one is refused. The stock programs additionally ride as `--upload-pack` / `--receive-pack` **flags**, which do override config where `-c` does not. |
| `http.*` — `proxy`, `sslVerify`, `sslCAInfo`, `curloptResolve`, `extraHeader`, … | refuse | Any `http.*` key outside a tiny performance-only safe set is refused. In its URL-scoped form this family outranks any override the proxy could set, and it is the route by which a credential would be handed to an attacker-chosen endpoint. |
| Submodule recursion | pin + flag | `fetch`/`push.recurseSubmodules` and `submodule.recurse` pinned off, plus `--no-recurse-submodules`. A submodule's own config names hosts the allow-list never saw. |
| A `.git` **gitfile** redirecting `GIT_DIR` elsewhere | refuse | The resolved git directory must live under the work-tree root, so a one-line `.git` file cannot aim the daemon at another repository's config, refs and remotes. The one exception is a genuine linked worktree — see below. |
| A **forged linked-worktree** link | verify | Linked worktrees are supported (`tclaude worktree` makes them), and their git dir legitimately sits outside the work tree. Git does *not* validate the link: point a hand-written `.git` file at another repository's `.git/worktrees/<name>` and git reports **your** directory as the work tree while handing over **their** remotes and objects. So the proxy checks the back-pointer — `<common>/worktrees/<name>/gitdir` must name this work tree's own `.git`. Writing that needs write access to the main repository's admin directory, which already implies being able to rewrite its config. |
| A work tree at or above `$HOME` | refuse | Stops an agent launched outside a repository from having git walk up into the operator's home. |
| A repo-local `credential.*` key (the program git runs to obtain a credential) | refuse + pin | Any `credential.*` key in the **local** or **worktree** scope is refused. On top of that the helper list is reset and repopulated from **global/system** configuration only, so your real helper keeps working. |
| Argument injection | validate | Every parameter is charset-validated and refused if it begins with `-`. No passthrough flag, no `--` escape. The GitHub half has no argv at all: a value reaches a query parameter, a JSON request body, or a typed GraphQL **variable**. It is never a command-line argument, and never interpolated into a GraphQL operation document — the documents are package-level constants, so no caller string can become query syntax. |
| A hung transport | bound | Every call is time-bounded and runs in a private process group that is killed **on cancellation**, so an `ssh` child cannot outlive a timed-out request. (The kill is deliberately not issued after `Wait`: the leader's pid is reaped by then and could already belong to a stranger's process group.) |
| Secrets in `/proc` | design | The GitHub token reaches the API as an `Authorization` header from daemon memory — never a child's argv or environment. (`gh auth token` is a child, but it is how the token is *obtained*, not how it is spent.) PR bodies and comments travel in a JSON request body — no argv, and no temporary file. |

Nothing here ever runs a shell. A configuration probe that cannot be *run* is
treated as a refusal, not as "nothing configured" — a gate that reads a failed
check as clean is worse than no gate.

### What runs where

**Every credentialed command runs from a daemon-owned transfer directory**, not
from the agent's repository. It is a bare git dir under the private data tree,
created and configured only by the daemon, and it is removed when the request
ends. The destination is always the validated URL, spelled out: push sends a
resolved SHA, fetch carries refspecs the daemon wrote, and neither has a remote
name for git to look up.

That is what closes the **check/use race**. Every refusal gate reads the agent's
`.git/config` in its own short-lived process; the credentialed command used to
read it again moments later, and the agent can rewrite the file in between. Pins
ride on the argv and were always immune, but the keys that matter most here
cannot be pinned: `url.*.insteadOf` has no reset form, and a URL-scoped
`http.<url>.*` outranks a generic override. Running somewhere the agent cannot
write is what removes the second read altogether.

**Remote-scoped permissions are therefore exact for every verb.** `push`,
`ls-remote`, `fetch` and the GitHub operations all authorize the resolved
destination they subsequently contact, so a `remote` scope on `proxy.git.read` or
`proxy.git.push` is a hard boundary rather than a check that could be raced.

Fetch takes two extra steps to get there, because unlike push it has to *leave*
results in the agent's repository:

| | How |
|---|---|
| Objects | The transfer directory's object store **is** the agent's, named through `GIT_OBJECT_DIRECTORY`, so the fetched pack lands exactly where an ordinary `git fetch` would have put it. Nothing is copied afterwards, and there is no quarantine to garbage-collect. |
| Refs | The agent's remote-tracking refs and tags are copied into the transfer directory **before** the fetch, and mirrored back **after** it in one atomic `update-ref` transaction. Every update is a compare-and-swap against what was read at the start, so a ref that moved underneath the fetch aborts the import instead of silently discarding that write. The import is the only command touching agent-writable state, and it holds nothing worth stealing: no credential, no network. |

Seeding the refs first is not an optimisation. It gives the fetch its
negotiation "have"s (without them the server resends the whole history every
time), it makes the summary report real deltas instead of `[new branch]` for
everything, it is what lets `--prune` decide what went stale, and it puts the
tags the agent already has in front of git's refuse-to-clobber check.

Two consequences worth knowing:

- **The repository's own `remote.<name>.fetch` refspec is ignored.** A fetch
  always retrieves `+refs/heads/*:refs/remotes/<name>/*` (or the one branch you
  named), plus `refs/tags/*:refs/tags/*` with `--tags`. That key is
  agent-writable and is not one of the ones the gates inspect, and a value like
  `+refs/*:refs/*` would have a fetch overwrite the agent's own branches.
- **`FETCH_HEAD` is not written** in your repository, because the fetch did not
  run there. Use the remote-tracking ref — `refs/remotes/<remote>/<branch>` —
  which is what `tclaude proxy git pull` already does.

Tags follow git's ordinary rules: a new tag is imported, an existing one is
never overwritten or pruned. As with plain `git fetch --tags`, a tag you hold
at a different commit is **rejected** and the command exits non-zero — the
refspec is deliberately unforced. Everything else in that fetch still lands:
a partial rejection is not a failed fetch, and the refs that moved are imported
regardless of the exit code.

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
  for the life of the call. (This is about `git` only. The GitHub half never
  hands its token to a child process — see
  [Where the GitHub token comes from](#where-the-github-token-comes-from) for
  the one command it runs to *obtain* one.)
- **Everything the GitHub half writes is attributed to your GitHub account.** A
  PR the agent opens is a PR you opened.

## Auditing

Every proxied call — including the reads, which is why they are POSTs — writes
an `audit_log` row visible in the dashboard: who ran what, against which remote
and ref, and with what exit code. Bodies, titles, tokens and subprocess output
are deliberately never recorded.

```bash
tclaude proxy git push        # → audit verb "git.push"
tclaude proxy github pr create # → audit verb "github.pr.create"
```

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| `503 git_proxy_disabled` | An unscoped grant has no legacy `allowed_remotes` policy. Add a `remote` scope to the grant (preferred), or configure the legacy list. |
| `403` naming a slug | The agent lacks `proxy.git.read` / `proxy.git.push` / `proxy.github.read` / `proxy.github.write`. Grant it, or the agent can retry with `--ask-human`. |
| `token_missing` | `gh auth token` could not supply one — gh is not installed, or the account agentd runs as is not logged in. The message carries gh's own words. Run `gh auth login` as that account, or set `github_token_file` to skip gh entirely. |
| `token_unreadable` | The configured `github_token_file` could not be read, is empty, or contains a control character (usually a stray newline mid-value). A configured file is never silently skipped in favour of another source. |
| `response_too_large` | A read's answer exceeded 1 MiB. Ask for fewer items with `--limit`. The bound exists because these answers land in an agent's context window and in the idempotency store; it is a refusal rather than a truncation, because half a JSON document is worse than none. |
| `remote … is not on the operator's allow-list` | Run `tclaude proxy git remotes` to see the resolved `host/owner/repo`, then add a matching pattern. |
| `protected_ref` | The branch is in `protected_refs`. Push a feature branch and open a PR. |
| `force_push_disabled` | Set `allow_force_push: true` if you want it. |
| `this repository rewrites its … URL (url.*.insteadOf)` | The repo has a rewrite rule that would redirect the validated URL. Remove it, or point the remote directly at the real URL. |
| `refusing an 'ext::' remote URL` | The remote names a command, not a server. Something has rewritten `.git/config`; inspect it. |
| `this repository sets remote.X.uploadpack …` | The repository configures a program-selecting key for that remote. Remove it with `git config --unset remote.X.uploadpack` (or `receivepack` / `vcs` / `proxy`). |
| `this repository configures http.…` | An `http.*` setting that can redirect the connection or weaken TLS. Remove it, or move it to your global config only if you genuinely need it — the proxy refuses it wherever it is set. |
| `this repository configures credential.…` | A `credential.*` key is set in the repository (or its worktree config, or a file it `include.path`s). Remove it with `git config --unset`, or move it to your **global** config, which the proxy honours. |
| `git directory … lives outside the work tree` | A `.git` gitfile points at another repository. Ordinary linked worktrees are fine; this means the target is not one, or is not registered against this work tree. Check `git worktree list` from the main checkout, and `git worktree repair` if the registration is stale. |
| `contains the operator's home directory` | Your launch directory is not inside a repository, so git walked up into `$HOME`. Work inside an actual project checkout. |
| `could not inspect this repository's configuration` | A config probe failed to run. The proxy refuses rather than assuming the repository is safe; check that `git` works in that directory. |
| `cannot lock ref … but expected …` after a fetch | The fetch landed its objects, but a remote-tracking ref moved while it was running (usually a second fetch). Nothing was written; run the fetch again. |
| `too_many_refs` | A fetch will not mirror a `refs/remotes/<remote>/…` or `refs/tags/…` namespace with more than 20,000 refs, because it cannot act on a partial view of one. Local branches are not subject to this. |
| `tool_missing` | `git` is not installed on the host running agentd. (`gh` has its own diagnosis — see `token_missing`.) |
| A push hangs then times out | Usually a passphrase-protected key that is not loaded into an ssh-agent. The proxy runs `ssh -o BatchMode=yes`, so it fails rather than prompting — load the key with `ssh-add`, or set `ssh_key`. |
| `Host key verification failed` | The account agentd runs as has no `known_hosts` entry for the forge, and `BatchMode=yes` cannot prompt to accept one. Run `ssh -T git@github.com` once as that account, or add the host key with `ssh-keyscan`. |
| `Permission denied (publickey)` | The key agentd offered is not one the forge accepts. **Setting `ssh_key` narrows this**: it adds `-o IdentitiesOnly=yes`, so ssh offers *only* that key — an agent key or a default `~/.ssh/id_*` that would otherwise have worked is not tried. Reproduce exactly what the daemon does with `ssh -v -o BatchMode=yes -o IdentitiesOnly=yes -i <key> -T git@github.com`; a passphrase-protected key with no agent fails this way too. Clearing `ssh_key` falls back to the ambient SSH setup, which is the better posture unless you specifically want one identity. |

## See also

- [Agent coordination](agent.md) — the permission model and the CLI surface.
- [Sandbox hardening](sandbox-hardening.md) — the OS sandbox this feature
  assumes you have set up.
- [How sandboxing works](sandboxing.md) — the operator mental model.
