# Dashboard spawn — separate "Initial msg" from "Descr"

Shipped: the "spawn a new agent" modal has two distinct fields where it
used to have one. A long task brief no longer has to be smuggled in via
the description, where it bloated the dashboard's description column.

The initial message is delivered to the new agent's **inbox** — as part
of a single "Startup context" `agent_messages` row — not typed into its
tmux pane. Newlines survive, multi-line briefs survive, and a large
briefing can't overflow CC's input-box size limit.

## The problem

The spawn modal had a single `Descr` field that did double duty: it was
both the dashboard's per-member description AND the only way to hand the
new agent any context. People who wanted to give the agent a real task
up front pasted it into `Descr` — and that whole wall of text then
rendered in the dashboard's "Description" column for the lifetime of the
agent.

A first cut split the fields but delivered the brief via tmux
`send-keys`, which forced newlines to collapse to spaces (each raw
newline lands as a premature prompt-submit). A second cut tried a
bracketed tmux paste — newlines survived, but a large briefing pasted
into CC's input box risks overflowing its input-size limit. The shipped
fix routes the brief through the inbox, where it is just stored text the
agent fetches on its own turn.

## What shipped

The spawn modal (`#agent-spawn-modal`) has:

- **Descr** — short, one-line description shown on the dashboard. Stored
  on the group-member row, surfaces in the members table's description
  column.
- **Initial msg** — the new agent's task brief. Delivered to its inbox;
  never stored as member metadata, so it never reaches the dashboard's
  description column.

The two are independent — fill in either, both, or neither.

## Delivery — the startup briefing

`handleGroupSpawn`, after the group-membership write, assembles a
**startup briefing** and inserts it into the new agent's inbox as a
single `agent_messages` row (`FromConv` blank — it is from the human,
who has no agent conv-id; `Subject` "Startup context"). The insert is
best-effort: the agent already spawned and joined, so a failed insert is
logged, not bubbled.

`buildSpawnContextBody(groupName, groupContext, initialMessage)` builds
the body from up to two sections, `---`-separated, each under a
plain-text header:

1. the group's `default_context` — shared guidance, included unless the
   spawn opted out (see `DONE/group-startup-context.md`);
2. the per-spawn `initial_message` — this agent's task brief.

Either section may be empty (or whitespace-only); when both are, the
result is `""` and no message is inserted. So group-context and the
initial message are *merged into one inbox message* — the agent has one
`tclaude agent inbox read` to run, not two, and never receives an empty
briefing.

`runSpawnPostInit` injects, in order, each its own turn:

1. `/rename <alias>` — when alias is a valid rename title.
2. The welcome `[system: ...]` line.

It does not type the briefing into the pane. Once the welcome lands it
calls `db.MarkAgentMessageDelivered` — the welcome line doubles as the
inbox nudge.

`buildSpawnWelcome` takes `spawnContextMsgID int64` + `hasInitialMessage
bool`, giving the welcome's trailing instruction three forms:

- no briefing message → `"Wait for the first instruction."`
- a briefing that includes a task brief → point at the inbox message,
  `"… read it with `tclaude agent inbox read N`, then act on the brief."`
- a briefing with only the group's startup context → point at the inbox
  message, `"… then wait for the first instruction."`

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
  `initial_message` with `isValidInitialMessage`, assembles the briefing
  via `buildSpawnContextBody`, inserts it as one `agent_messages` row,
  and passes the message id to `runSpawnPostInit` (which marks it
  delivered after the welcome). `buildSpawnWelcome` takes
  `spawnContextMsgID` + `hasInitialMessage` and points the agent at its
  inbox.
- `pkg/claude/agentd/handlers.go` — `isValidInitialMessage` (newline /
  tab tolerant, control-char + length gate).
- `pkg/claude/agent/spawn.go` — `SpawnParams.InitialMessage`
  (`--initial-message`/`-m`), validated, sent as `initial_message`.
- `pkg/claude/agent/lifecycle.go` — `isValidInitialMessage` CLI mirror.
- `pkg/claude/agentd/dashboard.html` — "Initial msg" textarea
  (`#agent-spawn-init-msg`); placeholder reworded ("delivered to its
  inbox, newlines preserved"); `submitAgentSpawn` sends the value
  verbatim.
- `pkg/claude/agentd/lifecycle_test.go` — `buildSpawnWelcome` unit tests
  cover all three trailing-instruction forms.
- `pkg/claude/agentd/handlers_test.go` — `TestIsValidInitialMessage`.
- `pkg/claude/agentd/spawn_initial_message_flow_test.go` — flow tests:
  `TestSpawn_InitialMessageDeliveredToInbox` (brief lands in the inbox,
  not the pane; welcome points at it; member `descr` stays the short
  label), `TestSpawn_InitialMessageMultiLinePreserved` (multi-line brief
  survives verbatim), `TestSpawn_InitialMessageRejectsControlChars`
  (NUL ⇒ 400).
- `pkg/claude/agentd/group_default_context_flow_test.go` —
  `TestGroupDefaultContext_MergedWithInitialMessage` (group context +
  initial message land in one inbox briefing).
- `pkg/claude/agentd/spawn_cli_flow_test.go` —
  `TestSpawnCLI_MultiLineInitialMessagePreserved` (CLI → wire → daemon →
  DB newline round-trip).

## Related

- `DONE/group-startup-context.md` — the per-group `default_context`
  feature whose delivery this briefing absorbed.
