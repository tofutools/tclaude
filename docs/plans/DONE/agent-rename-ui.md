# Agent rename UI — folded into the edit button + click-editable name

**Status: shipped.** Branch `feat/agent-rename-ui`.

## What shipped

Agent rename on the dashboard was a standalone "rename" button that
opened its own modal. It now lives in two places, both about editing an
agent's name — and the standalone button/modal is gone.

### Change 1 — rename folded into the per-agent edit panel

The per-agent **edit** button (grouped-member rows) opened a modal that
edited the group role + description. It now also edits the agent's
**title**: a Title text field plus an **auto** checkbox ("let the agent
choose its own title"). It is the single panel for editing an agent.

The modal (`editMemberModal`) yields up to two independent edits:
- a rename — `{title}` or `{auto:true}` — applied via
  `POST /api/agents/{conv}/rename`;
- a membership change — `role` / `descr` — applied via
  `PATCH /api/groups/{group}/members/{conv}`.

Each is applied on its own; one failing does not swallow the other.
The title is charset-validated client-side (`isValidRenameTitleJS`,
mirrors the daemon's `isValidRenameTitle`) so a bad title is caught in
the modal instead of bouncing off a 400.

### Change 2 — the agent name is click-to-edit

The agent name in a member row (`.rowname-text` span) is click-to-edit:
click → `<input>` → Enter `POST`s `/api/agents/{conv}/rename {title}` →
Esc / blur cancels. Mirrors the group-header click-to-edit chips.

A shared **`inlineEdit`** primitive (row-actions.js) was extracted for
it: swap element → focused input, commit on Enter, revert on Esc/blur,
suspend the 5s auto-refresh (`renameEditing`) while open, and park the
host row's `draggable` so text selection can't start a row drag.

> The four pre-existing group-header inline edits (rename-group,
> set-group-dir / -descr / -max-members) still hand-roll the same
> pattern. Migrating them onto `inlineEdit` is a deliberate follow-up,
> kept out of this rename-focused PR.

## Backend

**No backend changes.** Both surfaces POST the existing
`/api/agents/{conv}/rename` endpoint (`dashboardRenameAgent` →
`handleAgentRename` → `runRenameOrchestration`), gated by the same
dashboard cookie + Origin pin and the same `agent.rename` permission
path as before.

## Files

- `pkg/claude/agentd/dashboard/js/render.js` — `.rowname-text`
  click-to-edit span in `memberRowHTML`.
- `pkg/claude/agentd/dashboard/js/row-actions.js` — `inlineEdit`
  primitive; `rename-name` handler; `edit-member` handler extended to
  apply rename + membership; old `rename-agent` handler removed.
- `pkg/claude/agentd/dashboard/js/refresh.js` — `editMemberModal`
  gained Title + auto; `isValidRenameTitleJS`.
- `pkg/claude/agentd/dashboard/js/helpers.js` — `renameAgentButton`
  removed; `editMemberButton` carries `data-current` (title).
- `pkg/claude/agentd/dashboard/js/modal-spawn.js` — standalone rename
  modal removed.
- `pkg/claude/agentd/dashboard/js/dashboard.js` — dropped
  `bindRenameAgentModal`.
- `pkg/claude/agentd/dashboard/dashboard.html` — edit modal gained the
  Title field, auto checkbox, error line; `rename-agent-modal` removed.
- `pkg/claude/agentd/dashboard/dashboard.css` — `.rowname-text` /
  `.rowname-input` / `.edit-member-auto-row`.

## Tests

- `pkg/claude/agentd/dashboard_rename_flow_test.go` — flow tests on the
  rename endpoint both surfaces share: explicit-title rename lands on
  the members surface + `/api/snapshot`; `{auto:true}` injects a
  self-rename nudge and leaves the title untouched; an invalid title is
  rejected 400 with nothing injected.
- `pkg/claude/agentd/dashboard_rename_ui_test.go` — content test pinning
  the JS wiring: the standalone modal stays gone, the edit panel has the
  Title + auto fields, the name cell is click-to-edit through
  `inlineEdit`, and both surfaces POST the rename endpoint.

## Notes / scope decisions

- Ungrouped-agent rows have no edit button (no group membership to
  edit); their rename path is the click-to-edit name cell. The old
  rename button — including its auto option — is gone from those rows.
  Auto-rename for an ungrouped agent stays reachable via the CLI / the
  agent itself; a dashboard affordance for it was not re-added.
- The edit modal keeps its `edit-member` identifiers (no half-rename to
  `edit-agent`); only the user-facing heading reads "Edit agent".
