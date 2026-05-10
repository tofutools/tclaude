# `tclaude agent sudo` — v2 follow-ups

V1 shipped 2026-05; see
[`DONE/agent-sudo-elevation-v1.md`](../../DONE/agent-sudo-elevation-v1.md)
for what's already in. This file tracks the deferred surfaces.

Slices that have already shipped (see DONE files for shape +
test surface):

- `agent-sudo-elevation-config-defaults` — config-driven defaults
  + per-conv overrides
- `agent-sudo-elevation-audit-annotations` — `via-sudo:grant-id=<n>`
  on downstream `granted_by` columns

## Dashboard panel + per-row indicator

A new "Sudo" tab on the dashboard listing every active grant:

| Conv | Slug | Granted at | Expires in | Reason | |
|------|------|------------|------------|--------|-|
| alice | `groups.spawn` | 18:30 | 4m 12s | bootstrap | [Revoke] |

Per-row indicator on the **Groups** and **Agents** tabs: a 🔓 emoji
(or coloured highlight) when the agent currently holds ≥1 active
grant. Clicking the indicator could open a popover listing the
agent's slugs + remaining time + revoke buttons.

Mutates via cookie-auth twins:

- `DELETE /api/sudo/{id}` — single revoke
- `DELETE /api/sudo?conv=<selector>` — bulk per conv
- `DELETE /api/sudo?all=1` — nuke

Daemon endpoints already ship; just need the cookie-auth twin
handlers in `dashboard_edit.go` (mirror the pattern from
`/api/groups/...` and `/api/agents/...`).

The snapshot's `agents[]` array should gain an `active_sudo[]`
field surfacing the slugs each agent currently holds — single round
trip, both tabs render off the same blob.

## Tray-icon orange state

Existing colour matrix:

- Green: idle
- Yellow: pending approval
- Red: daemon down (planned)

Add **Orange** for "at least one active sudo grant somewhere".
Tooltip surfaces the count + soonest expiry, so the human knows
the elevation window without opening the dashboard. Polls
`db.ListAllActiveSudoGrants()` on the same 200ms tick the
pending-approval poller uses.

## Manager-pattern approval (deferred — explicit trust laundering)

Could a group owner approve sudo for a group member instead of
the human? Out of v1 scope intentionally — sudo is the human
escape hatch by design. If the demand shows up, ship a
`sudo.approve` slug (default human-only) that lets a trusted
manager approve sudo without the popup. The audit trail records
who approved (`granted_by = "agent:<conv>:via-slug=sudo.approve"`)
so the chain stays inspectable.

## Periodic cleanup (housekeeping)

Hard-delete rows older than e.g. 30 days. Correctness doesn't
depend on it (the active-grants probe filters by `expires_at` on
every check), but a long-running daemon's table grows. The
`PurgeExpiredSudoGrants(olderThan)` helper is already shipped;
just needs a cron job to call it. Slot into the existing
`agent_cron_jobs` runner.

## Test coverage (v2)

In addition to the v1 6 flow tests:

- **Dashboard panel** (cookie-auth list + revoke endpoints).

## Files (when implementing)

- `pkg/claude/agentd/dashboard.html` — new "Sudo" tab + per-row
  indicator
- `pkg/claude/agentd/dashboard_edit.go` — cookie-auth twins for
  the revoke endpoints
- `pkg/claude/agentd/dashboard.go` — extend snapshot's
  `dashboardAgent` with an `active_sudo[]` field
- `pkg/claude/agentd/tray.go` — orange state + tooltip wiring

## Cross-references

- [`DONE/agent-sudo-elevation-v1.md`](../../DONE/agent-sudo-elevation-v1.md)
  — what shipped
- [`med-prio/system-tray-icon.md`](../med-prio/system-tray-icon.md)
  — orange state slots into the existing colour matrix
