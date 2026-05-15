# Dashboard "Ungrouped" virtual group (2026-05)

The dashboard's Groups tab now renders a virtual **"Ungrouped"**
group, always last in the listing, holding every online agent that
isn't a member of any real group. It is the drag-and-drop hub for
membership management — and the place orphaned agents resurface when
their group is deleted.

Builds directly on `dashboard-snapshot-ungrouped.md` (the backend
`ungrouped[]` array). Frontend-only feature work plus one small
backend correctness fix.

## What it does

- **Renders** a synthetic group from `snapshot.ungrouped[]`, appended
  after the real groups so it always sorts to the bottom. Shown only
  when non-empty (an empty derived bucket is just noise).
- **Hideable** via a new "show ungrouped" checkbox in the Groups
  filter bar — persisted to `localStorage` like "show offline".
  Cannot be deleted (it has no DB row to delete).
- **Inert as a group**: no rename / delete / multicast / cron /
  add-member / spawn buttons, no default-cwd / default-context, no
  per-group offline toggle. Styled distinctly (dashed left rule,
  muted name, a `virtual` badge) so it doesn't read as a real group.
- **Drag-and-drop**:
  - drag an Ungrouped row onto a real group header → the agent joins
    that group (`runDndAddToGroup`: `POST /api/groups/{B}/members`);
  - drag a real group's member row onto the Ungrouped header → the
    agent leaves that group (`runDndRemoveFromGroup`:
    `DELETE /api/groups/{A}/members/{conv}`); if it was the agent's
    only group, the agent reappears in the virtual group;
  - Ctrl/⌘-drag onto a real group still clones; the clone modifier is
    suppressed when the drop target is the Ungrouped group.
- **Member-row actions** are the agent-level set (focus / term /
  shut down / wake, clone, reincarnate, rename, sudo, self-nudge
  cron, delete) — the group-affecting buttons (edit alias/role/descr,
  owner toggle, remove-from-group) are omitted, since the agent
  belongs to no group.

## Collision-proofing

A human CAN create a real group literally named "Ungrouped"
(`validateGroupName` is liberal). The virtual group therefore never
keys off its display name:

- DnD branches on explicit `data-dnd-target-ungrouped` /
  `data-dnd-source-ungrouped` attributes, never the name.
- `localStorage` expand-state + the DOM `data-group-key` use the
  sentinel `' ungrouped-virtual'` (leading space — rejected by
  `validateGroupName`, so it can never collide with a real group).
- The virtual group has no backend representation, so group-mutation
  endpoints aimed at the name "Ungrouped" 404 like any unknown group.

## Backend fix

`handleDashboardSnapshot` previously emitted `ungrouped[]` for any
agent with `len(Groups) == 0`. The per-conv permission loop adds
*every* grant-holder to `agentRows` regardless of online state, so a
long-dead conv that still carried a permission grant would shore up
in `ungrouped[]` forever. The partition step now also requires
`a.Online`, matching the array's documented "live loose convs"
contract. (The loose-session enumeration was already online-gated;
this closes the grant-holder leak.)

## Files

- `pkg/claude/agentd/dashboard.go` — `ungrouped[]` partition now
  gated on `a.Online`; field doc updated.
- `pkg/claude/agentd/dashboard.html` — virtual-group CSS, the "show
  ungrouped" checkbox, `virtualUngroupedGroup` / `ungroupedVisible` /
  `renderVirtualGroup` / `memberRowHTML` / `ungroupedMemberActions`,
  `renderGroupsTab` append, `bindFilter` wiring, and the DnD rewrite
  (`DND_TARGET_SEL`, virtual-aware drag handlers, `runDndAddToGroup`,
  `runDndRemoveFromGroup`).
- `pkg/claude/agentd/dashboard_ungrouped_dnd_flow_test.go` — flow
  tests (new).

## Tests

Flow scenarios via the existing testharness + `BuildDashboardHandlerForTest()`:

- `TestDashboardUngrouped_DragIntoGroupAddsAndLeavesUngrouped` —
  ungrouped online conv → `POST` → in target group, gone from
  `ungrouped[]`.
- `TestDashboardUngrouped_DragOutOfGroupReturnsToUngrouped` —
  sole-group member → `DELETE` → back in `ungrouped[]`.
- `TestDashboardUngrouped_DeletedGroupMembersSurfaceInUngrouped` —
  delete a group, its still-online member resurfaces in `ungrouped[]`.
- `TestDashboardSnapshot_UngroupedExcludesOfflineGrantHolders` —
  pins the `a.Online` backend gate (offline grant-holder excluded,
  online one included).
- `TestDashboardUngrouped_VirtualNameHasNoBackendGroup` — delete /
  rename aimed at the name "Ungrouped" fail (no backing DB row).

## Cross-references

- `DONE/dashboard-snapshot-ungrouped.md` — the backend `ungrouped[]`
  array this renders from.
- `DONE/dashboard-dnd-move.md` / `DONE/dashboard-dnd-clone.md` — the
  real-group drag-and-drop this extends.
