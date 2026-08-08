# Linear proxy 📋

**Audience:** operators who track work in [Linear](https://linear.app/) and want
their sandboxed agents to read the ticket they are on, and report back on it,
without handing them a Linear API key.

## The problem this solves

An agent spawned against `TCL-568` cannot read `TCL-568`. The ticket holds the
acceptance criteria, the discussion, and the reason the work exists — and all of
it is behind a credential the agent should not have.

The usual escape is to give the agent a Linear key, or point its harness at
Linear's MCP server. Both work, and both mean the agent holds a credential to
your whole workspace.

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
ties a conversation to an issue. **The team allow-list is therefore the only
scope gate**, which is why it is mandatory, fail-closed, and checked twice.

## Enabling it

The proxy is **off** until you allow-list at least one team. There is no
"allow everything" setting.

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
| `allowed_teams` | Team keys the proxy may act on — the prefix in an issue identifier, so `TCL` authorizes `TCL-568`. Matched case-insensitively. **Empty or absent disables the proxy entirely.** |
| `api_key_file` | A file whose contents are the Linear personal API key. Empty falls back to `LINEAR_API_KEY` in the environment agentd runs under; with neither, the proxy refuses. |
| `allow_write` | Permits the mutating verbs at all. Default off. |

`api_key_file` accepts `~/…`, expanded against the home directory of the account
agentd runs as. Shell variables are **not** expanded — a config file is not a
shell, so `"${HOME}/key.txt"` is taken literally.

The key is deliberately not stored in `config.json` itself: that file is
plaintext, appears in the dashboard's Config tab, and is the sort of thing that
ends up in a bug report.

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
| `linear.read` | `whoami`, `issue view/ls/search/comments` |
| `linear.write` | `issue create/comment/update/link` |

```bash
tclaude agent permissions grant <agent> linear.read
tclaude agent permissions grant <agent> linear.write
```

**Writing needs both the slug and `allow_write`.** They answer different
questions: the slug says *this agent* may write, `allow_write` says the operator
wants *any* agent to be able to. A grant cannot override the config, and the
config cannot grant an agent anything.

An agent without a grant can still ask for a one-off with `--ask-human 60s`.

## What an agent can do

```bash
# Discovery — run this first, and whenever something is refused.
tclaude proxy linear whoami

# Reads (linear.read)
tclaude proxy linear issue view TCL-568
tclaude proxy linear issue ls --team TCL --state "In Progress"
tclaude proxy linear issue ls --assigned-me --limit 10
tclaude proxy linear issue search "flaky dashsnap"
tclaude proxy linear issue comments TCL-568

# Writes (linear.write + allow_write)
tclaude proxy linear issue comment TCL-568 --body-file progress.md
tclaude proxy linear issue update TCL-568 --state "In Review"
tclaude proxy linear issue create --team TCL --title "…" --description-file spec.md
tclaude proxy linear issue link TCL-568 --url https://github.com/acme/repo/pull/42
```

`whoami` is the command to point an agent at when something is refused: it lists
every team the key can see with the allow-list verdict beside it, so the agent
can tell you exactly which key to add instead of guessing from a 403.

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

> **`issue comments` carries third-party prose into an agent's context.** A
> Linear comment can be written by anyone with access to the workspace. Every
> other read returns structured data; this one returns free text that an agent
> will read as part of its instructions unless it has been told otherwise. The
> bundled `proxy-linear` skill says so; keep it in mind if you write your own.

## How the team gate holds

The allow-list is checked **twice**, and the second check is the load-bearing
one.

1. **On the identifier the caller supplied.** `TCL-568` → `TCL` → is it
   allow-listed? This runs before any network call, so a refusal costs nothing.
2. **On the team Linear actually reported.** The daemon reads the issue and
   refuses on `issue.team.key` before rendering anything.

The two can disagree. An issue moved between teams keeps answering to its old
identifier, so a proxy that trusted the prefix alone would be checking a label
rather than the thing it reached. Every issue-shaped GraphQL selection therefore
asks for `team { key }` — removing that field does not weaken the gate quietly,
it turns the verb into an error.

The same rule applies to listings: the request carries the allow-list as a
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
| `503 linear_proxy_disabled` | No `allowed_teams` configured. Add the block above. |
| `503 key_missing` | No `api_key_file` and no `LINEAR_API_KEY`. |
| `503 key_unreadable` | The configured file could not be read, or is empty. Check it is readable by the account agentd runs as, and that you used `~/` or an absolute path rather than `${HOME}`. |
| `503 linear_auth` | Linear rejected the key. It may be revoked, expired, or lack the permission the verb needs — a read-only key cannot comment. |
| `403` naming a slug | The agent lacks `linear.read` / `linear.write`. Grant it, or the agent can retry with `--ask-human`. |
| `403 linear_write_disabled` | The slug is granted but `allow_write` is false. Both are required. |
| `403 team_not_allowed` | The team is not on `allowed_teams`. Run `tclaude proxy linear whoami` to see the exact key to add. |
| `400` on an identifier | Only `TEAM-123` form is accepted; a UUID is refused on purpose. |
| `400 unknown_state` | The state name is not one of the team's; the message lists the real ones. |
| `429 linear_rate_limited` | Linear's budget is spent (2,500 requests/hour per key). The message carries the reset time. |
| `502 linear_schema_drift` | tclaude's query no longer matches Linear's schema. A tclaude bug — do not retry; run the schema-drift test above. |
| `502 linear_unreachable` | The daemon could not reach `api.linear.app`. |

## See also

- [Git & GitHub proxy](git-proxy.md) — the sibling feature and the shared design.
- [Agent coordination](agent.md) — the permission model and the CLI surface.
- [Task-reference links](agent.md) — `tclaude agent task set <linear-url>`, which
  is how an agent records *which* ticket it is on for the dashboard. That is a
  display link, unrelated to this proxy's scope gate.
