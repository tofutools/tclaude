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
- `agent-sudo-elevation-tray-orange` — orange tray icon + tooltip
  when ≥1 sudo grant is active anywhere
- `agent-sudo-elevation-dashboard-api` — cookie-auth twin endpoints
  + snapshot extension surfacing per-agent + global active grants
  (Go side; JS rendering still open)

## Dashboard JS rendering (Go side shipped — JS still open)

The Go API + snapshot extensions for the Sudo tab landed —
see `DONE/agent-sudo-elevation-dashboard-api.md` for the wire
surface. What's still open is the actual JavaScript rendering
inside `dashboard.html`:

- A new "Sudo" tab consuming `snapshot.sudo[]`. Columns: conv |
  slug | granted_at | expires_in | reason | revoke. Group rows by
  conv-id with the soonest expiry first inside each block.
- A 🔓 badge on the Groups + Agents tabs for any agent whose
  `active_sudo[]` is non-empty. Click could open a popover with the
  agent's slugs + remaining time + per-row revoke.
- Click handlers wired to `DELETE /api/sudo/{id}`,
  `DELETE /api/sudo?conv=…`, `DELETE /api/sudo?all=1`.

Carved out of the Go-side slice because Go flow tests can't
exercise browser JS — needs hand-written + browser-tested. All the
data is already on the wire.

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
  indicator (consumes the already-shipped snapshot fields)

## Cross-references

- [`DONE/agent-sudo-elevation-v1.md`](../../DONE/agent-sudo-elevation-v1.md)
  — what shipped
- [`med-prio/system-tray-icon.md`](../med-prio/system-tray-icon.md)
  — orange state slots into the existing colour matrix
