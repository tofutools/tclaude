# Spawning and lifecycle

Agents create other agents, and someone has to keep that safe. This page
covers `tclaude agent spawn`, the seven guardrails that bound what a
spawning agent can create, the profile and role libraries that make spawns
repeatable, and the lifecycle verbs that manage an agent from birth to
retirement — including the `--target` manager pattern they all share.

Group membership, messaging, and what a spawned agent receives at startup
are on [Agents and groups](agents-and-groups.md).

## Spawning an agent

```bash
tclaude agent spawn myteam --profile worker \
  --name reviewer-1 --role reviewer \
  --file brief.md --task https://linear.app/acme/issue/JOH-123
```

The daemon launches `tclaude session new -d --global`, waits for the new
conversation id to materialize, and adds it to the group. Spawning requires
the `groups.members.spawn` permission — human-only by default; group
ownership contributes it scoped to owned groups, and the guardrails below
still apply to agent callers.

The flags that matter most:

- **Identity**: `--name` (becomes the conversation title; charset
  `[A-Za-z0-9_-]`, 1–64 characters, auto-normalized), `--role` (free-text
  display and routing label), `--role-ref` (a saved role — brief plus
  baseline permissions, see below), `--descr` (one-line dashboard label),
  `--task <url>` and `--task-label` (the dashboard Task column link),
  `--owner` / `--no-owner` (owner spawns need `groups.owners.manage`
  authority).
- **Brief**: `--initial-message`, or `--file <path>` (`-` reads stdin;
  16384-byte cap) for anything long, multi-line, or containing backticks.
  `--reply-to` picks who the agent's first reply reaches; it defaults to
  the spawning agent, and is empty for human spawns.
- **Launch shape**: `--harness`, `--model`, `--effort`, `--sandbox`
  (harness-builtin mode), `--sandbox-impl` (OS containment — see
  [Sandboxing](sandboxing.md)), `--ask-for-approval`, and per-harness
  toggles (see [Harnesses](harnesses.md)). `--profile` pre-fills all of
  these from a saved spawn profile — with a profile, usually no other
  launch flag is needed.
- **Worktree**: `--worktree <branch>` creates or reuses a git worktree on
  that branch and spawns the agent into it; `--worktree-base` picks the
  branch it is cut from; `--worktree-repo` covers the monorepo case — the
  agent launches in `--cwd` and the worktree's path and branch ride into
  its welcome instead. The checkout runs in the daemon, and a rejected
  spawn removes a worktree or branch the daemon created for it. See
  [Worktrees](worktrees.md).
- **Attention**: `--auto-focus` opens a terminal attached to the new agent
  (off by default on the CLI; the dashboard spawn modal defaults it on).
  `--no-group-context` skips the group's shared startup context for this
  one spawn.

How the new agent actually starts is harness-specific. Claude Code is
launched with the conversation id and welcome preset — no keystroke
injection. Codex receives the `[system: …]` welcome as its required
first-turn seed, with the title written to Codex's store out-of-band.
Either way the briefing lands durably in the agent's inbox — see
[Agents and groups](agents-and-groups.md#what-a-spawned-agent-receives).

### Default resolution

Each launch field (`--harness`, `--model`, `--effort`, `--sandbox`,
`--sandbox-impl`, `--ask-for-approval`, `--ask-user-question-timeout`) is
resolved independently, highest tier first:

1. the explicit flag
2. `--profile` (a named spawn profile)
3. the group's default spawn profile
4. the global default spawn profile
5. the harness's own default

The harness resolves through the full chain *first*; the other fields then
validate against it. An incompatible explicit flag is a loud error; an
incompatible value from a lower profile tier is skipped, falls through, and
is disclosed in the resolved-shape echo.

!!! warning
    A spawn profile carries its own harness, so leaving `--harness` unset
    is **not** the same as Claude Code: a group or global default profile
    that selects Codex sends a no-flag spawn to Codex, on that profile's
    model. When policy requires a specific vendor or model, pin it with
    explicit `--harness` and `--model`, or a `--profile` that pins them.
    The spawn output echoes the resolved Harness/Model/Effort and where
    each came from; inspect the ambient defaults with
    `tclaude agent profiles default show` and `tclaude agent groups ls`.

## The seven spawn guardrails

A human spawning agents is one thing; agents spawning agents is where
things can run away. Seven checks bound every agent-initiated spawn. The
human bypasses the agent-only ones — but not all seven are agent-only.

**1. Group restriction.** An agent may only spawn into a group it belongs
to or owns (`403 group_restricted`). Config:
`agent.spawn_group_restriction`, `agent.spawn_allowed_groups`.

**2. Rate limit.** At most 10 spawns per agent per rolling hour by default
(`429 rate_limited`; `agent.spawn_max_per_hour`). A runaway loop stalls
instead of forking exponentially.

**3. Cross-harness matrix.** Whether a Claude Code agent may spawn a Codex
worker (and every other directed pair) is an operator decision: a global
matrix with per-group overrides, and denials carry the operator's stated
reason (`403 cross_harness_spawn_denied`). The matrix covers every spawn
path — direct, template, wave, process, scribe, and clone.

**4. Sandbox lineage.** A child may not launch under a weaker sandbox than
its parent (`403 sandbox_restricted`). Confinement can only ratchet down
the tree, never widen. Launches under tclaude's own sandbox layer are
classified by their *real* confinement, not the `off` or
`danger-full-access` value recorded for the inner harness. See
[Sandboxing](sandboxing.md).

**5. Approval lineage.** A child may not auto-accept a broader class of
actions than its parent: each approval posture maps to a capability set
(auto-edits, auto-commands, machine-reviewer, unreviewed), and the child's
set must be a subset of the caller's (`403 approval_restricted`). A child
posture left unset is narrowed to the caller's own same-harness posture
rather than failing; an `inherit` parent may mint an exactly-`inherit`
child. One residual gap is documented: the guard compares *requested*
postures, so a parent that can write the child's working directory could
widen the child's effective posture through its settings file — bounded in
practice by sandbox lineage, which governs who can write where.

**6. Directory write-proof.** A sandboxed agent must *prove* it can write
every directory the spawn would give the child write access to — the
launch directory, the worktree, and the repo root plus its git admin
directory. The mechanism is a challenge handshake: the daemon rejects the
first attempt with a single-use token and the list of directories, the
caller creates a `.tclaude-write-proof-<token>` file in each, and
retries; the daemon verifies and re-verifies the proofs immediately before
forking the session. The elegance is that the caller's *own sandbox*
answers the question. The daemon never has to parse or trust a sandbox
profile to know what the caller can touch: if the caller's confinement
forbids writing a directory, the proof file simply cannot be created, and
the spawn fails closed. The CLI answers the handshake automatically, so an
authorized caller never sees it. Exempt: humans, unconfined parents, and
Codex read-only children.

**7. Group size cap.** A group's `set-max-members` limit is enforced at
spawn (`409 group_full`) — and unlike the six above, it binds the human
too. See [max members](agents-and-groups.md#max-members).

Guardrail denials, like all permission denials, land in the audit trail —
see [Permissions and audit](permissions-and-audit.md).

## Spawn profiles and roles

Two libraries make spawns repeatable instead of flag soups. Reads are open
to every agent; writes are gated (`profiles.manage`, `roles.manage`) and
effectively human-only by default.

**Spawn profiles** (`tclaude agent profiles ls|show|create|edit|rm|
disable|enable|default`) are named bundles of the whole spawn dialog:
launch shape (harness, model, effort, sandbox, approval, per-harness
toggles), identity (`agent_name`, `role`, `role_refs`, `descr`,
`initial_message`, `startup_context`), and birth-time access (`is_owner`,
`permission_overrides`). They are JSON-authored. `profiles default
show|set|clear` manages the global default profile that sits at tier 4 of
the resolution chain. A disabled profile fails spawns loudly
(`profile_disabled`, with the operator's reason) rather than silently
falling through, and a profile marked `operator_only` refuses
agent-originated use. Birth-time access is resolved against the caller's
own authority: a named profile the caller cannot honor is a refusal, while
an ambient default profile's unauthorized access fields are skipped and
disclosed.

**Roles** (`tclaude agent roles ls|show|create|edit|rm`) are named
bundles of a canonical role brief — prepended to the agent's startup
context — plus a default permission set. Reference one with `--role-ref`,
from template specs, or from a profile's `role_refs`; it resolves at spawn
time, and `rm` is refused while anything references the role.

Group templates and task forces build whole rosters out of these — see
[Teams at scale](teams-at-scale.md).

## Promote

`tclaude agent promote <selector>` enrolls an existing plain conversation
as an agent, and also reinstates a retired one. Authorization:
`agent.promote`, or `groups.members.promote` under the manager pattern
below.

## The manager pattern

Every lifecycle verb defaults to acting on the calling agent itself, and
most accept `--target <selector>` to act on another agent. The
authorization rule is uniform, and this is its one canonical statement —
other pages link here:

- Acting on **yourself** needs the matching `self.<verb>` slug. The
  standard set is granted by
  `tclaude setup --install-default-agent-permissions`; the one exception
  is self-reincarnation, which needs no slug at all.
- Acting on **another agent** needs either the global `agent.<verb>` slug,
  or `groups.members.<verb>` with scopes covering **every** active group
  the target currently belongs to. If any one of the target's groups is
  uncovered, the check rejects atomically. Archived and historical
  memberships are ignored.
- **Ownership contributes** the group-scoped `groups.members.<verb>` slugs
  for owned groups — so a group owner can manage members whose only groups
  it owns, without any explicit grant.

The every-group rule is what makes it safe: an agent in two groups cannot
be stopped, renamed, or retired by a manager who only has authority over
one of them.

Most verbs also take `--ask-human <duration>` (capped at 300s; timeout is
a deny) to escalate a denial into a dashboard approval popup — see
[Permissions and audit](permissions-and-audit.md).

## Lifecycle verbs

### rename

`tclaude agent rename "<title>"` retitles a conversation through the
harness-appropriate path (Claude Code: `/rename` injection; Codex: title
store; Copilot API drive: typed call). The title charset
(`[A-Za-z0-9_\-\[\]{}() ]`, max 64) is a hard security gate, because on
some harnesses the title becomes keystrokes. `--auto` instead queues an
inbox instruction asking the target to name itself.

### compact and context-info

`compact [follow-up]` injects `/compact`, with an optional follow-up
message queued after it (timing best-effort). `context-info` reads the
context meter (populated by the status line) for yourself — ungated — a
`--target`, or a whole team at once with `--group`.

### reincarnate

`reincarnate <follow-up>` replaces an agent with a fresh successor: the
daemon spawns a new session, migrates the identity — the stable agent id,
groups, grants, ownerships — onto the new conversation, and soft-stops the
old one. The follow-up argument is required, because the successor starts
with a clean context window and would otherwise sit idle. The daemon
migrates *identity*, not work: persist task state to a handoff file before
reincarnating. Self-reincarnation is the one lifecycle action that needs
no permission slug. It is primarily a Claude Code tool for context
pressure; Codex agents should run on and let native auto-compaction do its
job. Cross-agent handoffs record the caller as the originator.

### clone

`clone [follow-up]` forks an agent into a sibling that inherits its
identity (groups, grants, ownerships) while the original keeps running.
The sibling is renamed `<title>-c-<N>` and gets a copy of the conversation
transcript by default (`--no-copy-conv` for a blank-context sibling).
Agent-initiated clones are rate-limited by the daemon's clone cooldown
(default one minute).

### seance

`seance [question]` asks a dead predecessor one question: the daemon
resumes the retired incarnation headlessly for a single turn and returns
only its answer — nothing is reanimated. `--back N` walks further up the
chain; `--target` consults another agent's predecessor or an exact dead
generation; `--model`/`--effort` pick the medium; `--print-cmd` dry-runs
for free. Turns are capped at 10 minutes, are billable, and need the
predecessor's launch directory to still exist.

### stop and resume

`stop <selector>` soft-exits the target's session, escalating after about
ten seconds through pane kill, SIGTERM, and finally SIGKILL of the frozen
pane's process group, re-checking identity at each step. Idempotent;
`--force` skips the soft phase.

`resume <selector>` relaunches an offline agent into its recorded working
directory. If that directory has vanished, resume fails with
`error:missing_cwd` (`--recreate-dir` is human-or-approved-only), and
malformed or tampered directory provenance fails closed. Managed Codex
agents also auto-recover from crashes with exponential backoff.

### retire, reinstate, delete

`retire <selector>` soft-deletes an agent: memberships dropped,
permissions and sudo revoked, the conversation kept and reinstatable. By
default it also soft-exits the live session (`--no-shutdown` keeps it) and
removes the agent's linked worktree and branch (`--no-delete-worktree`;
the main repo and shared worktrees are always kept). `--reason` is
recorded.

`reinstate <selector>` returns a retired agent to active status — old
memberships and grants do **not** come back. Authorization is
`agent.promote` or ownership.

`delete <selector>` is the permanent wipe: all database rows, the
transcript, the session token. It refuses while the tmux session is alive
unless `--force`, requires `agent.delete` (not granted by default) or
group ownership, and refuses self-deletion (use `tclaude conv rm`).

### dir

`dir [selector]` reports the directory an agent is working in — the
current dir by default, `--start` for the immutable launch dir,
`--worktree` for the repo root. `--open` opens a terminal there via the
daemon, and `--repair` recreates your own deleted startup directory
(self-only, `self.dir-repair`, at a daemon-selected path).

### Other verbs

- `remote-control [on|off|toggle|status]` toggles Claude Code Remote
  Access on an agent; `status` reads the live pane and self-heals the
  tracked flag. Claude Code only — see [Remote](remote.md).
- `interrupt` interrupts an active Codex app-server turn;
  `codex-app-server status` shows drive diagnostics.
- `sandbox-impl show|set` reads or assigns the durable sandbox
  implementation an *offline* agent relaunches under. Its slug
  (`agent.sandbox-impl`) is deliberately not owner-conferred and not
  default-granted — effectively human-only. See
  [Sandboxing](sandboxing.md).

Cross-cutting conveniences that ride on the same identity — head aliases,
tags, task links, `present-pr`, exports, and the human-clipboard bridge —
are covered in [Teams at scale](teams-at-scale.md).
