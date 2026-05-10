# Inbox & message UX — open

The `tclaude agent inbox` CLI is shipped: `ls`, `read`, `sent`,
`prune`, `reply`. Multi-recipient send via `--cc`, threading via
`parent_id`, flush-on-online, archived/expired suffixes — all live.
See DONE/index.md for the shipped log.

## Open

### Interactive mailbox inspector

`tclaude agent mailbox <conv> -w` (or some better verb — possibly
`inbox watch`, `mail`, etc.). Lists messages with sender / subject /
date, lets the user select one to read, marks read on view, supports
reply.

Reuse `pkg/claude/common/table` (the same interactive table that
backs `conv ls -w` and `session ls -w`) so filtering, sorting, and
key bindings feel consistent.

Two views are probably useful:
- `tclaude agent mailbox <agent>` — that agent's inbox (the
  operator's debugging/auditing view).
- `tclaude agent mailbox` (no arg) — current conversation's inbox,
  intended to be invoked by a running agent that just got nudged.

## Files
- `pkg/claude/agent/inbox.go` — CLI verb
- `pkg/claude/common/table` — interactive table
