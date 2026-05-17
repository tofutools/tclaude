# Agent Coordination 🤝

> **BETA / EXPERIMENTAL**
>
> The agent feature is under active development. Commands, flags,
> permission slugs, the daemon wire format, the dashboard, and the
> SQLite schema may all change without notice. Per-agent permission
> grants and time-bounded elevations are stored in tables that have
> not been field-tested across many upgrades. **Don't build automation
> against this yet that you can't readily migrate.** See
> [`docs/plans/agents_todo.md`](plans/agents_todo.md) for what's still
> in flight.

Cross-session coordination between Claude Code conversations on the
same machine: messaging, group membership, agent lifecycle (spawn,
clone, reincarnate), scheduled nudges, and a browser dashboard — all
gated by a permission model the human curates.

`tclaude agent` (the CLI) talks to `tclaude agentd` (a long-running
daemon) over a Unix socket. The daemon owns the database, tmux nudges,
and permission gating; the CLI is a thin client. Identity is derived
from the connecting socket peer's PID — no tokens to manage, and
`/fork` keeps working because the daemon re-reads each caller's
current conv-id on every request.

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
  bundled `agent-*` skills under `~/.claude/skills/`. Without these
  skills installed, agents won't know to use these commands.
- **`tclaude setup --install-default-agent-permissions`** — grants the
  self-targeted slugs the bundled skills exercise (`self.rename`,
  `self.compact`, `self.reincarnate`, `self.clone`, `self.schedule`) as
  agent defaults. Idempotent; only adds missing slugs.
- **`tclaude agentd serve`** — running in a non-sandboxed shell. The
  CLI refuses to fall back to direct DB access when the daemon is
  down — that's deliberate, so the auth model can't be bypassed by
  killing the daemon.

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

## Quick start

```bash
# 1. Run the daemon in a separate (non-sandboxed) terminal.
tclaude agentd serve

# 2. Install the agent skills so CC agents know about these commands.
tclaude setup --install-agent-skills

# 3. As the human, set up a group and add some sessions.
tclaude agent groups create reviewer-team --descr "code review crew"
tclaude agent groups add reviewer-team <conv1> --role tech-lead
tclaude agent groups add reviewer-team <conv2> --role test-runner

# 4. From inside a CC session, an agent can now reach peers.
tclaude agent ls
tclaude agent message lead "Can you review PR #42?"

# Or open the dashboard and drive it from a browser.
tclaude agent dashboard
```

## Identity

Every command is associated with a *caller identity*:

- **Human** — invocation has no `claude`/`node` ancestor in its
  process tree. Bypasses every permission gate.
- **Agent** — invocation came from inside a CC session. The daemon
  walks up to the nearest claude/node ancestor and reads
  `~/.claude/sessions/<pid>.json` for its current conv-id. That
  conv-id is the agent's identity.

`tclaude agent whoami` prints whichever the daemon resolved.

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
tclaude agent groups archive <group>                      # soft-delete: freeze, hide, keep history
tclaude agent groups unarchive <group>                    # reverse an archive
tclaude agent groups clone <source> [new-name]            # fork every member into a brand-new group
tclaude agent groups stop <group> [--force]                # soft /exit (or hard kill-session) every member
tclaude agent groups resume <group>                        # spawn a session for every offline member
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
```

A **clone** inherits identity (group memberships, per-conv grants,
ownerships) and, by default, a copy of the conversation jsonl — the
original stays alive, and the clone is renamed to a `-c-<N>` title
suffix. A **reincarnate**
migrates identity onto a fresh conv-id and soft-stops the old one; the
follow-up prompt is mandatory so the successor isn't left idle.

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

Every mutating action is gated by a *permission slug*. The daemon
checks the caller's identity:

1. **Human?** Pass.
2. **Agent?** Allowed iff the slug is in `default_permissions`
   (global), the agent's per-conv grants (SQLite), or an active
   `sudo` elevation. Owning a group also passes the `agent.*`
   manager-pattern checks against members of that group.

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
| `self.*`      | `self.rename`, `self.compact`, `self.reincarnate`, `self.clone`, `self.schedule` |
| `agent.*`     | `agent.rename`, `agent.compact`, `agent.reincarnate`, `agent.clone`, `agent.resume`, `agent.stop`, `agent.delete`, `agent.schedule`, `agent.promote`, `agent.retire` |
| `groups.*`    | `groups.create`, `groups.rm`, `groups.archive`, `groups.stop`, `groups.resume`, `groups.spawn`, `groups.own`, `groups.link.add`, `groups.link.rm`, `groups.export`, `groups.import` |
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
via `tclaude setup --install-agent-skills`:

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
- Identity is **peer-cred-based**, not token-based. There's no API
  key to leak.
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
| Dashboard shows `403` on `GET /`                                         | Open it via `tclaude agent dashboard` — the cookie is only issued by the init-token exchange. |
| Popup didn't open / opened wrong browser                                 | On WSL, the daemon shells out to `/mnt/c/Windows/System32/cmd.exe /c start`. Check `tclaude agentd serve` logs. |
| `no_tmux` 503 on `agent rename`                                          | Caller has no live tmux session for the daemon to inject into.  |

## See also

- [Agent Dashboard](dashboard.md) — the browser operations console.
- `docs/plans/agent-coord.md` — design doc for `tclaude agent`.
- `docs/plans/agentd.md` — design doc for the daemon (peer-cred
  identity, socket layout).
- `docs/plans/agents_todo.md` — running TODO backlog of what's coming
  next.
