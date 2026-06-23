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
a Claude Code session is an *agent*, identified by its conv-id (re-read
on every request, so `/fork` and `/resume` keep working); the *human operator*
authenticates with an operator token. A caller it can confirm as
neither is refused. See [Identity](#identity).

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
- **Schedule recurring nudges.** Cron jobs deliver a message to an
  agent (or a whole group) on an interval.
- **Delegate capabilities.** Per-agent permissions live in SQLite;
  you (the human) decide which agents can rename themselves, manage
  group membership, etc. Agents can request one-off escalation via a
  browser approval popup, or a bounded `sudo` elevation.
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
  `self.compact`, `self.reincarnate`, `self.clone`, `self.schedule`,
  `self.remote-control`) as agent defaults. Idempotent; only adds missing slugs.
- **`tclaude agentd serve`** — running in a non-sandboxed shell. The
  CLI refuses to fall back to direct DB access when the daemon is
  down — that's deliberate, so the auth model can't be bypassed by
  killing the daemon. On startup the daemon prints an **operator
  token**; the human exports it as `TCLAUDE_HUMAN_TOKEN` to run
  human-only commands (see [Identity](#identity)).

The daemon binds two sockets:

- `~/.tclaude/agentd.sock` — Unix socket for `tclaude agent` traffic.
- `127.0.0.1:<random>` — loopback HTTP for the human-approval popup
  and the [dashboard](dashboard.md).

By default `agentd serve` also adds a system tray icon (Open
dashboard, Reinstall agent skills, Open config, pending-approvals
submenu, Quit). On hosts without a tray host (WSL, headless servers,
pure Wayland) the icon silently doesn't appear — the daemon still
works. Pass `--no-tray` to skip the tray entirely. Pass
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
  to the daemon's own `sessions` table keyed by host PID. That conv-id
  is the agent's identity, and the agent is gated by the
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

`tclaude agent whoami` reports the resolved identity — an agent's
conv-id, `<human>`, or neither if the daemon couldn't confirm the
caller.

Most commands accept a *selector* wherever they take an agent: a full
conv-id, an 8+-char conv-id prefix, a global head alias, or a current
display title.

## Commands

The CLI surface is large; `tclaude agent <cmd> --help` is always the
authoritative reference. The sections below group the commands by what
they're for.

### whoami / lookup / ls

```bash
tclaude agent whoami           # who am I (or <human>)
tclaude agent lookup <name>    # resolve prefix / title to a full conv-id
tclaude agent ls               # peers in any group I'm in (online indicator + groups)
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
tclaude agent groups rm <group>                           # destroys the group + history; fails if messages reference it
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
tclaude agent groups resume <group>                        # spawn a session for every offline member
tclaude agent groups retire <group> [--no-shutdown]        # retire (soft-delete) every OTHER member; bulk parallel of `agent retire`
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
explosion), an **agent** caller is bound by three checks — a human
bypasses the agent-only ones, exactly as humans bypass every other
permission gate:

| Guardrail | Default | Refusal |
|-----------|---------|---------|
| **Group restriction** — an agent may only spawn into a group it is a member or owner of | on | `403 group_restricted` |
| **Rate limit** — spawns per caller-agent per rolling hour | 10 | `429 rate_limited` |
| **Max group size** — `agent_groups.max_members`; binds the human too | unlimited (0) | `409 group_full` |

The first two are tuned in `~/.tclaude/config.json` under `agent`
(`spawn_group_restriction`, `spawn_allowed_groups`, `spawn_max_per_hour`);
the member cap is a per-group property — `groups set-max-members`, or the
👥 chip on the dashboard's Groups tab. See [Permission model](#permission-model).

### clone / reincarnate / compact / context-info

Lifecycle commands. By default they target the calling agent itself;
`--target <selector>` retargets another agent (the **manager
pattern**, gated on the `agent.*` slug or group ownership).

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

## Permission model

Every mutating action is gated by a *permission slug*. Once the daemon
has [classified the caller](#identity), it decides:

1. **Human?** Pass — the human bypasses every gate.
2. **Agent?** Allowed iff the slug is in `default_permissions`
   (global), the agent's per-conv grants (SQLite), or an active
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
| SQLite `agent_permissions` table          | Per-conv grants (additive on top of defaults) | `permissions grant <conv> <slug>` (writes the DB row) |
| SQLite sudo-elevation table               | Time-bounded grants from `sudo` | `sudo request` / `sudo revoke` |

An agent's effective permission set is
`union(defaults, grants, active sudo elevations)`.

### Slugs

Slugs are grouped by family. `self.*` acts on the calling agent;
`agent.*` is the manager pattern (act on another agent); the rest
gate group, messaging, template, and permission administration.

| Family        | Slugs |
|---------------|-------|
| `self.*`      | `self.rename`, `self.compact`, `self.reincarnate`, `self.clone`, `self.schedule`, `self.remote-control` |
| `agent.*`     | `agent.rename`, `agent.compact`, `agent.reincarnate`, `agent.clone`, `agent.context-info`, `agent.resume`, `agent.stop`, `agent.delete`, `agent.schedule`, `agent.promote`, `agent.retire`, `agent.remote-control` |
| `groups.*`    | `groups.create`, `groups.rm`, `groups.archive`, `groups.stop`, `groups.resume`, `groups.retire`, `groups.spawn`, `groups.own`, `groups.link.add`, `groups.link.rm`, `groups.export`, `groups.import` |
| `member.*`    | `member.add`, `member.remove`, `member.redesignate` |
| `permissions.*` | `permissions.grant`, `permissions.revoke` |
| `message.*`   | `message.direct` |
| `templates.*` | `templates.manage`, `templates.instantiate` |
| `human.*`     | `human.notify` |

Run `tclaude agent permissions slugs` for the live registry with
descriptions — it is the source of truth; this table can drift.

### Ad-hoc human approval (`--ask-human`)

Most mutating commands take `--ask-human <duration>` (e.g. `30s`,
`2m`, or a bare integer for seconds; capped at 300s). On permission
denial, the daemon opens a browser popup with **Approve / Deny /
+5min** buttons:

```bash
tclaude agent groups create foo --ask-human 30s
# → CLI prints "Waiting up to 30s for human approval..."
# → browser popup opens, shows the requester / target / body
# → human clicks Approve → CLI proceeds
# → human clicks Deny or timeout fires → CLI fails with 403
```

**Timeout = Deny** so an unattended popup never silently grants. The
popup is loopback-only and authenticated by the same init-token
exchange the [dashboard](dashboard.md#auth) uses.

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

Six skills ship with the binary and install to `~/.claude/skills/`
for Claude Code, plus both `~/.agents/skills/` and `$CODEX_HOME/skills`
(default `~/.codex/skills`) for Codex CLI, via
`tclaude setup --install-agent-skills`:

- **`agent-coord`** — the day-to-day "talk to other agents" skill.
  Triggered by `[system: new agent message #...]` nudges and by user
  prompts asking the agent to coordinate.
- **`agent-rename`** — split out as its own skill so renames are
  obvious in the skill list.
- **`agent-lifecycle`** — context-window self-management: `compact`,
  `reincarnate`, `clone`, `context-info`.
- **`agent-dir`** — report or open a terminal in an agent's working
  directory.
- **`agent-schedule`** — set up and manage recurring `cron` nudges.
- **`human-notify`** — send the human a notification via
  `tclaude agent notify-human`; it lands in the dashboard
  [Messages tab](dashboard.md#messages).

Re-run `tclaude setup --install-agent-skills` after `go install
…@latest` to refresh the on-disk copies with whatever the new binary
embeds.

## Design notes

- The daemon is **foreground-only**. Run it in a tmux pane / a long-
  running terminal; restart manually after upgrades. (No launchd /
  systemd unit yet.)
- Identity is resolved from the **socket peer** plus, for the human
  operator, a per-daemon-lifetime **operator token** that is held in
  memory only — never persisted to disk. A daemon restart mints a
  fresh token.
- `agent_messages` rows accumulate forever for now (no auto-prune);
  bodies are short, so this is fine for a long while.
- The popup and the dashboard share the daemon's loopback port;
  closing the daemon closes both listeners.

## Troubleshooting

| Symptom                                                                 | Fix                                                             |
|--------------------------------------------------------------------------|-----------------------------------------------------------------|
| `Error: tclaude agentd is not running.`                                  | Start it: `tclaude agentd serve` (in a non-sandboxed shell).    |
| `Error: not in a shared group with target`                               | Add both convs to the same group, or add an inter-group link.   |
| `Error: selector matches multiple conversations`                         | Use the 8-char conv-id prefix instead of the title.            |
| `Error: caller is not granted permission "<slug>"`                       | Grant via `permissions grant`, retry with `--ask-human`, or `sudo request`. |
| `Error: unconfirmed caller: not a known agent, and no valid operator token` | You're the human: `export TCLAUDE_HUMAN_TOKEN=…` from the agentd startup banner. See [Identity](#identity). |
| Dashboard shows `403` on `GET /`                                         | Open it via `tclaude agent dashboard` — the cookie is only issued by the init-token exchange. |
| Popup didn't open / opened wrong browser                                 | On WSL, the daemon shells out to `/mnt/c/Windows/System32/cmd.exe /c start`. Check `tclaude agentd serve` logs. |
| `no_tmux` 503 on `agent rename`                                          | Caller has no live tmux session for the daemon to inject into.  |

## See also

- [Agent Dashboard](dashboard.md) — the browser operations console.
