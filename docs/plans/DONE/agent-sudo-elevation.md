# `tclaude agent sudo` — shipped surfaces overview

V1 + the v2 surfaces shipped 2026-05. Sibling DONE files carry the
per-slice detail; this file ties them together and documents the
single shelved follow-up.

## Shipped slices

- [`agent-sudo-elevation-v1.md`](agent-sudo-elevation-v1.md) —
  daemon endpoints, blocklist, popup flow,
  `tclaude agent sudo {request,ls,revoke}`.
- [`agent-sudo-elevation-config-defaults.md`](agent-sudo-elevation-config-defaults.md)
  — `agent.sudo.*` config + per-conv overrides.
- [`agent-sudo-elevation-audit-annotations.md`](agent-sudo-elevation-audit-annotations.md)
  — `via-sudo:grant-id=<n>` on downstream `granted_by` columns.
- [`agent-sudo-elevation-tray-orange.md`](agent-sudo-elevation-tray-orange.md)
  — tray icon + tooltip when ≥1 grant is active.
- [`agent-sudo-elevation-dashboard-api.md`](agent-sudo-elevation-dashboard-api.md)
  — cookie-auth twin endpoints + snapshot extension (Go side).
- [`agent-sudo-elevation-dashboard-proactive-grant.md`](agent-sudo-elevation-dashboard-proactive-grant.md)
  — `POST /api/sudo` + `<human-dashboard>:proactive` audit label.
- [`agent-sudo-elevation-dashboard-ui.md`](agent-sudo-elevation-dashboard-ui.md)
  — Sudo tab, agent picker overlay, grant modal (with all/none
  toolbar), per-row `+ sudo` buttons, 🔓 badges, CLI `--target`.
- [`agent-sudo-elevation-periodic-cleanup.md`](agent-sudo-elevation-periodic-cleanup.md)
  — hourly sweep, 30d retention.

## Shelved (not in flight)

### Manager-pattern approval — explicit trust laundering

Could a group owner approve sudo for a group member instead of
the human? Out of scope intentionally — sudo is the human escape
hatch by design. If the demand shows up, ship a `sudo.approve`
slug (default human-only) that lets a trusted manager approve
sudo without the popup. The audit trail would record who approved
(`granted_by = "agent:<conv>:via-slug=sudo.approve"`) so the
chain stays inspectable.

No use case has surfaced yet; resist shipping until one does.

## Cross-references

- [`med-prio/system-tray-icon.md`](../med-prio/system-tray-icon.md)
  — orange state slots into the existing colour matrix
