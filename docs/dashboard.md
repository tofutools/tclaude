# Agent Dashboard 📊

The **agentd dashboard** is a browser UI for inspecting and operating the
agent-coordination system — groups, agents, permissions, scheduled jobs, and
time-bounded elevations — without dropping to the CLI for every action. It is
served by `tclaude agentd` on its loopback port and is **human-only**.

> **BETA / EXPERIMENTAL**
>
> The dashboard is part of the experimental [Agent Coordination](agent.md)
> feature. Tabs, endpoints, the wire format, and the SQLite schema can all
> change without notice.

It used to be a read-only viewer; it is now a full operations console. Almost
everything you can do with `tclaude agent` on the command line you can also do
here — spawn agents, edit group membership, wake/stop sessions, schedule cron
jobs, grant elevations.

## Opening the dashboard

The daemon (`tclaude agentd serve`) must be running. Then, as the human:

```bash
tclaude agent dashboard          # open in your default browser
tclaude agent dashboard --print  # print the one-shot URL instead of opening
tclaude agent ui                 # 'ui' is an alias for 'dashboard'
```

Other entry points:

- **System tray** — `agentd serve` adds a tray icon on hosts that support one;
  its **Open dashboard** item opens the dashboard with no terminal round-trip.
  The icon's **colour** is a glanceable summary of the daemon's state, in
  priority order:
  - **blinking green↔red** — at least one agent is blocked on you: a Claude
    Code permission prompt / elicitation dialog (`awaiting_*`), a turn that
    ended in error, or a pending `--ask-human` approval popup. Act now.
  - **orange** — a sudo grant is currently active somewhere (a passive
    "an elevation window is open" reminder).
  - **yellow** — every online agent is idle (the quiet state — nothing is
    working and nothing needs you).
  - **green** — at least one agent is working, or there are no online agents
    (the default).

  The same colours match the per-agent status dots/pills on the dashboard.
  Hover the tray icon for the count behind whichever colour is showing.
  Pass `--no-tray` (or set `agent.disable_tray: true` in
  `~/.tclaude/config.json`, or tick **System tray → hide** in the Config tab)
  to run the daemon without the tray icon.
- **On startup** — `tclaude agentd serve --auto-launch-dashboard` (or
  `agent.auto_launch_dashboard: true` in `~/.tclaude/config.json`) pops the
  dashboard automatically when the daemon comes up. Off by default — a fresh
  daemon doesn't open a browser tab uninvited.

The `--print` URL carries a single-use token that expires in ~60 seconds, so
use it immediately.

## Fixed loopback port

By default the dashboard (and the approval popup it shares a listener with)
binds a **random** free loopback port each time `agentd serve` starts. To pin a
**fixed** port instead — for a bookmarkable URL, a reverse proxy, or a firewall
rule — pass `tclaude agentd serve --dashboard-port <port>`, or set
`agent.dashboard_port` in `~/.tclaude/config.json` (also editable from the
**Config** tab). Resolution order is flag > config > random.

Binding is strict: if the configured port is already in use (or out of range),
`agentd serve` **fails to start** with an error rather than silently falling
back to a random port — a silent fallback would break the bookmark / proxy /
rule the fixed port was set up for. The port is loopback-only and stays
human-gated (token + cookie) either way.

## Auth

The dashboard's `/api/*` endpoints perform admin mutations that deliberately
**bypass the per-agent permission system** — they are the human's controls, not
an agent's. To stop an agent that can open a loopback socket from reaching
them, access is gated by an **init-token exchange**:

1. `tclaude agent dashboard` calls the daemon's human-only
   `/v1/dashboard/open` endpoint over the peer-credential-authenticated Unix
   socket. Any caller with a Claude Code ancestor process (i.e. an agent) gets
   a `403`; the human gets a URL carrying a one-shot `init_token`.
2. Opening that URL exchanges the token for an `HttpOnly` / `SameSite=Strict`
   session cookie, then 303-redirects to the bare path so the token never
   lingers in the address bar, browser history, or an access log.
3. Subsequent `/api/*` calls are authorised by that cookie. A bare `GET /` with
   neither a token nor a cookie is refused — the cookie is never handed out for
   free.

Init tokens live in memory, expire after ~60s, and are single-use. Restarting
the daemon drops every pending token; just reopen the dashboard.

**Threat model.** Loopback-only, same-user trust boundary — the same as the
[approval popup](agent.md#ad-hoc-human-approval---ask-human). A same-user
process could still scrape the human browser's on-disk cookie store; that is
the genuine trust floor, far above "make one unauthenticated HTTP request," and
is blocked by the Claude Code bash sandbox anyway.

## Layout

A single-page app that polls `GET /api/snapshot` every 5 seconds and renders
nine tabs. Common affordances across the data tabs:

- **Click-to-sort** — column headers toggle ascending/descending.
- **Search box** — per-tab text filter. On Groups it also matches role,
  description, conv-id, and working directory; on Cron it matches the job
  subject and body.
- **Show offline** — the Groups tab has a toggle that hides agents whose
  tmux pane isn't alive, plus a per-group override
  (`inherit → always show → always hide`).
- **Expandable rows** — `<details>` open/closed state persists in
  `localStorage` across polls.
- All edits are **optimistic**: the UI applies the change locally, fires the
  API call, and rolls back on failure; the next 5s poll reconciles to
  canonical state.

### Groups

Every group, expandable to its members. Each member row shows the status
dot, role / description, working directory, git branch or
worktree, effective permissions, and an **owner** badge where applicable.

The **status dot** is the agent's power control: click an online (green)
dot to turn the agent off — a confirm offers **Soft exit** (inject
`/exit`) or **Force kill** (`tmux kill-session`) — and click an offline
(grey) dot to resume it. There are no separate per-row wake/shutdown
buttons; the dot does both.

The **state cell** also carries an **activity badge** — `🤖+N`, shown when
the agent has *N* sub-agents (Task-tool children) still running. It appears
even when the agent's own turn has ended (status `idle` / `main_agent_idle`),
and that is exactly the point: a sub-agent launched in the background
outlives the parent's turn, so the badge flags that an idle-looking agent
is not actually finished. Hover it for the exact count. The badge is shown
only for a live agent — an offline agent's sub-agents died with its process.

> **No background-shell count.** tclaude deliberately does *not* show an
> equivalent badge for background shell commands (`Bash` with
> `run_in_background: true`). Claude Code fires a hook when such a shell
> *launches* but none when it *exits*, so any count tclaude tried to keep
> could only ever grow — it would display long-finished "ghost" shells for
> the whole idle window, which is precisely when the badge would be read.
> Sub-agents are safe because `SubagentStart` and `SubagentStop` are both
> real hooks, so `🤖+N` decrements correctly. (A future process-tree
> liveness reconcile in `agentd` — counting an agent's live shell
> descendants instead of relying on hooks — could make a trustworthy
> background-shell badge feasible.)

The **working-directory** cell is clickable — clicking a path opens a terminal
window there (the same out-of-sandbox spawn the **term** button does, minus the
dir picker). The **branch** cell links to the branch's GitHub compare view, and
when the branch has a pull request a `#<num>` link to it is shown alongside.
Branch/PR links resolve in the background (cached, best-effort) and are simply
absent for a non-GitHub repo or when `gh` is unavailable.

Per-member actions: **focus** the session, open a **terminal** in its
working directory, **clone**, **reincarnate**, **rename**, edit
**role/descr**, toggle ownership, grant a **sudo** elevation, edit
**permissions**, schedule a **cron** job, and **remove** it from the
group. (Turning the agent on/off is the status dot's job — see above.
Permanently *deleting* an agent is offered on the virtual Ungrouped
group's rows, not on grouped rows — see below.)

Per-group actions live in the group header: **+ spawn agent**, **+ add
member** (a searchable keyboard-navigable overlay), **⏰ multicast** cron,
**✉ message** (a one-shot message to the group or a ticked subset),
**rename**, **⤓ export** (the whole group to a portable `.zip`), **🧹
cleanup** (bulk-remove confirmed-offline members — see [Cleanup](#cleanup)),
**🪟 windows…** (bulk focus/unfocus the members' terminal windows —
optionally auto-tiled into a grid, see [Config](#config)), **🟢
power on** (resume every offline member), **🛑 shutdown** (stop every
running member), and **delete group**. The
header also carries three click-to-edit chips: **📁 start-dir** (the default
working directory for agents spawned into the group), **📋 startup-context**
(shared guidance delivered to each spawned agent's inbox), and a **👥
member-cap** chip (`agent_groups.max_members` — a spawn that would exceed it
is refused; the chip turns orange when the group is full).

The tab's filter bar carries **+ new group**, **⤒ import** (recreate a
group from an exported `.zip`), **⎘ from template** (spawn a whole team from
a [template](#templates)), and a **🧹 clean up** button (the all-categories
cleanup tool — see [Cleanup](#cleanup)). Toggles surface three **virtual
groups** below the real ones: **Ungrouped** (online agents in no group),
**Retired** (agents demoted to plain conversations, each with a
**reinstate** button), and **Conversations** (recent non-agent
conversations, each with a **promote** button). Dragging a row onto or off
these virtual groups joins / leaves a group or promotes a conversation into
an agent.

Any **online** conversation is enrolled as an agent automatically — a
terminal-launched session (`tclaude conv new`) surfaces in **Ungrouped**
the moment it starts, the same way a web-UI spawn does, with no manual
promote needed. (A session tclaude did not launch — a plain reattach, or
a session predating this behaviour — is picked up by the daemon's online
sweep within a reaper interval instead of instantly.) The **promote**
button is therefore mainly for *offline* past conversations you want back
on the roster; a conversation you deliberately **retire** stays retired
even while its pane is still running.

Retired conversations are kept **forever** by default — retire is the
non-destructive half of cleanup. If you'd rather reclaim the long tail
automatically, the Config tab's **Retired-agent cleanup** toggle opts into
a periodic sweep (every 30 min, and at `agentd serve` startup) that
*permanently deletes* anything retired longer than a window you set
(default ≈ 1 year). It's off until you enable it, and deleting a
conversation never loses its recorded cost — spend totals survive.

**Drag-and-drop.** Drag a member row onto another group's header to **move**
it; hold **Ctrl** (**Cmd** on macOS) while dragging to **clone** it into the
target group instead, leaving the original in place. A hint pill follows the
cursor and the drop target's outline flips colour to show which effect is
armed.

### Templates

Reusable **group blueprints**. A template is a recipe for a whole team — a
group name, a shared default context, and an ordered list of agent specs
(name, role, description, task brief, owner flag, permission slugs). Unlike a
group [export](#groups), a template holds no conv-ids; it describes a group
that does not exist yet.

**+ new template** defines one from scratch; **⤓ from a group** snapshots an
existing group's structure into a new template. Instantiating a template
creates a fresh group and spawns its entire agent team in one action.

### Cron

The scheduled-job table — name, owner, target, interval, last run, status pill,
and body summary. Per-row buttons: enable/disable, **run now**, edit, delete.
**+ new cron job** opens a create form (also reachable pre-filled from the ⏰
buttons on the Groups tab). See
[Agent Coordination → cron](agent.md#cron) for what cron jobs do.

### Sudo

Active **time-bounded permission elevations**. Shows who holds what, the reason,
and the expiry. The human can proactively **grant** an elevation to an agent or
**revoke** one early. See
[Agent Coordination → sudo](agent.md#permissions--sudo) for the
elevation model.

### Links

**Inter-group communication links** — directed edges that let one group's
members message another group's members without co-membership. Add, edit the
mode of, and remove links here.

### Permissions

Every permission slug, expandable to the list of agents that currently hold it
(via defaults, per-conv grants, or active sudo elevations).

### Slug registry

The full registry of known permission slugs with their descriptions — the
browser equivalent of `tclaude agent permissions slugs`.

### Messages

Notifications agents have sent the human via `tclaude agent notify-human`
(see [Agent Coordination → bundled skills](agent.md#bundled-skills)). Each row
shows the sender, group, subject, and body; the nav tab carries an
unread-count badge. **✓ mark all read** clears the badge; **🧹 clear read**
deletes every already-read message. It is the human's side of the
human-notify channel — an explicit nudge surface kept separate from the busy
terminal.

### Config

A visual editor for `~/.tclaude/config.json`, covering the settings this
build of tclaude recognises. Edits are staged in the form until you press
**Save changes**, which shows a confirm diff before anything is written. Most
settings apply on next use; a few resolved at `agentd` startup (spawn
rate-limit, clone cooldown) take effect only after an agentd restart.

The **Window focus** field also holds a **set the `tclaude:<id>` window/tab
title** toggle (`focus.window_title`, on by default). tclaude normally stamps
a `tclaude:<id>` title on each agent's terminal so it can find that window
again to **raise** (focus) or **auto-tile** it. Some find the title ugly on a
plain desktop terminal — unchecking it leaves the terminal's own tab title
alone. The trade-off: focus and tiling can no longer locate the window, so
"focus" falls back to opening a *new* window instead of raising the existing
one (this affects WSL and native-Linux/X11; the explicit **open window**
action is unaffected). **Leave it on for WSL**, where window focus depends on
the title.

The **Window focus** field also holds an opt-in **auto-tile** toggle: when
on, focusing/​showing more than one agent's window (the 🪟 windows… modal or
the command palette) rearranges that set into a tidy layout — `grid`
(default), `columns`, `rows`, or `cascade` — instead of leaving each window
where the OS dropped it, with configurable inter-tile **gap** and screen-edge
**margin**. All windows are gathered onto **one monitor** — the one the first
window is on — so a multi-monitor setup isn't scattered across screens. By
default windows keep their **current size** and are only repositioned; tick
**resize windows to fill the screen** for the older screen-filling grid. It is
best-effort per platform (macOS AppleScript, Linux xdotool/kdotool, WSL
PowerShell); an unsupported desktop simply leaves the windows as-is. A single
focused window is never tiled.

## Spawning agents from the dashboard

A group header's **+ spawn agent** button opens the spawn modal: name, role,
description, an optional **initial message** (a task brief delivered to the new
agent's inbox), and a working-directory field with a git-worktree picker. The
group's start-dir default pre-fills the directory when present, and — when the
group has a shared startup context — a checkbox offers to include it in the
briefing.

The modal also takes **attachments**, added three ways: click **📎 Attach
files** to pick one or more with the native picker; **drag files from
Finder/Explorer** onto the dialog (it highlights as a drop target); or **paste**
(⌘/Ctrl-V anywhere in the dialog) — a clipboard screenshot is packaged as a PNG,
and a file copied in Finder/Explorer (⌘/Ctrl-C) is attached as-is. Each pending
attachment shows in a list with a thumbnail (for images) and a remove button.
On submit the files are uploaded to a temp dir (`POST /api/spawn-attachments`)
and their paths are folded into the new agent's startup briefing under an
"Attached files" heading, so the agent can open them with its own file tools on
its first turn. Attachments are per-spawn — they aren't stored in a spawn
profile — and the temp copies are swept after a day.

The modal has an **Auto focus** checkbox (default on): when checked, the daemon
opens a terminal window attached to the freshly-spawned session — via
`tclaude session attach`, so the reattached session keeps its status bar and
focus/notify wiring — so you can watch and talk to the new agent immediately. A
detached spawn otherwise has no window of its own.

## Cleanup

Long-running coordination sessions accumulate dead agents — exited workers,
abandoned experiments. The **🧹 cleanup** affordances bulk-prune them.

Two entry points, both on the Groups tab:

- **Per group** (group header → 🧹 cleanup) — removes confirmed-offline
  *members* from that one group. The conversations keep running and stay on
  disk; only the membership is dropped.
- **All categories** (Groups tab filter bar → 🧹 clean up) — the rich modal.
  It spans three conversation categories — active agents, retired agents, and
  plain conversations — and offers four tiers:

  | Tier        | Acts on                                                            |
  |-------------|--------------------------------------------------------------------|
  | `unjoin`    | active agents — drop their group memberships                       |
  | `retire`    | active agents — demote to a plain conversation                     |
  | `delete`    | any conversation — wipes history from disk, drops every group / owner / permission row |
  | `reinstate` | retired agents — return them to the active roster                  |

  A target whose tier doesn't apply is reported *skipped*, never *failed*, so
  a mixed-category selection degrades gracefully.

The modal lists the affected agents as an editable include/exclude checklist,
with an "inactive ≥ N h" quick-filter for picking by staleness. Nothing is
trusted blindly: **the daemon re-checks tmux liveness for every agent at
execute time**, so one that came back online between the snapshot and your
click is reported *skipped*, never touched. After running, the modal shows a
per-agent outcome log.

**Owners.** Offline group owners are excluded by default. Tick **include
offline owners** to remove them too — that also strips their owner status. A
group left with no owners is flagged with a warning.

**Worktrees.** When cleanup *deletes* an agent (and likewise the per-row
**delete** button), it offers to also remove the git worktree that agent was
working in. The worktree *directory* is removed; its **branch and commits are
kept**. Two worktrees are always spared: the repo's **main** worktree, and any
worktree another, surviving agent is still working in (a "shared" worktree).
For a single delete the checkbox is greyed out and labelled when the worktree
can't be removed; an already-deleted worktree is a silent no-op.

Cleanup is **human-only** — these endpoints live on the loopback dashboard
server behind the same cookie + Origin pinning as every other mutation; agents
on the `/v1` socket have no path to them.

## See also

- [Agent Coordination](agent.md) — the `tclaude agent` CLI, `agentd`, groups,
  permissions, and the approval popup the dashboard shares a port with.
