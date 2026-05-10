# Inbox & message UX — open follow-ups

The `tclaude agent inbox` CLI is shipped: `ls`, `read`, `sent`,
`prune`, `reply`. Multi-recipient send via `--cc`, threading via
`parent_id`, flush-on-online, archived/expired suffixes — all live.

Interactive mailbox v1 shipped 2026-05: `tclaude agent inbox watch`
(aliased as `mailbox watch` / `mail watch`). Auto-refreshes every
3s, up/down nav, Enter loads + marks read, esc/q returns to list /
quits. Background poll suspends while in the read view to avoid
list-shuffle surprises.

## Open

### Reply from the watch view

Today reply still requires quitting the watch and running
`tclaude agent reply <id> "..."` from the shell. v1 deliberately
deferred reply to keep the surface small. Plan: add a `r` key in
the read view that opens a textarea below the body; submit via
ctrl+enter, esc cancels. The daemon endpoint exists already; only
the UI input + key handling is missing.

### Search / filter inside the watch

`/` to text-search inbox entries by subject/from/group — same shape
as `conv ls -w`. Compose with the existing `--unread` flag.

### Operator view: watch another agent's inbox

`tclaude agent inbox watch --target <conv>` (manager pattern). The
existing `/v1/inbox` endpoint reads the caller's inbox; an operator
view needs either a new endpoint or a header that passes the target
conv through (mirroring `--target` on lifecycle verbs). Permission:
reuse the `agent.<verb>` slug pattern (default human-only,
group-owner implicit power).

### Delete from the watch

`del` / `backspace` to delete a single message. Confirm modal.
Backed by a daemon endpoint that prunes one message ID — the
existing `inbox prune` is bulk by `--older-than`.

## Files
- `pkg/claude/agent/inbox_watch.go` — bubbletea model (v1)
- `pkg/claude/agent/inbox.go` — sibling CLI verbs (read, reply, etc.)
- `pkg/claude/common/table` — interactive table (reused)
