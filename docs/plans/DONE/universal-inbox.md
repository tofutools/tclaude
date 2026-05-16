# Universal inbox — decouple agent messaging from groups-as-mechanism

Status: **design of record / implementing** (Phase 3 — PO green light given,
msg #95). Migration numbers and symbol names below are reconciled against
`main` after PRs #99–#107 merged.

## Problem

Today the inbox (`agent_messages`) **requires** a `group_id`. The column is

```sql
group_id INTEGER NOT NULL REFERENCES agent_groups(id) ON DELETE RESTRICT
```

so a row literally cannot be written unless the sender and recipient share a
routing group. Consequences:

- **Solo agents have no inbox.** A conv in no group can neither be messaged
  nor receive a clone/reincarnate handoff through the normal pipeline.
- **Brittle send-keys fallback.** Because solo agents can't get an
  `agent_messages` row, `clone` and `reincarnate` fall back to typing the
  follow-up straight into the new pane via `tmux send-keys`
  (`injectFollowUpDirect` + the solo branch of `runReincarnatePostSpawn` in
  `reincarnate.go` / `clone.go`). That path is racy (keystrokes interleave
  with whatever the TUI is doing), unpersisted (nothing survives if the pane
  isn't ready), and invisible (`inbox ls` never shows it).
- **A whole charset-rule split exists only to serve that fallback.**
  `soloFollowUpRejection` (`handlers.go`) rejects a clone/reincarnate
  follow-up for a *solo* successor unless it clears the strict
  `isValidFollowUp` gate (≤4096 bytes, no control characters — because each
  newline would submit as its own turn when typed into a pane), while a
  *grouped* successor's handoff rides the inbox and may use the lenient
  `isValidInitialMessage` rule (≤16384 bytes, newlines OK). Two rules for
  one logical thing, purely because solo had no inbox.
- **Group membership is overloaded.** It is currently *both* the transport
  mechanism (no group → no row) *and* the authorization policy (shared group
  → allowed to send). Those are two different concerns wearing one hat.

`tclaude agent cron` has the **same** wart: `agent_cron.go` keys its jobs on
`GroupID int64 // 0 → solo (direct send-keys), >0 → enqueue agent_messages`.
Solo cron jobs send-keys too. Not in this feature's scope, but it falls out
for free once the inbox is universal — see "Follow-ups".

## Goal

> Human's steer (verbatim intent): *"All agent-agent messages should go via
> the inbox. The group limitation should be a PERMISSION DEFAULT —
> intra-group messaging allowed by default — NOT a mechanism constraint.
> Messaging OUTSIDE one's group should require elevated permission (direct /
> link / admin-style slugs)."*

Split the two concerns:

- **Mechanism** — `agent_messages` becomes the universal transport. *Any*
  conv → *any* conv, group or no group. The inbox is keyed on `to_conv`,
  which it already is for reads (`ListAgentMessagesForConv`, `inbox ls`).
  Only the write side (the FK) blocks universality today.
- **Policy** — group membership becomes purely an authorization rule:
  *intra-group messaging is allowed by default*. Messaging across a group
  boundary (or to/from an ungrouped agent) requires an elevated permission.

The shipped authorization machinery (`db.CanSenderReachTarget`) already
encodes the *group-routed* policy — shared-group / owner-of-group / via-link.
Tier 2 does **not** reinvent it; it **generalizes** the picture: the inbox is
the mechanism, `CanSenderReachTarget` + one new slug are the policy.

## Design

### (a) Schema change — `agent_messages.group_id` becomes optional

**Decision: keep the column, make `0` a valid value meaning "direct (no
routing group)". Drop the FK.**

Rationale for keep-column-with-`0`-sentinel over re-keying messages off
conv-id:

- `0` is never a real `agent_groups.id` (AUTOINCREMENT starts at 1), so it
  is a safe sentinel — and it is *already* the established convention in
  this codebase: `agent_cron_jobs.group_id` documents `0 → solo`.
- Group-routed messages still want their `group_id`: it drives `via_group`
  in the send response and reply threading. Dropping the column entirely
  would touch every `InsertAgentMessage` call site, multicast, and the
  reply path for no real gain.
- Minimal blast radius — only the FK and the `NOT NULL` semantics change.

New shape:

```sql
group_id INTEGER NOT NULL DEFAULT 0   -- 0 = direct (no routing group)
```

The FK (`REFERENCES agent_groups(id) ON DELETE RESTRICT`) is **dropped**.
`0` could never satisfy a FK anyway, and the FK was only ever load-bearing
as the thing `DeleteAgentGroup` had to work around.

**Migration v35 → v36.** `currentVersion` on `main` is **35** (`migrate.go`);
this migration is `migrateV35toV36` and bumps `currentVersion` to **36**.
(Re-verify the number at implementation time — a parallel branch can grab
v36 first, in which case renumber.) SQLite cannot `ALTER COLUMN` to drop a
`NOT NULL` constraint or an FK, so this is the standard table rebuild.
`agent_messages` is referenced by no other table (`parent_id` is a
self-column with no declared FK), so the rebuild is a straight column-named
copy:

```sql
CREATE TABLE agent_messages_new (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    group_id         INTEGER NOT NULL DEFAULT 0,   -- 0 = direct
    from_conv        TEXT NOT NULL,
    to_conv          TEXT NOT NULL,
    subject          TEXT NOT NULL DEFAULT '',
    body             TEXT NOT NULL DEFAULT '',
    created_at       TEXT NOT NULL,
    delivered_at     TEXT NOT NULL DEFAULT '',
    read_at          TEXT NOT NULL DEFAULT '',
    parent_id        INTEGER NOT NULL DEFAULT 0,
    to_recipients    TEXT NOT NULL DEFAULT '',
    cc_recipients    TEXT NOT NULL DEFAULT '',
    original_to_conv TEXT NOT NULL DEFAULT ''
);
INSERT INTO agent_messages_new
    (id, group_id, from_conv, to_conv, subject, body, created_at,
     delivered_at, read_at, parent_id, to_recipients, cc_recipients,
     original_to_conv)
    SELECT id, group_id, from_conv, to_conv, subject, body, created_at,
           delivered_at, read_at, parent_id, to_recipients, cc_recipients,
           original_to_conv
    FROM agent_messages;
DROP TABLE agent_messages;
ALTER TABLE agent_messages_new RENAME TO agent_messages;
CREATE INDEX IF NOT EXISTS idx_agent_messages_to_conv
    ON agent_messages(to_conv, created_at);
CREATE INDEX IF NOT EXISTS idx_agent_messages_parent
    ON agent_messages(parent_id);
UPDATE schema_version SET version = 36;
```

Existing rows keep their real `group_id` — no data loss, no behavioural
change for already-grouped history.

*FK-pragma note:* the standard "12-step" rebuild disables `foreign_keys`
only when the rebuilt table is *referenced by* other tables (so child rows
aren't orphaned mid-rebuild). Nothing references `agent_messages`, and the
rebuilt table declares no FK, so a straight rebuild is safe with FKs
enforced. Still: write and test the migration deliberately.

**`DeleteAgentGroup` follow-on — Q1 resolved: PRESERVE** (PO, msg #95, human
decision). With the FK gone, the `ON DELETE RESTRICT` protection disappears
and the explicit "purge `agent_messages` first" step in `DeleteAgentGroup`
is no longer required. It changes from

```sql
DELETE FROM agent_messages WHERE group_id = ?
```

to

```sql
UPDATE agent_messages SET group_id = 0 WHERE group_id = ?
```

so deleting a group **preserves** its message history as direct messages
instead of destroying it, and leaves no dangling `group_id`.

`Go` side: `AgentMessage.GroupID int64` is unchanged — `0` is already its
zero value, so `scanAgentMessage` / `InsertAgentMessage` need no struct
change. `InsertAgentMessage` simply stops being rejected by the FK when
`GroupID == 0`.

### (b) How a solo agent gets an inbox

It already has one, structurally. `agent_messages` rows are addressed by
`to_conv`; the read path (`ListAgentMessagesForConv`,
`ListAgentMessagesFromConv`, `inbox ls/read`, the flush/nudge pipeline)
never joins on a group. The **only** thing standing between a solo agent and
a working inbox is the write-side FK. Once (a) lands, *any* conv-id can be a
`to_conv` and `inbox ls` works for it unchanged.

So "give solo agents an inbox" is not new plumbing — it is the removal of
one constraint. Nudge delivery (`nudgeIfAlive`), offline queueing
(`delivered_at`), and flush-on-online (`maybeFlushUndelivered`) are all
already group-agnostic and work for a solo recipient as-is.

### (c) Elevated cross-group messaging — the new slug

**One new permission slug: `message.direct`.**

```
message.direct — Send a 1:1 message to ANY agent regardless of shared-group
                 membership (the off-group escape hatch). Intra-group
                 messaging needs no slug.
```

- **Default: NOT granted.** It is not added to
  `agent.default_permissions`. Cross-group messaging is deliberately
  elevated, per the human's steer.
- Registered in `permissionRegistry` (`permissions.go`) and as a
  `PermMessageDirect = "message.direct"` constant in `identity.go`,
  alongside the existing slugs.
- Name confirmed (PO, msg #71): chosen over `message.cross-group` — which
  is inaccurate, since either party may be ungrouped and so nothing is
  strictly "crossed" — and over `message.any` / `agent.message`. It reads
  cleanly in CLI grants (`tclaude agent permissions grant <conv>
  message.direct`) and establishes a `message.*` namespace.
- Scope: **1:1 direct sends only.** It does *not* authorize multicast into
  a foreign group — `handleMulticast` keeps its member-or-owner gate
  (broadcasting *into* a group you are not part of is a bigger act than
  pinging one agent, and the link mechanism already exists for
  group→group reach).

The human's "direct / link / admin-style slugs" maps to **three** reach
paths, only one of which is a *new* slug:

| Path        | Mechanism                              | New slug?            |
|-------------|----------------------------------------|----------------------|
| direct      | `message.direct` — message any conv    | **yes** — this one   |
| link        | `agent_group_links` (group→group edge) | no — shipped feature |
| admin-style | owner-of-a-group-containing-the-target | no — `agent_group_owners`, shipped |

So Tier 2 adds exactly **one** slug.

### (d) How the default intra-group policy folds into `CanSenderReachTarget`

It already *is* the policy and needs **no change**. `CanSenderReachTarget`'s
first rule — "shared membership: pick the first group both belong to" — is
exactly "intra-group messaging allowed by default". Owner-of-group and
via-link are the second and third rules. The function stays a *pure DB-level
authorization predicate*: it answers "is there a group-policy path?" and
returns the routing group + a reason label.

What changes is *only* the `agentd` handler layer. Permission slugs are an
`agentd` concept (checked against `config` defaults + the
`agent_permissions` table + sudo grants), not a `db`-package concept, so the
slug check stays out of `CanSenderReachTarget`. Today `handleMessages` does:

```go
via, _, err := db.CanSenderReachTarget(fromID, finalConv)
if via == nil { 403 }
```

After Tier 2 it becomes a small composed authorizer — proposed helper
`resolveMessageRouting(w, fromID, targetID) (groupID int64, viaName string,
ok bool)`:

1. `CanSenderReachTarget` — if it returns a group, route through it
   (`groupID = via.ID`, `viaName = via.Name`). This is the default
   intra-group path **plus** owner / link. `requireGroupActive` still
   applies. **Unchanged behaviour.**
2. Otherwise, check the `message.direct` slug via a new `holdsPermission
   (convID, slug)` helper — the same triple-source check the lifecycle
   verbs use (`config` default-permissions → `agent_permissions` row →
   active `agent_sudo_grants`). If held → allow with `groupID = 0`,
   `viaName = ""` (direct). No `requireGroupActive` — there is no group.
3. Otherwise → `403` naming `message.direct` so the agent can tell its
   human what to grant (or get a `tclaude agent sudo` grant).

The slug fallback is **strictly additive** — a sender that *can* reach the
target through a group is still routed through that group (preserving
`via_group` and reply threading). `message.direct` only ever fires when no
group-policy path exists. The same composed check applies uniformly to the
primary recipient and to each `--cc` recipient in `handleMultiRecipient`.

*Popup escape hatch:* `requirePermission` lets a denied agent open an
`X-Tclaude-Ask-Human` approval popup. `resolveMessageRouting` deliberately
uses the non-interactive `holdsPermission` instead, because `handleMessages`
has already consumed the request body (it needs `req.To` to resolve the
target) and the multi-recipient path would otherwise open one popup per CC.
The `agent_sudo_grants` source *is* the time-bounded escape hatch and is
included in `holdsPermission`. Wiring a popup specifically for off-group
sends is a noted follow-up, not part of this PR.

### (e) Fate of the clone / reincarnate solo send-keys fallback — it disappears

After (a), clone and reincarnate **always** insert an `agent_messages` row
for the handoff follow-up — `GroupID = oldMembers[0].GroupID` when grouped,
`GroupID = 0` when solo — and **always** deliver it through the normal
flush/nudge pipeline (`deliverHandoffViaFlush` → `flush` → `nudgeIfAlive`).

Concretely:

- **Delete** `injectFollowUpDirect` (`reincarnate.go`).
- **Delete** the solo branch of `runReincarnatePostSpawn` — the
  `if hasGroup { flush } else { send-keys }` collapses to always-flush.
  `runReincarnatePostSpawn` loses its `hasGroup` (and now-unused
  `followUp`) parameters.
- **Delete** the `else { injectFollowUpDirect }` branch in `clone.go`'s
  follow-up block — clone always does `InsertAgentMessage` +
  `deliverHandoffViaFlush`.
- **Delete `soloFollowUpRejection`** (`handlers.go`) and its two call sites
  (`clone.go`, `reincarnate.go`). With every handoff riding the inbox, the
  strict solo-pane charset rule no longer applies; the lenient
  `isValidInitialMessage` gate already enforced in `decodeReincarnate
  FollowUp` / `decodeCloneBody` is the only rule, for grouped and solo
  alike. Update the comments at the call sites that reference it.
- No slug check on this path: the daemon is performing a lifecycle
  operation it has *already* authorized (`self.clone` / `agent.clone` /
  `self.reincarnate` / `agent.reincarnate`). The handoff insert with
  `GroupID = 0` is a daemon-internal write, not a `POST /v1/messages`, so
  it never touches `message.direct`.

UX consequence, called out so it is a *decision* not a surprise: a solo
handoff stops being raw text typed into the pane and becomes an
`agent_messages` row — the successor sees the standard
`[system: new agent message #N ...]` nudge and runs `tclaude agent inbox
read N`. This makes solo handoffs **consistent** with grouped handoffs
(which already work exactly this way today) and gains persistence +
`inbox ls` visibility + atomic claim-delivery. Confirmed acceptable by PO
(msg #71).

### Reply path

`handleMessageReply` currently does `db.GetAgentGroupByID(orig.GroupID)` and
**500s** ("original message references a group that no longer exists") if
the group is gone. With direct messages, `orig.GroupID == 0`:

- `orig.GroupID == 0` → the reply is itself direct: insert with
  `GroupID = 0`, `via_group ""`, skip the group lookup and
  `requireGroupActive`. Replies are already allowed back to the sender
  regardless of current group membership, so no extra authorization is
  needed for the reply direction.
- `orig.GroupID > 0` but the group was since deleted → with Q1 resolved
  to PRESERVE, `DeleteAgentGroup` rewrites those rows to `group_id = 0`,
  so this case folds into the above. The stale-group `500` becomes
  unreachable and is removed; defensively, a `nil` group from
  `GetAgentGroupByID` is treated as direct rather than erroring.

### Touch-point checklist (for the impl PR)

- `db/migrate.go` — `migrateV35toV36` (table rebuild), bump
  `currentVersion` to 36.
- `db/agent.go` — `InsertAgentMessage` (no struct change; works with
  `GroupID == 0`), `DeleteAgentGroup` (purge → `SET group_id = 0`).
  Update the stale FK comment on `DeleteAgentGroup`.
- `agentd/identity.go` — `PermMessageDirect` constant.
- `agentd/permissions.go` — register `message.direct` in
  `permissionRegistry`.
- `agentd/handlers.go` — `holdsPermission` + `resolveMessageRouting`
  helpers; `handleMessages` + `handleMultiRecipient` use them
  (`handleMultiRecipient`'s `primaryVia *db.AgentGroup` param becomes
  `primaryGroupID int64` + `primaryViaName string`); `handleMessageReply`
  handles `GroupID == 0`; delete `soloFollowUpRejection`.
- `agentd/reincarnate.go` — delete `injectFollowUpDirect`, drop the solo
  branch + `hasGroup`/`followUp` params of `runReincarnatePostSpawn`,
  always `InsertAgentMessage` + flush; drop the `soloFollowUpRejection`
  call.
- `agentd/clone.go` — delete the `injectFollowUpDirect` branch, always
  `InsertAgentMessage` + `deliverHandoffViaFlush`; drop the
  `soloFollowUpRejection` call.
- CLI display — `agent message` / `inbox read` rendering of an empty
  `via_group` (show `(direct)` or omit, rather than a blank `Group:`).
- Skill docs — `agent/skills/agent-coord` (mention `message.direct` and
  that off-group sends need it).

### (f) Flow-test plan

Flow tests under `pkg/claude/agentd/*_flow_test.go`, testharness v2 (`Flow`
DSL — `newFlow(t)`, `HaveGroup` / `HaveMember` / `HaveAliveSession` /
`HaveEnrolledAgent`, `AsAgent` / `AsHuman`, `Reincarnate` / `CloneFresh`,
`AssertSentContains`, `f.do(...)` for raw HTTP). A grant in a test is just
`db.GrantAgentPermission(conv, agentd.PermMessageDirect, "test")`.

New `universal_inbox_flow_test.go`:

1. **Solo → solo with `message.direct`.** Two enrolled agents, neither in
   any group; sender granted `message.direct`. `POST /v1/messages`
   succeeds, row written with `group_id = 0`, recipient's
   `ListAgentMessagesForConv` shows it, an alive recipient gets the
   `[system: new agent message #N]` nudge.
2. **Solo → solo WITHOUT the slug → 403** naming `message.direct`. No row
   written.
3. **Intra-group still needs no slug.** Two members of one group, neither
   holding `message.direct` → send succeeds, `via_group` = the group name,
   `group_id` = that group.
4. **Cross-group, no link / no shared group / not owner.** A in group X,
   B in group Y. Without `message.direct` → 403. With it → 200, row has
   `group_id = 0`, `via_group` empty.
5. **`message.direct` is a strict fallback.** Sender that *does* share a
   group with the target still routes via the group even while holding
   `message.direct` (`via_group` non-empty, `group_id > 0`).
6. **Reincarnate of a solo agent.** Alive solo session, no group;
   `Reincarnate(target, "follow up")`. Assert: a `group_id = 0`
   `agent_messages` row addressed to the new conv exists; it is delivered
   via the flush pipeline (the `[system: new agent message #N]` nudge
   lands in the new pane); the pane did **not** receive the literal
   follow-up string as a bare prompt (`injectFollowUpDirect` is gone).
7. **Clone of a solo agent.** Same as 6 for `CloneFresh` + a follow-up.
8. **Reply to a direct message.** Recipient of a `group_id = 0` message
   replies via `POST /v1/messages/{id}/reply`; reply row also has
   `group_id = 0`; lands in original sender's inbox. No "group no longer
   exists" error.
9. **`--cc` with an off-group CC.** Primary in-group, one CC off-group:
   without `message.direct` → 403 (whole send rejected, pre-validation);
   with it → all rows written, off-group CC row has `group_id = 0`.
10. **Group deletion preserves history.** Group with messages →
    `groups rm` → group gone, message rows survive with `group_id = 0`,
    still in both parties' `ListAgentMessagesForConv`.

DB-level / migration test in `pkg/claude/common/db/` (`migrate` or
`agent_test.go`): a v35 DB with existing `agent_messages` rows migrates to
v36 with `group_id` values preserved; a subsequent `InsertAgentMessage`
with `GroupID: 0` succeeds (would have failed the FK pre-migration).

## Rollout

- **Phase 1 — design (this doc).** ✅ PR #102 (doc only).
- **Phase 2 — park.** ✅ Held until the in-flight `alias-removal`,
  `spawn-guardrails`, and Tier 1 cap PRs (now #99–#107) merged.
- **Phase 3 — implement.** ⏳ In progress: fresh worktree, separate PR
  rebased on current `main`, with the flow tests above. This doc moves to
  `docs/plans/DONE/` as part of that PR.

## Follow-ups (out of scope, noted)

- **`tclaude agent cron` solo path.** `agent_cron.go` has the identical
  `GroupID == 0 → send-keys` fallback. Once the inbox is universal, solo
  cron jobs can drop their send-keys branch and always enqueue an
  `agent_messages` row. Natural fast-follow; not bundled here to keep the
  PR scoped.
- **Popup escape hatch for off-group sends.** `resolveMessageRouting` uses
  the non-interactive `holdsPermission` (which already includes
  time-bounded `agent_sudo_grants`). Wiring an `X-Tclaude-Ask-Human`
  popup specifically for `POST /v1/messages` would let an agent ask the
  human for a one-off off-group send; deferred because the body is already
  consumed at auth time and the multi-recipient path complicates it.

## Open questions

All resolved.

- **Slug name → `message.direct`.** Confirmed (PO, msg #71).
- **Solo handoff via inbox → yes.** Confirmed (PO, msg #71).
- **`DeleteAgentGroup` on group delete → PRESERVE** via
  `UPDATE agent_messages SET group_id = 0`. Confirmed (PO, msg #95 —
  human decision).
