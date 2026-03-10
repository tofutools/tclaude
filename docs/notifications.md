# OS Notifications

Get notified when Claude sessions need attention.

## Overview

tclaude can send OS notifications when Claude sessions transition to states that require user attention (idle, awaiting permission, awaiting input). This is useful when running multiple sessions or working in a different window.

**Disabled by default** - run `tclaude setup` to enable.

## Quick Setup

The easiest way to enable notifications:

```bash
tclaude setup
```

This will:
1. Install Claude hooks for status tracking
2. Register the protocol handler (WSL/Windows) for clickable notifications
3. Ask if you want to enable notifications

You can check your setup status anytime:

```bash
tclaude setup --check
```

## Manual Configuration

Alternatively, create `~/.tclaude/config.json` manually:

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

### Transitions

Each transition rule has `from` and `to` fields. Use `*` as a wildcard to match any state.

**Default transitions:**
- `*` → `idle` - Claude finished processing
- `*` → `awaiting_permission` - Claude needs permission to proceed
- `*` → `awaiting_input` - Claude is asking a question
- `*` → `exited` - Session ended

**Available states:** `working`, `idle`, `awaiting_permission`, `awaiting_input`, `exited`

### Custom Notification Command

You can override the platform-specific notification mechanism with a custom command. The command is specified as an array of strings (program + arguments), where each element may contain these template placeholders:

| Placeholder     | Value                                                       |
|-----------------|-------------------------------------------------------------|
| `{{sessionID}}` | The full session ID                                         |
| `{{title}}`     | Notification title (e.g., "Claude: Idle")                   |
| `{{body}}`      | Notification body (session ID, project, conversation title) |

All placeholders are optional — use only the ones you need, in any order.

```json
{
  "notifications": {
    "enabled": true,
    "notification_command": ["notify-send", "{{title}}", "{{body}}"]
  }
}
```

Pass parameters as flags in any order:
```json
{
  "notifications": {
    "enabled": true,
    "notification_command": ["my-notifier", "--session", "{{sessionID}}", "--msg", "{{body}}"]
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
- **Title:** `Claude: <state>` (e.g., "Claude: Idle")
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
2. Check that `~/.tclaude/config.json` has `"enabled": true`
3. Check that the session state transition matches your configured rules

### WSL-specific issues

WSL requires PowerShell access for notifications. tclaude automatically uses PowerShell toast notifications when running in WSL. If notifications still don't work:

1. Verify PowerShell is accessible: `ls /mnt/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe`
2. Check Windows notification settings allow toast notifications
3. Run `tclaude setup` to ensure hooks and protocol handler are configured

### Cooldown

If you're not seeing notifications for rapid state changes, it's likely the cooldown. Notifications are rate-limited per session to prevent spam. Adjust `cooldown_seconds` if needed.
