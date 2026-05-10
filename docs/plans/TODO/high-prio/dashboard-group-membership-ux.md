# Dashboard: group membership editing UX (Part 1 — drag-and-drop)

Direct-manipulation editing on the Groups tab. The originally-paired
overlay (`+ add member`) shipped 2026-05; see
[`DONE/dashboard-add-member-overlay.md`](../../DONE/dashboard-add-member-overlay.md).
This file now tracks **only** the drag-and-drop half.

## Drag-and-drop

Three behaviours, in priority order:

1. **Move (no modifier).** Drag a member row from group A onto group
   B's header → the conv leaves A and joins B. Order the calls:
   `POST groups/B/members` then `DELETE groups/A/members/{conv}`,
   so the conv is never groupless mid-drag. On a failed delete the
   conv ends up in both groups (visible, recoverable) rather than
   nowhere (silently lost).
2. **Clone (Ctrl+drag).** Drops an `agent clone` of the source row
   into the target group; original stays in A. Uses the already-
   shipped `POST /v1/agent/{conv}/clone` endpoint (the same one
   `tclaude agent clone` calls). Inherits the `-clone-<N>` alias
   suffix scheme already wired into `runCloneOrchestration`.
3. **Ungrouped virtual group.** Pinned row at the top or bottom of
   the Groups tab listing every conv-id that is online OR has a
   recent session AND holds zero `agent_group_members` rows. Acts
   as a drag SOURCE: drag an ungrouped agent into a real group to
   add it. Hidden / shows an empty-state line when the set is empty.

Drop targets pulse on hover. A small modifier-hint pill ("→ move",
"→ clone") follows the cursor while dragging so the user can see
which behaviour they're about to commit before releasing.

## Daemon endpoints (all already ship)

| Behaviour | Endpoint(s) |
|-----------|-------------|
| Move | `POST /v1/groups/{B}/members` (also at `/api/groups/{B}/members` for dashboard cookie auth, shipped with the add-member overlay) then `DELETE /v1/groups/{A}/members/{conv}` (also `/api/groups/{A}/members/{conv}`) |
| Clone | `POST /v1/agent/{conv}/clone` (target-group in body) |
| Ungrouped enum source | shipped — `/api/snapshot.ungrouped[]`, see [`DONE/dashboard-snapshot-ungrouped.md`](../../DONE/dashboard-snapshot-ungrouped.md) |

Auth: dashboard cookie. Humans bypass slug checks; the underlying
mutations go through the same daemon code paths the CLI uses, so
audit columns (`granted_by`) get filled the usual way.

## Optimistic UI + rollback

Feels broken without optimistic updates — the dropped row should
appear in the target group immediately, not after the next 5-second
poll. On success: nothing more to do (the next poll confirms). On
failure: snap back to the prior state and surface the error in a
toast. The add-member overlay already pioneered this pattern in
vanilla JS; reuse the same approach.

## Framework question

The original parent doc gated this on a JS framework migration.
After 5+ dashboard features shipped in vanilla JS (rename modal,
edit-member modal, wake/shutdown buttons, groups clone, add-member
overlay), the migration is no longer a hard prerequisite — but DnD
has higher state-juggling needs (ghost images / drop-zone hover /
modifier-hint pill / multi-step optimistic flow) than any of the
shipped features. **Decide before starting**: vanilla JS with
discipline, or migrate first.

## Explicitly out of scope (deferred)

- **Shift+drag (multi-membership).** Add to B without removing from
  A. Useful eventually; ship after move + clone bake.
- **Drop ON the ungrouped row** to remove from every group ("kick
  from all groups I'm in"). Destructive and easy to misclick.
- **Inline rename.** Already shipped (separate item).
- **Cross-group bulk move/clone via multi-select.** Single-row DnD
  first.

## Test coverage

Per project convention: add a flow test under
`pkg/claude/agentd/*_flow_test.go` exercising:

- **Move**: set up agent in group A, call the add+remove sequence,
  assert membership in B and not A via the production read path
  (`GET /v1/groups/{name}/members` and snapshot).
- **Clone-into-group**: assert the clone lands in target group with
  a `-clone-<N>` alias and the original is untouched in source.

The DnD interaction itself is JS — out of scope for Go flow tests.

## Files (when implementing)

- `pkg/claude/agentd/dashboard.html` (or new component tree under
  `pkg/claude/agentd/dashboard/` post-migration)
- `pkg/claude/agentd/dashboard_edit.go` — sanity-check the clone
  path is reachable from the dashboard cookie auth (the move
  endpoints already are)

## Cross-references

- [`DONE/dashboard-add-member-overlay.md`](../../DONE/dashboard-add-member-overlay.md)
  — Part 2 of this feature, shipped
- [`DONE/dashboard-snapshot-ungrouped.md`](../../DONE/dashboard-snapshot-ungrouped.md)
  — backend foundation for the ungrouped virtual group
- [`TODO/med-prio/web-dashboard.md`](../med-prio/web-dashboard.md)
  — parent v2 dashboard plan (DnD reference here, no edits)

## When done

Move this file to `DONE/dashboard-group-membership-dnd.md` and
rewrite to describe what shipped.
