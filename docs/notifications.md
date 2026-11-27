# Notifications

tclaude raises a desktop notification when a session transitions into a
state that needs you: by default `idle`, `awaiting_permission`,
`awaiting_input`, `error` and `exited`. Clicking the notification focuses the
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
      {"from": "*", "to": "error"},
      {"from": "*", "to": "exited"}
    ],
    "cooldown_seconds": 5
  }
}
```

- `transitions` — `{from, to}` rules with `*` wildcards; the defaults above
  apply when the list is omitted.
- `known_types` — bookkeeping for `tclaude setup`: the per-type
  categories this block has already been offered. Setup only adds a
  category that is new to your config; one you switched off stays off.
  Written automatically; you never need to edit it.
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

## Presented PRs

`tclaude agent present-pr` always puts the PR on the agent's dashboard row.
It can optionally raise a notification too — the presenting agent, the PR
URL, and (when they fit) the `--summary` and the agent's group — so a review
request reaches you without watching the dashboard:

```json
{
  "agent": {
    "present_pr_notification": true
  }
}
```

Off by default, and the `notifications.enabled` master switch still applies:
both must be on for it to fire. It then goes wherever `delivery` sends every
other notification — the desktop, a dashboard tab, or both — so under
`"delivery": "browser"` it is a dashboard banner rather than a desktop one.
The per-agent and per-group filters apply as well, so a muted agent's PR
still appears on the dashboard row, it just raises nothing. Only presenting
a PR notifies — `--handled` retires one silently. The Config tab has a
checkbox for this under Notifications → Presented PRs, and so does the
header bell's popover ("Presents a pull request").

The PR URL is the part that has to survive: a banner body is capped, and
when the whole message does not fit, the summary and group are dropped and a
long agent name is trimmed before the URL is touched.

An `idle` notification means the harness finished and no tracked subagent,
background shell, or monitor is still working — tclaude holds the session at
an internal `main_agent_idle` state until those settle, so you are not
pinged while a subagent is still running.

!!! note "Harness labeling caveat"
    Notification titles are `<Harness>: <Status>`: `Claude: …`, `Codex: …`,
    `OpenCode: …`, `Copilot: …`, `Gemini: …`, `pi: …` or `Shell: …`. Agent
    `notify-human` messages, presented PRs and access requests say `tclaude: …`.
    The task runner and rate-limit warnings always say `Claude: …`, whichever
    harness is involved.

## Push to your phone (Web Push)

Push sends notifications to the devices you subscribe, through each
browser's push service. A locked phone, or a home-screen app in the
background, still gets them. Nothing is sent until a device subscribes.

Push has its own on/off for each category. Two things are shared with the
desktop: the `enabled` master switch, and the per-agent and per-group
mutes. Desktop banners and push therefore never force each other on.

| Category | Default | Pushed when |
|---|---|---|
| `access_requests` | on | an agent's `--ask-human` / sudo request waits for approval |
| `operator_questions` | on | a question waits for your answer: a taskboard `ask-operator` question, a `notify-human` decision card (with options), a process step waiting on a person |
| `awaiting_permission` | on | an agent waits for a tool permission |
| `awaiting_input` | on | an agent asked you something in its harness (Claude Code's question dialog, Codex's user-input request), not an ordinary end of turn |
| `human_messages` | off | any other `notify-human` message (often a status update) |
| `present_pr` | off | an agent presents a PR |
| `idle`, `error`, `exited` | off | an agent changes to that state |

```json
{
  "notifications": {
    "enabled": true,
    "push": {
      "categories": {"idle": true, "awaiting_input": false},
      "content_free": false
    }
  }
}
```

Only the categories you change are stored. The block is this node's own: no
federation peer changes it, and no agent can.

- **Privacy.** A push is encrypted end to end (RFC 8291). The push service
  (Apple, Google, Mozilla, Microsoft) relays it but can't read it. Even so,
  the text is kept to what a lock screen would show:
  - An access request names who asks and for which permission, never the
    request's path, body or reason.
  - A message or question gives its subject and sender, never its body or
    context.
  - With `content_free` on, a push carries no text at all. The device shows
    "tclaude needs you", and the app shows what it is once opened.
- **Delivery.** A push not delivered within 10 minutes is dropped. A device
  whose subscription the browser dropped (the push service answers 404 or
  410) is removed. A busy push service is retried, honouring its
  Retry-After.
- **Federation.** Each node pushes only its own notifications, to the
  devices subscribed on it. To get a peer's, subscribe on that peer's own
  dashboard.
- **CLI.** Every control has a CLI twin, refused to agents:

```bash
tclaude notifications push status          # categories, content-free, devices
tclaude notifications push devices
tclaude notifications push set idle on     # any category, or content-free
tclaude notifications push test [<device>]
tclaude notifications push rm <device>
tclaude notifications push reset-keys --yes   # new key; every device re-subscribes
```

A device subscribes from the dashboard (see [Dashboard](dashboard.md)).
Push needs HTTPS (the [remote](remote.md) listener with its CA trusted, or a
tunnel). On iPhone it works only for the app added to the Home Screen.

## Click-to-focus per platform

| Platform | Notifier | Clickable | Focus method |
|----------|----------|-----------|--------------|
| Linux | D-Bus | Yes | `xdotool` or `kdotool` |
| macOS | `terminal-notifier` (or osascript) | Yes | iTerm2 / Terminal.app AppleScript |
| WSL | PowerShell toast | Yes | Windows Terminal, via `tclaude://` handler |
| Windows (native) | Not implemented | — | — |

### Linux

Notifications go through D-Bus; window focus on click needs `xdotool`
(`sudo apt install xdotool`), or `kdotool` on KDE Plasma Wayland. Without either, notifications still appear but are
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
