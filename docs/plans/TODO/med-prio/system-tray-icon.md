# System tray icon — v2 follow-ups

V1 shipped (PR-era 2026-05): `fyne.io/systray`. Menu — Open
dashboard, Reinstall agent skills, Open config.json, copy-paste rows
for socket + popup URL, Quit. `--no-tray` opt-out for headless. Runs
on main goroutine; signal/server-error/Quit converge on a single
shutdown path. Linux/Windows pure-Go; macOS uses cgo.

## Open follow-ups

- ~~**Yellow on pending approval**~~ — **shipped (2026-05).** Tray
  goroutine polls `approvals.pendingCount()` on a 200ms tick.
  Icon flips green↔yellow on count change; tooltip surfaces
  "tclaude agentd · N pending approval(s)". Pure
  function `pickTrayIcon` makes the policy unit-testable.
- ~~**Pending approvals submenu**~~ — **shipped (2026-05).** Tray
  exposes a 5-slot pre-allocated submenu refreshed on the same
  200ms tick. Slots bind to oldest-first pending approvals; click
  re-opens `/approve/{id}` (helps when the auto-opened tab got
  buried). Overflow surfaces as "Pending approvals (+N more…)"
  on the disabled header row. Pure helpers
  (`refreshApprovalsSubmenu`, `formatApprovalSlotLabel`, etc.)
  unit-tested. See `DONE/system-tray-icon-approvals-submenu.md`.
- **Red on daemon down / shutting down**.
- **Flashing on unread inbox** — opt-in (loud).
- **Tray-mediated approve** — pair with the popup so Approve/Deny
  also requires a tray click within N seconds (kills the residual
  /proc cmdline-scrape attack — see
  `popup-transport-hardening.md`).
- **Focus dashboard tab on icon click** — same window-focus tricks
  the WSL notifications already use.

## Files
- `pkg/claude/agentd/tray.go`
