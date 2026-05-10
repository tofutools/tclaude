# System tray icon — v2 follow-ups

V1 shipped (PR-era 2026-05): `fyne.io/systray`. Menu — Open
dashboard, Reinstall agent skills, Open config.json, copy-paste rows
for socket + popup URL, Quit. `--no-tray` opt-out for headless. Runs
on main goroutine; signal/server-error/Quit converge on a single
shutdown path. Linux/Windows pure-Go; macOS uses cgo.

## Open follow-ups

- **Yellow on pending approval** — flip icon to yellow while a
  `--ask-human` popup is awaiting decision; back to green on
  approve/deny/timeout.
- **Red on daemon down / shutting down**.
- **Flashing on unread inbox** — opt-in (loud).
- **Pending approvals submenu** — list waiting requests; click
  re-opens `/approve/{id}` (helps when the auto-opened tab got
  buried).
- **Tray-mediated approve** — pair with the popup so Approve/Deny
  also requires a tray click within N seconds (kills the residual
  /proc cmdline-scrape attack — see
  `popup-transport-hardening.md`).
- **Focus dashboard tab on icon click** — same window-focus tricks
  the WSL notifications already use.

## Files
- `pkg/claude/agentd/tray.go`
