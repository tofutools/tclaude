# Inbox & message UX — open follow-ups

The `tclaude agent inbox` CLI is shipped: `ls`, `read`, `sent`,
`prune`, `reply`. Multi-recipient send via `--cc`, threading via
`parent_id`, flush-on-online, archived/expired suffixes — all live.

Interactive mailbox v1 shipped 2026-05: `tclaude agent inbox watch`
(aliased as `mailbox watch` / `mail watch`). Auto-refreshes every
3s, up/down nav, Enter loads + marks read, esc/q returns to list /
quits. Background poll suspends while in the read view to avoid
list-shuffle surprises.

`/` text search shipped 2026-05: in the list view, `/` focuses a
filter input that case-insensitive-substring-matches across subject,
preview, from, from_short, and group. Composes with `--unread`
(daemon-side filter). Esc clears the filter then exits search;
enter commits and unfocuses; up/down arrow exits search and moves
the cursor in one keystroke. Filter persists across the 3s
background reload and across read-view round-trips. Cursor always
indexes the filtered slice — `enter` reads the visually selected
row, not entries[m.cursor]. Empty/whitespace filter is treated as
no filter. Header surfaces `[N/M messages]` while filtering.

Files: `pkg/claude/agent/inbox_watch.go` (model + handler + view),
`pkg/claude/agent/inbox_watch_test.go` (6 new unit tests:
SearchEscapeLadder, FilterMatchesAcrossFields,
NavigationRespectsFilter, EnterOnFilteredCursorReadsCorrectID,
FilterPersistsAcrossReload, ArrowFromSearchUnfocusesAndMoves).

## Open

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
