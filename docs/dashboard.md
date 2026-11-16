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
| **Now** | What needs you: access requests and decisions, each with its buttons; process runs waiting on your verdict, with **Approve** and **Request changes**; agents asking a question, with **Open** for the terminal and ✉ to message them. Below that: agents in an error state, agents that are working, your open PRs and usage. |
| **Agents** | Your agents by group, with a chip per group and its agent count. Tap chips to show only those groups, as many as you like; **All** shows every group again. Under each group's name a line counts its agents by state, loudest first (for example 1 error · 3 working · 2 idle), folded or not. Tap the caret (or the space left of the name) to fold a group; it stays folded next time. Tap the group's name to open its page (below); **Ungrouped** has no page, so its name folds it. An agent that another agent spawned sits indented under that agent, which says how many workers it has. Each row shows the agent with a state pill (approval, asking, working, idle, error, offline), its role, harness, context and its PR with checks, or its branch. Offline agents are listed dimmed. Tap an online agent to open its terminal, or an offline one to resume it. **⋯** has the rest: message, shut down or power on, restart, rename and retire, and for a Claude Code agent **Open in Claude app ↗** (or **Turn on Remote Access & open**), which opens the same session in Anthropic's Claude app ([Remote](remote.md#opening-the-session-in-the-claude-app)). **Board ›** opens a group's board. **＋** spawns an agent from a short sheet: a group, a profile and a prompt, with **All options…** for the full form.<br><br>Groups are listed as the desktop's Groups tree nests them: subgroups are indented on a guide line under their parent, each with its state line. A group with nothing online is dimmed and starts folded. The sliders in the top bar open **View**. **Layout** is **Tree** (subgroups nested, workers under their leads) or **List** (one level: every subgroup its own section, every agent its own row). **Show** has **Offline agents** (on by default; off hides offline agents and the groups with nobody online, though crashed agents and ones waking up stay listed), **Empty groups** (off by default; a group with no agents is tagged *empty*) and **Archived groups** (off by default; they come last). **Order groups by** is **Your group order** (as on the desktop, the default), **Needs you first** (errors, approvals and questions), **Most active** (working first) or **Name**. **Reset** puts every setting back. The phone remembers these settings, and the sliders carry a dot while any of them is off its default. The chips and their counts follow what is shown. A group's page shows where the group sits (tap a parent to go there), how much of it is online, and **⏻ Power on all**, **⏻ Shut down all**, **＋ Spawn**, **Board ›** and **Mail ›**, which ask first as the desktop does. Below that come its subgroups (tap one for its page) and its members, workers under their leads; tap an offline member to resume it. An archived group's page offers only **Mail ›**. |
| **Mail** | [Messages](dashboard-messages.md) as a phone mail app: folder chips at the top, then one row per message with its first line and attachments. A pending decision or access request has its buttons right on the row. Tap a message to read it, with **Reply** at the bottom (above the bottom bar, which **▾** beside it hides; see below) and **Terminal ›** at the top. **✎ Write** starts a new message. |
| **Board** | A group's [task board](taskboard.md), one column at a time: **Idea**, **Next**, **Started**, the board's own columns and **Done**. Each column is split into ready, waiting on you, queued and blocked. A card shows who holds the task, its PR and refs. A question waiting on you has its options right on the card. **Move ▸** moves a task to another column, or up or down. **Comment** adds a comment. Tap the group name in the title to pick another group, and **＋** to add a task. Tap a task for its page: brief, refs, activity, its open questions and a comment box. Editing a task is on the desktop. |
| **More** | Every other tab, each with a badge for what you can do there on a phone: read-only (blue), or a few things such as **on/off, run now** (green). **Diagnostics** shows the Doctor's count. Then **Power on all**, **Shut down all…**, **Notifications** (on or off; tap it to change it), **Update tclaude** and **Desktop site**. **Update tclaude** runs this node's saved update method, as **Config › Installation › Updates** does. It shows the version, the method and whether a newer release is known. **Update…** asks *Update and restart agentd?*, saying what the method will do. It then follows the run: its phase, agentd restarting, and the new version once agentd is back, or the error with the run's output. Back and a swipe wait while it runs, until agentd stops answering for its restart. **Close** is offered then, and the update goes on at the node. With no method chosen, or **Notify me (manual)**, it links to **Config › Updates** on the desktop site instead. On a peer's page it needs that node's `node.update` grant. The menu opens above the bottom bar: tap **More** again, tap outside it or drag its handle down to close it |

**Automations** (in **More**) lists cron jobs, standing orders, triggers and
exports by kind. Each has a switch to turn it on or off, and a cron job has ▶ to
run it now. Tap one for its details and its recent runs or firings. **Runs**
lists process runs from every group: waiting on you, running and done. Tap one
for its steps, with who did each finished step and when. Its gate is a card on
top: **Approve** in one tap, or **Request changes** with a note. A parked step
shows its reason with **Retry** and **Skip this step**. **Pause** in the
top bar pauses the run. Creating and editing automations and process templates
are desktop only.

**Open sessions** (in **More**) is the phone's Collab: a plain list, by group,
of the agents, sessions and shells running. Each row shows its state, and an
agent waiting on you says **needs you**. Tap a row to open its terminal full
screen. Drawing boards, processes and pages are on the desktop's Collab.

Overlay windows, the side panel, drag and drop and the voice wake word aren't
on the phone layout.

In a terminal and in a message with **Reply**, the bottom bar stays under the
view, and **▾** left of the message or reply field hides it, giving its
height to the terminal or the message. **▴** brings it back. While it is
hidden, **▴** carries a red count of what the bar's badges would show (Now
and Mail together). Terminals and mail each remember their own choice, on
every device. Tapping a view in the bar from a terminal closes the terminal.
The bar is never shown over the phone's keyboard.

### Installing it as an app

The dashboard can be installed on a phone's home screen, and then opens as an
app of its own: its own window and icon, with no address bar.

- **Android (Chrome):** open the dashboard, then **⋮ › Add to home screen ›
  Install** (or **Install app**).
- **iPhone and iPad (Safari):** **Share › Add to Home Screen**.
- A desktop Chrome or Edge offers the same as the install icon in the address
  bar.

Chrome and Edge offer to install only on a secure origin: `https://` (the
[remote-access listener](remote.md#remote-dashboard-access) with its CA
trusted on the device, `tailscale serve`, a tunnel) or `localhost`. Over plain
`http://` on a LAN address, Chrome makes a shortcut that opens in the browser.

Installed, it works as it does in the browser, with these differences:

- **Pull down on the phone layout's header bar to reload.** An installed app has no reload
  button, and the browser's own pull-to-refresh is off in it, so the dashboard
  does the pull itself: pull the bar at the top down until the hint says
  **Release to reload**. In a browser tab the browser's pull-to-refresh does
  this as before. A desktop install reloads with Ctrl+R or the app's menu.
- **Links to other sites open in the browser** (or in that site's app), not
  inside the installed app: a GitHub PR, a claude.ai link. Links within the
  dashboard stay in it.
- **Back:** on Android the phone's Back works as in the browser and closes
  what is open. On an iPhone, which has no Back button for a home-screen app,
  use the dashboard's own **‹** buttons.

There is no service worker: nothing is cached for offline use and there are no
push notifications. Every start loads the dashboard from the node, as a
browser tab does, so an installed app picks up a new version the same way; with
no connection to the node it shows the browser's error page.

The app's manifest (`/manifest.webmanifest`) and its icons (under `/pwa/`) are
served without a dashboard session, because the browser fetches them on its
own, outside the page (to check that it can install it, and later to update
it). They describe the app and hold nothing about the node; every other page,
file and API still needs the session. Whatever stands in front of the
dashboard still applies to them: the page requests the manifest with
credentials, so a Cloudflare Access cookie goes along, and on the
remote-access listener they need the client certificate and passphrase like
everything else.

Each icon's URL carries a hash of its content, so a changed icon after an
upgrade is never served from an old cache (a CDN in front of a tunnel, the
phone's own), and the manifest is fetched fresh every time. The phone still
updates an installed app's icon on its own schedule: Android when Chrome next
refreshes the installed app (it can take a day), or right away if you
reinstall it.

### Terminals on a phone

**Open** on an agent, from Now, Agents or Mail, shows its terminal full
screen, one at a time. ‹ or the phone's Back closes it and returns to where you
opened it. Only the terminal on screen is connected: closing it, or switching
away from the browser, disconnects it, and coming back reconnects.

- **⋯** in the top bar is the agent's menu, as on its row; for a Claude Code
  agent it offers **Open in Claude app ↗**, the same session in the Claude
  app.
- **Message *agent*…** at the bottom opens the message sheet, the same one as
  Ctrl+M on a desktop. This is the usual way to talk to an agent from a phone.
- The keys above it send **1**, **2**, **3**, **↑**, **↓**, **⏎** (Enter),
  **Esc** and **^C** (Ctrl+C), which is enough to answer a harness's choices
  and interrupt it.
- A shell's terminal (a group's or an agent's shell, or an agent running the
  `shell` harness) has **› Type into *shell*…** in place of the message
  sheet: what you write goes into the shell as keystrokes, with no message
  header, by **Type** or **Type + ⏎** (then Enter). Its keys are **←**,
  **→**, **↑**, **↓**, **⇥** (Tab), **␣** (Space), **⏎**, **Esc** and **^C**.
- **⌨** raises the phone's keyboard for typing straight into the terminal.
  Tapping the terminal doesn't raise it.
- Pinch to change the text size. The terminal refits its columns to the new
  size, and the size is remembered for each terminal.
- Swipe up or down with one finger to scroll, exactly as the mouse wheel
  scrolls that terminal on a desktop. Each harness keeps its own scrolling:
  Claude Code's fullscreen TUI scrolls its own conversation history, and Codex
  and shell sessions scroll tmux's history (copy-mode). Where nothing takes the
  mouse, a swipe sends the arrow keys a desktop wheel would. The text moves
  about as far as your finger does: in Claude Code's fullscreen TUI, which
  scrolls a line a wheel notch and speeds up on quick notches, the swipe
  sends the notches that keep up with the finger. A quick flick keeps
  scrolling after you lift your finger and slows down as a native list does.
  In tmux's history (Codex,
  shells), **↓ Live** scrolls back down to the live screen while you're
  scrolled back, and so does swiping back down; pressing a key or typing
  scrolls back down first, so tmux doesn't take the key. Claude Code has its
  own way back to the bottom, and takes keys while scrolled back, as on a
  desktop.

### Settings on a phone

**Settings** (under **More**) is a list. Tap a section to open it as a page; ‹
or the phone's Back returns to the list. A link to a section, such as
`/config/notifications`, opens that section.

- **This phone** has **Desktop site** and the wizard theme. Neither is saved to
  `config.json`; they apply to this browser only.
- **Notifications**, **Usage & costs** and **Steward** can be changed on the
  phone. They use the same form as the desktop: **Save changes…** shows the diff
  before anything is written. The phone leaves out the `config.json` key under
  each setting, and Notifications leaves out its **Advanced** block (raw
  transition rules and the notification command).
- Every other section can be read but not changed on a phone. Use **Desktop
  site** to change it.

### Access and Usage on a phone

**Access** (under **More**) has four views:

- **Requests**: access requests waiting on you, with the same buttons as on
  Now. It opens here while any request waits.
- **Sudo**: active sudo grants with the time left and **Revoke**. **+ Grant
  sudo** grants a new one.
- **Agents**: each agent's grants and denials beyond the defaults. You can only
  read them here; change them on the desktop.
- **Slugs**: the permission slug registry, with a search.

**Usage** (under **More**) has the same three views as on the desktop, as
chips: **Overview**, **Quota** and **Costs**.

- **Overview**: the same summary cards as on the desktop.
- **Quota**: one card per quota window, with the windows predicted to run out
  first. Each card shows the % used with a bar, when the window resets, a
  sparkline and the prediction.
- **Costs**: chips for the span (**This month**, **Last 7d**, **Last 30d**,
  **Last 90d**), then the total, the totals per provider and per model, and
  the top five agents.

Use the desktop for the rest: history ranges and prediction choices, past
months, charts, filters, multipliers and the per-agent table. Provider and
model choices made on the desktop still apply on the phone.

### Diagnostics, Plugins and Fleet on a phone

**Diagnostics** (under **More**) opens on the same four views, with **Doctor**
first:

- **Doctor**: the same checks and decisions as on the desktop, with its
  categories as a row of tabs.
- **Audit** and **Logs**: one row per event or log line. The first line says
  who did what, or the message; the second gives the target and detail, or
  the fields. The outcome or level is on the right. Tap a row to see all of it.
  Each has one search, plus **Failed only** (Audit) or a level (Logs).
- **Performance**: the dashboard's poll timings, one card per endpoint.
  Daemon profiling and the per-phase breakdowns are desktop only.

**Plugins** shows a card per installed plugin, with its description. The
switch activates or deactivates it.

- A plugin whose checks all pass is folded to one line ("2 steps · checked 3m
  ago"). A plugin that is off is folded and dimmed. Tap the name to show the
  steps.
- A plugin that needs a look shows its steps right away, each with its last
  output.
- Each step has ▶ to run it or ■ to stop it, as its lamp offers on the desktop.
- **Check** re-runs the plugin's checks.
- **Check all**, in the top bar, checks every plugin.

Installing from the catalog and editing plugins are desktop only.

**Fleet** (under **More**) is read-only: this node and its hub connection, the
trusted peers (online, or when last seen, and their access level), and the
instances asking to be trusted. Trust, grants, hub settings and the rest of
Fleet admin are desktop only.

### Views that are desktop only

Fleet admin's sections (beyond Fleet's own read-only view), the Fleet map, the
Groups route map and Vegas need a wide screen. On a phone they show a page that
says so, with **Desktop site** to open the same page in the full dashboard. The
Fleet map's page also has **Show fleet**, and the route map's page has **Show
agents**.

Links into the desktop's views still work on a phone:

- A Groups run link (`/groups/runs/<group>/<run>`) opens the run in
  **Automations**. **Back** then shows the Runs list.
- A drawing board opens read-only, fitted to its contents. You can pan and zoom
  but not draw. Drawing and editing are desktop only.

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
