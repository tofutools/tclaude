# Dashboard spawn — separate "Initial msg" from "Descr"

Shipped: the "spawn a new agent" modal now has two distinct fields where
it used to have one. A long task brief no longer has to be smuggled in
via the description, where it bloated the dashboard's description column.

## The problem

The spawn modal had a single `Descr` field that did double duty: it was
both the dashboard's per-member description AND the only way to hand the
new agent any context. People who wanted to give the agent a real task
up front pasted it into `Descr` — and that whole wall of text then
rendered in the dashboard's "Description" column for the lifetime of the
agent.

## What shipped

The spawn modal (`#agent-spawn-modal`) now has:

- **Descr** — short, one-line description shown on the dashboard. Stored
  on the group-member row, surfaces in the members table's description
  column.
- **Initial msg** — sent to the new agent as its first real prompt, as
  its own turn right after the welcome. Never stored as member metadata,
  so it never reaches the dashboard.

The two are independent — fill in either, both, or neither.

## Delivery

`runSpawnPostInit` now injects, in order, each as its own turn:

1. `/rename <alias>` — when alias is a valid rename title.
2. The welcome `[system: ...]` line.
3. The initial message — when one was supplied.

`buildSpawnWelcome` gained a `hasInitialMessage bool`: with a message
queued, the trailing instruction flips from "Wait for the first
instruction." to "Your first instructions follow in the next message."
so the agent acts instead of sitting idle.

The function was restructured to resolve the tmux target once (via
`pickAliveSession`) and run the three injections through
`injectTextAndSubmit` directly, replacing the old `injectSlashCommand`
rename+follow-up combo (which only supported one follow-up).

## Wire surface

`POST /v1/groups/{name}/spawn` (and the dashboard twin) gained an
optional `initial_message` string in the request body. It is validated
with `isValidFollowUp` — 1-4096 printable chars, no control characters,
because each newline would land as a premature submit through tmux
send-keys. Rejection is `400 invalid_initial_message`.

The dashboard collapses newlines client-side (`normaliseFollowUp`, shared
with the clone modal) so a multi-line textarea stays ergonomic; the
server-side validation is the backstop for CLI / agent-API callers.

## CLI

`tclaude agent spawn` gained `--initial-message` / `-m`. `--descr` /
`-d`'s help text now says it's the short dashboard label. Newlines in
`--initial-message` are rejected (not collapsed), matching `agent clone`
/ `agent reincarnate`'s follow-up handling.

## Files

- `pkg/claude/agentd/lifecycle.go` — `handleGroupSpawn` body struct gained
  `InitialMessage`; validated with `isValidFollowUp`; passed to
  `runSpawnPostInit`, which was restructured (see Delivery above).
  `buildSpawnWelcome` gained `hasInitialMessage bool`.
- `pkg/claude/agent/spawn.go` — `SpawnParams.InitialMessage`
  (`--initial-message`/`-m`), validated, sent as `initial_message`.
- `pkg/claude/agentd/dashboard.html` — new "Initial msg" textarea
  (`#agent-spawn-init-msg`); `Descr` placeholder reworded + shrunk to
  2 rows; reset in `openAgentSpawnModal`, normalised + sent in
  `submitAgentSpawn`.
- `pkg/claude/agentd/lifecycle_test.go` — `buildSpawnWelcome` unit tests
  cover both the wait-line and the follow-line variants.
- `pkg/claude/agentd/spawn_initial_message_flow_test.go` — flow tests:
  `TestSpawn_InitialMessageDeliveredSeparateFromDescr` (initial message
  reaches the pane as its own turn; member `descr` stays the short
  label), `TestSpawn_InitialMessageRejectsControlChars` (newline ⇒ 400).
