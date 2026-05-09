# Agent coordination — TODO / DONE

Living todo list for agent coordination work in tclaude. Update as items
ship or get scoped out. The detailed v1 design lives in
[`agent-coord.md`](agent-coord.md).

---

## In progress

- **v1 plumbing for `tclaude agent` (this branch)**
  - `tclaude agent whoami`
  - `tclaude agent lookup <name>`
  - `tclaude agent ls` (peers across my groups)
  - `tclaude agent message <target> <body>` (with `--stdin` / `--file`,
    optional `--subject`)
  - `tclaude agent groups create|rm|ls|members|add|remove`
  - `tclaude agent inbox ls|read` (basic listing of received messages)
  - DB tables: `agent_groups`, `agent_group_members`, `agent_messages` (v8).
    `agent_messages` has columns: `from_conv`, `to_conv`, `subject`, `body`,
    `created_at`, `delivered_at`, `read_at`. Each conv's inbox is the rows
    where `to_conv = me`.
  - Tmux nudge via `send-keys` when target session is alive
  - Group-shared check enforced before sending (hard refuse otherwise)
  - Mutating `groups …` commands refuse when the parent process tree
    contains a `claude` ancestor; overridable via
    `agent.allow_agent_mutate_groups` in config or per-call
    `--allow-from-agent`.
  - Example skill `examples/skills/agent-coord/SKILL.md`

---

## TODO

### Session shortcuts
- `tclaude --join-group <group>` — start a fresh session and auto-join the
  given group on first message.
- `tclaude --join-group <group> --agent-name <name> [--agent-role <role>]
  [--agent-descr <text>]` — combine session creation, conv rename, group
  join, and role assignment in one command.
- Decide where the join happens: pre-launch (DB only) vs first-tick
  (after the conv-id is known). Probably first-tick via a hook.

### Group lifecycle
- `tclaude agent groups stop <group>` — gracefully end all sessions in a
  group. Open question: send `/exit` via tmux, or just kill the tmux
  sessions, or post a "wrap up" nudge and let agents finish.
- `tclaude agent groups archive <group>` — soft-delete (so message
  history stays queryable but membership is sealed).

### Discovery / state
- `tclaude agent groups ls --state=online|offline` — filter by whether
  members have a live tmux session right now.
- `tclaude agent ls --state=online|offline` — same filter, but for peers.
- Add a `groups` column to `tclaude conv ls` (and `-w` watch mode), so the
  user can search/sort/filter conversations by group and attach quickly.
  Probably needs a join in the conv list query plus an extra column in the
  bubbletea table. Useful even before all the messaging features are in
  daily use.
- Selectable filtering: pressing `g` in `conv ls -w` could open a fuzzy
  group picker.

### Inbox & message UX
- **Interactive mailbox inspector**: `tclaude agent mailbox <conv> -w` (or
  some better verb — possibly `inbox watch`, `mail`, etc.). Lists mails
  with sender/subject/date, lets the user select one to read, marks read
  on view, supports reply. Reuse `pkg/claude/common/table` (the same
  interactive table that backs `conv ls -w` and `session ls -w`) so
  filtering, sorting, and key bindings feel consistent. Two views are
  probably useful:
  - `tclaude agent mailbox <agent>` — that agent's inbox (the operator's
    debugging/auditing view).
  - `tclaude agent mailbox` (no arg) — current conversation's inbox,
    intended to be invoked by a running agent that just got nudged.
- Each conv now has an implicit inbox (rows in `agent_messages` where
  `to_conv = me`). `tclaude agent inbox ls` and `inbox read <id>` are the
  v1 readers. Conversations also keep an outbox view of their own sent
  messages — currently only via direct DB query; surface as
  `inbox sent`.
- Multi-recipient messages: add `to_convs` (or normalise to a
  per-recipient row table) plus a `cc_convs` list. The "from / to / cc /
  subject / body / read" mental model maps directly onto email and is
  intuitive for agents to reason about.
- Optional Reply-To / In-Reply-To threading so an agent can quote what it
  is replying to. Lightweight: just a nullable `parent_id` column on
  `agent_messages`.
- On session resume, flush undelivered nudges (`delivered_at IS NULL`) so
  messages sent while the target was offline still get surfaced.
- `tclaude agent inbox prune --older-than 30d --read-only` — delete
  `agent_messages` rows whose `read_at` is set and older than the
  cutoff. **TODO:** until this exists, message rows accumulate forever
  in SQLite. Bodies are short, so this is fine for a long while, but
  the option should exist.
- Conversation thread IDs surfaced to agents (so a reply can quote
  the parent). v1 just records `from`/`to`/`group`.

### Authority / safety
- v1 detector for mutating `groups create|rm|add|remove`: walk the parent
  process tree; if any ancestor is `claude` (or `node`, since CC runs as
  node), refuse by default. Override via `agent.allow_agent_mutate_groups`
  in `~/.tclaude/config.json` or per-call `--allow-from-agent`.
- Possible refinement: more granular config, e.g. allow `add` but not
  `rm`/`create`. Useful if we want agents to self-onboard into known
  groups.
- Possible refinement: extend the same gate to other sensitive commands
  (e.g. spawning new sessions, killing groups via `groups stop`). Map
  command → required policy in config.

### Delivery architecture (sandbox-aware)

**Problem:** when a sandboxed agent calls `tclaude agent message …`, the
DB write succeeds (because `~/.tclaude/db.sqlite` is allow-listed) but
the *tmux nudge* requires hitting `/tmp/tmux-…/tclaude` — a socket the
sender's sandbox typically blocks. The message is persisted but the
target sees nothing until they run `inbox ls` themselves. Same problem
applies to any cross-cutting concern (process-tree walks, lookups by
file path, etc.): they only work if the per-agent sandbox happens to
allow them.

The user-facing symptom is `(queued; target not online)` even when the
target's tmux session is very much alive.

**Three possible directions, in order of weight:**

1. **Hook-based lazy nudge (lightest).** Use the hook callback already
   wired up via `tclaude setup`. On any inbound hook
   (`PostToolUse`/`Notification`/etc.) the *receiver* checks for
   `agent_messages` rows where `to_conv = me` and `delivered_at IS NULL`,
   and the hook process (running in CC's environment, not the sender's
   sandbox) does the tmux send-keys to its own pane. No daemon.
   Latency = "next time the receiver does anything", which is usually
   sub-second. Best risk/reward for v1.

2. **`tclaude agentd` daemon.** A long-lived process started by
   `tclaude setup` (launchd on macOS, systemd user unit on Linux). Lives
   outside any agent sandbox. Watches `agent_messages.delivered_at IS
   NULL` (poll or SQLite hook), resolves target → tmux pane, sends the
   nudge, marks delivered. Could also handle: garbage-collecting dead
   session rows, refreshing tmux session names when CC restarts,
   exposing a richer query API. Cost: a new process to monitor, install,
   and reason about.

3. **Daemon over a Unix socket as the single agent API.** Instead of
   each `tclaude agent …` writing to SQLite directly, the CLI talks to
   the daemon over a socket, and the daemon owns DB + tmux + permission
   gating. Strongest authority story (the daemon decides who can talk to
   whom) but biggest rewrite — every existing agent CLI path goes
   through IPC. Aligns with "we can't always be aware of what sessions
   we're allowed to talk to": that lookup happens daemon-side, where it
   has the full picture.

**Decision:** foreground daemon, `tclaude agentd serve`. After a
discussion about `/fork` and inheritable env vars, the transport
pivoted from loopback HTTP+token to **HTTP over a Unix domain
socket** with **peer-cred identity** (no tokens). The daemon reads
the connecting peer's PID, walks to a `claude`/`node` ancestor, and
reads `~/.claude/sessions/<pid>.json` for the *current* conv-id —
which automatically tracks `/fork`/`/clear`/`/resume`. Full design
in [`agentd.md`](agentd.md).

### Cross-machine (far future)

When/if we ever want to span hosts: federate `tclaude agentd` instances
over the network. Each host's daemon owns its local conv pool and proxies
messages destined for remote convs to the appropriate peer daemon. Keeps
the per-host peer-cred identity model intact. **Explicitly out of scope
for now** — single-host first.

### (legacy) Cross-machine
- For now everything is keyed off the local SQLite + filesystem inbox. A
  future variant could publish messages over the existing `git` sync
  channel (`pkg/claude/git`) so agents on different machines can talk.
- Likely needs a real message-id namespace (UUIDs) and conflict-free
  message ordering.

---

## DONE

(empty — first ship hasn't happened yet)
