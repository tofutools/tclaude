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
problem, so look there first. To make a Steward helper, click **✦ Steward** at the
bottom right; see [Ask the Steward](learn/steward.md).

## The header

Across the top, from left to right:

| Control | What it does |
|---|---|
| Activity bots | One face per agent, with corner tags: ❓ asking, 💥 error, 💀 crashed, 💤 offline. Hover to group them by state. A `!` opens your newest notification. |
| Usage | One line per provider with a bar, percentage and what's left. Claude shows 5-hour and 7-day windows, Codex 5-hour and weekly, Copilot its monthly allowance, plus API cost. A selector switches units where they apply (`% / ≈$ / AIC`). Click for [Usage](dashboard-usage.md). |
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
| [Collab](dashboard-terminals.md) | Work in terminals and [drawing boards](drawing-boards.md) in the browser. |
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

On every tab, the [companion](companion.md) sits at the bottom right.
The footer shows the tclaude version, the last refresh,
[Open PRs](dashboard-groups.md#open-prs) and, with a GitLab instance
configured, [Open MRs](dashboard-groups.md#open-mrs).

### Banners

**🔐 An agent is requesting access** with **Review** appears on any tab when an agent
is waiting on you. Spending limits raise their own banner.

## Command palette

Press **Ctrl K** (**⌘ K** on a Mac) or click 🔍, then type. It covers tabs, windows, spawning, retiring,
power, repository updates and every manager dialog.

- `spawn luna` finds saved profiles by name or alias. Add `fast` for fast mode on
  harnesses that have it, at a higher cost. It opens the spawn dialog with that
  filled in, for the last group you used.
- `terminal <group>` or `shell <group>` opens a shell in the group's default
  directory.
- `drawing board <group>` (or `whiteboard`, or a phrase such as `open drawing
  drawing board in tclaude`) makes a drawing board in that group and opens it.
  `page <group>` makes a live page there, and `process <group>` opens the
  group's start form. The 🧙 words `slate`, `looking glass` and `rite` find
  them too, in either theme.
- **Announce to all live agents…** sends one message to everyone running.

On a peer's page, it runs on that peer, limited to what the peer allows.

## Keyboard shortcuts

**Config › Keyboard** lists every shortcut, including those of the process
editor, drawing boards and the image viewer, with its keys on Windows and Linux and on
macOS, and lets you [rebind](dashboard-config.md#keyboard) the dashboard-wide and
terminal ones. The table below shows the defaults. Keys are written one name at a time: **Ctrl ,** is Ctrl and the comma
key, not a plus. Shortcuts follow the key's position, so they sit on the same
keys on any keyboard layout, and the list labels them with what those keys type
on yours.

<!-- shortcuts:begin -->
| Action | Windows & Linux | macOS | In a terminal |
|---|---|---|---|
| **Anywhere** | | | |
| Open the command palette | <kbd>Ctrl</kbd> <kbd>K</kbd> | <kbd>⌘</kbd> <kbd>K</kbd> | with **Command palette in terminals** |
| Open or minimize the Steward window | <kbd>Ctrl</kbd> <kbd>.</kbd> | <kbd>Ctrl</kbd> <kbd>.</kbd> | yes |
| Mark up the page for the agent in the window | <kbd>Ctrl</kbd> <kbd>Shift</kbd> <kbd>.</kbd> | <kbd>Ctrl</kbd> <kbd>Shift</kbd> <kbd>.</kbd> | yes |
| Show all windows side by side, to pick one | <kbd>Ctrl</kbd> <kbd>,</kbd> | <kbd>Ctrl</kbd> <kbd>,</kbd> | yes |
| Minimize all windows, or bring back the ones it minimized | <kbd>Ctrl</kbd> <kbd>;</kbd> | <kbd>Ctrl</kbd> <kbd>;</kbd> | yes |
| Message the agent in the current terminal or window | <kbd>Ctrl</kbd> <kbd>M</kbd> | <kbd>Ctrl</kbd> <kbd>M</kbd> | yes |
| Next dashboard tab | <kbd>]</kbd> | <kbd>]</kbd> | no |
| Previous dashboard tab | <kbd>[</kbd> | <kbd>[</kbd> | no |
| Switch node: this node, then peers in order | <kbd>Alt</kbd> <kbd>1…9</kbd> | <kbd>Option</kbd> <kbd>1…9</kbd> | no |
| Toggle wizard mode | <kbd>Ctrl</kbd> <kbd>Alt</kbd> <kbd>Shift</kbd> <kbd>W</kbd> | <kbd>Option</kbd> <kbd>Shift</kbd> <kbd>⌘</kbd> <kbd>W</kbd> | no |
| Toggle slop mode | <kbd>Ctrl</kbd> <kbd>Alt</kbd> <kbd>Shift</kbd> <kbd>S</kbd> | <kbd>Option</kbd> <kbd>Shift</kbd> <kbd>⌘</kbd> <kbd>S</kbd> | no |
| **Web terminals** | | | |
| Copy the selected terminal text | <kbd>Ctrl</kbd> <kbd>Shift</kbd> <kbd>C</kbd> | <kbd>Shift</kbd> <kbd>⌘</kbd> <kbd>C</kbd> | yes |
| Paste text or files | <kbd>Ctrl</kbd> <kbd>V</kbd> | <kbd>⌘</kbd> <kbd>V</kbd> | yes |
| New line in the prompt | <kbd>Shift</kbd> <kbd>Enter</kbd> | <kbd>Shift</kbd> <kbd>Enter</kbd> | yes |
| Move the focused terminal tab or tab group left | <kbd>Alt</kbd> <kbd>Shift</kbd> <kbd>←</kbd> | <kbd>Option</kbd> <kbd>Shift</kbd> <kbd>←</kbd> | no |
| Move the focused terminal tab or tab group right | <kbd>Alt</kbd> <kbd>Shift</kbd> <kbd>→</kbd> | <kbd>Option</kbd> <kbd>Shift</kbd> <kbd>→</kbd> | no |
| **Dialogs** | | | |
| Submit the dialog or send the message | <kbd>Ctrl</kbd> <kbd>Enter</kbd> | <kbd>⌘</kbd> <kbd>Enter</kbd> | no |
| Spawn dialog: add to the queue | <kbd>Ctrl</kbd> <kbd>Shift</kbd> <kbd>Enter</kbd> | <kbd>Shift</kbd> <kbd>⌘</kbd> <kbd>Enter</kbd> | no |
| Undo in the group and markup editors | <kbd>Ctrl</kbd> <kbd>Z</kbd> | <kbd>⌘</kbd> <kbd>Z</kbd> | no |
<!-- shortcuts:end -->

The command palette inside a terminal needs **Command palette in terminals**
under [Config › Experimental](dashboard-config.md#experimental), because
Ctrl K clears the input line there. ← and → move along a focused tab bar.

In any ⚙ menu, start typing to filter, then use ↑, ↓ and Enter.

## Menus and popovers

A menu or popover you open with a click closes on a click outside it, on `Esc`,
or when the pointer stays away for a moment. Moving back in time keeps it open.
Text you've typed in it, or a field you're using, keeps it open when the pointer
leaves, so nothing is lost; a click outside or `Esc` still closes it. Dialogs
don't work this way: they stay until you answer them. A filter or search box in a
menu has the cursor as soon as the menu opens, so you can start typing.

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

## On a phone

On a phone the dashboard has a simpler layout. Tablets and narrow desktop
windows keep the full dashboard. A bar at the bottom has five views:

| View | What it shows |
|---|---|
| **Now** | What needs you: access requests and decisions, each with its buttons; agents asking a question, with **Open** for the terminal and ✉ to message them. Below that: agents in an error state, agents that are working, your open PRs and usage. |
| **Agents** | Your agents by group, with a tab per group. Each row shows the agent's state, role, harness, context and its PR with checks, or its branch. Offline agents are folded away. Tap an agent for its actions: open the terminal, message, shut down or power on, restart, rename and retire. **＋ New** spawns an agent into a group. |
| **Mail** | The [Messages](dashboard-messages.md) tab |
| **Board** | Each group's [task board](taskboard.md), one group at a time, as lists: waiting on you, blocked, in progress, next and ideas. Tap a task to open it. |
| **More** | Every other tab, **Power on all**, **Shut down all…** and **Desktop site** |

Overlay windows, the side panel, drag and drop and the voice wake word aren't
on the phone layout.

### Terminals on a phone

**Open** on an agent, from Now, Agents or Mail, shows its terminal full
screen, one at a time. ‹ or the phone's Back closes it and returns to where you
opened it. Only the terminal on screen is connected: closing it, or switching
away from the browser, disconnects it, and coming back reconnects.

- **Message *agent*…** at the bottom opens the message sheet, the same one as
  Ctrl+M on a desktop. This is the usual way to talk to an agent from a phone.
- The keys above it send **1**, **2**, **3**, **↑**, **↓**, **⏎** (Enter),
  **Esc** and **^C** (Ctrl+C), which is enough to answer a harness's choices
  and interrupt it.
- **⌨** raises the phone's keyboard for typing straight into the terminal.
  Tapping the terminal doesn't raise it.
- Pinch to change the text size. The terminal refits its columns to the new
  size, and the size is remembered for each terminal.

**Desktop site** in **More** shows the full dashboard on this phone, and **Phone
view** at the bottom of that page switches back. The choice is kept in this
browser only. You can also add `?layout=phone`, `?layout=desktop` or
`?layout=auto` (back to detection) to any dashboard address.

## Themes

The **wizard** and **slop** themes re-skin the same dashboard. They're not different
feature sets. Wizard renames things (Parties, Scrying, Labours and so on), and slop goes
full casino. Click the header icon to switch between regular and wizard, or use
Ctrl Alt Shift W (Option Shift ⌘ W on a Mac). Slop is unfinished, so it's left out of that cycle:
Ctrl Alt Shift S (Option Shift ⌘ S on a Mac) toggles it. `?wizard=1` and `?slop=1` force one. A **Vegas** tab
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
