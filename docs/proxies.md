# Credential proxies

A hardened sandbox denies `~/.ssh`, `~/.config/gh`, and every other credential
store — which also strips the agent of `git push`, `gh pr create`, and the
issue tracker. The credential proxies give those workflows back without
putting a secret inside the wall: the agent describes a *semantic* operation
("push my branch", "comment on this PR"), and the `agentd` daemon builds the
actual git invocation or API call on the host, where the credentials live.
There is no passthrough flag and no raw-query escape hatch; every gate is
enforced daemon-side.

Four proxies exist, as subcommands of a top-level command:

```bash
tclaude proxy git     # fetch, pull, push through the daemon
tclaude proxy github  # PRs, issues, and Actions runs (alias: gh)
tclaude proxy linear  # Linear issues, bounded by a team allow-list
tclaude proxy awb     # AWB issues, bounded by a workspace allow-list
```

None of their permissions are granted by default, and none are implied by
group ownership. Every gated verb supports `--ask-human <timeout>` (capped at
300 seconds): on a permission denial, the operator gets a popup approval, and a
timeout counts as a deny.

## The command only appears when configured

`tclaude proxy` is **conditionally registered**. On a host where no proxy is
configured, it is an unknown command and does not appear in `tclaude --help` —
by design, so unconfigured operators do not advertise a capability their
agents cannot use. The command registers when any proxy family is configured:

- `agent.git_proxy.allowed_remotes` is non-empty (Git and GitHub), or
- `agent.linear_proxy` names an allow-list, a key file or a workspace route, or
- `agent.awb_proxy.url` is set, or
- the caller is a managed agent and a capability probe of agentd's
  `GET /v1/info` reports proxy support (daemons predating that projection keep
  the command visible).

The **permission catalog** follows the same per-family answer: a host
advertises `proxy.git.*` / `proxy.github.*`, `proxy.linear.*` and `proxy.awb.*`
only where that family could work, plus any family the calling agent holds a
scoped grant for. The two must agree — advertising a slug whose command is not
registered would let an operator grant a capability nothing can exercise.

One asymmetry is deliberate and worth knowing. The daemon also counts
`LINEAR_API_KEY` in **its own** environment as a configured Linear family, so a
managed agent (which reads the projection) gets the command from an
environment-only Linear setup. A plain host shell does not: it answers from the
config file alone, because probing the daemon on every `tclaude` invocation
would cost a round trip on every command. An operator running an
environment-only Linear proxy who wants `tclaude proxy` in their own shell
should give `agent.linear_proxy` an `api_key_file` (or an `allowed_teams` list)
rather than relying on the variable.

Visibility is not enforcement. The full registry still backs validation and
stored-grant resolution, so hiding a slug never withdraws a grant made under it.

A managed agent is recognised by any of `TCLAUDE_AGENTD_SOCKET`,
`CODEX_PERMISSION_PROFILE=tclaude-agent`, or `TCLAUDE_AGENT_HINT=1` in its
environment. Only the last is carried by every managed launch: the socket is
pinned for sandboxed agents alone, so an unsandboxed managed pane has the hint
and nothing else.
The local config read cannot stand in for that test: a sandbox that denies
`~/.tclaude/data` by mounting it empty makes an enabled config look exactly
like an absent one, with no error to distinguish them.

If an agent reports that `tclaude proxy` does not exist, that is the symptom
of an operator who has not opted in — not a broken install.

## Git proxy

```bash
tclaude proxy git remotes     # list remotes with a per-remote verdict
tclaude proxy git ls-remote origin
tclaude proxy git fetch origin
tclaude proxy git pull        # daemon fetches, then fast-forwards locally
tclaude proxy git push origin my-branch
```

`pull` is deliberately split: the daemon only fetches, and the fast-forward
happens locally, so the daemon never merges or checks out in the agent's
tree. `push` supports force only as `--force-with-lease`, and only when the
operator config sets `allow_force_push`; plain `--force` is never available.
Pushes to `protected_refs` are refused.

Permissions, both scopable to a `remote` dimension:

- `proxy.git.read` — `remotes`, `ls-remote`, `fetch`, `pull`.
- `proxy.git.push` — `push`.

### Operator configuration

The `agent.git_proxy` block lives in `~/.tclaude/data/config.json` (private
daemon state; migrated automatically from the legacy `~/.tclaude/config.json`):

- `allowed_remotes` — the operator-global allow-list of `host/owner[/repo]`
  patterns, matched segment-wise (`*` matches one segment, and a shorter
  pattern is a prefix match). SSH and HTTPS URLs normalize to the same key.
  Authorization is the per-agent `remote`-scoped grant, or this list, or both
  must match when both exist; there is deliberately no "allow everything"
  setting. A non-empty list is also what registers the command, and it bounds
  the dashboard's Branch-column PR/branch links, which spend the same `gh`
  credentials.
- `protected_refs` — default `["main", "master"]`; an explicit `[]` disables.
- `allow_force_push` — default false.
- `ssh_key` — pin one key; empty uses the daemon's ambient ssh-agent.
- `github_token_file` — the GitHub token source (GitHub proxy only); empty
  falls back to `gh auth token`, and that is the entire token chain.

`~/` expands in these values; shell variables do not.

## GitHub proxy

```bash
tclaude proxy github pr create --title "Fix the flake" --body-file pr.md
tclaude proxy github pr view 2277
# other pr verbs: ls, checks, comments, ready, comment, edit, merge
# issue verbs:    ls, view, comment
# run verbs:      ls, log-failed, artifacts, download
```

Operations are restricted to the repository that the agent's own recorded
launch remote resolves to — and only when that remote is allowed. The token
travels as an Authorization header to the API, never through child process
argv or environment.

Permissions:

- `proxy.github.read` — `pr ls/view/checks/comments`, `issue ls/view`, and the
  `run` verbs. `run download` does write, but only into `.tclaude-artifacts/`
  inside the agent's own work tree (a computed destination, capped at 512 MiB
  compressed / 2 GiB unpacked per download, with at most 3 run directories
  kept).
- `proxy.github.write` — `pr create/edit/comment/ready`, `issue comment`. Does
  not imply merge.
- `proxy.github.merge` — `pr merge` only, split from write on purpose: an
  operator can let agents open and discuss PRs while keeping the merge button
  human. GitHub's own branch protection still applies; `protected_refs` does
  not (it bounds direct pushes only).

## Linear proxy

```bash
tclaude proxy linear whoami            # key identity + reachable teams
tclaude proxy linear issue view TCL-123
# other issue verbs: ls, search, comments, comment, create, update, link
```

There is no CLI tool underneath: the daemon speaks Linear GraphQL directly,
and every GraphQL document is a compile-time constant with caller values
carried only in variables. Unlike git, Linear has no filesystem anchor — no
remote to resolve — so the **team allow-list is the entire scope gate,
mandatory and fail-closed**.

Permissions, scoped on the `linear_team` dimension (for example
`--scope linear_team=TCL`):

- `proxy.linear.read`
- `proxy.linear.write` — additionally requires the operator config
  `agent.linear_proxy.allow_write`.

Scoped grants intersect with the operator's `allowed_teams` list when one
exists; an *unscoped* grant is refused outright when the operator has no list.
Read and write scopes are independent.

The `agent.linear_proxy` block in `~/.tclaude/data/config.json`:

- `allowed_teams` — team keys, case-insensitive, no wildcard by design.
- `api_key_file` — the default key; empty falls back to `LINEAR_API_KEY` in
  agentd's environment, and with neither the proxy refuses.
- `workspaces` — routes named teams to a different key, because one Linear
  personal key is scoped to one workspace; each workspace entry requires its
  own `api_key_file`, with no environment fallback.

## AWB proxy

```bash
tclaude proxy awb whoami                  # server, account, reachable workspaces
tclaude proxy awb ready --compact         # the primary entry point
tclaude proxy awb claim awb-a3f9c1
tclaude proxy awb comment add awb-a3f9c1 --body-file findings.md
# other verbs: show, list, blocked, search, create, update, close, reopen,
#              release, delete, label add|rm, dep add|rm|tree,
#              comment list, activity, attach add|list|show|get|delete
```

[AWB](https://github.com/tofutools/awb) — Agent Work Board — is an agent-first
issue tracker with an HTTP API. As with Linear there is no CLI tool underneath:
the daemon speaks AWB's REST API directly, every path is assembled from
compile-time constants plus a validated issue reference, and every caller value
travels as a query parameter or a marshalled JSON field. And as with Linear
there is no filesystem anchor, so the **workspace allow-list is the entire scope
gate**, checked once on the reference the caller supplied and again on the
workspace AWB reports for the issue it returned.

The verbs and flags mirror `awb`'s own one for one, minus five that name a local
database or a terminal rather than the data: `--db`, `--attachments`,
`--no-context`, `--color`, `--no-color`. Two differences are worth knowing:

- **A bare hash is refused.** `awb` accepts `a3f9c1`; the proxy requires
  `awb-a3f9c1` (a hash prefix is fine), because a reference carrying no workspace
  key could only be gated after the issue had been fetched.
- **Listings are bounded.** `awb` returns every row by default; the proxy
  defaults to 50, capped at 500, because the rows land in an agent's context.
  `comment list` and `activity` are bounded the same way, and additionally cap
  `--offset`.

Comments are an append-only timeline shared with AWB's change records.
`activity` reads the whole thing (`--kind comment|change` narrows it) and
`comment list` is the same read with the kind fixed. A close reason lives there
too: since AWB 0.6 `close --reason` records a typed comment rather than setting
a field on the issue, and the issue carries no `close_reason` at all.

`dep tree` is pruned to the caller's workspaces: AWB follows children across
workspace boundaries by design, so a child outside the gate is dropped with its
subtree. `whoami` describes only the workspaces the caller may reach; one it may
not is reported as its key alone, which is what a refused agent needs in order
to ask for it and nothing more. Attachment content travels through the daemon in request and response
bodies rather than as a path it would read from the agent's work tree, which
caps it at 8 MiB either way.

Permissions, scoped on the `awb_workspace` dimension (for example
`--scope awb_workspace=awb`):

- `proxy.awb.read` — `whoami`, `show`, `list`, `ready`, `blocked`, `search`,
  `dep tree`, `comment list`, `activity`, `attach list/show/get`. `comment list`
  and `activity` are the reads that carry third-party prose into an agent's
  context: anyone with tracker access can write a comment.
- `proxy.awb.write` — everything that changes the tracker, including the hard
  `delete` (which additionally needs `--force`). Requires the operator config
  `agent.awb_proxy.allow_write`.

Scoped grants intersect with the operator's `allowed_workspaces` list when one
exists; an *unscoped* grant is refused outright when the operator has no list.
Read and write scopes are independent.

The `agent.awb_proxy` block in `~/.tclaude/data/config.json`:

- `url` — the AWB server's base URL. This is what registers the command; only
  http/https, and a URL carrying userinfo is refused rather than stripped.
- `username` — the account every proxied call authenticates as, and the identity
  AWB attributes writes to. It is also what `claim` records without `--as` and
  what `--mine` filters on. Empty suits a server whose database holds no user,
  which AWB treats as unauthenticated.
- `password_file` — empty falls back to `AWB_PASSWORD` in agentd's environment;
  with a username and neither, the proxy refuses rather than sending half a
  credential. The password is never a field of `config.json` itself.
- `allowed_workspaces` — workspace keys, case-insensitive, no wildcard by design.
- `allow_write` — default false.

AWB applies its own authorization underneath: the daemon's account works in the
workspaces it is a member of, and one it holds no access to answers `404`. That
bounds the operator; the allow-list above bounds the agent.

## Teaching agents to use the proxies

```bash
tclaude setup --install-proxy-skills
```

installs the `proxy-git`, `proxy-linear` and `proxy-awb` agent skills into the
Claude Code and Codex skill directories, so agents discover the semantic commands instead
of fighting their missing credentials. The flag is deliberately excluded from
`--install-agent-skills` and `--install-all`: an operator who has not
configured the proxies should not advertise them to agents.

## See also

- [Sandboxing](sandboxing.md) — the hardening posture these proxies make
  survivable.
- [Permissions and audit](permissions-and-audit.md) — scoped grants, denies,
  and `--ask-human`.
- [Network filtering](network-filtering.md) — reaching hosts directly when a
  proxy is the wrong shape.
