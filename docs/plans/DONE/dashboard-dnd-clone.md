# Dashboard: drag-and-drop Ctrl-clone into a group

Shipped 2026-05.

Closes the Ctrl-clone slice of
[`TODO/high-prio/dashboard-group-membership-ux.md`](../TODO/high-prio/dashboard-group-membership-ux.md)
on top of the just-shipped Move
([`DONE/dashboard-dnd-move.md`](dashboard-dnd-move.md)). With both
Move and Ctrl-clone landed, the only remaining slice in the parent
TODO is the deferred-by-design ungrouped-as-source pattern (already
covered by the `+ add member` overlay).

## What ships

**Ctrl+drag** (or **Cmd+drag** on macOS) a member row from group A
onto group B's `<summary>` header → fork a sibling clone of the
source row that lands in B. The original stays untouched in A.

The drag-and-drop flow now disambiguates two effects via the
modifier:

- No modifier → **move** (shipped 3b18b2a).
- Ctrl / Cmd → **clone**.

A small modifier-hint pill (`→ move` / `→ clone`) tracks the cursor
during the drag and flips text + color (blue / green) live as the
modifier is held / released. The drop-target summary's outline also
flips green for the clone effect so the visual state on the target
matches the pill.

## JS

Same `bindDnd()` block that ships Move; one extra branch on
`e.ctrlKey || e.metaKey` at drop time:

- `dragstart` sets `effectAllowed = 'copyMove'` so both effects can
  resolve through `dropEffect` on `dragover`.
- `dragover` reads the modifier each tick (it can change mid-drag),
  flips `dropEffect` + the pill class + the target outline class
  accordingly.
- `drop` dispatches to `runDndClone` (Ctrl) or `runDndMove`
  (no modifier).

`runDndClone` issues two calls:

1. **`POST /api/agents/{conv}/clone`** — daemon forks a sibling
   that inherits source's identity (groups / perms / ownership)
   plus the per-group `-c-<N>` alias suffix scheme.
2. **`POST /api/groups/{target}/members`** — adds the new conv to
   the drop-target group. Idempotent if the clone already inherited
   that group from the source's memberships (the
   "Ctrl-drag onto source group" sibling case).

No optimistic UI for clone — the new conv-id isn't known until the
response lands; inventing a placeholder row would confuse the user
when the real conv-id replaces it on the next poll. We just await
both calls and let the next 5s poll render canonical state.

## Daemon endpoint (new cookie-auth twin)

`POST /api/agents/{conv}/clone` is the cookie-auth twin of the
shipped `POST /v1/agent/{conv}/clone`. Same body shape
(`{follow_up?, no_copy_conv?}`). Auth is the dashboard cookie
(human bypass), so no slug check; the audit trail records
`<human-dashboard>` as the granter via the existing
`runCloneOrchestration` granter compose path
(`system:clone:by=<human-dashboard>`).

Wired through `handleDashboardAgentsAPI` as a third sub-verb
alongside `stop` and `resume`. The handler's `decodeCloneBody`
helper is shared with the v1 endpoint so validation rules stay in
lockstep (follow-up charset, etc.).

## Tests

`pkg/claude/agentd/dashboard_dnd_clone_flow_test.go`:

- `TestDashboardDnDClone_PostsCloneThenAddsToTargetGroup` — runs
  the same POST → POST sequence the JS issues; asserts the clone
  inherits source's per-group membership AND lands in the target
  group with the inherited `-c-<N>` alias intact. Original
  untouched in source; doesn't appear in the target.
- `TestDashboardDnDClone_OntoSourceGroupYieldsSibling` — pins the
  "Ctrl+drag onto source group" branch the JS allows: a single
  clone POST yields original + sibling in the same group. Confirms
  drop-onto-self for clone is not a no-op (unlike move).

JS-side modifier-hint pill / pointer offset / outline flipping
isn't covered by Go flow tests (no JS test infra yet); the daemon
contract is what the JS leans on.

## Files

- `pkg/claude/agentd/dashboard.html` — `.dnd-mod-pill` CSS,
  `.dnd-effect-clone` outline-color override, `#dnd-pill` markup,
  `updateDndPill(e, hovering, isClone)` helper, `bindDnd()`
  branches on `e.ctrlKey || e.metaKey`, new `runDndClone(payload,
  targetGroup)` function.
- `pkg/claude/agentd/dashboard_edit.go` — `dashboardCloneAgent`
  helper (cookie-auth twin of `handleAgentClone`),
  `handleDashboardAgentsAPI` sub-verb dispatch on `clone`.
- `pkg/claude/agentd/dashboard_dnd_clone_flow_test.go` — 2 flow
  tests for the daemon-side sequence.

## Out of scope (deferred)

- **Clone with target alias / role / descr.** The new clone joins
  the target group with the inherited `-c-<N>` alias; if the human
  wants to refine it, the existing per-row edit-member modal
  handles that post-clone. Adding a "set alias inline at clone
  time" prompt would clutter the drag UX.
- **Ctrl-drag onto the dashboard header / outside any group.**
  Treated as a cancelled drag (no drop target). Drop-onto-empty-area
  could fork the source into the ungrouped pool, but ungrouped[]
  isn't a virtual group in the UI yet (the parent TODO's "ungrouped
  virtual group" deferred slice).
- **Shift+drag (multi-membership add-without-remove).** Different
  modifier; design-defer until Ctrl-clone bakes.

## Cross-references

- [`DONE/dashboard-dnd-move.md`](dashboard-dnd-move.md) — Move
  shipped first; this file extends the same `bindDnd()` block.
- [`DONE/dashboard-add-member-overlay.md`](dashboard-add-member-overlay.md)
  — covers the "drag from ungrouped" use case via the keyboard
  candidate-list overlay.
- [`DONE/clone-and-suffix-scheme.md`](clone-and-suffix-scheme.md)
  — the `-c-<N>` alias scheme this leans on.
