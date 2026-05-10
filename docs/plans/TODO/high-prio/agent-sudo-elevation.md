# `tclaude agent sudo` — open follow-ups

V1 + the v2 surfaces shipped 2026-05. See `DONE/agent-sudo-*.md`
for shape + tests:

- `agent-sudo-elevation-v1.md` — daemon endpoints, blocklist,
  popup flow, `tclaude agent sudo {request,ls,revoke}`
- `agent-sudo-elevation-config-defaults.md` — `agent.sudo.*` config
  + per-conv overrides
- `agent-sudo-elevation-audit-annotations.md` —
  `via-sudo:grant-id=<n>` on downstream `granted_by` columns
- `agent-sudo-elevation-tray-orange.md` — tray icon + tooltip
- `agent-sudo-elevation-dashboard-api.md` — cookie-auth twin
  endpoints + snapshot extension
- `agent-sudo-elevation-dashboard-proactive-grant.md` — proactive
  grants from CLI (`--target`) + dashboard (Sudo tab + per-row
  buttons + 🔓 badges + agent picker)

## Open

### Manager-pattern approval (deferred — explicit trust laundering)

Could a group owner approve sudo for a group member instead of
the human? Out of v1 scope intentionally — sudo is the human
escape hatch by design. If the demand shows up, ship a
`sudo.approve` slug (default human-only) that lets a trusted
manager approve sudo without the popup. The audit trail records
who approved (`granted_by = "agent:<conv>:via-slug=sudo.approve"`)
so the chain stays inspectable.

### Periodic cleanup (housekeeping)

Hard-delete rows older than e.g. 30 days. Correctness doesn't
depend on it (the active-grants probe filters by `expires_at` on
every check), but a long-running daemon's table grows. The
`PurgeExpiredSudoGrants(olderThan)` helper is already shipped;
just needs a cron job to call it. Slot into the existing
`agent_cron_jobs` runner.

## Cross-references

- [`med-prio/system-tray-icon.md`](../med-prio/system-tray-icon.md)
  — orange state slots into the existing colour matrix
