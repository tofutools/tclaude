# Dashboard: group membership editing UX (Part 1B — Ctrl-clone)

Drag-and-drop **Move** shipped 2026-05; see
[`DONE/dashboard-dnd-move.md`](../../DONE/dashboard-dnd-move.md). The
[+ add member overlay](../../DONE/dashboard-add-member-overlay.md)
shipped earlier the same week. This file now tracks **only the
remaining Ctrl-clone slice** + the deferred ungrouped-as-source
detail.

## Ctrl-clone

**Ctrl+drag** a member row from group A onto group B's header →
drop a clone of the source row into B; original stays in A. Uses
the already-shipped `POST /v1/agent/{conv}/clone` endpoint (the
same one `tclaude agent clone` calls). Inherits the `-clone-<N>`
alias suffix scheme already wired into `runCloneOrchestration`.

A **modifier-hint pill** (`→ move`, `→ clone`) follows the cursor
during the drag so the user can see which behaviour they're about
to commit before releasing. With Ctrl-clone added there's a real
1-of-2 disambiguation that earns the pill's weight (with Move
alone the drop-target highlight was sufficient).

### Daemon endpoint (need to add a cookie-auth twin)

The shipped `POST /v1/agent/{conv}/clone` is on the `/v1` socket
mux (peer-cred auth); the dashboard speaks cookie-auth on the
loopback HTTP server. Add a `POST /api/agents/{conv}/clone` twin
that calls the same `runCloneOrchestration` helper with `caller`
recorded as the dashboard granter (`<human-dashboard>`).

The endpoint should accept the same `{follow_up?, no_copy_conv?}`
body the v1 endpoint takes, plus an optional `target_group` field
that triggers the post-clone `POST groups/{target_group}/members`
follow-up so the clone lands in the requested group when it isn't
already a member of one of the source's groups.

### JS

On `dragstart`, attach a `dragover`-tracking listener that flips
the cursor pill text + the drop effect (`copy` for Ctrl, `move`
otherwise). On `drop` with `e.ctrlKey === true`, fire the new
`/api/agents/{conv}/clone` endpoint with the target group rather
than the move sequence. Optimistic UI is harder for clone (the
new conv-id isn't known until the response lands), so just await
the response and trigger a `refresh()` on success — no local
optimistic mutation.

### Test coverage

Per project convention: flow test under
`pkg/claude/agentd/*_flow_test.go`:

- **Clone-into-group** — POST `/api/agents/{conv}/clone` with a
  `target_group` body field; assert the clone lands in target
  group with a `-clone-<N>` alias and the original is untouched in
  source.

## Ungrouped virtual group as drag SOURCE (deferred)

The `+ add member` overlay already covers this flow (arrow-nav +
Enter from a candidate list that includes ungrouped[]). Pinned-row
DnD source for ungrouped agents stays open as a possible future
slice if the overlay UX turns out to be too keyboard-heavy in
practice. No daemon work needed when it ships — same
`/api/snapshot.ungrouped[]` foundation.

## Out of scope (deferred to later slices)

- **Shift+drag (multi-membership).** Add to B without removing from
  A. Useful eventually; ship after Ctrl-clone bakes.
- **Drop ON the ungrouped row** to remove from every group ("kick
  from all groups"). Destructive and easy to misclick; defer.
- **Cross-group bulk move/clone via multi-select.** Single-row
  first.
- **Inline group rename.** Already shipped (separate item).

## Files (when implementing)

- `pkg/claude/agentd/dashboard.html` — modifier-hint pill CSS +
  JS branch on `e.ctrlKey` in the existing `bindDnd()` handler
- `pkg/claude/agentd/dashboard_edit.go` — `dashboardCloneAgent`
  helper + dispatcher branch on `/api/agents/{conv}/clone`
- `pkg/claude/agentd/clone.go` — sanity-check `runCloneOrchestration`
  is reachable from the dashboard-cookie path with no slug
  prerequisite

## Cross-references

- [`DONE/dashboard-dnd-move.md`](../../DONE/dashboard-dnd-move.md)
  — Move shipped; this file is now the remaining slice
- [`DONE/dashboard-add-member-overlay.md`](../../DONE/dashboard-add-member-overlay.md)
  — covers the ungrouped-as-source flow today
- [`DONE/dashboard-snapshot-ungrouped.md`](../../DONE/dashboard-snapshot-ungrouped.md)
  — backend foundation, untouched

## When done

Move this file to `DONE/dashboard-dnd-clone.md` and rewrite to
describe what shipped.
