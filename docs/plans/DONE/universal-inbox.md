# Universal inbox — agent_messages as the universal transport

Status: **shipped.** Tier 2. `agent_messages` is now the transport for
*all* agent→agent messages; group membership is an authorisation policy,
not a mechanism constraint.

## What shipped

Before this change `agent_messages.group_id` was
`INTEGER NOT NULL REFERENCES agent_groups(id) ON DELETE RESTRICT` — a row
could not exist without a shared routing group. Solo (groupless) agents
therefore had no inbox, and `clone` / `reincarnate` fell back to a brittle
`tmux send-keys` path to hand a solo successor its follow-up.

Now the inbox is universal: any conv → any conv, group or not. Group
membership decides *authorisation* (intra-group messaging is allowed by
default; off-group needs an elevated slug), not whether a message row can
be written.

### Schema — migration v35 → v36 (`db/migrate.go`)

`migrateV35toV36` rebuilds `agent_messages`: `group_id` becomes
`INTEGER NOT NULL DEFAULT 0` (where **0 = direct**, no routing group) and
the foreign key to `agent_groups` is **dropped**. SQLite cannot drop an FK
or relax `NOT NULL` in place, so it is the standard table rebuild —
create `agent_messages_v2`, `INSERT…SELECT` all 13 columns, drop the old
table, rename, recreate both indexes — wrapped in one explicit
transaction so it is atomic (no partial state, no orphaned scratch
table). `currentVersion` bumped 35 → 36.

Scratch-table naming convention adopted here: `<table>_vN` for the Nth
rebuild of a table (so a future second rebuild uses `_v3`), making the
migration history unambiguous.

### `db.DeleteAgentGroup` preserves message history (`db/agent.go`)

With the FK gone, group deletion no longer needs to purge messages to
satisfy `ON DELETE RESTRICT`. `DeleteAgentGroup` now does
`UPDATE agent_messages SET group_id = 0 WHERE group_id = ?` instead of
`DELETE` — a deleted group's conversation history survives as direct
messages. (Human decision, relayed by PO.)

### New permission slug — `message.direct`

`PermMessageDirect = "message.direct"` (`agentd/identity.go`), registered
in `permissionRegistry` (`agentd/permissions.go`). **Not** default-granted.

It is the off-group escape hatch: sending a 1:1 message to an agent you
share no group with — including any ungrouped solo agent — requires this
slug. Intra-group messaging, owner-of-group, and via-link reach need no
slug. Multicast (`group:<name>`) keeps its existing member-or-owner gate.

### Routing (`agentd/handlers.go`)

Two new helpers:

- `holdsPermission(convID, slug)` — non-interactive triple-source check
  (config default-permissions → `agent_permissions` row → active
  `agent_sudo_grants`); the allow-sources of `requirePermission` minus
  the popup.
- `resolveMessageRouting(w, fromID, targetID) (groupID, viaName, ok)` —
  composes the policy. `db.CanSenderReachTarget` first (shared-group /
  owner / via-link → routes through that group, **unchanged** default
  policy); on no group-path, falls back to the `message.direct` slug →
  allowed as a direct message (`group_id 0`); otherwise a 403 naming the
  slug. The slug is a *strict fallback* — a sender that can reach the
  target through a group is still routed through it.

`handleMessages`, `handleMultiRecipient` (its `primaryVia *db.AgentGroup`
param became `primaryGroupID int64` + `primaryViaName string`), and each
`--cc` recipient all route through `resolveMessageRouting`.
`handleMessageReply` handles `group_id == 0` originals — the reply is
itself direct; the old "group no longer exists" 500 is gone.

### Clone / reincarnate solo send-keys fallback removed

`clone` and `reincarnate` now **always** write the handoff as an
`agent_messages` row (`group_id 0` when the successor is solo) and
deliver it through the normal flush/nudge pipeline. Deleted:
`injectFollowUpDirect`, the solo branch + `hasGroup`/`followUp` params of
`runReincarnatePostSpawn`, and `soloFollowUpRejection` (the strict
solo-pane charset gate — every handoff rides the inbox now, so the
lenient `isValidInitialMessage` rule is the only one). A solo handoff is
now identical to a grouped one: persisted, `inbox`-visible,
atomically claim-delivered.

### CLI + skill

- `agent message` / `agent reply` render an off-group send as
  "directly" rather than `via group ""`; `inbox read` shows
  `Group: (direct)` for a `group_id 0` message
  (`agent/message.go`, `agent/reply.go`, `agent/inbox.go`).
- `agent-coord/SKILL.md` corrected: it no longer claims "you can only
  message peers in a group with you" — it now describes the
  `message.direct` requirement for off-group sends.

## Files

- `pkg/claude/common/db/migrate.go` — `migrateV35toV36`, `currentVersion` → 36.
- `pkg/claude/common/db/agent.go` — `DeleteAgentGroup`, `InsertAgentMessage` doc.
- `pkg/claude/agentd/identity.go` — `PermMessageDirect`.
- `pkg/claude/agentd/permissions.go` — registry entry.
- `pkg/claude/agentd/handlers.go` — `holdsPermission`, `resolveMessageRouting`,
  `handleMessages` / `handleMultiRecipient` / `handleMessageReply`;
  `soloFollowUpRejection` deleted.
- `pkg/claude/agentd/reincarnate.go`, `clone.go` — always-inbox handoff;
  `injectFollowUpDirect` deleted.
- `pkg/claude/agent/message.go`, `reply.go`, `inbox.go` — direct-message rendering.
- `pkg/claude/agent/skills/agent-coord/SKILL.md` — messaging-rules accuracy fix.

## Tests

- `pkg/claude/common/db/universal_inbox_test.go` —
  `TestMigrateV35toV36_MakesGroupIDOptional` (rebuild preserves rows,
  FK gone, `group_id 0` insert works) and
  `TestDeleteAgentGroup_PreservesMessagesAsDirect`.
- `pkg/claude/agentd/universal_inbox_flow_test.go` — solo→solo with /
  without the slug, intra-group needs no slug, cross-group requires the
  slug, the slug is a strict fallback, reply-to-direct stays direct,
  `--cc` with an off-group CC.
- `pkg/claude/agentd/handoff_followup_cap_flow_test.go` — the two solo
  tests were inverted: a solo clone/reincarnate handoff now *rides the
  inbox* (`group_id 0`) instead of being rejected.
- `pkg/claude/common/db/agent_test.go` — `TestAgentMessageInsertAndList`
  updated for the preserve-on-delete behaviour.

## Follow-ups (out of scope)

- **`tclaude agent cron` solo path.** `agent_cron.go` has the identical
  `GroupID == 0 → send-keys` fallback; the universal inbox unlocks
  dropping it (solo cron jobs enqueue an `agent_messages` row instead).
- **Popup escape hatch for off-group sends.** `resolveMessageRouting`
  uses non-interactive `holdsPermission` (which already includes
  time-bounded `agent_sudo_grants`); an `X-Tclaude-Ask-Human` popup on
  `POST /v1/messages` was deferred — the body is consumed at auth time
  and the multi-recipient path complicates it.
