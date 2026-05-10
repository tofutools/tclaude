# Dashboard: edit member alias / role / descr

Shipped 2026-05.

## What ships

Per-row "edit" button on every member in the Groups tab. Clicking
opens a small modal with three fields pre-filled with the member's
current values:

- **Alias** — single-line text input
- **Role** — single-line text input
- **Description** — `<textarea>` (3 rows default, vertical resize)

Save submits to `PATCH /api/groups/{group}/members/{conv}`. Cancel,
Esc, click-outside, or submitting unchanged values closes without a
round trip. Ctrl/Cmd+Enter saves from anywhere in the form so power
users don't have to mouse over to Save.

The PATCH only sends fields that **actually changed**. Unchanged
fields are sent as `null` (omitted from the JSON body), preserving
the daemon's nil-as-leave-alone semantics so a stray click on Save
doesn't blank a field.

Auto-refresh suspends while the modal is open via the `modalEditing`
flag (the same shape as `renameEditing`), so the 5s snapshot tick
doesn't refocus the textarea or blow input away mid-keystroke.

## Picked the per-row modal pattern over inline-edit

Of the four UX options the TODO doc enumerated, this picks the
**per-row "edit" button → modal** variant:

- Three fields are typically tweaked together when refining what an
  agent does — modal lets the human see and revise all three before
  committing (one POST, one optimistic refresh).
- Description benefits from a proper textarea (multi-line). Inline
  cell-editing forces a single-line input or awkward expand-on-focus.
- Reuses the existing `confirmModal` infrastructure pattern (modal
  overlay + Esc handling + outside-click cancel).
- Discoverability: the "edit" button is obvious; click-to-edit on
  text needs a hover hint and isn't keyboard-accessible.

Trade-off: one extra click vs cell click-to-edit. For 3 interrelated
fields, worth it. The TODO doc explicitly says "Flexibility over
prescription. Pick whichever fits."

## Daemon side

`PATCH /v1/groups/{name}/members/{conv}` (already shipped as part of
`groups update-member`) is now mirrored at
`PATCH /api/groups/{name}/members/{conv}` for the dashboard cookie
auth path. Same `db.UpdateAgentGroupMember`, same nil-as-leave-alone
contract, same 200/400/404 surface.

## Tests

`pkg/claude/agentd/dashboard_edit_test.go`:
- `TestDashboardEdit_UpdateMember` — full update lands all three fields
- `TestDashboardEdit_UpdateMember_PartialFields` — only the touched
  field changes, others stay at current values
- `TestDashboardEdit_UpdateMember_EmptyBodyIs400` — `{}` body refused
- `TestDashboardEdit_UpdateMember_MissingIs404` — typo'd member
  surfaces clearly instead of silently no-opping

JS not unit-tested (no JS test infra yet); the optimistic UI is
straightforward enough that the Go-side guarantees catch regressions.

## Files

- `pkg/claude/agentd/dashboard_edit.go` — `dashboardUpdateMember`
  + dispatcher branch for PATCH
- `pkg/claude/agentd/dashboard_edit_test.go` — 4 unit tests
- `pkg/claude/agentd/dashboard.html` — modal markup, CSS for
  `.modal .field`, `editMemberModal` helper, `edit-member` case in
  `bindRowActions`, `modalEditing` flag suspends auto-refresh

## Out of scope (deferred)

- **Multi-role support** — schema change, not requested yet
- **Role autocomplete from existing values** — small dropdown of
  recently-seen roles. Trivial to add later
- **Bulk multi-row edit** — power-user feature; ship per-row first
  and watch usage
- **Agents-tab parity** — when a conv is expanded to show its
  per-group memberships, the same edit modal could open from
  there. Trivial follow-up; defer until requested

## Cross-references

- [`DONE/polish-post-pr47.md`](polish-post-pr47.md) — daemon verb
  shipped earlier
- [`DONE/groups-rename.md`](groups-rename.md) — sibling dashboard
  shape (inline-edit instead of modal — different tradeoff for
  single-field edits)
