# Human message tab

Shipped 2026-05.

A permission-gated way for a coordinating agent (the PO, or any group
owner) to reach the human outside their busy terminal — messages land
in a dedicated **Messages tab** in the agentd dashboard.

## Why

The human talks to the PO only in the PO's terminal, which is also full
of agent-to-agent traffic and tool output. They wanted PO status
updates and human-decision questions on a surface they can check
separately. An earlier iteration explored an external Telegram
transport; the human redirected to a simpler in-dashboard tab — no
external services.

`notify-human` is an **extra** channel: agents still do their normal
terminal output as their primary channel; the Messages tab is an
explicit nudge layered on top.

## What shipped

### CLI

`tclaude agent notify-human "<body>"` — `--subject`, `--file` (`-` =
stdin), `--ask-human`. Sends one message to the human.

### Daemon

- `POST /v1/notify-human` — gated via `requireNotifyHumanPermission`:
  passes for the human, a holder of the `human.notify` slug, **any
  group owner** (a trusted coordinating role — checked before the slug
  gate), or an `X-Tclaude-Ask-Human` popup approval. Persists a
  `human_messages` row, snapshotting the caller's title + group.
- `POST /api/human-messages/read` — mark one (`{"id":N}`) or all
  (`{"all":true}`) read. Cookie-authed (dashboard).
- `POST /api/human-messages/clear` — delete every read message.
- `/api/snapshot` carries `messages` + `messages_unread`.

### Permission slug

`human.notify` — registered in `permissionRegistry`, **not**
default-granted. The human grants it to the PO; group owners bypass.

### Schema

Migration **v43→v44** adds `human_messages` (id, from_conv, from_title,
group_name, subject, body, created_at, read_at). `from_title` /
`group_name` are insert-time snapshots — a later rename/delete of the
sender cannot blank an old message. `read_at` empty = unread.

### Dashboard

New **Messages** tab (last in the nav) with an unread-count badge on
the nav button — visible from any tab. Messages render newest-first
with sender, group, subject, body, timestamp, and an unread marker.
Per-message **Focus** button raises the sending agent's terminal
window (via `POST /api/jump/{conv}`) and marks the message read;
disabled when that agent is offline. Plus mark-read, mark-all-read,
and a manual clear-read control. Retention is unbounded by design —
clear-read is the only pruning, low-volume + human-curated.

### Skill

Bundled `human-notify` skill — when to use `notify-human` (vs
agent-to-agent `agent-coord`), the permission model, and that it is an
extra channel, not a replacement for normal terminal output.

## Source files

- `pkg/claude/common/db/human_messages.go`, `migrate.go` (v43→v44)
- `pkg/claude/agentd/notify_human.go` — `/v1` + dashboard handlers
- `pkg/claude/agentd/dashboard.go` / `dashboard.html` — snapshot + tab
- `pkg/claude/agentd/identity.go`, `permissions.go` — the slug
- `pkg/claude/agent/notify_human.go` — CLI verb
- `pkg/claude/agent/skills/human-notify/SKILL.md`

## Tests

- `db/human_messages_test.go`, `db/migrate_v44_test.go`.
- `agentd/notify_human_flow_test.go` — slug gating, group-owner bypass,
  worker 403, human bypass, empty body, method, snapshot wire shape,
  dashboard read / mark-all / clear.

## Follow-up

A notification *setting* in `~/.tclaude/config.json` (e.g. an OS
desktop notification when a new human message arrives) — scoped
separately.
