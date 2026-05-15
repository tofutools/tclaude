# Dashboard enrollment-surface fixes (2026-05)

A batch of correctness fixes + UX adjustments to the dashboard's
agent surfaces, all rooted in the agent-enrollment model
(`agent-enrollment.md`).

## 1. Promoted offline conversations now reach the Ungrouped group

**Bug:** clicking *promote* on a conversation in the Groups tab made
it an agent (it appeared on the Agents tab roster) but it never
showed up in the virtual "Ungrouped" group, so it couldn't be
dragged into a real group.

**Cause — two layers:**

- `handleDashboardSnapshot` gated the `ungrouped[]` array on
  `a.Online`. A freshly-promoted conversation usually has no live
  tmux session, so the online gate dropped it.
- The frontend `virtualUngroupedGroup` *also* re-filtered to
  `a.online` ("defensively, so a future backend change can't leak an
  offline row") — so even after the backend gate was removed the
  Ungrouped group still dropped offline agents.

**Fix:**

- Backend: `ungrouped[]` is no longer online-gated — it means "every
  active agent in no group", online or offline. `snapshotPayload.Ungrouped`
  doc updated.
- Frontend: `virtualUngroupedGroup` keeps every row; `renderVirtualGroup`
  now applies the tab-wide "show offline" checkbox at render time,
  exactly like a real group (hidden members still count toward the
  header, "N offline hidden" note shown).
- The `+ add member` overlay applies its own online filter on top, so
  offline rows don't leak into that live-roster picker.

## 2. Agents tab: Retired + Conversations as collapsible sections

**Gap:** the Agents tab's *Conversations* sub-list (promotion
candidates) was always rendered, with no way to fold it away, and it
sat *above* the (already collapsible) *Retired agents* section.

**Fix:** the two secondary sections are now both collapsible
(collapsed by default, caret toggle — `bindSubListToggle` factored
out of the old retired-only handler) and reordered so **Retired
agents comes before Conversations**. Each header carries a `(N)` /
`(none)` count. Pure client-side view state, not persisted —
matching the pre-existing Retired-section behaviour.

## 3. Drag an agent onto the Conversations group → retire it

The virtual "Conversations" group on the Groups tab is now a drag
**target**. Dropping an agent row (a real-group member or a virtual-
Ungrouped row) onto its header retires the agent — demoting it back
to a plain conversation via `POST /api/agents/{conv}/retire`. A
confirm modal (the same one the per-row *retire* button uses) guards
it, since retire revokes group memberships + grants.

## 4. Drag a conversation onto the Ungrouped group → promote, no group

The virtual "Ungrouped" group is now also a drop target for
conversation rows. Dropping a Conversations-group row onto the
Ungrouped header promotes it (`POST /api/agents/{conv}/promote`) but
joins it to no group — it lands directly in the Ungrouped virtual
group.

This required splitting the drag-source flag: Conversations-group
rows used to reuse `data-dnd-source-ungrouped`, conflating "a
conversation" with "an ungrouped agent". They now carry
`data-dnd-source-conversation`, so the drop handler can tell a
demote-target no-op from a real op and route promote vs add
correctly. The full Groups-tab DnD matrix:

| source ↓ \ target → | real group   | Ungrouped         | Conversations |
|---------------------|--------------|-------------------|---------------|
| real-group member   | move / clone | remove from group | retire        |
| Ungrouped agent     | add / clone  | (no-op)           | retire        |
| conversation        | promote+add  | promote (no group)| (no-op)       |

## 5. Reincarnation-predecessor ghost agents

**Bug:** reincarnated agents left a trail of ghost agents on the
roster — one per predecessor conv in the succession chain. None of
them could be retired: `Retire failed: … "conv <head> is not an
active agent (enrollment: retired)"`.

**Cause:** the v29→v30 enrollment backfill (`backfillAgentEnrollment`)
drew conv-ids from `agent_conv_succession.old_conv_id`, so every
reincarnation predecessor got enrolled as an active agent. The live
reincarnate path (`reincarnate.go`) deletes a predecessor's
enrollment, but the migration never applied that rule to historical
chains. The ghosts were un-retireable because the enrollment verbs
resolve their selector through `ResolveSelector` →
`ResolveLatestConv`, which walks the succession chain forward — so
retiring a predecessor actually hit the chain head, and once the head
was retired every attempt 409'd.

**Fix — three layers:**

- **Migration v30→v31** (`migrateV30toV31`) — deletes enrollment rows
  for every conv that appears as an `old_conv_id` in
  `agent_conv_succession`. Cleans already-upgraded databases. On the
  reporting user's DB this dropped the active-agent count from 10 to 2.
- **`backfillAgentEnrollment` fixed** — drops the `old_conv_id` UNION
  arm and adds a `conv_id NOT IN (SELECT old_conv_id …)` WHERE
  exclusion, so a predecessor stays un-enrolled even when it is also
  referenced by another agentic table (it almost always is —
  `agent_messages`, `agent_workdir`). The chain head only appears as
  `new_conv_id`, so it is kept.
- **Read-time guard** — `handleDashboardSnapshot` builds a
  `supersededSet` from `db.ListAgentConvSuccessions()` and skips those
  convs when populating `agents[]` / `ungrouped[]`, symmetric with the
  existing `retiredSet` belt-and-braces guard. Catches a ghost left by
  a partially-applied reincarnate.

`currentVersion` bumped to 31.

## Files

- `pkg/claude/common/db/migrate.go` — `currentVersion` 30→31,
  `migrateV30toV31`, `backfillAgentEnrollment` fix.
- `pkg/claude/agentd/dashboard.go` — `ungrouped[]` online gate
  removed; `supersededSet` guard; `snapshotPayload.Ungrouped` doc.
- `pkg/claude/agentd/dashboard.html` — `virtualUngroupedGroup` /
  `renderVirtualGroup` offline handling; Agents-tab section reorder +
  collapsible Conversations; virtual Conversations group as a drag
  target; `data-dnd-source-conversation` split; `runDndRetireToConversation`
  / `runDndPromoteToUngrouped`.

## Tests

- `db/agent_enrollment_test.go` — `TestBackfillAgentEnrollment`
  updated (a superseded predecessor is NOT enrolled, even with a
  workdir row); new `TestMigrateV30toV31RemovesSupersededEnrollments`.
- `agentd/agent_enrollment_flow_test.go` — new
  `TestEnrollment_PromoteOfflineConvSurfacesInUngrouped` and
  `TestEnrollment_SupersededPredecessorIsNotAnAgent`.
- `agentd/dashboard_ungrouped_flow_test.go` /
  `dashboard_ungrouped_dnd_flow_test.go` — the two tests that pinned
  the old online-only `ungrouped[]` contract
  (`…UngroupedFiltersOfflineSessions`,
  `…UngroupedExcludesOfflineGrantHolders`) rewritten to assert offline
  ungrouped agents / grant-holders DO appear.
- The new drag gestures (3, 4) are frontend wiring over the existing
  `POST /api/agents/{conv}/retire` and `/promote` endpoints, already
  covered by `TestEnrollment_RetireDemotesAndRevokes` and
  `TestEnrollment_PromoteOfflineConvSurfacesInUngrouped`. The Agents-tab
  section reorder / collapsible UI (2) is pure HTML/JS — not reachable
  by the Go flow harness.

## Cross-references

- `agent-enrollment.md` — the enrollment model; its backfill no
  longer enrolls succession predecessors.
- `dashboard-snapshot-ungrouped.md` / `dashboard-ungrouped-virtual-group.md`
  — the `ungrouped[]` array; it is no longer online-only.
- `dashboard-dnd-move.md` / `dashboard-dnd-clone.md` — the
  drag-and-drop infrastructure this extends.
