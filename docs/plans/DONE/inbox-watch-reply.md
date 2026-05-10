# Inbox watch — inline reply (2026-05)

Lets the human reply to a message from inside `tclaude agent inbox
watch` without quitting the TUI and shelling out to `agent reply <id>
"..."`. Carved out of the inbox-watch v2 follow-up list.

## CLI surface

In the read view (after pressing Enter on a message):

- `r` — opens a multi-line textarea below the body.
- `ctrl+enter` / `alt+enter` — submits the reply.
- `esc` — cancels; clears the draft.

The textarea is a `bubbles/textarea` model (multi-line). Default
height 4 rows. Empty submit short-circuits with a "no-op" status
without spamming the daemon.

## Daemon endpoint

Reuses the existing `POST /v1/messages/{id}/reply` — same path
`tclaude agent reply <id> "..."` calls. The daemon resolves the
sender, validates the caller is the recipient, routes through the
shared send path. No new server-side work.

## Failure handling

- Successful send → status line announces "reply to #N sent",
  textarea blurs and clears, reply mode exits but the read view
  stays open.
- Failed send → status line shows "reply failed: <err>", reply
  mode stays open with the draft preserved so the user can edit
  + retry without re-typing.

## Key isolation

While `replyFocused` is on, all list-mode keys (`j`, `k`, `q`,
`g`, `G`) and read-mode keys (`q` for back) route to the textarea
instead. Only `esc` and `ctrl+enter` are intercepted. Pinned by
`TestInboxWatch_ReplyOpensAndIsolatesKeys` — catches the bug
class where a stray `j` press while typing the reply scrolls the
underlying inbox table.

## Tests

Unit tests on the model state transitions:

- `TestInboxWatch_ReplyOpensAndIsolatesKeys` — `r` enters reply
  mode; list / read keys don't escape it; `esc` returns to read
  view (NOT to list).
- `TestInboxWatch_ReplySentSuccessClearsTextarea` — successful
  send clears draft, exits reply mode, status announces success.
- `TestInboxWatch_ReplyFailureKeepsDraft` — failed send keeps
  draft + reply mode so the user can retry.

Pure TUI feature, no daemon path beyond the existing reply
endpoint — only unit tests.

## Files

- `pkg/claude/agent/inbox_watch.go` — model + handlers + view
- `pkg/claude/agent/inbox_watch_test.go` — state-transition tests
- `pkg/claude/agent/reply.go` — sibling CLI command (unchanged,
  shares the daemon endpoint)
