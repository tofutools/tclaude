# Agent Coordination 🤝

Cross-session coordination between coding-harness conversations on the same
machine: messaging, group membership, agent lifecycle (spawn, clone,
reincarnate), scheduled nudges, reusable task forces, and a browser dashboard —
all gated by a permission model the human curates. Claude Code and Codex agents
can share the same group and use the same coordination API.

`tclaude agent` (the CLI) talks to `tclaude agentd` (a long-running
daemon) over a Unix socket. The daemon owns the database, tmux nudges,
and permission gating; the CLI is a thin client. The daemon resolves
every caller from the connecting socket peer: a caller running inside
a recognized coding-harness session is an *agent*, identified by a stable `agent_id`
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
  threaded follow-ups between harness sessions. The receiver gets a tmux
  nudge if they're online; otherwise the message queues in their
  inbox.
- **Group sessions.** Allow-list who can talk to whom. Out-of-group
  messages are refused server-side. Inter-group links open up
  directed cross-group messaging without co-membership.
- **Spawn and manage agents.** Spawn fresh Claude Code or Codex sessions straight into
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
  `self.remote-control`, `self.task`, `self.pr`, `self.tags`,
  `self.dir-repair`) as agent
  defaults. Self-reincarnation needs no slug. Idempotent; only adds missing slugs.
- **`tclaude agentd serve`** — running in a non-sandboxed shell. The
  CLI refuses to fall back to direct DB access when the daemon is
  down — that's deliberate, so the auth model can't be bypassed by
  killing the daemon. On startup the daemon prints an **operator
  token**; the human exports it as `TCLAUDE_HUMAN_TOKEN` to run
  human-only commands (see [Identity](#identity)).

The daemon binds one canonical socket plus two temporary compatibility sockets:

- `~/.tclaude/api/agentd.sock` — canonical, state-free Unix socket for all
  `tclaude agent` traffic. It is separated from the denied
  `~/.tclaude/data/` private-state tree.
- `~/.tclaude-agentd.sock` and `~/.tclaude/agentd.sock` — temporary
  compatibility listeners for older clients and previously installed sandbox
  settings. New clients and generated settings do not use them.
- `127.0.0.1:<random>` — loopback HTTP for the human-approval popup
  and the [dashboard](dashboard.md).

By default `agentd serve` also adds a system tray icon (Open
dashboard, Reinstall agent skills, Open config, pending-approvals
submenu, Quit). On Linux hosts without a reachable session DBus (common
in WSL and headless sessions), agentd reports that the tray is unavailable
and continues without it. A missing tray host on an otherwise working bus
also leaves the daemon running normally. Pass `--no-tray` (or set
`agent.disable_tray: true` in
`~/.tclaude/data/config.json`) to skip the tray entirely. Pass
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
walks the host process tree looking for a recognized harness runtime (`claude`,
`codex`, or Claude Code's `node` process), and
checks the request for an operator token. The verdict is one of:

- **Agent** — a coding-harness ancestor is present in the process tree. For
  Claude Code, the daemon can read `~/.claude/sessions/<pid>.json`; for every
  harness it can fall back to the daemon's `sessions` table keyed by the
  harness or pane-wrapper host PID. It then maps
  that live conv-id to the agent's stable `agent_id` — the
  rotation-immune identity that outlives any conv-id rotation
  (reincarnate, `/clear`) — and gates the agent by the
  [permission model](#permission-model).
- **Human** — the operator. There are two ways to reach this class:
  a CLI caller with **no** coding-harness ancestor that presents a
  valid operator token, or a request from the cookie-authenticated
  browser [dashboard](dashboard.md). The human bypasses every
  permission gate.
- **Refused** — a caller the daemon can confirm as neither. No
  harness ancestor and no valid token → `403 unconfirmed`; a
  harness ancestor whose conv-id can't be resolved → `403`; a peer
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
`agent.persist_operator_token: true` in `~/.tclaude/data/config.json`, also a
checkbox on the dashboard's Config tab — the two OR together). The daemon
then generates the token once and stores it, reusing it across restarts,
so you export it a single time. It is stored in the **OS keychain** when
one is reachable (macOS Keychain, Linux Secret Service, Windows Credential
Manager); on a host with no keychain backend (headless Linux, WSL without
D-Bus) it falls back to a `0600 ~/.tclaude/data/operator_token` file. The
secret is deliberately **not** written into `config.json` (which is
plaintext and shows up in the Config-tab diff and backups); the file
fallback keeps the same boundary as the in-memory token, since the agent
sandbox already denies reads to `~/.tclaude`. You can also pin your own
token by writing that file directly. Default (off) is the
fresh-token-each-boot behaviour described above.

**A coding-harness ancestor always wins over the token.** Because the
human exports `TCLAUDE_HUMAN_TOKEN` into their shell, a harness session
launched from that shell would inherit it — so the daemon classifies
*agent-ness first* and never offers the token branch to a caller with
a recognized harness ancestor. An agent therefore cannot escalate to the
human even if it holds the token (and `agentd` also strips
`TCLAUDE_HUMAN_TOKEN` from the environment of every session it
spawns). The flip side: a human running `tclaude agent` from a shell
that happens to descend from a non-harness `node` process can be classified
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
tclaude agent ls               # peers in any group I'm in (runtime, activity, role, group, worktree)
tclaude agent ls --json
```

`ls` is restricted to peers reachable through a shared group — the
group acts as an allow-list. Each row includes the peer's harness, reported
model and reasoning effort, dashboard-aligned state, and live sub-agent
count. The JSON form exposes those fields under `state`.

### message / reply / inbox

```bash
# direct
tclaude agent message <peer> "your message text"
tclaude agent message <peer> --body "your message text"  # equivalent flag form
tclaude agent message <peer> --subject "ack" --stdin <<EOF
multi-line body
EOF
tclaude agent message <peer> --file plan.md

# broadcast to every member of a group except yourself
tclaude agent message group:reviewer-team "PR #42 ready for eyes"

# reply (looks up the sender from the original message id; no need to
# copy conv-ids out of the headers)
tclaude agent reply <id> "got it"
tclaude agent reply <id> --body "got it"  # equivalent flag form
```

`reply` takes its body from the same four interchangeable sources as
`message`: positional text, `--body`, `--stdin`, or `--file`. Exactly one
may be given.

For direct messages the sender and target must share a group (or be
bridged by an inter-group [link](#groups)), otherwise the daemon
refuses with `not in a shared group`. For multicast (`group:<name>`
target) the sender must be a member of that group.

All durable post-startup agent mail enters the inbox before any asynchronous
tmux notification is attempted. For an online target the nudge worker resolves
the current agent generation, waits while its input is blocked, retries
transient tmux failures, and chooses inline or inbox-pointer delivery. For an
offline target, regular-message notification attempts are discarded instead
of accumulating and bursting after resume; the unread inbox rows remain
durable and the existing unread reminder can summarize them later. Internal
lifecycle, process, and scheduler nudges remain queued because dropping those
could break correctness. All pane-input paths share the same cross-process lock
so multi-command tmux sequences cannot interleave.
Startup greetings and briefings are the intentional exception: the harness
launch prompt owns their first delivery.

User-initiated one-shot sends are backpressured at 10 unprocessed regular
messages per target. A full target rejects a direct send or reply with
`queue_full` and does not write or discard a message; retry after the target
processes or explicitly reads pending mail. Group and CC sends continue for
available recipients and report each full recipient as a warning. Lifecycle,
process, scheduler, and other correctness-critical internal messages are
exempt from this regular-send cap.

Regular rows track notification and processing separately. An inline prompt is
correlated by its server-authored message ID in the shared Claude Code/Codex
`UserPromptSubmit` hook, then acknowledged as processed by `Stop` or
`StopFailure`. A later correlated message also acknowledges earlier rows whose
inline bodies were already marked read, which tolerates a missed hook without
polling transcript files. Pointer notifications remain unprocessed until
`inbox read` fetches the body. Notification delivery alone never frees sender
capacity. Sent-message API, CLI, and dashboard views label a suppressed
notification as `discarded while offline` rather than reporting it delivered.

Short printable messages are included directly in a nudge marked
`delivery: inline`, and their archival inbox copy is atomically marked
delivered/read. The complete body follows the nudge's closing bracket, so no
`inbox read` call is needed. Newlines and tabs are preserved through a bounded
bracketed paste; other control-bearing bodies use the stable `inbox read <id>`
pointer. Configure the rune threshold with
`agent.message_inline_max_chars` (default 2000; `0` disables regular-message
inlining). Agent-authored inline mail names the sender but omits reply
instructions; senderless system mail uses the same envelope without inventing
a sender. Human-authored dashboard mail is explicitly labelled **human
operator** and is not replyable. Pointer nudges contain only the message ID and
fetch command. Attachments are listed by durable, agent-readable absolute path
in both eligible inline delivery and `inbox read` output.

```bash
tclaude agent inbox ls                # last 20, all
tclaude agent inbox ls --unread       # only unread
tclaude agent inbox read <id>         # marks read; --keep-unread to defer
```

`inbox` has aliases `mailbox` / `mail`. Reading a message returns
RFC-822-shaped headers — `From`, `To`, `Group`, `Subject`, `Date`,
`Replyable`, and, when replyable, `Reply-To` and `Reply-Cmd` — followed by the
body.

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
[below](#ad-hoc-human-approval-ask-human)).

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

Sandbox profiles are operator-authored bundles of filesystem access rules,
environment configuration, optional agent-owned directory declarations, and
an optional `network_access` posture. Filesystem access accepts `read`,
`write`, or `deny`; deny blocks both reads and writes and dominates an
exact-path grant from any other applied profile. This lets an explicit
per-spawn profile subtract access inherited from a global or group profile.
They do not select a harness, model, or sandbox posture; those belong to spawn
profiles. Environment values
are stored and displayed as ordinary **non-secret configuration** — do not put
credentials in them. Profile payload reads and all mutations require the
`sandbox-profiles.manage` permission.

`network_access` accepts `internet`, `none`, or may be omitted to inherit the
harness's existing behavior. On the Codex managed `tclaude-agent` sandbox, both
explicit values use Codex's network boundary. `internet` explicitly disables
an inherited managed proxy and permits ordinary IP networking. On macOS,
`none` uses Codex's deny-by-default managed proxy so
Seatbelt can preserve the agentd Unix-socket exception. Independently of this
setting, every managed Codex profile denies the private directory containing
tclaude's tmux server socket; a Codex agent therefore cannot control its host
tmux server even when Internet access is enabled. Network policies currently
require Codex's managed sandbox; a Claude launch or a raw Codex sandbox mode
is rejected instead of silently dropping the rule. Linux/WSL `none` launches
are also rejected: Codex's current restricted-network seccomp denies the
`connect` syscall for Unix sockets as well as IP sockets, which would sever
the agentd control channel. The profile value remains portable so this can be
enabled when Codex gains a compatible Linux boundary. Existing profiles omit
the field and therefore remain backward-compatible.

#### Deny rules, reopens, and common rules

By default a sandboxed agent inherits its harness's default read visibility,
which is broad: ordinary files across the operator's home and system are
readable even though writes are confined. That includes ambient credential
locations such as `~/.ssh`, `~/.aws`, and `~/.config/gh`. tclaude does not
construct that posture — it comes from the harness.

Narrowing it does **not** need a separate mechanism. There is exactly one:
the `filesystem` table. Strictness is composed from ordinary rows — a broad
`deny` plus narrower `read`/`write` rows that reopen the parts the agent
actually needs.

**Read the capability gate before you author one.** A `read`/`write` row
strictly beneath a `deny` row in the *effective* profile is a
**reopen-under-deny**, and it is not universally available.

Two things to be clear about before the matrix. First, a deny row binds only
where tclaude actually renders the policy: on Claude, a launch with sandbox
`off` drops the deny entirely, and an `inherit` launch emits it without
enabling the sandbox, so it takes effect only if the operator's own settings
already have the sandbox on. A deny row is not a promise by itself. Second,
**a broad deny is almost always a reopen-under-deny in practice**, because the
launch contract below auto-pairs reopens for the workspace, agent-owned
directories, Git roots, and the agentd socket. A bare `deny ~` with no
operator-authored reopens still resolves to the reopen shape, and is gated
accordingly. The narrow case that escapes the gate is a deny over a path the
launch contract does not touch — `deny ~/.ssh` on its own, say.

The gate exists because the reopen shape is where a harness can quietly fail
to enforce what the profile claims:

* **Claude Code** — allowed, but requires sandbox mode `on`. A narrower
  `allowRead` genuinely reopens a broader `denyRead`: the sandboxing docs state
  that "when read rules overlap, the more specific path wins", with
  `denyRead: ["~/"]` + `allowRead: ["."]` as the worked example. (Deny
  directories are applied shallowest-first, so a deny at the *same* depth as a
  grant would be order-sensitive; tclaude never emits that pairing.) Under
  sandbox `inherit` or `off`, tclaude cannot guarantee that either the deny or
  the reopen is applied, so the launch is refused with a typed capability error
  rather than running with a strict-looking profile and a broad baseline.
* **Codex** — requires the managed `tclaude-agent` permission profile, Linux,
  and a verified split-policy bubblewrap probe. Codex does **not** resolve
  overlaps by specificity in general: normally a deny dominates any narrower
  grant regardless of specificity or declaration order, which would silently
  mask the reopen entirely. Narrower reopens work only under the non-legacy
  landlock split policy, so tclaude runs an isolated behavioral probe
  (`use_legacy_landlock = false`, `RequireSplitPolicy`) proving a denied parent
  can retain a narrower readable child, and determining whether an exact
  executable leaf must be reopened. Raw Codex `--sandbox` modes are refused.
  **macOS is refused**: per openai/codex#21081 a deny mask dominates narrower
  reopens beneath it there, with no split-policy equivalent.

With that in hand, a denied-Home profile looks like this — note that the
reopens are the load-bearing part, and this list is a *starting point*, not a
complete one for your machine:

```json
{
  "name": "strict-home",
  "filesystem": [
    { "path": "~", "access": "deny" },
    { "path": "~/git/myproject", "access": "write" },
    { "path": "~/.codex", "access": "read" },
    { "path": "~/.claude/plugins", "access": "read" },
    { "path": "~/go", "access": "read" },
    { "path": "~/.cargo", "access": "read" }
  ]
}
```

Everything the agent's harness, toolchain, and language runtime read out of
Home must appear as a reopen or the agent will not start or will fail partway
through a build. tclaude only auto-pairs the launch contract below; the rest is
yours to enumerate. Author these against a throwaway agent first.

Two limits shape what you can write, and both bite hardest under `deny ~`:

* **Rows are directories, not files.** A path that is not a directory is
  rejected outright. Home-level dotfiles — `~/.gitconfig`, `~/.netrc`,
  `~/.npmrc`, shell rc files — therefore cannot be reopened individually under
  a Home deny; they stay denied. Losing `~/.gitconfig` is the usual first
  symptom (Git loses your identity and credential helper). If an agent needs
  that configuration, relocate it into a directory you reopen, or supply it
  through the profile's `environment` instead.
* **You cannot reopen a directory that contains a protected root.** `~/.claude`
  contains `~/.claude/sessions`, which is protected, and an ordinary
  (non-`deny`) row intersecting a protected root is rejected — ancestors count.
  So under `deny ~` the Claude harness state directory cannot be reopened
  wholesale; reopen the specific children the harness needs (`~/.claude/plugins`,
  `~/.claude/skills`, …) and expect to discover that list empirically.

The practical consequence: a denied Home is materially easier to run under
Codex than under Claude Code today.

The Claude renderer maps a `deny` row to `denyRead` + `denyWrite` and a `read`
row to `allowRead`; the Codex renderer maps a `deny` row to `"path" = "none"`.

Rows are normalized only per canonical path (`deny` > `write` > `read` on the
*same* path). Overlapping but distinct ancestor/descendant paths are kept as
authored, which is what makes a reopen expressible at all. Cross-scope
composition is unchanged: `deny` still dominates an exact-path grant from
another applied profile, so a global or group profile's deny cannot be
neutralized by an explicit profile naming the same path. A strictly narrower
row from a later scope survives into the effective profile as a
reopen-under-deny — and is then subject to the harness gate above, which is
what decides whether it is actually enforced.

There is deliberately **no** "minimal"/allowlist mode and no host-wide preset.
Everything is visible in the table; nothing is hidden behind a stored mode ID.
Deny-all is composed by hand. `deny ~` is the practical posture. A literal
`deny /` is substantially harder and is not recommended without careful
iteration: the harness binary, the language runtime, and system paths such as
`/usr`, `/etc`, `/lib`, `/bin`, and `/proc` all sit outside Home and are not
part of the launch contract, so each must be reopened explicitly before
anything executes.

**The launch contract still holds.** When a deny row covers paths tclaude must
keep usable, tclaude pairs explicit read reopens automatically: the workspace /
worktree, declared agent-owned directories, the proof-pinned Git write roots,
and the agentd Unix socket (so `tclaude agent …` keeps working). On Claude an
`allowWrite` does not imply readability beneath a denied ancestor, so the read
reopen is emitted alongside the write grant. Without these an agent could
neither do its job nor coordinate.

Under a denied Home, Codex reopens only the active workspace and the exact
verified Git common/admin paths, not the whole repository container. Direct
sibling-worktree creation is therefore unavailable; create or broker the
worktree before launch.

**Common rules** are an authoring convenience, not the "preset" disclaimed
above — they store nothing and change no behavior. The dashboard's
**Add common rule** menu inserts audited `deny` rows drawn from a versioned
catalog of default-location sensitive paths — `~/.ssh`, `~/.gnupg`, cloud
credentials, VCS tokens, toolchain caches, browser profiles, and the Home
directory itself — each with a warning shown at insertion. Deny-home in
particular warns that you must reopen the harness, tclaude, and toolchain
directories (`~/go`, `~/.cargo`, `~/.codex`, …) or the agent will not function.
After insertion they are ordinary, editable, deletable rows: no hidden state,
no stored preset ID, and the profile you export is exactly the rows you see.
The catalog covers audited default locations; it is not a claim to find every
application-configured credential or cache path.

**Deny-row lineage is contained.** Resume, reincarnation, and agent-initiated
child spawns cannot weaken a deny: the effective profile they launch under must
not drop a deny row the recorded parent had, and must not introduce a reopen
beneath one that the parent lacked. Both are treated as widening and refused
under the same lineage rules as break-glass.

Profiles stored or exported before this model existed may still carry the old
`read_baseline` / `read_baseline_exclusions` fields. They are **silently
dropped on load** — no error, and no claim that the old restriction is still
being enforced. Note what that
means: such a profile is no longer strict. It launches with the harness's
ordinary broad read visibility under its old strict-sounding name, and nothing
in the effective profile records the restriction it used to carry. Audit any
profile that used those fields and re-express the intent as deny rows.

#### `break_glass_filesystem` — exceptional protected-path access

tclaude denies every sandboxed agent access to two protected roots:
`~/.tclaude/data` (daemon database, authorization state, private runtime state)
and `~/.claude/sessions` (harness session transcripts). Ordinary `filesystem`
rules that intersect those locations are rejected, and that does not change.

`~/.codex` is **not** a protected root — it is ordinary harness state that an
agent normally needs to read. An ordinary `deny` row may cover it, and a
denied Home does; in that case reopen it explicitly (see the deny-home warning
above) or managed Codex agents will be stranded.

`break_glass_filesystem` is the narrow, operator-controlled exception, for one
legitimate case: launching a tightly scoped agent to **debug tclaude itself** —
inspecting daemon state, diagnosing a migration, deliberately testing state
repair. It is an exception mechanism, **not a recommended profile posture**.

```json
{
  "name": "debug-daemon-state",
  "break_glass_filesystem": [
    { "path": "~/.tclaude/data", "access": "read" }
  ]
}
```

Rules are exact-path and access-specific. `access` is `read` or `write` only —
`deny` is already the default, and **read never implies write**. Read-only
inspection of the daemon database is materially less dangerous than write
access; prefer it. Each path must sit at or inside a protected root: a path
that merely *contains* one (`~`, `/`) is rejected, so the hatch cannot become a
whole-host grant wearing a danger label.

**What it can actually do to you.** Read access can disclose daemon secrets,
agent authorization state, and harness session transcripts and credentials.
Write access can additionally corrupt the SQLite database, harness
configuration, and runtime state; invalidate the assumptions agent
authorization relies on; and break the daemon or the harness. tclaude's tmux
server socket directory is a **distinct and more severe class** — host control
over other sessions — and is *not* reachable through break-glass at all; it
stays denied unconditionally.

**Acknowledgement.** Creating, editing, importing, assigning, and selecting a
launch for a profile carrying protected access all require an explicit operator
acknowledgement — `break_glass_acknowledged: true` on the API, or
`--i-understand-break-glass-risk` on the CLI. Without it the surface returns
`break_glass_acknowledgement_required` listing the exact path/access pairs.
Dry-run previews and import inspection are deliberately ack-free so an operator
can look before deciding.

The acknowledgement is **transient**: it is never stored on the profile and
never exported. The durable danger marker is the `break_glass_filesystem` field
itself, so an import or an assignment on another machine must acknowledge again
after paths are canonicalized against that machine's protected roots. Global
and group assignment carry the strongest warning, because every agent launched
under that scope inherits the access for as long as the assignment stands.

**Composition never hides it.** Break-glass merges as a privilege-monotonic
union (write dominating read on one canonical path) rather than last-layer-wins,
and the resolved launch echo, audit record, and dashboard all name every
profile and scope that contributed each protected path — including through
includes.

**Lineage.** Agent-initiated launches can neither introduce nor widen protected
access: a child may inherit no more than its parent's recorded authority, and
protected `read → write` is widening. Resume and relaunch replay the recorded
decision from the frozen snapshot and never pick up protected access added to
an ambient profile afterwards. A recorded protected path that has since been
removed or retargeted fails the launch closed rather than launching with
different authority than was acknowledged.

**Protected roots are denied on every harness.** Both the Claude settings block
and the managed Codex permission profile explicitly deny all three roots, so
"denied by default" is true regardless of which harness an agent runs under —
that promise is what the acknowledgement is protecting.

**Harness support.** Both supported harnesses can represent break-glass, but
only in their policy-rendering modes (Codex `managed-profile`, Claude
`sandbox on`), and each requires tclaude to suppress its own protected deny for
exactly the acknowledged path: on Codex a deny dominates any narrower grant
regardless of order, and on Claude deny directories are applied
shallowest-first. Any other harness/mode combination is rejected with the typed
`unsupported_sandbox_profile_break_glass` error.

**Composition and audit.** Break-glass merges as a privilege-monotonic union
(write dominating read on one canonical path). Each rule records the exact
include route it arrived by — author → … → assigned profile — and a diamond
keeps *both* arms, so the resolved launch echo and audit record show every path
by which dangerous authority reached an agent. An innocent-looking wrapper can
never take the blame or the credit for a rule it inherited.

That provenance is **derived, never authored**: it is not part of the profile
wire shape, is stripped at every authoring and import boundary, and is honored
only on values this package computed. A caller cannot supply an attribution and
have tclaude present it as audit truth.

Creating, editing, importing, or assigning a profile that inherits break-glass
through an include requires the same acknowledgement as one that declares it
directly. Import evaluates the exact registry state the transaction will
produce — including bundle-internal nested includes, and honoring the
`skip`/`overwrite` conflict policy — so the gate always judges the row that
will actually be assigned.

**Resume and reincarnation never gain authority.** Both re-resolve ordinary
rules from the current registry, but the protected-access decision and the
recorded deny rows are clamped to what was captured at launch: break-glass is
intersected with the frozen snapshot (never added, never widened read → write),
and every deny row in the snapshot must still be present — with no reopen
beneath it that the snapshot did not already have. There is no human in the
loop on a relaunch to acknowledge new protected access, so it is never granted
implicitly. To widen a running agent's protected access, spawn a fresh one and
acknowledge it.

A relaunch also **preserves the sandbox mode the agent was launched under**
rather than re-deriving the harness default. This keeps an enforced `sandbox
on` posture from being silently dropped on resume, and it is what allows a
legitimately acknowledged break-glass agent to be resumed or reincarnated at
all — the capability gate correctly refuses to re-open protected denies under a
mode that cannot guarantee them.

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
tclaude agent spawn <group> [--profile P] [--name N --role R --descr T --cwd DIR]
                            [--initial-message MSG | --file PATH] [--reply-to SEL]
                            [--worktree BRANCH [--worktree-base B] [--worktree-repo DIR]]
                            [--auto-focus] [--no-group-context] [--timeout DUR]
```

Launches a fresh detached CC session, waits for its conv-id to
materialise, and adds it to `<group>`. The new session lands in
`--cwd` (defaults to the caller's cwd, or the group's
[default dir](#groups)). Requires the `groups.spawn` permission
(human-only by default).

**Prefer a spawn profile.** With `--profile <name>` (an operator-preconfigured
[spawn profile](#spawn-profiles)) or a group/global default profile, usually no
other launch flags are needed. Explicit harness/model/effort/sandbox/approval
flags are for when no suitable profile exists or a policy pins a specific
vendor/model; see the default-resolution chain in `tclaude agent spawn --help`.

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
`agent.spawn_legacy_injection: true` in `~/.tclaude/data/config.json`.

**Name charset + auto-normalization.** A spawn name doubles as a git
worktree branch token and the conversation title, so it is restricted to
`[A-Za-z0-9_-]` (1–64 chars). By default a name straying outside that set is
**auto-normalized** rather than rejected — runs of spaces/punctuation/unicode
collapse to a single `-` and the leading/trailing `-` that produces are
trimmed (a `_` you typed is kept), so `--name "code reviewer!"` lands as
`code-reviewer`. This applies uniformly to
`tclaude agent spawn`, `--join-group`, and the dashboard's spawn modal (which
previews the normalized name as you type). Set
`agent.spawn_name_normalize: false` in `~/.tclaude/data/config.json` (or untick
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
bound by six checks — a human
bypasses the agent-only ones, exactly as humans bypass every other
permission gate:

| Guardrail | Default | Refusal |
|-----------|---------|---------|
| **Group restriction** — an agent may only spawn into a group it is a member or owner of | on | `403 group_restricted` |
| **Rate limit** — spawns per caller-agent per rolling hour | 10 | `429 rate_limited` |
| **Cross-harness matrix** — the fully resolved child harness must be allowed for the caller's harness | allow | `403 cross_harness_spawn_denied` |
| **Sandbox lineage** — the child may not have a weaker launch sandbox than the spawning agent | on | `403 sandbox_restricted` |
| **Approval lineage** — the child may not automatically accept a broader class of commands than the spawning agent | on | `403 approval_restricted` |
| **Dir write-proof** — the caller must prove its own sandbox can write in the child's launch dirs | on | `403 write_proof_required` / `403 write_proof_failed` |
| **Max group size** — `agent_groups.max_members`; binds the human too | unlimited (0) | `409 group_full` |

The first two are tuned in `~/.tclaude/data/config.json` under `agent`
(`spawn_group_restriction`, `spawn_allowed_groups`, `spawn_max_per_hour`);
the member cap is a per-group property — `groups set-max-members`, or the
👥 chip on the dashboard's Groups tab. See [Permission model](#permission-model).

The cross-harness matrix is edited from the Groups tab's global ⚙ menu
(**cross-harness spawns…**) and from each group's own ⚙ menu. It is directed:
Claude → Codex and Codex → Claude are independent edges. A group edge may
inherit, explicitly allow, or explicitly deny the global edge; absent global
edges allow. Denials require a reason, which is returned verbatim to the agent
along with the source/target harness and whether the group or global rule
applied. Same-harness and human-initiated spawns are unaffected. The check runs
after named, group-default, and global-default profiles resolve the target, and
is repeated in the shared spawn core for template/wave/process launch paths.
Agent-triggered scribe summons and clones are covered too; a clone that will
inherit multiple group memberships or ownerships must be allowed by every
destination group's effective edge.

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

The approval-lineage guard separately compares the spawning agent's recorded
approval policy and auto-review setting with the fully resolved child launch.
It models automatic acceptance as capability sets rather than pretending every
mode has a useful linear rank.

Both sides are first resolved to a normalized capability shape, then compared as
a subset test. There are no per-direction or per-harness exceptions: Codex
approval policies and Claude permission modes are projected onto the *same*
axes, because their labels do not form one comparable authority lattice.

Human approval is baseline throughout. A posture that reaches a human — the
Claude approval popup, a Codex escalation prompt, the operator's own allow/deny
rules — grants the agent no automatic capability of its own. What the guard
gates is what an agent can cause *without* a human:

| Capability | Meaning | Held by |
|------------|---------|---------|
| auto-edits | writes files and runs common fs commands in its working dir with no human in the loop | Claude `acceptEdits`, and everything below |
| auto-commands | runs *arbitrary* commands with no human in the loop, inside its sandbox | Codex `never` / `on-request` / `on-failure`; Claude `auto` |
| machine reviewer | a model may approve, in a human's place, actions that escalate past the sandbox boundary | Codex Auto-review (except alongside `never`, which emits no requests) |
| unreviewed | auto-approves everything, with no reviewer of any kind | Claude `bypassPermissions` |

auto-edits and auto-commands are deliberately distinct. `acceptEdits`
auto-approves edits but still prompts a human before every other command, so it
must not be able to mint a child that runs `curl`, `git push`, or `rm -rf`
unattended — even though both postures are "automatic".

| Parent posture | Child postures allowed by the approval guard |
|----------------|-----------------------------------------------|
| Claude `plan` / `default` / `dontAsk` | the same baseline postures, and Codex `untrusted` without auto-review |
| Claude `acceptEdits` | the above, plus Claude `acceptEdits` |
| Claude `auto` | the above, plus Claude `auto` and Codex `never` / `on-request` / `on-failure` without auto-review |
| Claude `inherit` | the baseline postures above, plus an exact Claude `inherit` continuation |
| Claude `bypassPermissions` | any posture |
| Codex `untrusted`, auto-review off | Codex `untrusted` without auto-review; Claude `plan` / `default` / `dontAsk` |
| Codex `on-failure` / `on-request` / `never`, auto-review off | the above, plus Codex `never` / `on-request` / `on-failure` without auto-review and Claude `acceptEdits` / `auto` |
| Codex `on-failure` / `on-request` with active classifier review | any Codex posture, and every Claude posture except `bypassPermissions` — including `inherit`, whose capability shape is exactly this one |

The table governs *explicit* postures. When a child's posture is **unset** —
no `--ask-for-approval` flag and no spawn-profile value — tclaude applies the
harness default (Claude: `auto`), but first narrows it to something the caller
is actually allowed to grant — falling back to the caller's *own* posture when
the default would exceed it. So any parent that cannot mint `auto` — Claude
`inherit`, `acceptEdits`, `plan`, `default`, `dontAsk`, or Codex `untrusted` —
defaults its same-harness children to its own posture and keeps delegating,
instead of failing. The `inherit` case is the one you are most likely to meet:
it keeps bare delegation working from agents launched before `auto` became the
default and from your own `tclaude session new` session.

The fallback is same-harness only (postures are not interchangeable across
harnesses), it can only ever narrow, and the guard still checks the narrowed
value. Spawns with no agent caller (you, or the dashboard) always get the plain
harness default. An explicitly requested escalation — by flag or by spawn
profile — is never silently narrowed; it fails with `approval_restricted`.
When narrowing changes a direct spawn, its resolved-shape echo calls it out
explicitly, for example `approval inherit (harness default auto, narrowed to
caller posture)`. Returned template-instantiation results use the same note for
each adjusted agent. The common case stays quiet.

Claude `auto` is **not** the Codex Auto-review equivalent (see
[TCL-92](https://linear.app/johan-kjolhede/issue/TCL-92)). The `auto`
supervisor reviews and tightens operations that remain inside the Claude
sandbox; it is not a boundary-escalation grant, so a Codex `never` parent — the
safe unattended posture — may spawn a Claude `auto` child. Conversely, Claude
`acceptEdits` and `auto` cannot enable Codex Auto-review, because a machine
reviewer of *boundary escalation* is a capability neither of them holds.

Claude `inherit` is the one posture that cannot be resolved at spawn time: it
means "whatever the operator's live settings decide, plus the agentd approval
popup". The guard therefore uses **dual bounds**: an `inherit` parent receives
only the automatic capability it is proven to hold, while an `inherit` child is
charged the broadest non-bypass capability it could receive. This prevents an
unknown parent from minting an explicit `auto`, Codex unattended-execution, or
guardian posture.

One narrow compatibility exception keeps recursive Claude work practical: an
exact `inherit` parent may spawn an exact `inherit` child. That continuation
preserves the same operator-owned live posture instead of asserting a new
explicit capability. An `inherit` child under any different/narrower parent
still fails closed; the denial names an explicit mode that parent can actually
delegate, when one exists.

### Known limit: the child's project settings

The approval guard compares *requested* postures. It does not prove a Claude
child's **effective** posture, because Claude Code merges `permissions.allow`
rules, hooks, and sandbox settings from files in the child's cwd, and
`claude --settings` merges over those files rather than replacing them. A parent
that can write the child's cwd — which the dir write-proof confirms it can, by
design — could therefore write `.claude/settings.local.json` or a `PreToolUse`
hook that widens the child beyond the mode it was launched with.

This is a real residual gap, not a solved one. It is bounded by the separate
sandbox-lineage guard (which caps *where* the child may act) and by Claude
Code's folder-trust prompt (which does not help in an already-trusted repo). An
earlier version of this guard blunted it by refusing all delegation from
non-`bypassPermissions` Claude parents, but that also made every Claude agent
unable to spawn anything, so the restriction was removed rather than kept as a
partial mitigation with a large usability cost. Closing it properly requires
an immutable inherited configuration contract — including trusted MCPs and
instructions — rather than comparing modes harder or silently dropping the
operator's configured tools.

For Codex, `untrusted` is more restrictive than the other approval policies:
without auto-review it asks before every command outside Codex's trusted set.
`on-failure`, `on-request`, and `never` may execute commands automatically
while they remain inside the OS sandbox. Active classifier review is a second,
incomparable capability: `untrusted` plus classifier review may approve actions
that plain `untrusted` would ask a human about, but it may still reject an
in-sandbox action that `never` would run. `never` never produces an approval
request, so setting `auto_review=true` alongside it does not activate the
classifier capability.

A legacy Codex session whose durable spawn provenance proves it used the old
daemon default is reconstructed as `never` by the database migration and the
runtime compatibility guard. This is faithful launch-history reconstruction,
not an assumption that `never` is less capable than prompt-oriented policies.
Ambiguous direct/imported/template histories stay fail-closed; relaunching them
under the current version records the conservative `untrusted` posture. The
human spawn path bypasses approval lineage, as it does the sandbox-lineage
guard.

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
Agent-triggered clone and reincarnate operations inherit the live pane's
physical cwd. Offline resume instead validates daemon-private, durable
provenance captured from the target's physical cwd and Git metadata at launch
and again before a controlled stop. The daemon pins that exact identity through
the real session-new handoff; the caller never needs write access to the
target-owned directory. A failed stop-time capture never blocks the stop, but
it clears older provenance so the next resume fails closed. A direct human
resume, or an actually approved `--ask-human` recovery, may recapture the
current identity. Recreating a missing resume cwd remains human-only because
the daemon cannot prove an agent can write inside a path that does not yet
exist. Fresh spawns and caller-selected launch locations still use caller-side
directory proof.

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
tclaude agent resume <selector> [--ask-human 30s] [--recreate-dir] # bring an offline agent back into a tmux pane
tclaude agent dir [selector]                 # print an agent's working directory
tclaude agent dir --worktree                 # git worktree/repo root instead
tclaude agent dir --start                     # the launch directory instead
tclaude agent dir --open                      # open a terminal there (via the daemon)
tclaude agent dir --repair                    # recreate own deleted launch directory
```

`stop` / `resume` are idempotent — already-offline / already-online
agents come back as `skipped:...`. They are the single-conv variants
of `groups stop` / `groups resume`, and require `agent.stop` /
`agent.resume` (or group ownership) when targeting another agent.

Managed Codex agents also recover automatically after a proven nonzero
runtime exit with no lifecycle intent. Recovery resumes the same conversation
and stable agent identity. Retries use durable exponential backoff (5s, 10s,
20s, 40s, 80s, 160s, 5m, then 10m indefinitely); 30 minutes of healthy
runtime resets the sequence. Clean exits and ambiguous, pre-launch,
superseded, retired, or provenance-unverified sessions fail closed. A manual
`agent resume` retries immediately and supersedes the scheduled attempt;
stop, retire, and reincarnate cancel it. The dashboard distinguishes crashed,
restarting, crash-loop/backoff, recently recovered, and recovery-suppressed
states and shows the bounded retry evidence without storing pane content or
harness logs.

Resume is an in-place lifecycle operation: it reconstructs and revalidates the
target's durable physical-directory and Git identity plus its current effective
sandbox, then reuses that target authority through daemon-owned launch pins.
The caller does not need filesystem write access to the target's managed
directories. Missing, malformed, or changed provenance fails closed; a direct
human resume or an approved `--ask-human` request may trust and persist the
current identity. Denial or timeout leaves the stopped target unchanged.
Directory write proof remains required for fresh agent spawns and
caller-selected launch locations. A missing recorded launch directory also
fails closed; only a direct human or approved `--ask-human` recovery may opt
into recreating it with `--recreate-dir`.

A still-running agent whose launch directory was deleted can recover without
write access to the parent by running `agent dir --repair`. This self-only
operation is gated on `self.dir-repair` and recreates exactly the immutable
physical startup directory recorded by tclaude; it accepts no selector or path,
refuses symlink substitution, and does not reconstruct Git metadata or later
directories the agent moved into.

### cron

Recurring scheduled nudges. The daemon's scheduler ticks every 30s. By default,
a due tick is delivered only to recipients that are online at fire time; an
offline tick is recorded as skipped and creates no inbox row. Use
`--queue-when-offline` for jobs whose messages should survive downtime through
the same durable inbox and delivery queue as ordinary messages.

```bash
tclaude agent cron add --interval 10m --body "status check?" [--target SEL --name N]
tclaude agent cron add --interval 10m --run-immediately --body "start now, then repeat"
tclaude agent cron add --interval 10m --queue-when-offline --body "retain until resume"
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

New jobs wait for their first scheduled due time. `--run-immediately` opts
into one immediate first delivery and then preserves the normal cadence from
that fire. The persisted setting is also editable in the dashboard: changing
it from off to on fires once; saving it on again is inert, and turning it off
does not fire. `run-now` remains the explicit one-off action independent of
that setting. Daemon restarts never replay the immediate opt-in.

Offline delivery is a separate, default-off setting. `--queue-when-offline`
opts the job into durable delivery while its target is down. Group jobs apply
the policy per member: online members still receive a tick when other members
are offline. `cron logs` records `skipped_offline` when all eligible recipients
were offline and `partial_offline` for a mixed group delivery.

### permissions / sudo

```bash
tclaude agent permissions slugs                          # registry of known slugs + descriptions
tclaude agent permissions ls [<conv|title|default>]      # defaults + grants, or effective set for one agent
tclaude agent permissions grant <conv|title|default> <slug>
tclaude agent permissions revoke <conv|title|default> <slug>
```

`permissions ls` is fully daemon-backed: selector resolution, the
effective/owner-implied calculation and the display metadata (titles,
stable agent ids) all come from agentd, so a sandboxed agent that cannot
read `~/.tclaude/data` still gets a complete answer. A stale selector
comes back as one concise `not_found`; an ambiguous one lists candidates.

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
[Concepts](dashboard.md#concepts-pattern-process-rhythms) note covers the same
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
editing — set `scribe.profile` in `~/.tclaude/data/config.json` (or pick it from the
dashboard **Config tab → Ask & scribe defaults**) to the name of a saved [spawn
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
| `~/.tclaude/data/config.json` → `agent.default_permissions` | Slugs granted to **every** agent | hand-edit, or `permissions grant default <slug>` |
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
| `self.*`      | `self.rename`, `self.compact`, `self.clone`, `self.schedule`, `self.remote-control`, `self.task`, `self.pr`, `self.tags`, `self.dir-repair` |
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
command, use [`sudo`](#permissions-sudo) instead.

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
  stored in the OS keychain or a `0600 ~/.tclaude/data/operator_token` file;
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
| `Error: selector matches multiple conversations`                         | Use the stable `agent_id` (or a unique `agt_…` prefix) from `agent ls`. |
| `Error: caller is not granted permission "<slug>"`                       | Grant via `permissions grant`, retry with `--ask-human`, or `sudo request`. |
| `Error: unconfirmed caller: not a known agent, and no valid operator token` | You're the human: `export TCLAUDE_HUMAN_TOKEN=…` from the agentd startup banner. See [Identity](#identity). |
| Dashboard shows `403` on `GET /`                                         | Open it via `tclaude agent dashboard` — the cookie is only issued by the init-token exchange. |
| Access request did not open a browser                                    | Browser auto-open is off by default. Set `agent.access_request_auto_open_browser` to `true` if you want that extra alert. |
| `no_tmux` 503 on `agent rename`                                          | Caller has no live tmux session for the daemon to inject into.  |

## See also

- [Agent Dashboard](dashboard.md) — the browser operations console.
