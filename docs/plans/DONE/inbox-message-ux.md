# Inbox & message UX

The `tclaude agent inbox` CLI shipped first: `ls`, `read`, `sent`,
`prune`, `reply`. Multi-recipient send via `--cc`, threading via
`parent_id`, flush-on-online, archived/expired suffixes — all live.

Interactive mailbox v1 (2026-05): `tclaude agent inbox watch`
(aliased as `mailbox watch` / `mail watch`). Auto-refreshes every
3s, up/down nav, Enter loads + marks read, esc/q returns to list /
quits. Background poll suspends while in the read view to avoid
list-shuffle surprises.

## Search filter (2026-05)

`/` in the list view focuses a filter input that
case-insensitive-substring-matches across subject, preview, from,
from_short, and group. Composes with `--unread` (daemon-side filter).
Esc clears the filter then exits search; enter commits and unfocuses;
up/down arrow exits search and moves the cursor in one keystroke.
Filter persists across the 3s background reload and across read-view
round-trips. Cursor always indexes the filtered slice — `enter` reads
the visually selected row, not entries[m.cursor]. Empty/whitespace
filter is treated as no filter. Header surfaces `[N/M messages]`
while filtering.

Files: `pkg/claude/agent/inbox_watch.go` (model + handler + view),
`pkg/claude/agent/inbox_watch_test.go` (6 unit tests:
SearchEscapeLadder, FilterMatchesAcrossFields,
NavigationRespectsFilter, EnterOnFilteredCursorReadsCorrectID,
FilterPersistsAcrossReload, ArrowFromSearchUnfocusesAndMoves).

## Single-message delete (2026-05)

`del` / `backspace` in the list view opens a y/n confirm modal pinned
to the cursor's message; `y` optimistically removes the row and POSTs
`DELETE /v1/messages/{id}`, any other key cancels. Daemon endpoint
mirrors prune auth — sender or recipient may delete the shared row
(third party gets 403; missing id gets 404). Failure restores via
reload + status message; no silent loss. Help line: `del delete`.
Modal renders inline at the bottom of the table in the error style.

Files: `pkg/claude/common/db/agent.go` (`DeleteAgentMessageByID`),
`pkg/claude/agentd/handlers.go` (`handleMessageDelete` + dispatcher
wiring), `pkg/claude/agent/inbox_watch.go` (`deleteConfirmID` +
`submitDeleteCmd` + `removeEntryByID`),
`pkg/claude/agent/inbox_watch_test.go` (6 unit tests covering modal
open, optimistic remove, error reload, read-view ignore, empty-list
no-op, removeEntryByID),
`pkg/claude/agentd/inbox_delete_flow_test.go` (recipient purge round
trip + third-party 403).

## Operator view (2026-05)

`tclaude agent inbox {watch,ls,read} --target <selector>` lets a
caller view another agent's inbox. Wire shape: an `X-Tclaude-Target-Conv`
header on the existing `/v1/inbox` and `/v1/messages/{id}` endpoints —
header-based mirrors how the lifecycle verbs encode `--target`, but
without forcing two new endpoints for read-only operations. Resolved
daemon-side via `agent.ResolveSelector` so aliases / prefixes work
the same as elsewhere.

Permission: new slug `agent.inbox-watch` (default human-only) OR
group-owner implicit power (caller owns at least one group containing
the target). Same dual auth as the lifecycle verbs'
`requireCrossAgentPermission`. Popup escape hatch (`X-Tclaude-Ask-Human`)
also supported. Helper: `requireInboxAccess` in `agent_dispatch.go`.

Operator-mode safety: the `tclaude agent inbox watch --target` TUI
disables `r` (reply) and `del` (delete) — replying or deleting from
someone else's view is confusing for the actual recipient. The header
shows `Inbox of <prefix> (read-only)` and the help line surfaces
`(operator: read-only)`. The daemon also forces keep-unread on the
operator's `/v1/messages/{id}` calls so the recipient's read marker
isn't dirtied by an operator drive-by.

`DaemonOpts.TargetConv` field on the agent client routes the header
through; `--target` on `inbox ls` / `inbox read` / `inbox watch`
all flow through `DaemonRequest("GET", path, nil, &out, DaemonOpts{TargetConv: p.Target})`.

Files:
- `pkg/claude/agentd/identity.go` — `PermAgentInboxWatch` slug
- `pkg/claude/agentd/agent_dispatch.go` — `requireInboxAccess`
- `pkg/claude/agentd/handlers.go` — `handleInbox` + `handleMessageByID`
  switch to the helper; `isOperator` flag forces keep-unread
- `pkg/claude/agent/client.go` — `DaemonOpts.TargetConv` + header set
- `pkg/claude/agent/inbox.go` — `--target` on `ls` and `read`
- `pkg/claude/agent/inbox_watch.go` — `--target` on `watch`, gates reply
  + delete in operator mode, status header surfaces operator state
- `pkg/claude/agent/inbox_watch_test.go` — `OperatorViewIsReadOnly` test
- `pkg/claude/agentd/inbox_operator_flow_test.go` — slug grants,
  group-owner implicit, third-party 403, no-header regression

## Files (current)

- `pkg/claude/agent/inbox.go` — sibling CLI verbs (ls, read, sent, prune, reply)
- `pkg/claude/agent/inbox_watch.go` — bubbletea model
- `pkg/claude/agent/inbox_watch_test.go` — model unit tests
- `pkg/claude/agentd/handlers.go` — inbox + message endpoints
- `pkg/claude/common/table` — interactive table (reused)
