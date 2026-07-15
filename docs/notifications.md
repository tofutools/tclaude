# OS Notifications

Get notified when Claude Code or Codex sessions need attention.

## Overview

tclaude can send OS notifications when a coding session transitions to a state
that requires attention (`idle`, `awaiting_permission`, `awaiting_input`, or
`exited`). Notification titles identify the session's harness (`Claude: …` or
`Codex: …`). This is useful when running multiple sessions or working in a
different window.

**Disabled by default** - run `tclaude setup` to enable.

## Quick Setup

The easiest way to enable notifications:

```bash
tclaude setup
```

This will:
1. Install Claude Code hooks for status tracking and offer Codex setup when it is detected
2. Register the protocol handler (WSL/Windows) for clickable notifications
3. Ask if you want to enable notifications — but only on the first run

Re-running `tclaude setup` never changes notification settings you've
already chosen: a deliberately disabled block stays disabled (even with
`--yes`), and your transitions, cooldown and other tweaks are preserved.
Setup only *adds* notification categories introduced in a newer tclaude
version; to enable/disable notifications later, edit `~/.tclaude/data/config.json`
or use the dashboard's Config tab.

You can check your setup status anytime:

```bash
tclaude setup --check
tclaude setup --check --harness codex
```

## Manual Configuration

Alternatively, create `~/.tclaude/data/config.json` manually:

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

### Options

| Field                  | Description                                             | Default   |
|------------------------|---------------------------------------------------------|-----------|
| `enabled`              | Master switch for notifications                         | `false`   |
| `transitions`          | List of state transitions that trigger notifications    | See below |
| `cooldown_seconds`     | Minimum seconds between notifications per session       | `5`       |
| `notification_command` | Custom command to run instead of platform notifications | (none)    |
| `human_messages`       | Also notify on a `tclaude agent notify-human` message   | `true`*   |

\* Only takes effect when `enabled` is `true`. On by default within an
enabled block; set `false` to suppress just the human-message banners.

### Transitions

Each transition rule has `from` and `to` fields. Use `*` as a wildcard to match any state.

**Default transitions:**
- `*` → `idle` - the harness finished processing
- `*` → `awaiting_permission` - the harness needs permission to proceed
- `*` → `awaiting_input` - the harness is asking a question
- `*` → `exited` - Session ended

**Available states:** `working`, `idle`, `awaiting_permission`, `awaiting_input`, `exited`

### Human-message notifications

`tclaude agent notify-human` lets a coordinating agent reach you with a
message that lands in the dashboard's **Messages tab**. When
notifications are enabled, each such message **also raises an OS
notification** — the desktop companion to that tab — so you see it even
when the dashboard isn't open. Clicking the notification focuses the
sending agent's terminal (the same jump the tab's per-message button
does).

It rides on the same `enabled` master switch and the same
`notification_command` override as state-transition notifications. It is
**on by default** once notifications are enabled; silence just these
banners (while keeping session-state notifications) with:

```json
{
  "notifications": {
    "enabled": true,
    "human_messages": false
  }
}
```

The notification title is `Claude: <subject>` (or `Claude: <sender>
messaged you` when there's no subject); the body carries the message and
the sender's group. Unlike state-transition notifications, human messages
are **not** subject to `cooldown_seconds` — each one is an explicit,
deliberate nudge from an agent, not a state the system may flap into.

### Custom Notification Command

You can override the platform-specific notification mechanism with a custom command. The command is specified as an array of strings (program + arguments). When invoked, tclaude writes a JSON object to the command's stdin:

```json
{"title":"Claude: Idle","body":"abc12345 | myproject - My conversation","sessionID":"abc12345..."}
```

| Field       | Value                                                       |
|-------------|-------------------------------------------------------------|
| `sessionID` | The full session ID                                         |
| `title`     | Notification title (e.g., `"Claude: Idle"`)                 |
| `body`      | Notification body (session ID, project, conversation title) |

The command must complete within 5 seconds; a warning is logged if it times out.

```json
{
  "notifications": {
    "enabled": true,
    "notification_command": ["my-notifier"]
  }
}
```

The command can take additional fixed arguments:
```json
{
  "notifications": {
    "enabled": true,
    "notification_command": ["my-notifier", "--format", "json"]
  }
}
```

When `notification_command` is set, it completely replaces the built-in platform notification (D-Bus, terminal-notifier, PowerShell toast). If the command fails, a fallback message is written to stderr.

### Examples

Only notify when permission is needed:
```json
{
  "notifications": {
    "enabled": true,
    "transitions": [
      {"from": "*", "to": "awaiting_permission"}
    ]
  }
}
```

Notify on any state change from working:
```json
{
  "notifications": {
    "enabled": true,
    "transitions": [
      {"from": "working", "to": "*"}
    ]
  }
}
```

## Notification Content

Notifications display:
- **Title:** `<Harness>: <state>` (e.g., "Claude: Idle" or "Codex: Idle")
- **Body:** Session ID, project name, and conversation title/prompt

## Platform Support

| Platform         | Status            | Notifications                    | Clickable | Focus Method                    |
|------------------|-------------------|----------------------------------|-----------|---------------------------------|
| macOS            | ✅ Tested          | `terminal-notifier` or osascript | ✅ Yes     | iTerm2/Terminal.app AppleScript |
| WSL              | ✅ Tested          | PowerShell toast notifications   | ✅ Yes     | Windows Terminal focus*         |
| Linux (native)   | ✅ Tested          | D-Bus                            | ✅ Yes     | xdotool                         |
| Windows (native) | ❌ Not implemented | -                                | -         | -                               |

**Notes:**
- \* WSL focus works best when the target session is the active tab. If it's in a background tab, clicking the notification will detach that tab and open a new window with the session.

## Prerequisites

- **tmux** - Required for session management (checked by `tclaude setup`)

## Clickable Notifications

Notifications are clickable on all supported platforms. Clicking a notification will focus the terminal window running that session.

### macOS

Requires `terminal-notifier` for clickable notifications:

```bash
brew install terminal-notifier
```

The setup command will offer to install this automatically if Homebrew is available.

### Linux (native)

For window focus when clicking notifications, install `xdotool`:

```bash
sudo apt install xdotool
```

Without it, notifications still work but won't be clickable.

### WSL

On WSL, notifications use Windows Toast notifications via PowerShell. Clicking them will focus the Windows Terminal window. This requires the `tclaude://` protocol handler to be registered, which `tclaude setup` handles automatically.

**Note:** If the target session is in a background tab, clicking the notification will detach that tab from tmux and open a new Windows Terminal window with the session attached.

If clicking doesn't work:
1. Run `tclaude setup --check` to verify protocol handler is registered
2. Run `tclaude setup --force` to re-register the handler

## Troubleshooting

### Notifications not appearing

1. Run `tclaude setup --check` to verify everything is configured
2. Check that `~/.tclaude/data/config.json` has `"enabled": true`
3. Check that the session state transition matches your configured rules

### WSL-specific issues

WSL requires PowerShell access for notifications. tclaude automatically uses PowerShell toast notifications when running in WSL. If notifications still don't work:

1. Verify PowerShell is accessible: `ls /mnt/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe`
2. Check Windows notification settings allow toast notifications
3. Run `tclaude setup` to ensure hooks and protocol handler are configured

### Cooldown

If you're not seeing notifications for rapid state changes, it's likely the cooldown. Notifications are rate-limited per session to prevent spam. Adjust `cooldown_seconds` if needed.
