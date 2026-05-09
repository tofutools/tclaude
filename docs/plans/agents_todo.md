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

### Cross-machine
- For now everything is keyed off the local SQLite + filesystem inbox. A
  future variant could publish messages over the existing `git` sync
  channel (`pkg/claude/git`) so agents on different machines can talk.
- Likely needs a real message-id namespace (UUIDs) and conflict-free
  message ordering.

---

## DONE

(empty — first ship hasn't happened yet)
