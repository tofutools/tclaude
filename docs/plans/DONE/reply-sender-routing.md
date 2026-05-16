# Reply sender routing — spawn briefings carry a sender — shipped

Fixes the orphaned-reply bug: `tclaude agent reply` against a spawn's
"Startup context" briefing used to route into a dead `to_conv=""` row.

## The bug

`handleGroupSpawn` inserted the "Startup context" briefing with no
`FromConv`. When the spawned agent ran `tclaude agent reply <brief-id>`
(exactly what the brief's "reply to <PO>" instruction tells it to do),
`handleMessageReply` walked an empty sender, inserted the reply with
`to_conv=""` — an orphan row no inbox query ever matches — and the CLI
printed `queued; target not online`, a status that never resolves. The
reply silently went nowhere.

## What shipped

### A — reject senderless replies (`pkg/claude/agentd/handlers.go`)

`handleMessageReply` now rejects with `400` when the resolved reply
target is empty (`walkSuccession(orig.FromConv) == ""`), instead of
inserting an orphan and reporting "queued". The error tells the caller
to send a fresh message with `tclaude agent message <target>`.

### B — spawn briefings carry a sender (`lifecycle.go`, `agent/spawn.go`)

`handleGroupSpawn` stamps `FromConv` on the "Startup context" message:

- **Default**: the spawn requester's conv-id (`requirePermission`'s
  return value) — an agent (e.g. a PO orchestrating workers) → its
  conv-id; a human → `""`.
- **Explicit override**: a new optional `reply_to` knob.
  - Wire: `reply_to` field on `POST /v1/groups/{name}/spawn`.
  - CLI: `tclaude agent spawn --reply-to <conv|prefix|title|alias>`.
  - Resolved server-side via `agent.ResolveSelector`; an unresolvable
    / ambiguous selector fails the spawn with `400 invalid_reply_to`.
  - Lets a coordinator spawn a worker whose brief-replies route to a
    *third* agent rather than the spawner.

A human-initiated spawn still leaves `FromConv` empty — and A then
gives the spawned agent a clear error if it tries to reply to the
brief. Optional by design: no sender is required.

## Files

- `pkg/claude/agentd/handlers.go` — `handleMessageReply` empty-target guard.
- `pkg/claude/agentd/lifecycle.go` — `handleGroupSpawn`: capture
  `spawnerConvID`, decode `reply_to`, resolve it, stamp `FromConv`.
- `pkg/claude/agent/spawn.go` — `SpawnParams.ReplyTo` + `--reply-to`
  flag + `reply_to` in the request body.

## Tests — `pkg/claude/agentd/reply_sender_routing_flow_test.go`

- `TestSpawn_ReplyTo_RoutesStartupBriefToNamedTarget` — `reply_to`
  (group alias) set → brief `FromConv` = resolved target, not spawner.
- `TestSpawn_NoReplyTo_AgentCaller_WorkerReplyReachesSpawner` — omitted
  + agent caller → `FromConv` = caller; the worker replies to its
  brief and the reply lands in the PO's inbox, threaded under it.
- `TestSpawn_HumanCaller_BriefHasNoSender_ReplyRejected` — human
  spawn → empty `FromConv` → reply rejected `400`, no `to_conv=""`
  orphan.
- `TestSpawn_ReplyTo_UnresolvableSelector_Rejected` — bad `reply_to`
  fails the spawn fast.
