# Linear proxy 📋

**Audience:** operators who track work in [Linear](https://linear.app/) and want
their sandboxed agents to read the ticket they are on, and report back on it,
without handing them a Linear API key.

## The problem this solves

An agent spawned against `TCL-568` cannot read `TCL-568`. The ticket holds the
acceptance criteria, the discussion, and the reason the work exists — and all of
it is behind a credential the agent should not have.

The usual escape is to give the agent a Linear key, or point its harness at
Linear's MCP server. Both work, and both mean the agent holds a credential.
How much that credential reaches is up to you — Linear's own keys can be
restricted to read-only and to specific teams, and its MCP access can be
read-only too — but the agent holds it either way, and what bounds it lives in
Linear's settings rather than anywhere tclaude can see or audit.

`tclaude agentd` already runs unsandboxed on the host. So it calls Linear on the
agent's behalf: the agent issues a *semantic* request — "show me TCL-568",
"comment on it" — and never sees a key.

```bash
# Inside a sandboxed agent that holds no Linear credential:
tclaude proxy linear issue view TCL-568
tclaude proxy linear issue comment TCL-568 --body-file progress.md
```

This is a sibling of the [Git & GitHub proxy](git-proxy.md) and follows the same
shape. Read that page first if you have not; this one covers only what differs.

## What differs from the Git & GitHub proxy

**There is no tool.** Linear ships no official CLI, so the daemon speaks
Linear's GraphQL API directly. That removes a whole class of hazard — no argv to
inject into, no `/proc/<pid>/cmdline` leaking a comment body, no repo-local
config that could re-aim a child process — and replaces it with one rule:
**every GraphQL document is a compile-time constant, and every caller value
travels in `variables`.** A variable is substituted after the document is
parsed, so it can change what an operation is asked *about*, never which
operation runs. The whole GraphQL surface is one file,
`pkg/claude/agentd/linearproxy_queries.go`, so you can audit it in a single
read.

The key never enters a child process either. It goes from daemon memory into an
`Authorization` header, so unlike the GitHub half's `GH_TOKEN` there is no
window in which a same-uid process can read it out of `/proc`.

**There is no anchor.** The git proxy derives its repository from the agent's
daemon-recorded launch directory, so an agent can only reach the checkout it was
launched in. Linear has no filesystem artifact that corresponds to that — nothing
ties a conversation to an issue. **A team set is therefore the whole scope
gate**, which is why it is mandatory, fail-closed, and checked twice.

**The gate is a set, not one value per request.** The git proxy resolves each
request to a single remote and gates on that. Two Linear verbs — `issue ls` and
`issue search` with no `--team` — legitimately span every team the caller may
reach, so the daemon resolves the whole reachable set once per request and every
check reads it. One consequence is visible to you: a team outside that set is
refused outright, where an out-of-scope git remote can be escalated to an
`--ask-human` popup. There is no single team such a popup could name for a
cross-team listing.

## Enabling it

Install the agent-facing proxy skills explicitly:

```bash
tclaude setup --install-proxy-skills
```

They are not installed by `--install-agent-skills` or `--install-all`, because
operators who have not configured a credential proxy should not expose those
capabilities to their agents.

The proxy is **off** until at least one team is reachable — either from the
operator-global `allowed_teams` below, or from a per-agent
[team-scoped grant](#restricting-an-agent-to-certain-teams). There is no "allow
everything" setting.

Add an `agent.linear_proxy` block to `~/.tclaude/data/config.json`:

```json
{
  "agent": {
    "linear_proxy": {
      "allowed_teams": ["TCL"],
      "api_key_file": "~/.tclaude/linear-key.txt",
      "allow_write": false
    }
  }
}
```

| Field | Meaning |
|---|---|
| `allowed_teams` | Team keys the proxy may act on — the prefix in an issue identifier, so `TCL` authorizes `TCL-568`. Matched case-insensitively. **Empty or absent disables unscoped grants**; a [team-scoped grant](#restricting-an-agent-to-certain-teams) supplies its own teams instead. |
| `api_key_file` | A file whose contents are the Linear personal API key. Empty falls back to `LINEAR_API_KEY` in the environment agentd runs under; with neither, the proxy refuses. This is the **default** key: every allowed team that no `workspaces` entry claims is reached with it. |
| `workspaces` | Extra keys for teams in **other Linear workspaces**. Absent is the normal case — see [Teams in more than one Linear workspace](#teams-in-more-than-one-linear-workspace). |
| `allow_write` | Permits the mutating verbs at all. Default off. |

`api_key_file` accepts `~/…`, expanded against the home directory of the account
agentd runs as. Shell variables are **not** expanded — a config file is not a
shell, so `"${HOME}/key.txt"` is taken literally.

The key is deliberately not stored in `config.json` itself: that file is
plaintext, appears in the dashboard's Config tab, and is the sort of thing that
ends up in a bug report.

### Teams in more than one Linear workspace

**One key usually covers every team you need.** A Linear personal API key is
scoped to the workspace its creator was logged into, and within that workspace
it reaches every team the account can see — *whoever created those teams*. A
colleague's team in your workspace needs no extra key; it needs your account to
have access to it (join it, or be invited if it is
[private](https://linear.app/docs/private-teams)). When creating the key, under
**Team access**, choose *All teams you have access to* — or name the teams
explicitly, which narrows the key without needing a second one.

A second key is needed only when teams live in **separate workspaces**, because
no permission on the first key can reach across that boundary. Create one key
inside each workspace (switch workspaces from the top-left workspace name first
— the key belongs to whichever one you are in), then tell the daemon which teams
each key is for:

```json
{
  "agent": {
    "linear_proxy": {
      "allowed_teams": ["TCL", "ACM", "OPS"],
      "api_key_file": "~/.tclaude/linear-key.txt",
      "workspaces": [
        {
          "name": "acme",
          "api_key_file": "~/.tclaude/linear-acme-key.txt",
          "teams": ["ACM", "OPS"]
        }
      ],
      "allow_write": false
    }
  }
}
```

| Field | Meaning |
|---|---|
| `name` | A label for diagnostics — `whoami`'s breakdown, and the refusal an unreadable key produces. Nothing routes on it. `default` is reserved: it already names the key every unclaimed team uses. |
| `api_key_file` | The key created **in that workspace**. Required: there is no `LINEAR_API_KEY` fallback here, since one environment variable names one workspace's key. |
| `teams` | The team keys that workspace's key reaches. Required. |

Rules worth knowing:

- **A `workspaces` entry decides which key reaches a team, never whether an
  agent may.** `allowed_teams` and the agent's own grant scope remain the whole
  authorization gate, so a team listed here and missing from `allowed_teams`
  stays unreachable. Adding a workspace can never widen what an agent can touch.
- **Teams you do not list keep using `api_key_file`.** An operator with one
  workspace never writes this block, and their behaviour is unchanged.
- **A team key may appear in at most one entry**, and every entry needs both a
  key file and a team list. A policy that breaks either rule is refused whole
  (`503 linear_proxy_misconfigured`) before any credential is spent — guessing
  between two keys would query the wrong workspace, and Linear answers that with
  "no such issue" rather than an error.
- **Team keys are only unique within a workspace.** Two workspaces can each have
  an `OPS`, and nothing here can tell those apart: `allowed_teams`, grant scopes
  and issue identifiers all key on the bare team key. Colliding keys across
  workspaces are not supported — route the key to one workspace and reach the
  other's team some other way.
- **A verb that spans teams costs one call per workspace.** `issue ls` and
  `issue search` without `--team`, and `whoami`, query each key in turn and merge
  the results — newest-first for a listing, taking turns between workspaces for a
  search, bounded by your `--limit`. At most 8 workspaces may take part in one
  such verb. Everything else spends exactly one key, the one for the team in the
  identifier, however many workspaces you have configured.
- **`--assigned-me` means a different person in each workspace**: each key
  authenticates as its own Linear account, so a fanned-out `issue ls
  --assigned-me` means "assigned to whoever created that workspace's key".
- **Keys are read only when used.** An unreadable key for a workspace a request
  never touches does not fail that request. But a fanned-out listing does need
  every key it spans: one broken key fails the whole `issue ls`, rather than
  returning a short answer that looks complete. `whoami` deliberately differs —
  it reports each credential's failure and keeps going, because diagnosing that
  is what it is for.

`tclaude proxy linear whoami` reports each key separately under `workspaces` —
who it authenticates as, which teams it can see, which of your teams it answers
for, and an `error` for any that failed. With a single key the response also
keeps its familiar top-level `viewer` and `teams`.

### Team keys match whole, not as a prefix

Unlike the git proxy's remote patterns — where a shorter pattern deliberately
matches as a prefix, so `github.com/acme` covers every repo in that owner — a
team key must match **exactly**. Team keys are a flat namespace with no
hierarchy, so a prefix rule would let `TCL` authorize `TCLX`. For the same
reason there is no wildcard: "every team" is a setting you should have to write
out team by team.

### Scope the Linear key too

Create a **dedicated** personal API key rather than reusing your own
(Settings → Account → Security & Access → Personal API keys). Linear lets you
restrict a key to Read / Write / Admin *and* to specific teams, and give it an
expiry.

Do that. It is a real boundary enforced at Linear's end, and it is strictly
better than the tclaude-side allow-list because it holds even if the daemon is
wrong. A read-only, team-scoped key plus `"allow_write": false` is the posture
to start from.

### Granting the permissions

Two slugs, neither granted by default and neither conferred by group ownership:

| Slug | Allows |
|---|---|
| `proxy.linear.read` | `whoami`, `issue view/ls/search/comments` |
| `proxy.linear.write` | `issue create/comment/update/link` |

```bash
tclaude agent permissions grant <agent> proxy.linear.read
tclaude agent permissions grant <agent> proxy.linear.write
```

**Writing needs both the slug and `allow_write`.** They answer different
questions: the slug says *this agent* may write, `allow_write` says the operator
wants *any* agent to be able to. A grant cannot override the config, and the
config cannot grant an agent anything.

An agent without a grant can still ask for a one-off with `--ask-human 60s`.

### Restricting an agent to certain teams

`allowed_teams` is one list for every agent on the host. When you run several
agents against different parts of the tracker, that is wider than you want: a
ticket-worker on `TCL` has no business reading the `JOH` backlog just because
another agent does.

Both slugs accept a `linear_team` **grant scope**, which narrows one agent
without touching the global list — the same mechanism as the git proxy's
`remote` scope:

```bash
# this agent may read and write, but only within TCL
tclaude agent permissions grant ticket-worker proxy.linear.read  --scope linear_team=TCL
tclaude agent permissions grant ticket-worker proxy.linear.write --scope linear_team=TCL

# a lead that reads two teams but only writes to one
tclaude agent permissions grant lead proxy.linear.read  --scope linear_team=TCL,JOH
tclaude agent permissions grant lead proxy.linear.write --scope linear_team=TCL
```

The dashboard's permission editor edits the same thing, offering your
`allowed_teams` as the pickable values. The scopes are **per slug**, so read and
write reach can differ, as above.

Matchers follow the same whole-key, case-insensitive rule the config list does:
`linear_team=tcl` and `linear_team=TCL` name the same team, and neither covers
`TCLX`. There is no wildcard.

**Where both lists exist they are enforced together, and neither widens the
other** — a request must satisfy both. Where only one exists, it is the whole
policy, with one exception: an *unscoped* grant is never a policy, so an empty
`allowed_teams` refuses it rather than letting it through. In full:

| `allowed_teams` | grant scope | Effective reach |
|---|---|---|
| `TCL`, `JOH` | *(none)* | `TCL`, `JOH` — the historical behaviour |
| `TCL`, `JOH` | `TCL` | `TCL` |
| `TCL` | `TCL`, `SECRET` | `TCL` — a grant cannot reach past the operator |
| `TCL` | `SECRET` | nothing: `403 team_scope_empty` |
| *(empty)* | `TCL` | `TCL` — the scope is the whole policy |
| *(empty)* | *(none)* | nothing: `503 linear_proxy_disabled` |

The last two rows are the fail-closed pair worth internalising. A scoped grant
is a complete policy on its own, so you can run a purely per-agent posture with
no global list at all — but a grant *without* a scope never inherits that, or
the narrowest thing an operator can write would be the widest.

Everything the daemon returns echoes the caller's **effective** set as `teams`,
and `whoami` breaks it down into `operator_teams` and `grant_teams` so an agent
that is refused can tell you which of the two to widen rather than guessing.

A scope also changes which refusal you get, and they need different fixes:
`team_not_allowed` is the operator's list, `team_out_of_scope` is the agent's own
grant. Unlike an out-of-scope git remote, neither escalates to an `--ask-human`
popup — see [the set, not one value](#what-differs-from-the-git--github-proxy)
above.

## What an agent can do

```bash
# Discovery — run this first, and whenever something is refused.
tclaude proxy linear whoami

# Reads (proxy.linear.read)
tclaude proxy linear issue view TCL-568
tclaude proxy linear issue ls --team TCL --state "In Progress"
tclaude proxy linear issue ls --assigned-me --limit 10
tclaude proxy linear issue search "flaky dashsnap"
tclaude proxy linear issue comments TCL-568

# Writes (proxy.linear.write + allow_write)
tclaude proxy linear issue comment TCL-568 --body-file progress.md
tclaude proxy linear issue update TCL-568 --state "In Review"
tclaude proxy linear issue create --team TCL --title "…" --description-file spec.md
tclaude proxy linear issue link TCL-568 --url https://github.com/acme/repo/pull/42
```

`whoami` is the command to point an agent at when something is refused: it lists
every team the key can see with the caller's own verdict beside it, plus
`operator_teams` and `grant_teams` broken out, so the agent can tell you exactly
which list to widen instead of guessing from a 403. Past 100 teams the answer
says `teams_truncated` rather than presenting a partial list as the whole
workspace.

### Closing the loop with the GitHub proxy

`issue link` attaches a URL to the ticket, which is the step that connects the
two proxies:

```bash
tclaude proxy git push -u
tclaude proxy github pr create --title "Fix the thing" --body-file pr.md
tclaude proxy linear issue link TCL-568 --url <the PR url>
tclaude proxy linear issue update TCL-568 --state "In Review"
```

Only `http://` and `https://` URLs can be attached — an attachment renders as a
clickable link in your workspace, and a `javascript:` or `data:` URL there is a
trap for whoever clicks it.

### Identifiers, not UUIDs

Every verb takes the human identifier (`TCL-568`). A raw UUID is **refused**,
even though Linear accepts one, because a UUID carries no team key — there would
be nothing to check the allow-list against before spending your credential.

### Workflow states are matched exactly

`--state "In Review"` is resolved against the team's own workflow states,
case-insensitively but never fuzzily. A name that is not one of them is refused,
and the refusal lists the real ones. A near-match that silently moved a ticket
to the wrong column would be worse than an error.

### What `issue update` will not change

Only the title, state and priority — the same deliberate narrowness as the
GitHub half's `pr edit`. Moving an issue between teams would take it out of the
allow-list, and assignment is a workspace decision rather than a coding one.
There is no delete, no archive, and no raw-GraphQL escape hatch.

### Comment threads read chronologically

Linear's API returns the newest comments first, and offers no way to ask for the
other direction — `first: 25` means "the 25 most recent". That is the right set
and the wrong order to read a discussion in, so the daemon sorts them
chronologically before rendering. If the thread is longer than the limit, the
output says so: what is missing is the *start* of the discussion, not the end.

> **`issue comments` carries third-party prose into an agent's context.** A
> Linear comment can be written by anyone with access to the workspace. Every
> other read returns structured data; this one returns free text that an agent
> will read as part of its instructions unless it has been told otherwise. The
> bundled `proxy-linear` skill says so; keep it in mind if you write your own.

## How the team gate holds

The daemon resolves the operator's list and the caller's grant scope into one
**effective team set**, once, before any verb runs. Everything below reads that
set, which is what keeps the identifier gate, the listing filter and the
row-level drop from ever disagreeing about which teams a caller may reach.

The set is checked **twice**, and the second check is the load-bearing one.

1. **On the identifier the caller supplied.** `TCL-568` → `TCL` → is it in the
   set? This runs before any network call, so a refusal costs nothing.
2. **On the team Linear actually reported.** The daemon reads the issue and
   refuses on `issue.team.key` before rendering anything.

The two can disagree. An issue moved between teams keeps answering to its old
identifier, so a proxy that trusted the prefix alone would be checking a label
rather than the thing it reached. Every issue-shaped GraphQL selection therefore
asks for `team { key }` — removing that field does not weaken the gate quietly,
it turns the verb into an error.

The same rule applies to listings: the request carries the effective set as a
filter, *and* every returned row is re-checked before it is rendered. The filter
is a request Linear honours; only the check is a gate.

The write verbs go further and read the issue **before** mutating it, because
`commentCreate` and friends take an issue reference and would happily accept one
outside the allow-list.

## Auditing

Every proxied call — including the reads, which is why they are POSTs — writes
an `audit_log` row visible in the dashboard: who ran what, against which issue,
and with what outcome. Bodies, titles, keys and Linear's responses are
deliberately never recorded.

```bash
tclaude proxy linear issue view TCL-568     # → audit verb "linear.issue.view"
tclaude proxy linear issue comment TCL-568  # → audit verb "linear.issue.comment"
```

## Schema drift

Linear's GraphQL schema changes. The documents this proxy ships are validated
against the **live** schema by an opt-in test that needs no credential — Linear
validates a document before it authenticates, so a well-formed query comes back
`AUTHENTICATION_ERROR` while a stale one comes back
`GRAPHQL_VALIDATION_FAILED`:

```bash
TCLAUDE_LINEAR_SCHEMA_CHECK=1 go test ./pkg/claude/agentd/ -run TestLinearQueryDocuments -v
```

It is not wired into CI (it needs the network). Run it when you touch
`linearproxy_queries.go`, or when a `linear_schema_drift` error shows up in
production — that code means tclaude's own query no longer matches Linear's
schema, which is a tclaude bug and not something an agent should retry.

## What this is not

`agentd`'s permission layer is a coordination guardrail, not a security
boundary — see [Sandbox hardening](sandbox-hardening.md). **This feature does
not change that.** A same-uid agent that is not actually confined by the OS
sandbox can read the key file directly and has no need of the proxy.

The proxy is what makes *denying* the key survivable. It is not what enforces
the denial.

And, as with the GitHub half: **everything the proxy writes is attributed to
your Linear account.** A comment the agent posts is a comment you posted. An
issue it creates is a real ticket in your team's tracker, visible to everyone in
the workspace.

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| `503 linear_proxy_disabled` | No `allowed_teams` configured *and* the agent's grant carries no team scope. Add the block above, or scope the grant. |
| `503 key_missing` | No `api_key_file` and no `LINEAR_API_KEY`. The message names the teams that were left without a key. |
| `503 key_unreadable` | The configured file could not be read, or is empty. The message names which key — the default one or a `workspaces` entry. Check it is readable by the account agentd runs as, and that you used `~/` or an absolute path rather than `${HOME}`. |
| `503 linear_proxy_misconfigured` | A `workspaces` entry has no `api_key_file`, no `teams`, or the reserved name `default`; two entries claim the same team key; or a team-spanning verb would need more than 8 credentials. The message says which. See [Teams in more than one Linear workspace](#teams-in-more-than-one-linear-workspace). |
| `503 linear_auth` | Linear rejected the key. It may be revoked, expired, or lack the permission the verb needs — a read-only key cannot comment. |
| `403` naming a slug | The agent lacks `proxy.linear.read` / `proxy.linear.write`. Grant it, or the agent can retry with `--ask-human`. |
| `403 linear_write_disabled` | The slug is granted but `allow_write` is false. Both are required. |
| `403 team_not_allowed` | The operator has an `allowed_teams` list and the team is not on it. Run `tclaude proxy linear whoami` to see the exact key to add. |
| `403 team_out_of_scope` | This agent's grant is scoped to other teams — either the team is on `allowed_teams` and the grant excludes it, or there is no operator list and the grant is the whole policy. Widen the grant, using **the slug the refusal names**, since read and write carry independent scopes: `permissions grant <agent> proxy.linear.read --scope linear_team=…` or `… proxy.linear.write --scope linear_team=…` (naming every team it needs — a scope replaces the previous one). |
| `403 team_scope_empty` | The agent's team scope authorizes nothing at all: it overlaps a configured `allowed_teams` nowhere, or it names teams but constrains some other dimension a Linear request cannot describe, or it carries no `linear_team` at all. The message says which. |
| `400` on an identifier | Only `TEAM-123` form is accepted; a UUID is refused on purpose. |
| `400 unknown_state` | The state name is not one of the team's; the message lists the real ones. |
| `404 not_found` | No such issue or team, or the operator's key cannot see it. Usually a typo'd issue number — not something to escalate. |
| `429 linear_rate_limited` | Linear's budget is spent (2,500 requests/hour per key). The message carries the reset time. |
| `502 linear_schema_drift` | tclaude's query no longer matches Linear's schema. A tclaude bug — do not retry; run the schema-drift test above. |
| `502 linear_unreachable` | The daemon could not reach `api.linear.app`. |
| A write times out client-side | Every verb is bounded by one 60s budget across all the calls it makes, inside the CLI's 75s wait, so a slow Linear surfaces the daemon's answer rather than an ambiguous hang-up. If you see a client-side timeout anyway, the daemon is wedged — do not retry a write blindly. |

## See also

- [Git & GitHub proxy](git-proxy.md) — the sibling feature and the shared design.
- [Agent coordination](agent.md) — the permission model and the CLI surface.
- [Task-reference links](agent.md) — `tclaude agent task set <linear-url>`, which
  is how an agent records *which* ticket it is on for the dashboard. That is a
  display link, unrelated to this proxy's scope gate.
