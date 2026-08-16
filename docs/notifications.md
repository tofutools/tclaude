# Notifications

tclaude raises a desktop notification when a session transitions into a
state that needs you: by default `idle`, `awaiting_permission`,
`awaiting_input`, and `exited`. Clicking the notification focuses the
terminal window running that session. With many sessions in flight, this is
what lets you stop watching panes.

Notifications are disabled by default. `tclaude setup` asks once — on its
first run only — whether to enable them; re-runs never change a choice you
have already made (even with `--yes`), they only add notification categories
introduced by newer versions. To change your mind later, edit
`~/.tclaude/data/config.json` or use the dashboard's Config tab.

## What setup configures

```bash
tclaude setup
```

Along with its other work (see [the setup overview](index.md)), setup
installs the harness hooks that make status transitions visible, registers
the `tclaude://` protocol handler on WSL so notifications are clickable, and
offers the first-run notification enable prompt. On macOS it offers to
install `terminal-notifier` via Homebrew. `tclaude setup --check` reports
the current state; `--force` re-registers the protocol handler.

## Configuration

The `notifications` block in `~/.tclaude/data/config.json`:

```json
{
  "notifications": {
    "enabled": true,
    "transitions": [
      {"from": "*", "to": "idle"},
      {"from": "*", "to": "awaiting_permission"},
      {"from": "*", "to": "awaiting_input"},
      {"from": "*", "to": "exited"}
    ],
    "cooldown_seconds": 5
  }
}
```

- `transitions` — `{from, to}` rules with `*` wildcards; the defaults above
  apply when the list is omitted.
- `cooldown_seconds` — per-session rate limit (default 5) so a flapping
  state does not spam you.
- `human_messages` — also notify when an agent sends `tclaude agent
  notify-human` (default true when enabled; these banners are exempt from
  the cooldown). See [Agents and groups](agents-and-groups.md).
- `notification_command` — an argv array that replaces the platform
  mechanism entirely; tclaude writes `{"title", "body", "sessionID"}` as
  JSON to its stdin and gives it 5 seconds.
- `delivery` — `os` (default), `browser`, or `both`. Browser delivery
  raises banners in an open [dashboard](dashboard.md) tab via the Web
  Notification API instead of the daemon host's desktop — useful when you
  are [remote](remote.md). It needs an open tab, granted browser
  permission, and a secure context; queued banners expire after 10 minutes,
  and clicking one focuses the dashboard rather than a tmux window.

An `idle` notification means the harness finished and no tracked subagent,
background shell, or monitor is still working — tclaude holds the session at
an internal `main_agent_idle` state until those settle, so you are not
pinged while a subagent is still running.

!!! note "Harness labeling caveat"
    Notification titles are `<Harness>: <Status>` — but only Codex and
    shell sessions get their own label (`Codex: …`, `Shell: …`). OpenCode
    and Copilot sessions are currently titled `Claude: …` as well. This is
    a known cosmetic quirk of the title mapping; the notification itself
    fires and focuses correctly regardless of harness.

## Click-to-focus per platform

| Platform | Notifier | Clickable | Focus method |
|----------|----------|-----------|--------------|
| Linux | D-Bus | Yes | `xdotool` |
| macOS | `terminal-notifier` (or osascript) | Yes | iTerm2 / Terminal.app AppleScript |
| WSL | PowerShell toast | Yes | Windows Terminal, via `tclaude://` handler |
| Windows (native) | Not implemented | — | — |

### Linux

Notifications go through D-Bus; window focus on click needs `xdotool`
(`sudo apt install xdotool`). Without it, notifications still appear but are
not clickable.

### macOS

Clickable notifications need `terminal-notifier` (`brew install
terminal-notifier`; setup offers this when Homebrew is available). Without
it, tclaude falls back to osascript notifications.

### WSL

WSL uses Windows toast notifications via PowerShell, and clicking one
focuses the Windows Terminal window through the `tclaude://` protocol
handler that `tclaude setup` registers. If the target session lives in a
background tab, clicking detaches that tab and opens a new Windows Terminal
window with the session attached.

If clicking does nothing, run `tclaude setup --check` to verify the handler
and `tclaude setup --force` to re-register it. If notifications do not
appear at all, verify PowerShell is reachable
(`/mnt/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe`) and that
Windows notification settings allow toasts.

## Troubleshooting

1. `tclaude setup --check` — hooks and protocol handler in place?
2. `~/.tclaude/data/config.json` has `"enabled": true`?
3. Does the transition match your rules? The defaults do not fire on
   `*→working`, and rapid changes within `cooldown_seconds` are dropped.
