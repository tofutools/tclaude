# Dashboard: drag-and-drop move between groups

Shipped 2026-05.

Part 1 of the drag-and-drop carve-out from
[`TODO/high-prio/dashboard-group-membership-ux.md`](../TODO/high-prio/dashboard-group-membership-ux.md).
Ships **Move (no modifier)** only; Ctrl-clone and the
ungrouped-virtual-group drag source remain open in the parent TODO
(both genuinely additive, neither blocked on Move).

## What ships

Member rows in the Groups tab are now `draggable`. Dragging one onto
another group's `<summary>` header moves the conv: it joins the
target group and leaves the source.

Visual states:

- Source row dims (`opacity: 0.45`, subtle background) while a drag
  is in flight so the human can track its origin across the Groups
  tab.
- Drop-target summaries pulse a soft blue with a dashed outline on
  hover (`background: #1f3148`, `outline: 1px dashed #58a6ff`).
- The source's own summary stays un-highlighted — drop-onto-self is
  detected and treated as a no-op rather than rendering a useless
  highlight.

The drag uses HTML5 native DnD (`dragstart`/`dragover`/`drop` event
delegation at the document level, so the bindings survive every
re-render). Payload is JSON-encoded `{conv, sourceGroup, label}` on
the DataTransfer's `application/x-tclaude-member` MIME slot, with
`text/plain` as a fallback channel for browsers that strip the
custom type.

## Optimistic UI + rollback

The drop handler runs three steps in order:

1. **Optimistic local mutation.** Splice the member out of
   `lastSnapshot.groups[source].members` and append onto
   `lastSnapshot.groups[target].members`, then `renderGroupsTab()`.
   The user sees the move land immediately.
2. **`POST /api/groups/{target}/members`** with the conv body. POST
   first matters: on a failed DELETE later, the conv ends up in
   BOTH groups (visible + recoverable) rather than nowhere
   (silently lost). On a failed POST, the local mutation rolls
   back: re-insert at the original index so the visible ordering
   doesn't drift, then re-render.
3. **`DELETE /api/groups/{source}/members/{conv}`**. On success the
   move is committed; the next 5s poll confirms canonical state. On
   failure (rare — e.g. external write removed the row first), the
   optimistic mutation **stays** because the daemon really did add
   it to the target. A "partial" toast surfaces the dual-membership
   state so the human can clean up manually.

Auto-refresh suspends via the existing `modalEditing` flag while a
drag is active, so a 5s tick can't overwrite the optimistic state
mid-round-trip. `dragend` clears the flag and triggers a final
`refresh()` to reconcile.

## Why no modifier-hint pill yet

The TODO doc described a small ghost cursor pill (`→ move`,
`→ clone`) following the cursor during a drag. Punted to the
follow-up Ctrl-clone slice — the pill only earns its weight when
there's >1 effect to disambiguate. With Move alone the visual state
on the drop target (`dnd-drop-over` outline) is sufficient and the
pill would just add CSS surface for no information gain.

## Daemon endpoints — all shipped already

The dashboard cookie twins for these landed with the
`+ add member` overlay carve-out. No new daemon code in this commit.

| Step | Endpoint |
|------|----------|
| Add to target | `POST /api/groups/{name}/members` (cookie-auth twin of `/v1/groups/{name}/members`) |
| Remove from source | `DELETE /api/groups/{name}/members/{conv}` |

## Tests

`pkg/claude/agentd/dashboard_dnd_move_flow_test.go`:

- `TestDashboardDnDMove_AddThenRemoveLeavesConvInTargetOnly` — runs
  the same POST → DELETE sequence the JS issues; asserts the next
  snapshot shows the conv in the target group only AND the
  per-agent `groups[]` array reflects the move (the surface the
  Agents tab reads).
- `TestDashboardDnDMove_PartialFailureLeavesConvInBoth` — pins the
  daemon-side state for the partial-failure branch the JS rollback
  doesn't unwind: POST B succeeds, DELETE A is forced to fail, the
  conv shows up in BOTH groups in the next snapshot, matching the
  "visible + recoverable" claim.

JS-side rollback / optimistic mutation isn't covered by Go flow
tests (no JS test infra yet); the daemon-side guarantee is what the
JS leans on, and the manual smoke tested the full drop flow in a
browser.

## Files

- `pkg/claude/agentd/dashboard.html` — CSS for `.dnd-draggable`,
  `.dnd-source-row`, `summary.dnd-drop-over`. Member `<tr>` gains
  `draggable="true"` + `data-dnd-conv` / `data-dnd-source-group` /
  `data-dnd-label` attributes. Group `<summary>` gains
  `data-dnd-target-group`. New `bindDnd()` helper wired into the
  init block alongside the other binders. New `runDndMove(payload,
  targetGroup)` does the optimistic mutation + POST → DELETE
  sequence + rollback.
- `pkg/claude/agentd/dashboard_dnd_move_flow_test.go` — 2 flow
  tests for the daemon-side sequence.

## Out of scope (explicitly deferred)

- **Ctrl-clone.** Drops a clone of the source row into the target
  group; original stays in source. Needs a new cookie-auth dashboard
  endpoint twin (`/api/agents/{conv}/clone`), the modifier-hint pill,
  and a small JS branch on `e.ctrlKey` at drop time. Tracked in the
  parent TODO.
- **Modifier-hint pill** following the cursor. Earns its weight only
  alongside Ctrl-clone (1-of-2 effects to disambiguate); punted with
  the clone slice.
- **Ungrouped virtual group as drag SOURCE.** The
  `+ add member` overlay already covers this flow with arrow-nav +
  Enter; revisit if the DnD-from-ungrouped UX shows up as a real
  ask after the move + clone affordances bake.
- **Shift+drag (multi-membership).** Add to B without removing from
  A. Useful eventually; ship after move + clone bake.
- **Drop ON the ungrouped row** to remove from every group. Easy to
  misclick + destructive; deferred.
- **Cross-group bulk multi-select drag.** Single-row first.

## Cross-references

- [`DONE/dashboard-add-member-overlay.md`](dashboard-add-member-overlay.md)
  — Part 2 of the parent feature. Same data foundation
  (`POST /api/groups/{name}/members`).
- [`TODO/high-prio/dashboard-group-membership-ux.md`](../TODO/high-prio/dashboard-group-membership-ux.md)
  — parent file; trimmed to track only Ctrl-clone and the
  ungrouped-as-source slice.
