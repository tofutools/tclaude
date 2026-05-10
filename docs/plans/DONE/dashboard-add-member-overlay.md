# Dashboard: per-group `+ add member` overlay

Shipped 2026-05.

Carve-out from `TODO/high-prio/dashboard-group-membership-ux.md`.
Ships Part 2 of that file's two halves; Part 1 (drag-and-drop) stays
open in the parent doc.

## What ships

Each group's header in the Groups tab gains a `+ add member` button
alongside the existing `rename` and `delete group` affordances.
Clicking pops a centered overlay anchored conceptually to the group
(group name shown in the title bar).

Inside the overlay:

- **Autofocused text input** at the top — live-filters the candidate
  list as the user types. Case-insensitive substring match against
  alias, role, descr, conv-id, title, and the agent's current group
  tags.
- **Scrollable candidate list** — each row shows online dot, alias
  (or title), short conv-id, role pill, descr, and a `in: A, B, C`
  tag listing the candidate's current group memberships. Rows already
  in this group are filtered out (cleaner than disabling them).
- **Keyboard nav** — ↑/↓ moves the highlight, Enter adds the
  highlighted row, Esc closes. Click on a row also adds.
- **Mouse hover** moves the highlight so keyboard + mouse stay in
  sync.
- **Keep-open-after-add** — successful adds remove the row from the
  visible list and stay in the overlay. The user dismisses with Esc /
  click-outside / the row count hitting zero. Close-on-add is the
  exact pain this overlay fixes; the original UX required a re-click
  per member.

Default candidate pool is **online conversations** (the union of
`agents[]` and `ungrouped[]` from `/api/snapshot`, filtered to
online). A bottom-row checkbox **"Include offline / archived"** lifts
the online gate so the user can pull in a long-archived conv when
needed; off by default to keep the list manageable.

## Optimistic UI

After a successful POST, the overlay locally appends the conv to its
group's `lastSnapshot.members` array and calls `renderGroupsTab()` so
the Groups tab reflects the add immediately, without waiting for the
5s snapshot poll. The next poll overwrites `lastSnapshot` with the
canonical state. Failure surfaces in a toast and leaves the local
state untouched.

## Daemon side

`POST /v1/groups/{name}/members` (already shipped as part of
`groups members add`) is now mirrored at
`POST /api/groups/{name}/members` for the dashboard cookie auth
path. Same `agent.ResolveSelector` + `db.AddAgentGroupMember`, same
404-on-unknown-conv surface.

`db.AddAgentGroupMember` uses `INSERT OR REPLACE`, so re-adding the
same conv is idempotent and returns 200 — pinned by a flow test so
this doesn't accidentally start surfacing as a confusing failure
toast on the second click.

`auto-refresh` suspends while the overlay is open via the existing
`modalEditing` flag (same shape as the edit-member modal), so a 5s
tick doesn't reset the search input mid-keystroke.

## Vanilla JS, no framework migration

The parent TODO doc flagged a JS framework migration as a
prerequisite for this work. Subsequent dashboard features (rename,
edit-member modal, wake/shutdown buttons, groups clone) shipped in
vanilla JS at scale, so the framework migration is no longer a
blocker. The overlay is ~200 lines of JS in `dashboard.html` and
re-uses the existing `modal-overlay` CSS shell.

The DnD half (Part 1 of the parent TODO) stays open and may revisit
the framework question independently.

## Tests

`pkg/claude/agentd/dashboard_addmember_flow_test.go`:

- `TestDashboardAddMember_FromUngrouped` — loose conv appears in
  `ungrouped[]`, POST `/api/groups/{name}/members` lands it in the
  group, post-snapshot drops it from `ungrouped[]` and shows the
  group tag on its `agents[]` row.
- `TestDashboardAddMember_RepeatIsIdempotent` — second POST returns
  200 and the snapshot's per-group member list still has exactly one
  row for that conv (pins the INSERT OR REPLACE semantics).
- `TestDashboardAddMember_UnknownConvReturns404` — typo'd conv-id
  surfaces 404 so the overlay can render a readable error toast.

Asserts at the same surface the overlay reads: `/api/snapshot`'s
`Groups[*].Members[*]`, `Agents[*].Groups`, and `Ungrouped[*]`.

## Files

- `pkg/claude/agentd/dashboard_edit.go` — `dashboardAddMember`
  helper + `members` switch branch wiring POST through
  `handleDashboardGroupsAPI`
- `pkg/claude/agentd/dashboard.html` — `+ add member` button in the
  group-actions span, `#add-member-modal` markup, `addMemberModal`
  JS helper (build / filter / keyboard nav / optimistic refresh),
  `add-member` case in `bindRowActions`, CSS for the `.add-member-*`
  classes
- `pkg/claude/agentd/dashboard_addmember_flow_test.go` — 3 flow tests

## Out of scope (deferred)

- **Drag-and-drop between groups** — Part 1 of the parent TODO,
  still open. Move + Ctrl-clone + ungrouped-as-source. Larger UX
  surface; ship after the overlay bakes.
- **Anchored popover positioning** — currently the overlay is a
  centered modal; doesn't visually anchor to the clicked group's
  header. Shipping the simpler central modal first; anchored
  positioning is a CSS-only follow-up.
- **Add-with-alias-and-role at click time** — POST already accepts
  optional alias/role/descr in the body, but the overlay currently
  sends only `conv`. Adding inline metadata fields would clutter the
  candidate list; the existing edit-member modal handles that path
  post-add.

## Cross-references

- [`DONE/dashboard-snapshot-ungrouped.md`](dashboard-snapshot-ungrouped.md)
  — backend foundation this builds on
- [`DONE/dashboard-member-metadata-editing.md`](dashboard-member-metadata-editing.md)
  — sibling per-row modal pattern (alias/role/descr)
- [`TODO/high-prio/dashboard-group-membership-ux.md`](../TODO/high-prio/dashboard-group-membership-ux.md)
  — parent UX feature; Part 1 (DnD) still open
