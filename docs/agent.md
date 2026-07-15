# Agent Coordination 🤝

> **BETA / EXPERIMENTAL**
>
> The agent feature is under active development. Commands, flags,
> permission slugs, the daemon wire format, the dashboard, and the
> SQLite schema may all change without notice. Per-agent permission
> grants and time-bounded elevations are stored in tables that have
> not been field-tested across many upgrades. **Don't build automation
> against this yet that you can't readily migrate.** See the
> project's issue tracker for what's still in flight.

Cross-session coordination between Claude Code conversations on the
same machine: messaging, group membership, agent lifecycle (spawn,
clone, reincarnate), scheduled nudges, and a browser dashboard — all
gated by a permission model the human curates.

`tclaude agent` (the CLI) talks to `tclaude agentd` (a long-running
daemon) over a Unix socket. The daemon owns the database, tmux nudges,
and permission gating; the CLI is a thin client. The daemon resolves
every caller from the connecting socket peer: a caller running inside
a Claude Code session is an *agent*, identified by a stable `agent_id`
the daemon resolves from its live conv-id (re-read on every request, so
`/fork` and `/resume` keep working); the *human operator*
authenticates with an operator token. A caller it can confirm as
neither is refused. See [Identity](#identity).

> **Upgrade compatibility:** restart `agentd` after upgrading `tclaude`.
> The CLI and daemon share an evolving request schema; for example, a new CLI
> talking to an old daemon silently drops `--profile` launch fields because the
> old daemon does not understand the profile reference.

Brief daemon restarts are tolerated by the CLI. Connection failures retry after
1, 2, 4, 8, and 16 seconds; read requests that receive HTTP 5xx retry twice,
after 1 and 2 seconds. Retry notices are written to stderr, and other HTTP
errors fail immediately. Mutating requests carry a stable request ID across
retries. Agentd stores pending and completed request outcomes in SQLite,
replaying a completed response without rerunning the mutation. If a replaced
daemon left a request pending, the CLI reports that its outcome is unknown
instead of risking a duplicate mutation. A client talking to an older daemon
that does not advertise this ledger still retries reads, but sends mutations
only once.

## What you can do

- **Talk between conversations.** Send messages, replies, and
  threaded follow-ups between CC sessions. The receiver gets a tmux
  nudge if they're online; otherwise the message queues in their
  inbox.
- **Group sessions.** Allow-list who can talk to whom. Out-of-group
  messages are refused server-side. Inter-group links open up
  directed cross-group messaging without co-membership.
- **Spawn and manage agents.** Spawn fresh CC sessions straight into
  a group, clone an agent into a sibling, reincarnate it onto a fresh
  context window, wake/stop sessions, and open terminals — your own
  or, with the right permission, another agent's.
- **Deploy a task force.** Author a reusable team [template](#task-forces-cli)
  and deploy it against a mission — a whole roster spawned, briefed, and steered
  through an advisory process. See [Task forces](#task-forces-cli).
- **Schedule recurring nudges.** Cron jobs deliver a message to an
  agent (or a whole group) on an interval.
- **Delegate capabilities.** Per-agent permissions live in SQLite;
  you (the human) decide which agents can rename themselves, manage
  group membership, etc. Agents can request one-off escalation via a
  dashboard access request, or a bounded `sudo` elevation.
- **Operate it from a browser.** The [agent dashboard](dashboard.md)
  is a full operations console for everything above.

## Prerequisites

- **`tclaude setup`** — registers hooks and the agentd socket path.
- **`tclaude setup --install-agent-skills`** — materialises the
  bundled `agent-*` skills under `~/.claude/skills/` for Claude Code
  and both `~/.agents/skills/` and `$CODEX_HOME/skills` (default
  `~/.codex/skills`) for Codex CLI. Without these skills installed,
  agents won't know to use these commands.
- **`tclaude setup --install-default-agent-permissions`** — grants the
  self-targeted slugs the bundled skills exercise (`self.rename`,
  `self.compact`, `self.clone`, `self.schedule`,
  `self.remote-control`, `self.task`, `self.pr`, `self.tags`) as agent
  defaults. Self-reincarnation needs no slug. Idempotent; only adds missing slugs.
- **`tclaude agentd serve`** — running in a non-sandboxed shell. The
  CLI refuses to fall back to direct DB access when the daemon is
  down — that's deliberate, so the auth model can't be bypassed by
  killing the daemon. On startup the daemon prints an **operator
  token**; the human exports it as `TCLAUDE_HUMAN_TOKEN` to run
  human-only commands (see [Identity](#identity)).

The daemon binds two sockets:

- `~/.tclaude-agentd.sock` — canonical, state-free Unix socket for all
  `tclaude agent` traffic. Keeping it outside `~/.tclaude` lets every
  harness deny the private state tree wholesale.
- `~/.tclaude/agentd.sock` — temporary compatibility listener for older
  clients and previously installed Claude sandbox settings. New clients and
  generated settings do not use it.
- `127.0.0.1:<random>` — loopback HTTP for the human-approval popup
  and the [dashboard](dashboard.md).

By default `agentd serve` also adds a system tray icon (Open
dashboard, Reinstall agent skills, Open config, pending-approvals
submenu, Quit). On Linux hosts without a reachable session DBus (common
in WSL and headless sessions), agentd reports that the tray is unavailable
and continues without it. A missing tray host on an otherwise working bus
also leaves the daemon running normally. Pass `--no-tray` (or set
`agent.disable_tray: true` in
`~/.tclaude/config.json`) to skip the tray entirely. Pass
`--auto-launch-dashboard` (or set `agent.auto_launch_dashboard` in
config) to open the dashboard on startup.

`agentd serve` also accepts `--agent-clone-cooldown <duration>` — the
minimum cooldown between two clones of the same agent (a Go duration,
e.g. `1m`, `30s`; `0` disables it). It overrides the persistent
`agent.clone_cooldown` config.json field; the built-in default is
`1m`. Resolution order is flag > config > default. The cooldown bounds
a runaway *agent* loop, so it applies only to agent-initiated clones —
clones you trigger yourself (CLI or dashboard) are never rate-limited.

By default the dashboard + approval popup bind a **random** free
loopback port each start. Pass `--dashboard-port <port>` (or set
`agent.dashboard_port` in config, also editable from the dashboard's
Config tab) to pin a **fixed** port — handy for a bookmarkable URL, a
reverse proxy, or a firewall rule. Resolution order is flag > config >
random. The port is loopback-only and human-gated either way. Binding
is strict: if the configured port is already in use (or out of range)
`agentd serve` **fails to start** rather than silently falling back to a
random port — a silent fallback would break whatever the fixed port was
set up for.

## Quick start

```bash
# 1. Run the daemon in a separate (non-sandboxed) terminal.
tclaude agentd serve

# 2. Install the agent skills so agents know about these commands.
tclaude setup --install-agent-skills

# 3. As the human, set up a group and add some sessions.
tclaude agent groups create reviewer-team --descr "code review crew"
tclaude agent groups add reviewer-team <conv1> --role tech-lead
tclaude agent groups add reviewer-team <conv2> --role test-runner

# 4. From inside an agent session, an agent can now reach peers.
tclaude agent ls
tclaude agent message lead "Can you review PR #42?"

# Or open the dashboard and drive it from a browser.
tclaude agent dashboard
```

## Identity

On every request the daemon resolves the connecting socket peer into
exactly one *caller identity*. It reads the peer's PID from the kernel,
walks the host process tree looking for a `claude`/`node` ancestor, and
checks the request for an operator token. The verdict is one of:

- **Agent** — a `claude`/`node` ancestor is present in the process
  tree. The daemon resolves that ancestor's current conv-id — from
  `~/.claude/sessions/<pid>.json` (the `sessionId` Claude Code is
  *currently* on, so it follows `/fork` and `/resume`), falling back
  to the daemon's own `sessions` table keyed by host PID. It then maps
  that live conv-id to the agent's stable `agent_id` — the
  rotation-immune identity that outlives any conv-id rotation
  (reincarnate, `/clear`) — and gates the agent by the
  [permission model](#permission-model).
- **Human** — the operator. There are two ways to reach this class:
  a CLI caller with **no** `claude`/`node` ancestor that presents a
  valid operator token, or a request from the cookie-authenticated
  browser [dashboard](dashboard.md). The human bypasses every
  permission gate.
- **Refused** — a caller the daemon can confirm as neither. No
  `claude`/`node` ancestor and no valid token → `403 unconfirmed`; a
  Claude Code ancestor whose conv-id can't be resolved → `403`; a peer
  whose PID can't be read at all → `401`. There is **no** fail-open
  "assume human" path — an unproven caller is always refused.

### The operator token

The operator token is how a CLI human proves who they are. The daemon
mints a fresh one (`crypto/rand`, `tclo_` prefix) each time it starts,
holds it **in memory only** — never written to disk, never logged —
and prints it on the **startup banner**. The banner is the sole
delivery channel: there is no fetch endpoint, and the token only
prints when the daemon's stdout is a real terminal (a backgrounded or
log-redirected daemon withholds it, so it can't leak into a log file).

The human copies the `export TCLAUDE_HUMAN_TOKEN=…` line from the
banner into their shell; the CLI then attaches it to every daemon
request automatically. Restarting the daemon mints a new token, so the
human re-copies it. Agents never need a token.

**Opt in to a stable token.** If re-copying after every restart is
tiresome, pass `--persist-operator-token` to `agentd serve` (or set
`agent.persist_operator_token: true` in `~/.tclaude/config.json`, also a
checkbox on the dashboard's Config tab — the two OR together). The daemon
then generates the token once and stores it, reusing it across restarts,
so you export it a single time. It is stored in the **OS keychain** when
one is reachable (macOS Keychain, Linux Secret Service, Windows Credential
Manager); on a host with no keychain backend (headless Linux, WSL without
D-Bus) it falls back to a `0600 ~/.tclaude/operator_token` file. The
secret is deliberately **not** written into `config.json` (which is
plaintext and shows up in the Config-tab diff and backups); the file
fallback keeps the same boundary as the in-memory token, since the agent
sandbox already denies reads to `~/.tclaude`. You can also pin your own
token by writing that file directly. Default (off) is the
fresh-token-each-boot behaviour described above.

**A Claude Code ancestor always wins over the token.** Because the
human exports `TCLAUDE_HUMAN_TOKEN` into their shell, a CC session
launched from that shell would inherit it — so the daemon classifies
*agent-ness first* and never offers the token branch to a caller with
a `claude`/`node` ancestor. An agent therefore cannot escalate to the
human even if it holds the token (and `agentd` also strips
`TCLAUDE_HUMAN_TOKEN` from the environment of every CC session it
spawns). The flip side: a human running `tclaude agent` from a shell
that happens to descend from a non-Claude `node` process is classified
agent-side, not human, and the token won't rescue it — run operator
commands from a clean terminal, or use the dashboard.

`tclaude agent whoami` reports the resolved identity — an agent's stable
`agent_id`, `<human>`, or neither if the daemon couldn't confirm the
caller.

Most commands accept a *selector* wherever they take an agent: the stable
`agent_id` (full or a unique `agt_…` prefix — the canonical,
rotation-immune handle), a full conv-id, an 8+-char conv-id prefix, a
global head alias, or a current display title. Prefer the `agent_id`: a
conv-id rotates when the agent reincarnates or clones, the `agent_id`
never does.

## Commands

The CLI surface is large; `tclaude agent <cmd> --help` is always the
authoritative reference. The sections below group the commands by what
they're for.

### whoami / lookup / ls

```bash
tclaude agent whoami           # who am I (stable agent_id + name, or <human>)
tclaude agent lookup <name>    # resolve prefix / title to the stable agent_id
tclaude agent ls               # peers in any group I'm in (ID = agent_id; online indicator + groups)
tclaude agent ls --json
```

`ls` is restricted to peers reachable through a shared group — the
group acts as an allow-list.

### message / reply / inbox

```bash
# direct
tclaude agent message <peer> "your message text"
tclaude agent message <peer> --subject "ack" --stdin <<EOF
multi-line body
EOF
tclaude agent message <peer> --file plan.md

# broadcast to every member of a group except yourself
tclaude agent message group:reviewer-team "PR #42 ready for eyes"

# reply (looks up the sender from the original message id; no need to
# copy conv-ids out of the headers)
tclaude agent reply <id> "got it"
```

For direct messages the sender and target must share a group (or be
bridged by an inter-group [link](#groups)), otherwise the daemon
refuses with `not in a shared group`. For multicast (`group:<name>`
target) the sender must be a member of that group. If the target's
tmux session is alive they get a system-style nudge; otherwise the
message queues in their inbox until they `inbox ls`.

```bash
tclaude agent inbox ls                # last 20, all
tclaude agent inbox ls --unread       # only unread
tclaude agent inbox read <id>         # marks read; --keep-unread to defer
```

`inbox` has aliases `mailbox` / `mail`. Reading a message returns
RFC-822-shaped headers — `From`, `To`, `Group`, `Subject`, `Date`,
`Reply-To`, `Reply-Cmd` — followed by the body.

### groups

```bash
tclaude agent groups ls                                   # all groups + member/online counts
tclaude agent groups members <group>                      # members + ● online indicator
tclaude agent groups owners <group>                       # owners (can message members without being one)
tclaude agent groups create <group> [--descr "..."] [--member name=lead,role=...]
tclaude agent groups rm <group>                           # delete the group; sweeps its process/wave/cron state, keeps message history as direct messages
tclaude agent groups add <group> <conv> [--role R --descr T]
tclaude agent groups remove <group> <conv>
tclaude agent groups update-member <group> <conv> [--role R --descr T]
tclaude agent groups rename <group> <new-name>
tclaude agent groups grant-owner <group> <conv>
tclaude agent groups revoke-owner <group> <conv>
tclaude agent groups set-default-dir <group> [dir]        # default cwd for agents spawned into the group
tclaude agent groups set-max-members <group> [max]        # cap member count; a spawn that would exceed it is refused (0 = unlimited)
tclaude agent groups set-context <group> [text]           # shared startup context delivered to every spawned agent's inbox
tclaude agent groups set-remote-control <group> [inherit|optin|deny]  # group remote-control policy; overrides the spawn profile's default (Claude Code only)
tclaude agent groups archive <group>                      # soft-delete: freeze, hide, keep history
tclaude agent groups unarchive <group>                    # reverse an archive
tclaude agent groups clone <source> [new-name]            # fork every member into a brand-new group
tclaude agent groups stop <group> [--force]                # soft /exit (or hard kill-session) every member
tclaude agent groups resume <group>                        # spawn a session for every offline member; re-enables rhythms an emptying retire auto-disabled
tclaude agent groups retire <group> [--no-shutdown]        # retire (soft-delete) every OTHER member; if it empties the group, auto-disables its rhythms
tclaude agent groups rebrief <group>                       # re-deliver a deployed force's work pattern + mission (see Task forces)
tclaude agent groups export <group> [--out FILE]           # export the group (DB rows + members' .jsonl) to a portable .zip
tclaude agent groups import <file.zip> --into <dir>        # recreate an exported group on this machine
tclaude agent groups transfers                             # the group export / import audit log
tclaude agent groups why-can-i-message <target>            # explain which group/link authorises a send
```

`update-member` only touches the flags you pass; pass `--role=` (an
explicit empty string) to clear a field. To rename an agent use
`tclaude agent rename`. `--member` on `create` bootstraps a whole team
in one call (each value is a comma-separated `key=value` list —
`name=lead,role=tech-lead,cwd=.`).

**Inter-group links** are directed edges that let one group's members
message another group's members without co-membership:

```bash
tclaude agent groups link add <from-group> <to-group> [--mode ...]
tclaude agent groups link ls <group>
tclaude agent groups link set-mode <from> <to> <mode>
tclaude agent groups link rm <from-group> <to-group>
tclaude agent groups links                                 # every link, all groups
```

All mutating subcommands take `--ask-human <duration>` (see
[below](#ad-hoc-human-approval---ask-human)).

### spawn profiles

Spawn profiles are reusable launch and identity presets. They can be paused
without being deleted:

```bash
tclaude agent profiles ls
tclaude agent profiles show <name>
tclaude agent profiles disable <name> --reason "provider maintenance"
tclaude agent profiles enable <name>
# Later, reuse the remembered reason:
tclaude agent profiles disable <name>
```

A disabled profile remains listed, editable, exportable, and referenced by
aliases, defaults, roles, templates, and process performers. Any spawn that
would use it fails with `profile_disabled` and the stored reason; `tclaude ask`
also refuses a disabled selected profile. Neither silently falls through to
another profile. `enable` clears only the disabled switch and reactivates every
existing reference while retaining the last reason for review or reuse; pass a
new `--reason` to replace it on a later disable. Profile writes require
`profiles.manage`.

### sandbox profiles

Sandbox profiles are operator-authored, harness-neutral bundles of filesystem
access rules, environment configuration, and optional agent-owned directory
declarations. Filesystem access accepts `read`,
`write`, or `deny`; deny blocks both reads and writes and dominates an
exact-path grant from any other applied profile. This lets an explicit
per-spawn profile subtract access inherited from a global or group profile.
They do not select a harness, model, or sandbox posture; those belong to spawn
profiles. Environment values
are stored and displayed as ordinary **non-secret configuration** — do not put
credentials in them. Profile payload reads and all mutations require the
`sandbox-profiles.manage` permission.

`agent_directories` is a JSON array of environment-variable names, for example
`["GOCACHE", "GOLANGCI_LINT_CACHE"]`. At spawn, agentd creates a fresh private
directory for each name under tclaude's cache tree, adds it to that agent's
writable sandbox paths, and injects the literal path as the variable's value.
The generated paths are frozen in the launch snapshot: resume and reincarnate
retain them, while a clone receives fresh directories. Retiring an agent
deletes all of its generated directory roots; reinstating and resuming that
agent recreates the declared directories empty at their frozen paths. A name
cannot also have a literal `environment` value, and the normal reserved-variable
rules apply.

By default the shared parent root is granted once, so the agent can create,
rewrite, and delete its own env-var'd directories. Setting
`features.agent_dirs_mount_parent` to `false` (in the config file or dashboard
Config tab) restores per-directory grants: the agent can write inside each
directory but cannot delete the directory itself because its parent is not
writable. The setting is read at each launch and resume; env-var values are
unchanged either way.
At an agent resume boundary, the ordinary global, launch-group, and explicit
profile values are resolved again from the current registry before the pane is
started; a running agent is never widened in place. If the launch group is
ambiguous or the changed policy cannot be represented by the preserved harness
sandbox, resume fails before launch with a `sandbox_profile_changed` recovery
message.

Deny rules are enforced by the harness OS sandbox (Claude `denyRead` plus
`denyWrite`, Codex permission-profile `none`). An effective deny requires
Claude sandbox `on` or the Codex managed `tclaude-agent` sandbox; other launch
postures are rejected rather than silently ignoring the restriction.

```bash
tclaude agent sandbox-profiles ls [--json]
tclaude agent sandbox-profiles show <name> [--json]
tclaude agent sandbox-profiles create --file profile.json
tclaude agent sandbox-profiles edit <name> --file profile.json
tclaude agent sandbox-profiles rm <name>

tclaude agent sandbox-profiles default show [--json]
tclaude agent sandbox-profiles default set <name>
tclaude agent sandbox-profiles default clear

tclaude agent sandbox-profiles group show <group> [--json]
tclaude agent sandbox-profiles group set <group> <name>
tclaude agent sandbox-profiles group clear <group>

tclaude agent sandbox-profiles export [name...] [--include-assignments] [--file bundle.json]
tclaude agent sandbox-profiles import --file bundle.json [--on-conflict error|skip|overwrite]
                                        [--apply-assignments] [--json]

# Draft-only dashboard handoff (normally invoked by the summoned sandbox scribe)
tclaude agent sandbox-profiles draft --token <dashboard-token> --file profile.json
```

`show --json` emits the same profile shape accepted by `create` and `edit`,
including `filesystem`, `environment`, and `agent_directories` arrays.
The names `export` and `import` are reserved for the portable-transfer routes
and are rejected case-insensitively at create, rename, and import boundaries.
Export bundles are portable and versioned. Assignment export is opt-in, and an
import only applies included global/group assignments when
`--apply-assignments` is explicitly passed; missing groups are reported as
warnings. Filesystem paths do not have to exist when profiles are created,
edited, or imported; they are retained in canonical lexical form after
existing ancestors and protected roots are checked. Missing paths remain valid
at spawn time. Missing read/write rules are inactive for that launch; on a
later launch, a path that has become an ordinary directory is revalidated and
its frozen rule becomes active. A missing deny target fails launch because the
restriction cannot safely be omitted. The dashboard editor can explicitly create missing read/write paths
with `mkdir -p` semantics; saving alone never mutates the filesystem, and
deny-only paths are never created. Creation
rejects symlink substitutions; on
macOS, existing ancestors must also be readable because the platform has no
search-only directory descriptor. Without an explicit profile, resolution
falls back from a group assignment to the global default.

The `draft` command is deliberately not a mutation. It requires only
`sandbox-profiles.draft`, runs the normal server validation, and hands the
structured proposal to the human dashboard. It cannot create or edit a saved
profile, change global/group assignments, or launch an agent; the human must
preview the result and explicitly save it through the ordinary editor.

### spawn

```bash
tclaude agent spawn <group> [--name N --role R --descr T --cwd DIR]
                            [--initial-message MSG | --file PATH] [--reply-to SEL]
                            [--worktree BRANCH [--worktree-base B] [--worktree-repo DIR]]
                            [--auto-focus] [--no-group-context] [--timeout DUR]
```

Launches a fresh detached CC session, waits for its conv-id to
materialise, and adds it to `<group>`. The new session lands in
`--cwd` (defaults to the caller's cwd, or the group's
[default dir](#groups)). Requires the `groups.spawn` permission
(human-only by default).

`--initial-message` (or `--file PATH` / `--file -` for stdin) delivers
the new agent a task brief in its inbox; `--reply-to` routes its reply
to a coordinator other than the spawner.

**Spawn into a git worktree.** `--worktree BRANCH` creates (or reuses) a
git worktree on `BRANCH` and spawns the agent into it — the CLI
equivalent of the dashboard spawn modal's worktree picker. The worktree
is cut in the repo containing `--cwd`; `--worktree-base` picks the
branch it forks from (default: the repo's default branch). For a
monorepo launch dir whose code work belongs in a nested sub-repo, point
`--worktree-repo` at the sub-repo: the agent then launches in `--cwd`
and the worktree path/branch ride into its welcome message. If the
spawn is rejected outright, a freshly-created worktree is removed again
(the branch is kept).

**Other parity flags.** `--auto-focus` opens a terminal window attached
to the new agent once it lands (off by default for the CLI — spawns are
usually programmatic — whereas the dashboard modal defaults it on).
`--no-group-context` opts the new agent out of the group's shared
startup context (delivered by default, like every other spawn path).

**How the agent is named + greeted.** For a Claude Code spawn the daemon
preps the conversation id, then launches the agent already named and
greeted: `claude --session-id <uuid> --name <name> "<welcome>"`. The name
becomes the conversation title (Claude Code records `--name` as a
custom-title turn, exactly as `/rename` does) and the welcome — an
`[system: …]` line orienting the agent (who it is, the group, the
`tclaude agent` commands) — is the agent's first turn. No `/rename` or
welcome keystrokes are injected over tmux, so there is no post-connect
delay. To revert Claude Code to the older flow — launch a bare `claude`,
poll for its conv-id, then inject `/rename` + the welcome over tmux — set
`agent.spawn_legacy_injection: true` in `~/.tclaude/config.json`.

**Name charset + auto-normalization.** A spawn name doubles as a git
worktree branch token and the conversation title, so it is restricted to
`[A-Za-z0-9_-]` (1–64 chars). By default a name straying outside that set is
**auto-normalized** rather than rejected — runs of spaces/punctuation/unicode
collapse to a single `-` and the leading/trailing `-` that produces are
trimmed (a `_` you typed is kept), so `--name "code reviewer!"` lands as
`code-reviewer`. This applies uniformly to
`tclaude agent spawn`, `--join-group`, and the dashboard's spawn modal (which
previews the normalized name as you type). Set
`agent.spawn_name_normalize: false` in `~/.tclaude/config.json` (or untick
*Normalize spawn names* on the dashboard's Config tab) to restore the strict
behaviour, where an out-of-charset name is rejected. An empty name is always
valid — the agent gets an auto-generated label.

For a **Codex** spawn the daemon can't preset the conv-id (Codex generates
it at its first turn), but Codex must self-submit *some* first-turn prompt
for that id — and its on-disk history — to materialise at all. So that
required seed *is* the `[system: …]` welcome: the same greeting, delivered
as Codex's launch prompt rather than a separate post-connect injection. The
name is applied out-of-band (Codex has no `/rename`; the daemon writes
`threads.title`). The result is a single greeting turn that mirrors Claude
Code's — no inert placeholder seed, no second welcome message.

**Inlined briefings.** The startup briefing (group context + task brief)
is always saved to the new agent's inbox as its first message. When it's
short, it's *also* inlined into the launch prompt right after the welcome
— a real shell-quoted argv positional, so multi-line briefs ride along
unescaped and intact (unlike the legacy send-keys path, where a newline
would submit early) — so the agent acts on its first turn without a
`tclaude agent inbox read <id>` round-trip. A longer briefing keeps the
welcome that points at the inbox copy, so it stays scrollable there and
doesn't balloon the launch command. The cutoff is
`agent.spawn_inline_max_chars` (runes; default 2000, `0` disables inlining
so every spawn uses the inbox pointer).

An *inlined* briefing's inbox copy is marked **read** at spawn — the agent
already received its full text on its first turn, so the copy is just an
archival duplicate and is kept out of the dashboard's unread list. A briefing
that stays a pointer (over the cap, inlining disabled, or the legacy
post-connect path) is left **unread**, because the agent still has to open it
from the inbox.

This works for both harnesses, with one wrinkle from the conv-id timing.
Claude Code presets the conv-id, so its inbox briefing exists before launch
and the welcome can reference it by message id either way (inline + id note,
or pointer-with-id). Codex creates the inbox briefing only *after* it
connects, so at launch there is no message id yet: a short briefing inlines
into the seed in full (the agent needs nothing more), while a long one gets
a stand-by seed and its inbox-pointer welcome is injected post-connect, once
the inbox row exists. The legacy Claude-Code `spawn_legacy_injection` revert
always uses the post-connect inbox pointer.

**Spawn guardrails.** `groups.spawn` is human-only by default, but the
human can grant it to a coordinator agent so it can grow its own team.
To keep a spawn-capable agent from running away (a recursive spawn
explosion) or minting a less-confined child, an **agent** caller is
bound by five checks — a human
bypasses the agent-only ones, exactly as humans bypass every other
permission gate:

| Guardrail | Default | Refusal |
|-----------|---------|---------|
| **Group restriction** — an agent may only spawn into a group it is a member or owner of | on | `403 group_restricted` |
| **Rate limit** — spawns per caller-agent per rolling hour | 10 | `429 rate_limited` |
| **Sandbox lineage** — the child may not have a weaker launch sandbox than the spawning agent | on | `403 sandbox_restricted` |
| **Dir write-proof** — the caller must prove its own sandbox can write in the child's launch dirs | on | `403 write_proof_required` / `403 write_proof_failed` |
| **Max group size** — `agent_groups.max_members`; binds the human too | unlimited (0) | `409 group_full` |

The first two are tuned in `~/.tclaude/config.json` under `agent`
(`spawn_group_restriction`, `spawn_allowed_groups`, `spawn_max_per_hour`);
the member cap is a per-group property — `groups set-max-members`, or the
👥 chip on the dashboard's Groups tab. See [Permission model](#permission-model).

The sandbox lineage guard compares the spawning agent's recorded harness /
sandbox mode with the fully resolved child launch shape, after any group
default spawn profile has filled blank fields. In short:

- Claude `off` or Codex `danger-full-access` parents can spawn any sandbox.
- Claude `inherit`/`on` parents can spawn sandboxed Codex children
  (`tclaude-agent`, `workspace-write`, `read-only`) but not
  `danger-full-access`; `inherit` may spawn Claude `inherit`/`on`, while
  `on` may spawn Claude `on`.
- Codex `tclaude-agent` parents can spawn Codex children up to
  `tclaude-agent`, and Claude `inherit`/`on` children.
- Raw Codex `workspace-write` parents can spawn only raw Codex
  `workspace-write` or `read-only`; raw Codex `read-only` parents can spawn
  only raw Codex `read-only`.

The **dir write-proof** closes the lineage guard's remaining gap: sandboxes
grant write access rooted at the launch cwd, so an agent that picks the
child's launch directory picks where that write access lands — without a
check, a parent whose sandbox cannot touch a directory could spawn a child
into it and use the child as its writable proxy. A sandboxed agent caller
must therefore prove it can itself write in every directory the child would
get write access to (the launch cwd, an explicit worktree, and—when Git-backed
sandbox support applies—the minimal repository root covering the sibling
container, original/main worktree, and shared Git metadata, plus the checkout's
exact Git admin directory): the daemon answers
the first request with a `403 write_proof_required` challenge naming a single-use token, the caller
creates an empty file named `.tclaude-write-proof-<token>` in each listed
directory, and retries the same request with `write_proof_token` set; the
daemon verifies the files and pins the request to the resolved paths. The
launch wrapper then checks the cwd marker from inside the tmux pane after
tmux has established the pane's cwd inode, and agentd re-asserts the verified
paths immediately before every fork. That combination catches both symlink
retargeting and real-directory swaps between HTTP validation and launch.
`tclaude agent spawn` runs the handshake automatically — inside the caller's
own sandbox, which is exactly the capability being proven — so a permitted
spawn just works, and a forbidden one fails with a clear "cannot prove write
access" error. Humans, fully-open parents (Claude `off` / Codex
`danger-full-access`), and Codex `read-only` children (no cwd write to prove)
are exempt. The same handshake guards a clone's `cwd` override and the
template spawn surfaces (`instantiate` / `deploy` / `reinforce` — the whole
cast shares one proven launch cwd, plus any shared worktree and the
per-agent-worktree repo); the matching `tclaude agent templates …` /
`task-force` CLIs answer it transparently too. Template requests from agents
that create per-agent worktrees during the request fail closed when those new
checkout admin dirs were not part of the caller's proof; a human can launch
that topology without the agent-to-agent authority constraint. Separately,
agent-originated Codex spawns may pre-trust only a verified default sibling worktree; those worktrees are
always trusted automatically so a detached child cannot stop at Codex's
trust-folder modal. Other agent-selected paths remain forbidden. All extra
repository write grants are resolved, proved, and pinned before launch rather
than recomputed from a mutable cwd; Codex consumes them through its managed
profile and Claude Code through merged `sandbox.filesystem.allowWrite` paths.
Agent-triggered clone, reincarnate, and resume operations prove the inherited
child cwd plus every repository path added by the current sandbox profile. The
forked session binds the cwd marker after tmux changes directory and checks the
same marker in every pinned root immediately before starting the harness.
Recreating a missing resume cwd remains human-only because the daemon cannot
prove an agent can write inside a path that does not yet exist.

### clone / reincarnate / compact / context-info

Lifecycle commands. By default they target the calling agent itself;
`--target <selector>` retargets another agent (the **manager
pattern**, gated on the `agent.*` slug or group ownership).

For context pressure, reincarnation is primarily a Claude Code tool because
its compaction is comparatively slow and lossy. Codex CLI has effective,
efficient automatic compaction: normally let a Codex agent run to full context
and auto-compact. Do not reincarnate a Codex agent merely to free context space;
an explicit human request or another deliberate replacement reason can still
justify reincarnating either harness.

```bash
tclaude agent clone [follow-up]              # fork a sibling; the original keeps running
tclaude agent clone --no-copy-conv           # clone with a blank context instead of the copied jsonl
tclaude agent reincarnate "<follow-up>"      # replace self with a fresh successor (follow-up REQUIRED)
tclaude agent compact [follow-up]            # inject /compact into the pane
tclaude agent context-info                   # show this conversation's context-window state (read-only)
tclaude agent context-info --target <sel>    # read ANOTHER agent's context (gated: agent.context-info or group-owner)
tclaude agent context-info --group <name|id> # one table of every group member's context % (gated likewise)
```

A **clone** inherits identity (group memberships, per-conv grants,
ownerships) and, by default, a copy of the conversation jsonl — the
original stays alive, and the clone is renamed to a `-c-<N>` title
suffix. A **reincarnate**
migrates identity onto a fresh conv-id and soft-stops the old one; the
follow-up prompt is mandatory so the successor isn't left idle.

### remote-control

Arm Claude Code's built-in Remote Access (claude.ai/code + the mobile
app) so the agent can be driven from your phone. Defaults to the calling
agent (`self.remote-control`); `--target <selector>` retargets another
(gated on `agent.remote-control` or group ownership).

```bash
tclaude agent remote-control            # toggle (default intent)
tclaude agent remote-control on|off|status
tclaude agent remote-control on --target worker-3
```

Re-arming survives resume / reincarnate / clone, and a group or spawn
profile can default it on. Claude Code only (Codex has no remote access).
See **[Remote Control](remote-control.md)** for the full guide, the
claude.ai-login prerequisite, and the best-known-state caveat.

### stop / resume / dir

```bash
tclaude agent stop <selector> [--force]      # soft /exit, or kill-session with --force
tclaude agent resume <selector>              # bring an offline agent back into a tmux pane
tclaude agent dir [selector]                 # print an agent's working directory
tclaude agent dir --worktree                 # git worktree/repo root instead
tclaude agent dir --start                     # the launch directory instead
tclaude agent dir --open                      # open a terminal there (via the daemon)
```

`stop` / `resume` are idempotent — already-offline / already-online
agents come back as `skipped:...`. They are the single-conv variants
of `groups stop` / `groups resume`, and require `agent.stop` /
`agent.resume` (or group ownership) when targeting another agent.

### cron

Recurring scheduled nudges. The daemon's scheduler ticks every 30s and
fires due jobs by delivering a message (or a direct keystroke for solo
targets).

```bash
tclaude agent cron add --interval 10m --body "status check?" [--target SEL --name N]
tclaude agent cron add --cron "0 9 * * 1-5" --body "morning standup"   # cron expression instead of interval
tclaude agent cron ls
tclaude agent cron disable <id>      # pause without deleting
tclaude agent cron enable <id>
tclaude agent cron run-now <id>      # fire immediately
tclaude agent cron logs <id>         # recent execution history
tclaude agent cron rm <id>
```

Cron jobs default to self-targeted; `--target group:<name>`
multicasts. Managing your own jobs needs `self.schedule`; managing
another agent's needs `agent.schedule` (or group ownership).

### permissions / sudo

```bash
tclaude agent permissions slugs                          # registry of known slugs + descriptions
tclaude agent permissions ls [<conv|title|default>]      # defaults + grants, or effective set for one agent
tclaude agent permissions grant <conv|title|default> <slug>
tclaude agent permissions revoke <conv|title|default> <slug>
```

`sudo` requests a **bundle of slugs for a bounded duration** (capped
at 1h). The request triggers a human-approval popup; on approve, the
slugs join the agent's effective set until the window expires or a
human revokes early.

```bash
tclaude agent sudo request <slug>... [--duration 5m --reason "..."]
tclaude agent sudo ls [--all]
tclaude agent sudo revoke <id>
```

The human can also `sudo request --target <conv>` to grant an
elevation proactively (no popup — the shell *is* the consent).
`permissions.grant` / `permissions.revoke` are blocklisted from sudo
by design; a permanent grant is the only way to hand out those.

See [Permission model](#permission-model) for the full picture.

### rename / alias

```bash
tclaude agent rename "<title>"           # rename a conversation (self, or --target another)
tclaude agent alias set <handle> <conv>  # anchor a global head alias to a conv
tclaude agent alias ls / get / rm
```

`rename` injects `/rename <title>` into the target's CC pane via
`tmux send-keys`; gated on `self.rename` (self) or `agent.rename`
(another). The title charset is strict
(`[A-Za-z0-9_\-\[\]{}() ]`, single ASCII spaces, max 64 chars) — a
hard security gate, because the title becomes literal keystroke
input.

An agent has exactly one name: its conversation title, set with
`rename`. That title is what selectors resolve and what `ls` /
`groups members` display.

A **head alias** is a stable, daemon-wide handle (`po`, `ceo`, …) that
always resolves to the live head of a conv chain — it survives
arbitrary reincarnation depth without re-pointing. Unlike a title,
which moves with each rename and each reincarnation, a head alias is
the fixed handle you anchor once and keep using.

### delete

```bash
tclaude agent delete <selector> [--force] [--yes]
```

Permanently wipes every row referencing the conv-id (agent / conv /
cron / succession / session tables), the `.jsonl` file, and the
session-env token. Refuses while the target's tmux session is alive
unless `--force`. Requires `agent.delete` (not default-granted) or
group ownership; self-delete is refused — use `tclaude conv rm`.

### dashboard

```bash
tclaude agent dashboard               # open the browser dashboard
tclaude agent dashboard --print       # print the one-shot URL only
```

A browser operations console for everything above. See the
[Agent Dashboard](dashboard.md) page for the full tour — tabs, auth,
spawning, and drag-and-drop group editing.

## Task forces (CLI)

The [dashboard's Task forces journey](dashboard.md#task-forces) — author a
template, deploy it against a mission, steer the live force, wind it down — has
a full CLI twin. Every verb below is a thin client over the same `agentd`
endpoints the dashboard drives.

### Concepts: pattern, process & rhythms

A template carries three things that shape a deployed force, and they work
**together**:

- a **work pattern** — an ordered list of routed briefing messages delivered
  **once**, after the whole roster has spawned (`task-force deploy` delivers it;
  [`groups rebrief`](#groups-rebrief) re-delivers the template's *current*
  pattern to the live force on demand). It is a one-shot briefing, not a loop.
- a **process** — an ordered, **advisory** phase plan the force advances through
  (`task-force status` shows the current phase; `process advance` records a
  transition and nudges the entering roles). Advisory means exactly that:
  advancing blocks nothing, changes no permissions, and never auto-advances.
- **rhythms** — recurring nudges materialized as group cron jobs **at deploy
  time** (a snapshot). Editing the template afterwards does **not** retune a
  force already deployed. `task-force stand-down` deletes them; a `groups
  retire` that empties the group disables them (reversible via `groups resume`).

| Concept | Delivered | Repeats? | Enforced? | On stand-down |
|---|---|---|---|---|
| Work pattern | once, after the roster is up | no — `rebrief` re-sends on demand | no — it is a briefing | already delivered (nothing to sweep) |
| Process | snapshot at deploy | `process advance` by hand | no — advisory | phase history kept |
| Rhythms | cron jobs at deploy | yes, on a schedule | no — nudges | cron jobs deleted |

The verbs that drive each are below; the dashboard's
[Concepts](dashboard.md#concepts-pattern-process--rhythms) note covers the same
model from the UI side.

### templates

Manage reusable team blueprints. `ls` / `show` are open reads; `create` /
`edit` / `rm` / `from-group` need `templates.manage`; `instantiate` needs
`templates.instantiate` (both effectively human-only by default).

```bash
tclaude agent templates ls
tclaude agent templates show <name>                       # human-readable view
tclaude agent templates show <name> --json                # raw wire JSON (round-trips)
tclaude agent templates create --file <path>              # create from a JSON file ('-' = stdin)
tclaude agent templates edit <name> --file <path>         # full replace (not a field merge)
tclaude agent templates rm <name>                         # delete the blueprint; deployed groups untouched
tclaude agent templates instantiate <name> --group <g> [--task T | --task-file F] [--cwd DIR --descr D]
tclaude agent templates from-group <group> <template-name> [--update]   # snapshot a live group
tclaude agent templates export <name> [--file F]          # portable .task-force.json (stdout by default)
tclaude agent templates import --file <path> [--as NAME | --update]     # read one back
```

A template is structured (nested agents with multi-line briefs, an optional
work pattern, process, waves, and rhythms), so it is authored as JSON rather
than via flags. The **edit loop** round-trips through `show --json`:

```bash
tclaude agent templates show feature-team --json > ft.json
$EDITOR ft.json
tclaude agent templates edit feature-team --file ft.json
```

Set top-level `"per_agent_worktrees": true` to default the dashboard's
**Give each agent its own worktree** deploy option on for that template.
It remains a per-spawn choice: the deploy dialog is merely pre-filled, and the
human can turn it off (or enable it on a template whose default is false)
without changing the stored template.

**Agentic template editing (a "scribe" agent).** These are ordinary
permission-gated endpoints, so an agent can drive the whole edit loop by chat —
no dashboard required. Grant bundle:

- **`templates.manage`** — the one slug a scribe needs: `create`, `edit`, `rm`,
  `from-group`, and `import`. This is the whole job for authoring/editing
  circles.
- **`templates.instantiate`** — only if the scribe should also *spawn* whole
  teams (`instantiate` / `deploy`). Strictly more powerful, so usually left to
  the human.
- **`roles.manage`** / **`profiles.manage`** — only when the edit must also
  *create or change* the shared [role library](#roles) or spawn
  profiles a template references. A template merely *referencing* an existing
  role/profile by name needs neither.
- **Reads are open** — `ls`, `show`, and `export` need no grant, so a scribe can
  always discover and inspect circles.

Validation errors from `create`/`edit` are written to be actionable for an LLM:
each names the offending field (`role_ref`, `spawn_profile`, a `work_pattern`
`send_to`, a permission slug, …) and lists what *is* allowed. The bundled
**`agent-circles`** skill teaches an agent this loop, the full JSON wire shape,
and the wizard-mode vocabulary a human may speak; install it (with the other
agent skills) via `tclaude setup --install-agent-skills`.

**Scribe launch profile.** By default a summoned scribe launches on the
harness default (Claude Code at its default model/effort). To run scribes on a
different harness/model — e.g. Codex, or a cheaper model for their light
editing — set `scribe.profile` in `~/.tclaude/config.json` (or pick it from the
dashboard **Config tab → Scribe defaults**) to the name of a saved [spawn
profile](#roles); each fresh summon adopts that profile's whole launch
shape, and the harness-matched dir-trust pre-seed follows it automatically.
Resolved live at summon time — a deleted or renamed profile self-heals to the
default rather than wedging the summon. Every click creates an independently
named scribe, so live scribes keep working while the next one uses the current
profile.
Blank/absent = today's default. This mirrors the `ask.profile` knob `tclaude
ask` uses.

`from-group` bootstraps a template from a running group's structure (roles,
owners, per-agent permission grants, context); per-agent task briefs come
through blank (a live group stores none), so fill them in with `edit`. With
`--update` it re-snapshots into an existing template in place, keeping the
curated briefs of agents that round-trip by name. `export` / `import` share a
task-force blueprint between machines — the export **embeds the full definition
of every role and spawn profile the template references**, and import
materializes those only if they are missing locally (an existing role/profile of
the same name is kept, never overwritten). A name collision on the *template*
itself is an error unless you pass `--as <name>` (store under a new name) or
`--update` (overwrite in place); a spawn-profile reference that still can't be
resolved, and any unknown permission slug, degrade to a warning rather than
failing the import. See
[Sharing task forces](dashboard.md#sharing-task-forces-as-a-file).

**Starters** are the bundled, ready-to-run templates (a dev squad, a research
pod, a review crew):

```bash
tclaude agent templates starters ls
tclaude agent templates starters show <name> [--json]
tclaude agent templates starters install <name> [--as NAME]
```

Install stores a starter as an ordinary local template you can then deploy or
edit. It is idempotent and **never clobbers**: if a template of the target name
already exists the install is skipped (your edited copy is sacred) — pass `--as`
to install a fresh copy. See [Starter task forces](dashboard.md#starter-task-forces).

### roles

Manage the [role library](dashboard.md#roles-library) — named, reusable agent
defaults a template agent references via its `role_ref` field. `ls` / `show`
are open; writes need `roles.manage` (effectively human-only). Like templates, a
role carries a multi-line brief, so it is authored as JSON via `--file`. `show`
without `--json` prints the role's brief, launch shape and permission slugs — so
you can see at a glance what picking the role implies (the same transparency the
dashboard role picker surfaces inline):

```bash
tclaude agent roles ls
tclaude agent roles show <name> [--json]
tclaude agent roles create --file <path>          # {name, descr, brief, spawn_profile, harness, model, effort, sandbox, approval, permissions}
tclaude agent roles edit <name> --file <path>     # full replace
tclaude agent roles rm <name>                     # refused while a template still references the role; a deleted seed reappears on the next daemon open
```

Roles resolve at **deploy time**: editing a role changes what *future* deploys
of a referencing template inherit; already-deployed agents are untouched. Because
a live reference matters, `rm` is refused while any template still names the role
(the error lists them) — edit those templates to drop or repoint the reference
first.

### task-force deploy

Deploy a whole team against a mission — the mission-framed twin of `templates
instantiate`. Gated on `templates.instantiate`.

```bash
tclaude agent task-force deploy <template> --mission "<text>" [--group G] [--descr D]
tclaude agent task-force deploy <template> --mission-file <path>          # long / multi-line mission ('-' = stdin)
tclaude agent task-force deploy <template> --mission "<text>" --worktree <branch> [--worktree-base B]
```

`deploy` creates a fresh group, folds `--mission` into its shared context under
`## Mission`, spawns the roster (staged by wave), materializes the template's
rhythms as group cron jobs, seeds the process, and delivers the work pattern
once the roster is whole. With no `--group` the group name is derived from the
mission (a bare-URL mission falls back to the template name). `--worktree` lands
the whole force on its own branch in a git worktree, which becomes its working
directory.

### task-force ls

List the deployed forces — the groups a `deploy` created, carrying a mission and
a source template. A plain hand-built group (no source template) is **not** a
force and is not listed; `groups ls` covers those. Open read.

```bash
tclaude agent task-force ls           # aligned table
tclaude agent task-force ls --json    # composed rows (round-trips)
```

Each row shows the group name, its **mission** (truncated), the **source
template**, the current process **phase** (if any), the number of pending
staged-spawn **waves**, and **live/total** members. A force with no live members
is flagged dormant (`⏸`) — stood-down or otherwise idle. The verb is a thin
composition of the group reads the dashboard force block uses (the groups list
plus each force's process and waves reads).

### task-force status

The **CLI twin of the dashboard [force block](dashboard.md#the-force-block)** —
a single force's full read-back. Open read; the per-member liveness rollup rides
the group-context read (the human operator and group owners always pass, an agent
without context access sees the rest and a note in place of the rollup). The
group is inferred when you are in exactly one group.

```bash
tclaude agent task-force status <group>          # human-first block
tclaude agent task-force status <group> --json   # composed status (round-trips)
```

It shows the **mission + provenance** (source template), the process **phase +
phase map + recent transitions** (the same read `process show` uses), a
**per-role liveness rollup** (working `●` / idle `○` / offline `✕`, with context
`%` where the member snapshot carries it), any pending **waves**, and the group's
**rhythm** cron jobs (name, schedule, enabled/disabled). The liveness
classification matches the dashboard's exactly, so the two surfaces never
disagree about who is stalling: offline is dead, an online agent is *idle* only
when its status is literally idle, and anything else in flight is *working*. A
disabled rhythm distinguishes a tclaude-auto-paused job — `disabled (auto:
group-retired)`, from a `groups retire` that emptied the group — from a
hand-paused one. A **stood-down** force still renders (mission, provenance and
phase history survive) and reads as *dormant*. Running `status` on a plain group
(no source template) is refused with a pointer to `groups ls`, consistent with
`ls`'s force filter.

### task-force stand-down

Wind a force down — the **mirror of `deploy`**. It retires the whole roster and
sweeps (deletes) the deploy-seeded runtime — the group-target rhythm cron jobs
and any pending wave choreography — while **keeping the group row** as a dormant
record (mission, provenance, and process history preserved). It is deliberately
*not* a group delete (`groups rm` does that). Gated on the human, group owners,
or `groups.retire`.

```bash
tclaude agent task-force stand-down <group> [--no-shutdown] [--reason "<why>"] [--ask-human <duration>]
```

By default each live member's running pane is soft-exited (sends `/exit`); pass
`--no-shutdown` to leave the processes running. The caller's own conversation is
always skipped (an agent never retires itself). Standing down a plain group (no
template) simply retires its members — there is nothing to sweep. The command
prints the per-member retire table plus a sweep summary (e.g. *"2 rhythm job(s)
removed, 1 pending wave(s) cancelled"*).

> A plain **`groups retire`** that leaves a group with no live members instead
> **disables** (not deletes) its rhythms — reversible via `groups resume`. Use
> `stand-down` when you want the deploy-seeded runtime *gone*, `groups retire`
> when you may bring the force back. See
> [Winding a force down](dashboard.md#winding-a-force-down).

### process show / advance

Inspect or advance a deployed force's advisory [process](dashboard.md#steering-a-force).
`--group` is inferred when you are in exactly one group. `show` is open;
`advance` is gated on the human, group owners of the group, or the
`process.advance` slug.

```bash
tclaude agent process show [--group <name>]
tclaude agent process advance [--group <name>] [--to <phase>]   # next phase, or a named one for correction
```

The process is **advisory** — advancing records the transition and nudges the
roles active in the phase it enters, but enforces nothing.

### process-templates

Author process-template YAML through the same agentd REST handlers and
content-addressed store used by the dashboard editor:

```bash
tclaude agent process-templates ls
tclaude agent process-templates show <id>
tclaude agent process-templates validate --file <template.yaml>
tclaude agent process-templates save --file <template.yaml> [--expect-source-hash <hash>] [--ask-human 30s]
```

`ls`, `show`, and `validate` require `process.templates.read` (included by the
optional default-permission installer). `save` independently requires the
non-default `process.templates.manage`. Existing-template saves use the
`sourceHash` emitted in `show`; a stale hash is a conflict that must be resolved
by re-showing and merging, never by blind overwrite. Saving records the socket
peer's stable actor identity and has no execution or instantiation side effect.

### groups rebrief

Re-deliver a deployed force's **current** work pattern to its live members, with
the group's recorded mission interpolated. Gated on the human, group owners, or
the **`templates.instantiate`** slug.

```bash
tclaude agent groups rebrief <group> [--ask-human <duration>]
```

A group with no source template, a deleted template, or a template with no work
pattern is refused cleanly — nothing is sent. See
[Steering a force](dashboard.md#steering-a-force) for when to reach for it.

## Permission model

Every mutating action is gated by a *permission slug*. Once the daemon
has [classified the caller](#identity), it decides:

1. **Human?** Pass — the human bypasses every gate.
2. **Agent?** Allowed iff the slug is in `default_permissions`
   (global), a live grant from any active group it belongs to, the agent's
   per-conv grants (SQLite), or an active
   `sudo` elevation. **Group-owner state** raises an owner's default
   slugs: owning a group confers, for that group, the `agent.*`
   manager-pattern checks against its members, the group-lifecycle
   verbs (`groups.spawn` / `groups.stop` / `groups.retire` /
   `groups.resume`), and `human.notify` (owning any group). These owner
   defaults fill only the *undecided* gap — an explicit **deny** override
   is always authoritative and suppresses them, read or write.
3. **Neither?** Refused fail-closed — see [Identity](#identity).

### Storage split

| Where                                     | What                          | How to edit               |
|-------------------------------------------|-------------------------------|----------------------------|
| `~/.tclaude/config.json` → `agent.default_permissions` | Slugs granted to **every** agent | hand-edit, or `permissions grant default <slug>` |
| SQLite `agent_group_permissions` table    | Live additive grants for every current member of one active group | Groups tab → group ⚙ → **group permissions…** |
| SQLite `agent_permissions` table          | Per-conv grants (additive on top of defaults) | `permissions grant <conv> <slug>` (writes the DB row) |
| SQLite sudo-elevation table               | Time-bounded grants from `sudo` | `sudo request` / `sudo revoke` |

An agent's effective permission set is
`union(defaults, active-group grants, agent grants, active sudo elevations)`.
Group grants are membership policy, not spawn configuration: they take effect
immediately for existing members, disappear on leaving or archiving the group,
and are not copied into the agent's own override rows. An explicit per-agent
**deny** remains authoritative over defaults and group grants; sudo is the
time-bounded exception above it. Membership in multiple groups unions their
grants. Nested groups do not inherit membership or permissions.

### Slugs

Slugs are grouped by family. `self.*` acts on the calling agent;
`agent.*` is the manager pattern (act on another agent); the rest
gate group, messaging, template, and permission administration.

| Family        | Slugs |
|---------------|-------|
| `self.*`      | `self.rename`, `self.compact`, `self.clone`, `self.schedule`, `self.remote-control` |
| `agent.*`     | `agent.rename`, `agent.compact`, `agent.reincarnate`, `agent.clone`, `agent.context-info`, `agent.resume`, `agent.stop`, `agent.delete`, `agent.schedule`, `agent.promote`, `agent.retire`, `agent.remote-control` |
| `groups.*`    | `groups.create`, `groups.rm`, `groups.archive`, `groups.stop`, `groups.resume`, `groups.retire`, `groups.spawn`, `groups.own`, `groups.link.add`, `groups.link.rm`, `groups.export`, `groups.import` |
| `member.*`    | `member.add`, `member.remove`, `member.redesignate` |
| `permissions.*` | `permissions.grant`, `permissions.revoke` |
| `message.*`   | `message.direct` |
| `templates.*` | `templates.manage`, `templates.instantiate` |
| `process.templates.*` | `process.templates.read`, `process.templates.manage` |
| `human.*`     | `human.notify`, `human.clipboard` |

Run `tclaude agent permissions slugs` for the live registry with
descriptions — it is the source of truth; this table can drift.

### Ad-hoc human approval (`--ask-human`)

Most mutating commands take `--ask-human <duration>` (e.g. `30s`,
`2m`, or a bare integer for seconds; capped at 300s). On permission
denial, the daemon creates an access request in the dashboard Messages
tab with **Approve / Deny / +5min** buttons:

```bash
tclaude agent groups create foo --ask-human 30s
# → CLI prints "Waiting up to 30s for human approval..."
# → dashboard Messages → Access requests shows the requester / target / body
# → human clicks Approve → CLI proceeds
# → human clicks Deny or timeout fires → CLI fails with 403
```

**Timeout = Deny** so an unattended request never silently grants. The
request surface is authenticated by the same init-token exchange the
[dashboard](dashboard.md#auth) uses. By default no browser is opened and
no OS banner is sent; opt into those extra alerts with
`agent.access_request_auto_open_browser` and
`agent.access_request_system_notification`.

**Always allow for this agent.** For a small allowlist of low-blast-radius
slugs (today `human.clipboard` and `human.notify`), the access-request card shows a
fourth button — **Always allow for this agent**. It approves the pending
request *and* persists an allow override, so the agent's future calls skip
the approval request entirely. The grant is keyed on the agent's **stable identity**
(agent-id), so it follows the agent through `/clear` conv-rotation and
reincarnation — "this agent", not "this one conversation". It shows up in
the [dashboard](dashboard.md) permission editor as a normal allow override
(granted-by `human:popup-always`) and can be revoked there or with
`tclaude agent permissions revoke <agent> <slug>`. A **deny** override
still beats it — deny is always authoritative. The button is rendered (and
its action accepted) **only** for eligible slugs; destructive or
fleet-affecting slugs (e.g. `agent.delete`, `groups.rm`) never offer it.

For a *bundle* of slugs over a *window* of time rather than one
command, use [`sudo`](#permissions--sudo) instead.

### Recursion

`permissions.grant` and `permissions.revoke` are themselves slugs, so
`permissions grant` / `revoke` are gated like every other mutator: the
human can run them by default; an agent can only run them if it holds
the slug, or the human approves via `--ask-human`. Granting
`permissions.grant` to an agent makes it a co-administrator — handle
with care.

## Bundled skills

The bundled skills ship with the binary and install to `~/.claude/skills/`
for Claude Code, plus both `~/.agents/skills/` and `$CODEX_HOME/skills`
(default `~/.codex/skills`) for Codex CLI, via
`tclaude setup --install-agent-skills`:

- **`agent-coord`** — the day-to-day "talk to other agents" skill.
  Triggered by `[system: new agent message #...]` nudges and by user
  prompts asking the agent to coordinate.
- **`agent-rename`** — split out as its own skill so renames are
  obvious in the skill list.
- **`agent-task`** — set / clear / show an agent's task-reference link
  (the dashboard's Task column). The human operator can also click any
  agent's Task cell to attach, change, or clear its URL and optional display
  name; a blank display name derives a short label automatically. Existing
  short labels remain normal links; hovering or keyboard-focusing the Task
  cell reveals its edit pencil without widening the column.
- **`present-pr-to-operator`** — present a PR intentionally in the
  dashboard with `tclaude agent present-pr <url>`.
- **`agent-lifecycle`** — context-window self-management: `compact`,
  `reincarnate`, `clone`, `context-info`, including why context-driven
  reincarnation is for Claude Code while Codex should normally auto-compact.
- **`reincarnate`** — the do-it-now sibling of `agent-lifecycle`:
  invocable as `/reincarnate`, it carries the checkpoint-then-hand-off
  procedure (write a handoff notes file, run
  `tclaude agent reincarnate --file …`). `agent-lifecycle` stays the
  full reference.
- **`agent-dir`** — report or open a terminal in an agent's working
  directory.
- **`agent-schedule`** — set up and manage recurring `cron` nudges.
- **`agent-remote-control`** — toggle Claude Code Remote Access
  (claude.ai/code + the Claude mobile app) on/off via
  `tclaude agent remote-control`; Claude-Code-only.
- **`agent-circles`** — author and edit group templates ("summoning
  circles" / task forces): the JSON wire shape, the safe
  show-json → edit → edit-file round-trip, the scribe grant bundle, and
  the wizard-mode vocabulary. See [templates](#templates).
- **`process-templates`** — generate and safely CAS-edit process-template YAML
  through agentd, including the full node/performer shape, conflict etiquette,
  and preservation of editor-owned layout.
- **`human-notify`** — send the human a notification via
  `tclaude agent notify-human`; it lands in the dashboard
  [Messages tab](dashboard.md#messages). Add repeatable `--attach <path>` flags
  to publish generated files or directories as a downloadable artifact.
- **`human-clipboard`** — copy text to the human's system clipboard via
  `tclaude agent clipboard`; the daemon runs the platform copy tool on
  the host. Gated on `human.clipboard` (explicit grant or `--ask-human`
  popup; not owner-implied).

Re-run `tclaude setup --install-agent-skills` after `go install
…@latest` to refresh the on-disk copies with whatever the new binary
embeds.

## Design notes

- The daemon is **foreground-only**. Run it in a tmux pane / a long-
  running terminal; restart manually after upgrades. (No launchd /
  systemd unit yet.)
- Identity is resolved from the **socket peer** plus, for the human
  operator, a per-daemon-lifetime **operator token**. By default it is
  held in memory only — never persisted to disk — and a daemon restart
  mints a fresh one. Opting in (`--persist-operator-token` /
  `agent.persist_operator_token`) makes it stable across restarts,
  stored in the OS keychain or a `0600 ~/.tclaude/operator_token` file;
  see [The operator token](#the-operator-token).
- `agent_messages` rows accumulate forever for now (no auto-prune);
  bodies are short, so this is fine for a long while.
- Access requests and the dashboard share the daemon's loopback port;
  closing the daemon closes both surfaces.

## Troubleshooting

| Symptom                                                                 | Fix                                                             |
|--------------------------------------------------------------------------|-----------------------------------------------------------------|
| `Error: tclaude agentd is not running.`                                  | Start it: `tclaude agentd serve` (in a non-sandboxed shell).    |
| `Error: not in a shared group with target`                               | Add both convs to the same group, or add an inter-group link.   |
| `Error: selector matches multiple conversations`                         | Use the 8-char conv-id prefix instead of the title.            |
| `Error: caller is not granted permission "<slug>"`                       | Grant via `permissions grant`, retry with `--ask-human`, or `sudo request`. |
| `Error: unconfirmed caller: not a known agent, and no valid operator token` | You're the human: `export TCLAUDE_HUMAN_TOKEN=…` from the agentd startup banner. See [Identity](#identity). |
| Dashboard shows `403` on `GET /`                                         | Open it via `tclaude agent dashboard` — the cookie is only issued by the init-token exchange. |
| Access request did not open a browser                                    | Browser auto-open is off by default. Set `agent.access_request_auto_open_browser` to `true` if you want that extra alert. |
| `no_tmux` 503 on `agent rename`                                          | Caller has no live tmux session for the daemon to inject into.  |

## See also

- [Agent Dashboard](dashboard.md) — the browser operations console.
