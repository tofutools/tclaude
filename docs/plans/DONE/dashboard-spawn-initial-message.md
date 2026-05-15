# Dashboard spawn — separate "Initial msg" from "Descr"

Shipped: the "spawn a new agent" modal now has two distinct fields where
it used to have one. A long task brief no longer has to be smuggled in
via the description, where it bloated the dashboard's description column.

The initial message is delivered to the new agent's **inbox** (an
`agent_messages` row), not typed into its tmux pane — so newlines are
preserved and a real multi-line brief survives intact.

## The problem

The spawn modal had a single `Descr` field that did double duty: it was
both the dashboard's per-member description AND the only way to hand the
new agent any context. People who wanted to give the agent a real task
up front pasted it into `Descr` — and that whole wall of text then
rendered in the dashboard's "Description" column for the lifetime of the
agent.

A first cut split the fields but still delivered the brief via tmux
`send-keys`, which forced newlines to be collapsed to spaces (each raw
newline lands as a premature prompt-submit). That lost the structure of
any multi-paragraph brief. The fix below routes the brief through the
inbox instead, where newlines are just text.

## What shipped

The spawn modal (`#agent-spawn-modal`) has:

- **Descr** — short, one-line description shown on the dashboard. Stored
  on the group-member row, surfaces in the members table's description
  column.
- **Initial msg** — the new agent's task brief. Delivered to its inbox
  as an `agent_messages` row; never stored as member metadata, so it
  never reaches the dashboard's description column.

The two are independent — fill in either, both, or neither.

## Delivery

`handleGroupSpawn`, after the group-membership write, inserts the brief
as an `agent_messages` row addressed to the new conv (`FromConv` blank —
it is from the human, who has no agent conv-id; `Subject` "Initial
context"). The insert is best-effort: the agent already spawned and
joined, so a failed insert is logged, not bubbled.

`runSpawnPostInit` then injects, in order, each as its own turn:

1. `/rename <alias>` — when alias is a valid rename title.
2. The welcome `[system: ...]` line.

It no longer types the brief into the pane. Once the welcome lands it
calls `db.MarkAgentMessageDelivered` for the brief — the welcome line
doubles as the inbox nudge.

`buildSpawnWelcome` takes `initialMsgID int64`: when non-zero the
trailing instruction points the agent at the inbox message —
`"Your initial context / task brief is waiting in your inbox as message
#N — read it with `tclaude agent inbox read N`, then act on it."` Zero
falls back to `"Wait for the first instruction."`

## Wire surface

`POST /v1/groups/{name}/spawn` (and the dashboard twin) takes an
optional `initial_message` string in the request body. It is validated
with `isValidInitialMessage` — at most 4096 chars; newlines and tabs are
allowed (it is stored in the inbox, not typed into a pane), but NUL /
escape / carriage-return and other non-text control characters are
rejected. Rejection is `400 invalid_initial_message`.

The dashboard sends the textarea value verbatim (no `normaliseFollowUp`
collapse); the daemon trims it.

## CLI

`tclaude agent spawn` has `--initial-message` / `-m`. Validated
client-side with the agent-package mirror of `isValidInitialMessage`
(newlines preserved, not collapsed or rejected). `--descr` / `-d`'s
help text says it's the short dashboard label.

## Files

- `pkg/claude/agentd/lifecycle.go` — `handleGroupSpawn` validates
  `initial_message` with `isValidInitialMessage`, inserts it as an
  `agent_messages` row, and passes the message id to `runSpawnPostInit`
  (which marks it delivered after the welcome). `buildSpawnWelcome`
  takes `initialMsgID int64` and points the agent at its inbox.
- `pkg/claude/agentd/handlers.go` — `isValidInitialMessage` (newline /
  tab tolerant, control-char + length gate).
- `pkg/claude/agent/spawn.go` — `SpawnParams.InitialMessage`
  (`--initial-message`/`-m`), validated, sent as `initial_message`.
- `pkg/claude/agent/lifecycle.go` — `isValidInitialMessage` CLI mirror.
- `pkg/claude/agentd/dashboard.html` — "Initial msg" textarea
  (`#agent-spawn-init-msg`); placeholder reworded ("newlines
  preserved"); `submitAgentSpawn` sends the value verbatim.
- `pkg/claude/agentd/lifecycle_test.go` — `buildSpawnWelcome` unit tests
  cover the wait-line variant and the inbox-pointer variant.
- `pkg/claude/agentd/handlers_test.go` — `TestIsValidInitialMessage`.
- `pkg/claude/agentd/spawn_initial_message_flow_test.go` — flow tests:
  `TestSpawn_InitialMessageDeliveredToInbox` (brief lands in the inbox,
  not the pane; welcome points at it; member `descr` stays the short
  label), `TestSpawn_InitialMessageMultiLinePreserved` (multi-line brief
  survives verbatim), `TestSpawn_InitialMessageRejectsControlChars`
  (NUL ⇒ 400).
- `pkg/claude/agentd/spawn_cli_flow_test.go` —
  `TestSpawnCLI_MultiLineInitialMessagePreserved` (CLI → wire → daemon →
  DB newline round-trip).
