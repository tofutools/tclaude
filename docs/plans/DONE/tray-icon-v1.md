# System tray icon v1 (2026-05)

Persistent menubar / system-tray indicator for the daemon.

## Library

`fyne.io/systray`.

## Menu

- Open dashboard
- Reinstall agent skills
- Open config.json
- Copy-paste rows for socket + popup URL
- Quit

## Lifecycle

- Runs on the main goroutine.
- `--no-tray` opt-out for headless.
- Signal / server-error / Quit converge on a single shutdown
  path.

## Build matrix

- Linux / Windows: pure-Go.
- macOS: cgo (goreleaser splits builds: `CGO_ENABLED=0` for
  linux/windows, `=1` for darwin).

## Indicators / submenu (deferred to v2)

See `med-prio/system-tray-icon.md` for v2 follow-ups: yellow on
pending approval, red on daemon down, flashing on unread inbox,
pending-approvals submenu, tray-mediated approve, focus-
dashboard-on-click.
