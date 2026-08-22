# Architecture

This page is the mental model. Every other page documents one part of the
system; this one explains how the parts fit together and why the design looks
the way it does.

## What tclaude is

Running one coding agent in a terminal is easy. Running many — across
repositories, branches, and vendors — turns you into a window manager, a
librarian, and a security team. tclaude is the operations layer that absorbs
that work: a Go CLI plus a daemon that wrap agentic coding CLIs in tmux and
add durable sessions, conversation history and search, an operations
dashboard, agent-to-agent mail, teams, identity and permissions with an audit
trail, sandboxing, and network egress control.

Everything runs on machines you control. The daemon, the state, the dashboard,
and every gate and guardrail are local and open source; the model providers
behind the harnesses are the only external part.

## Harnesses

A *harness* is a wrapped coding CLI. tclaude supports four:
[Claude Code](https://claude.ai/code) (the default),
[OpenAI Codex CLI](https://developers.openai.com/codex/cli),
[OpenCode](https://opencode.ai), and
[GitHub Copilot CLI](https://github.com/features/copilot/cli). A group can mix
them freely, and the same commands drive all of them.

Harnesses do not expose identical primitives, so tclaude is built around
*capability contracts*: each harness registers a descriptor composed of
narrow contracts (spawning, asking, conversation storage, lifecycle commands,
sandbox and approval catalogs, and so on). A missing contract means the
capability is honestly absent — callers gate on it and refuse or degrade
rather than pretend. An unknown harness fails closed. The practical result is
visible all over the docs: phrases like "Claude Code only" or "not
packet-filtered" are contract facts, not editorial hedges. See
[Harnesses](harnesses.md) for the capability matrix.

`--harness shell` also exists, but it is not a harness — just a convenient
hack to bring up a plain terminal in a managed tmux session.

## Sessions, conversations, agents

Three nouns cover most of tclaude:

- A **session** is a live harness instance in an isolated tmux server. It can
  be attached, detached, watched, and stopped. See [Sessions](sessions.md).
- A **conversation** is the persisted thread: transcript, title, working
  directory, and — importantly — the harness that owns it. Resuming a
  conversation relaunches it through the recorded harness with its recorded
  launch posture. See [Conversations](conversations.md).
- An **agent** is a conversation registered with the daemon. It has a stable
  identity (`agt_…`), group memberships, a mailbox, permissions, and a
  lifecycle. A session becomes an agent by being spawned into a group or
  promoted later. See [Agents and groups](agents-and-groups.md).

State lives in SQLite under `~/.tclaude/data/db.sqlite`. The sibling
`~/.tclaude/api/` tree carries only the daemon's agent-reachable socket;
private daemon state stays under `~/.tclaude/data/`.

## The daemon: `agentd`

One daemon sits underneath everything multi-agent: `tclaude agentd serve`
(also shipped as the standalone `tclaude-agentd` binary). The CLI can do
nothing agent-related on its own — there is no direct-database fallback, so
killing the daemon disables the features rather than bypassing their gates.

`agentd` has two faces:

- a **Unix socket** (`~/.tclaude/api/agentd-socket/agentd.sock`) for agents, and
- a **web API** on loopback for the human, hosting the
  [dashboard](dashboard.md).

Every agent-to-agent message, every permissioned operation, every spawn, and
every sandbox decision routes through it — which is what makes identity,
gating, and audit possible at all.

### Identity without tokens

The daemon resolves every socket request's caller from kernel peer
credentials: it takes the connecting process, walks the process tree, and
looks for a known harness *binary* (the executable path, not the process
name). The verdict is agent, human, or refused — there is no fail-open path,
and the human's credentials are ignored for anything with a harness ancestor.
There is no token an agent could steal, because identity is not a token.

Agent identity is deliberately more durable than any single conversation: it
survives `/clear`, context compaction, and
[reincarnation](spawning-and-lifecycle.md), so grants, group memberships, and
queued mail follow the agent rather than the transcript.

## Teams: groups, mail, guardrails

A **group** is the team unit and the allow-list: members may message each
other, and coordination topology is a design choice rather than an accident
of who spawned whom. The human is a first-class participant on the same
rails — the dashboard's Messages tab is a mailbox like any agent's, and
delivery is durable-first: the inbox row lands before any notification is
attempted.

Agents can spawn agents, behind guardrails enforced by the daemon — group
restriction, rate limits, a cross-harness matrix, group size caps, and two
lineage rules: a child may never be *less confined* than its parent, and may
never *auto-approve more* than its parent. Where the child would live is
verified by a write-proof challenge answered by the caller's own sandbox. See
[Spawning and lifecycle](spawning-and-lifecycle.md).

Above single spawns sit reusable libraries — spawn profiles, roles, and group
templates that deploy a whole roster against one mission — plus scheduling
and standing automation. See [Teams at scale](teams-at-scale.md).

## Permissions and audit

Permissioned operations are gated on **slugs** (`self.rename`,
`groups.members.stop`, `human.notify`, …), optionally narrowed by scopes.
Grants can be scoped; denies never are. Time-bounded `sudo` elevations and
`--ask-human` escalations bring the human into the loop, with timeout meaning
deny. Every daemon-proxied operation lands in the audit trail — including
denials, so the record answers "who *tried* to do what", not only what
happened. See [Permissions and audit](permissions-and-audit.md).

## Confinement

Sandboxing is split into two questions, asked per launch:

- **What does the harness's own sandbox do?** (`--sandbox`, per-harness
  modes.)
- **Who enforces containment?** (`--sandbox-impl`: the harness's built-in
  sandbox, tclaude's own layer — bubblewrap on Linux, Seatbelt on macOS — an
  experimental stacked combination, a resource-only cgroup, or off.)

Policy comes from declarative JSON **sandbox profiles** — filesystem grants
with carve-outs in both directions, environment, resources, and network —
attached globally, per group, or per launch. When a backend cannot faithfully
enforce what a profile demands, tclaude refuses to launch rather than
silently degrading. Network egress gets its own engines: a Linux packet
engine and a name-based filtering proxy engine. See
[Sandboxing](sandboxing.md) and [Network filtering](network-filtering.md).
For agents that should hold no credentials at all, daemon-side
[credential proxies](proxies.md) perform git, GitHub, and Linear operations
on their behalf, bounded by operator allow-lists.

## Design principles

The same few principles repeat everywhere and are worth naming once:

- **Fail closed.** Unknown harness, missing capability, unreachable daemon,
  unverifiable identity — each refuses rather than guesses.
- **Honest capability gaps.** Features gate on contracts; the docs and the
  CLI report differences instead of pretending.
- **Durable first.** Mail, identity, grants, and history persist in SQLite
  and survive restarts, `/clear`, and reincarnation. tmux keystroke injection
  is only ever a best-effort notifier, never the transport.
- **The human stays in the loop.** Approvals, sudo, audit, and the dashboard
  exist so autonomy comes with a defensible trail.
- **Self-hosted, no lock-in.** Vendors are pluggable at the harness seam;
  everything else is yours.
