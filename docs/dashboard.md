# Dashboard

!!! tip "New to tclaude?"
    The [Learn path](learn/index.md) walks through the everyday parts, starting from [Get started](learn/get-started.md).

The dashboard is the browser console for your agents. One screen shows every group
and agent: status, context, cost, branch and CI. Any row opens a live terminal on
that agent's real session, so you can go from "everything looks fine" to typing into
one agent in two clicks.

The `agentd` daemon serves it. It's for you, the human; agents use the daemon's API
instead. See [Agents and groups](agents-and-groups.md) for how they relate.

This page is a tour: how to open the dashboard and what's on screen. Each tab has
its own page.

## Opening it

```bash
tclaude agent dashboard          # open it in your default browser
tclaude agent dashboard --print  # print the one-time URL instead
tclaude agent ui                 # same as dashboard
```

The URL's token works once and expires in about a minute, so use a printed URL right
away. `--slop` and `--wizard` open the [cosmetic themes](#themes).

The dashboard only listens on `127.0.0.1` by default, on a random port that changes
when the daemon restarts. To fix the port, set **Dashboard port** in
[Config › Daemon & network](dashboard-config.md#daemon-network). A port that's taken
then stops the daemon with an error. **Auto-launch dashboard** in the same section
opens a browser when `tclaude agentd serve` starts. To reach it from another machine
or a phone, see [Operating remotely](remote.md).

## What you see first

A new install usually needs a few things. The **Diagnostics** tab shows a blue or red
badge when [Doctor](dashboard-diagnostics.md#doctor) has a question or a blocking
problem, so look there first. To turn on the Steward helper, use **Collab** under
[Config › Experimental](dashboard-config.md#experimental).

## The header

Across the top, from left to right:

| Control | What it does |
|---|---|
| Activity bots | One face per agent, with corner tags: ❓ asking, 💥 error, 💀 crashed, 💤 offline. Hover to group them by state. A `!` opens your newest notification. |
| Usage | One line per provider with a bar, percentage and what's left. Claude shows 5-hour and 7-day windows, Codex 5-hour and weekly, Copilot its monthly allowance, plus API cost. Click for [Usage](dashboard-usage.md). |
| Bell | Browser notifications: an on/off switch, per-type rows and delivery. Groups have their own mute. See [Notifications](notifications.md). |
| `live` | The connection to the daemon. |
| 🔍 | The [command palette](#command-palette) |
| 🪟 windows… | Focus or hide native agent windows, by group or role |
| 🟢 power on all | Wake every offline agent |
| 🛑 shutdown all | Stop every running agent: `/exit`, then force after a grace period. Agents can be resumed. |

## The tabs

| Tab | Use it to |
|---|---|
| [Groups](dashboard-groups.md) | See your agents, start new ones, open terminals |
| [Collab / Terminals](dashboard-terminals.md) | Work in terminals and [boards](boards.md) in the browser. Called Terminals until Collab is on. |
| [Automations](dashboard-automations.md) | Schedule jobs, set standing orders and triggers, edit processes |
| [Plugins](dashboard-automations.md#plugins) | Run shell-step plugins. Hidden until you have one. |
| [Access](dashboard-access.md) | See and change permissions, grant sudo |
| [Messages](dashboard-messages.md) | Read agent mail and notifications, answer access requests |
| [Usage](dashboard-usage.md) | Track spend and quota. Hidden until there's something to show. |
| [Diagnostics](dashboard-diagnostics.md) | Check Doctor, the audit trail, logs and performance |
| [Config](dashboard-config.md) | Change settings |

On the right of the tab bar are **⌂** for this node, **nodes ▾**, and **Fleet**,
with a **set up** pill until you've looked at it. See
[Fleet UI](fleet-ui.md) to link machines.

On every tab, the [companion](companion.md) sits at the bottom right with Collab on.
The footer shows the tclaude version, the last refresh and
[Open PRs](dashboard-groups.md#open-prs).

### Banners

**🔐 An agent is requesting access** with **Review** appears on any tab when an agent
is waiting on you. Spending limits raise their own banner.

## Command palette

Press Ctrl/Cmd-K or click 🔍, then type. It covers tabs, windows, spawning, retiring,
power, repository updates and every manager dialog.

- `spawn luna` finds saved profiles by name or alias. Add `fast` for fast mode on
  harnesses that have it, at a higher cost. It opens the spawn dialog with that
  filled in, for the last group you used.
- `terminal <group>` or `shell <group>` opens a shell in the group's default
  directory.
- **Announce to all live agents…** sends one message to everyone running.

On a peer's page, it runs on that peer, limited to what the peer allows.

## Keyboard shortcuts

| Keys | Action |
|---|---|
| Ctrl/Cmd-K | Command palette (inside a terminal, it needs **Command palette in terminals** under [Config › Experimental](dashboard-config.md#experimental)) |
| `[` and `]` | Previous and next tab |
| ← and → | Move along the focused tab bar |
| Alt+1, Alt+2…9 | This node, then peers in order |
| Ctrl/Cmd+M | Message the agent in the current terminal |
| Ctrl+. | Open or minimize the companion |
| Ctrl+Shift+. | Mark up the page |
| Ctrl/Cmd+Enter | Submit the spawn dialog |

In any ⚙ menu, start typing to filter, then use ↑, ↓ and Enter.

## Links and history

Every view has its own address, so Back and Forward work and you can bookmark or share
any view. Two overlays have addresses as well: a library manager
(`?manage=profiles`, `roles`, `sandbox` or `templates`) and the notification reader
(`?notification=<agent>:<message-id>`). Back closes them.

## How it stays current

The dashboard refreshes every 2 seconds, or every 10 when the browser tab is hidden.
Two failures in a row show a "Disconnected from agentd" overlay, which clears when the
daemon comes back.

When you upgrade tclaude and restart the daemon, open dashboards show "tclaude updated —
reloading…" and reload. An open terminal can ask you to confirm first so you don't lose
your session. If you cancel, the dashboard waits for the next change.

## Themes

The **wizard** and **slop** themes re-skin the same dashboard. They're not different
feature sets. Wizard renames things (Parties, Scrying, Labours and so on), and slop goes
full casino. Click the header icon to switch between regular and wizard, or use
Ctrl/⌘+Alt+Shift+W. Slop is unfinished, so it's left out of that cycle:
Ctrl/⌘+Alt+Shift+S toggles it. `?wizard=1` and `?slop=1` force one. A **Vegas** tab
appears in these themes.

## The TUI dashboard

A terminal fallback for when you have no browser. It's a list of agents, not the full
console.

```bash
# in the daemon, locally:
tclaude agentd serve --tui

# on its own, against a running daemon, local or over the network:
tclaude agent tui-dashboard --connect-to <url|host[:port]>
```

`--tui` replaces the web dashboard unless you also pass a dashboard flag. The
standalone form (alias `tui`) requires `--connect-to`. It signs in with
`--operator-token`, or `--remote-operator-token user@host:/abs/path`, which reads the
token over ssh, or else a local token.

It shows AGENT, GROUP, STATUS, HARNESS, DIRECTORY and BRANCH, plus plain tmux sessions
(local only) and a usage line.

| Key | Action |
|---|---|
| ↑ ↓ or `j` `k` | Move |
| `enter` | Attach, or resume an offline agent. Remotely, detach with `ctrl-] d`. |
| `n` | Spawn form |
| `s` | Local shell (local only) |
| `delete` | Step from live to offline to retired, with a confirmation |
| `f` | Show only active |
| `r` | Refresh |
| `?` or `h` | Help |
| `q`, `esc`, `ctrl+c` | Quit, with a confirmation. Quitting a `--tui` daemon **shuts down the daemon**. |

The spawn form has Model and Effort fields. Leave them blank to use the profile or
harness default. Text fields take your terminal's paste (`ctrl+shift+v`, `cmd+v` or
tmux's `prefix ]`). `ctrl+v` is deliberately not bound, because the console can be a
pane an agent drives through tmux `send-keys`.

The TUI can only spawn, resume, stop, retire and create worktrees. It can't connect
through the remote-access mTLS listener, so over a network, use the plain dashboard
listener (`--dashboard-bind`) behind your own auth or VPN. See
[Operating remotely](remote.md).
