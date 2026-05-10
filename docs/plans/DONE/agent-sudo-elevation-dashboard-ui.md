# `tclaude agent sudo` — dashboard JS UI + CLI --target

Shipped 2026-05.

The Go side of v2 slice 3 (snapshot extensions + cookie-auth
revoke twins) shipped earlier under
`agent-sudo-elevation-dashboard-api.md`. The proactive-grant
endpoint shipped under
`agent-sudo-elevation-dashboard-proactive-grant.md`. This file
covers the **user-facing affordances** that consume those
endpoints: the dashboard JS UI and the CLI's `--target` flag
mirroring the same path through the daemon.

## CLI

`tclaude agent sudo request --target <conv> <slug>... [-d ...] [-r ...]`

Same shape as the agent-initiated path; just adds an optional
`Target` field on the body. Daemon dispatches:

- `body.target` set + caller human → proactive grant, no popup,
  `granted_by = "<human-cli>:proactive"`.
- `body.target` set + caller agent → 403 (manager-pattern
  approval is deferred, see TODO).
- `body.target` unset + caller agent → existing popup-gated path.
- `body.target` unset + caller human → 400 (humans hold every
  permission implicitly; sudo without a target is meaningless
  for them).

Output verb shifts to "Sudo granted to <short-id>" so the
operator sees clearly that this wasn't a self-elevation.

## Dashboard

### New "Sudo" tab

- System-wide list of every active grant.
- Columns: conv | slug | granted_at | expires_in | reason |
  granted_by | revoke.
- "+ Grant sudo" button at the tab top opens an **agent picker
  overlay** (filtered list of all agents, online-first; reuses
  the `.add-member-modal` CSS shape) → on Enter / click → opens
  the Grant modal pre-filled with that conv. Mirrors the doc's
  "matches the tray's orange-state oversight view" intent.
- Filter bar matches the existing pattern (Groups / Agents / Cron).

### Per-row affordances on Groups + Agents tabs

- **`+ sudo` button** in row-actions on every member / agent row.
  Skips the agent picker (already knows the conv) and opens the
  Grant modal directly. Primary affordance for "this agent needs
  elevation right now".
- **🔓 badge** in the Name column when an agent currently holds
  ≥1 active grant. Tooltip lists slugs + remaining time per
  grant. Click jumps to the Sudo tab pre-filtered to that
  agent's short-id so revoke is one click away.

### Grant modal

`#sudo-grant-modal`:

- Conv selector (free-text; the picker pre-fills it).
- Slug picker built from the snapshot's slug registry. Blocklisted
  slugs (`permissions.grant`, `permissions.revoke`) render as
  greyed/disabled with a tooltip explaining why.
- **Select all / Select none** toolbar buttons. "All" skips
  disabled slugs; "None" clears.
- Duration (free-text, parsed by Go's `time.ParseDuration`; empty
  uses the resolved default).
- Reason (free-text; logged to `agent_sudo_grants.reason`).
- Submit → `POST /api/sudo` → toast on success → `refresh()`
  pulls the new row into the Sudo tab.

### Per-row revoke

Routes through the existing `confirmModal` with title "Revoke
sudo grant?" + meta `#<id> · <slug> · <conv>`. Single endpoint:
`DELETE /api/sudo/{id}`.

## Files

- `pkg/claude/agent/sudo.go` — `--target` flag on
  `sudoRequestParams`; body carries `target`; output verb
  switches based on whether target is set.
- `pkg/claude/agentd/sudo.go` — refactored: `sudoRequestBody`
  gains `Target`; `handleSudoRequest` dispatches to either
  `handleSudoProactiveGrant` (target set) or the existing
  popup path (unset). New helpers
  `handleSudoProactiveGrant`, `blockedSlugs`,
  `resolveSudoDuration`, `insertSudoBundle`,
  `sudoBundleResponse` — all shared with the dashboard's
  `handleDashboardSudoGrant`.
- `pkg/claude/agentd/dashboard_edit.go` — collapsed
  `handleDashboardSudoGrant` to use the shared helpers.
- `pkg/claude/agentd/dashboard.html` — Sudo tab markup, agent
  picker overlay, grant modal, per-row buttons, 🔓 badges, all
  the JS handlers.
- `pkg/claude/agentd/sudo_flow_test.go` —
  `TestSudo_Proactive_HumanWithTarget_NoPopup` and
  `TestSudo_Proactive_AgentWithTarget_Refused`.

## Why no Go test for the JS

dashboard.html is rendered by a browser; Go flow tests can't
exercise the modal, the picker overlay, the per-row buttons, or
the badge click. Browser-side smoke testing is on the operator
(check that the Sudo tab populates, the picker opens, the modal
submits, the badge appears + jumps).

## Cross-references

- [`agent-sudo-elevation-v1.md`](agent-sudo-elevation-v1.md)
- [`agent-sudo-elevation-dashboard-api.md`](agent-sudo-elevation-dashboard-api.md)
- [`agent-sudo-elevation-dashboard-proactive-grant.md`](agent-sudo-elevation-dashboard-proactive-grant.md)
- [`dashboard-add-member-overlay.md`](dashboard-add-member-overlay.md)
  — the overlay pattern the agent picker reuses.
