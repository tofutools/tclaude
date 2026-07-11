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
> Sub-agents have both `SubagentStart` and `SubagentStop` hooks, but even
> that pair is lossy — Claude Code fires no hooks at all on a user
> interrupt, for one — so `🤖+N` is not a raw event tally: tclaude keeps a
> self-healing per-`agent_id` ledger (any hook fired from inside a
> sub-agent re-adds/refreshes it, a staleness TTL ages out entries whose
> Stop was lost, and process (re)starts, interrupts, and exits reset it to
> zero). (A future process-tree liveness reconcile in `agentd` — counting
> an agent's live shell descendants instead of relying on hooks — could
> make a trustworthy background-shell badge feasible.)

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

Per-group quick actions live above the roster as icon-only buttons (hover for
their labels): **spawn agent**, **create subgroup**, **power on**, and
**shutdown**. The remaining actions live in the group header menu: **+ add
member** (a searchable keyboard-navigable overlay), **⏰ multicast** cron,
**✉ message** (a one-shot message to the group or a ticked subset),
**rename**, **⤓ export** (the whole group to a portable `.zip`), **🧹
cleanup** (bulk-remove confirmed-offline members — see [Cleanup](#cleanup)),
**🪟 windows…** (bulk focus/unfocus the members' terminal windows —
optionally auto-tiled into a grid, see [Config](#config)), and **delete group**.
The subgroup shortcut opens the standard create-group form with the current
group fixed as its parent. The
header also carries three click-to-edit chips: **📁 start-dir** (the default
working directory for agents spawned into the group), **📋 startup-context**
(shared guidance delivered to each spawned agent's inbox), and a **👥
member-cap** chip (`agent_groups.max_members` — a spawn that would exceed it
is refused; the chip turns orange when the group is full).

The tab's filter bar carries **+ new group** and a **⚙ cog** menu holding the
less-frequent group-wide actions: **⤒ import** (recreate a group from an
exported `.zip`), **🧹 clean up** (the all-categories cleanup tool — see
[Cleanup](#cleanup)), **🗑 delete retired…**, **⎘ from template** (spawn a whole
team from a [template](#templates)), **⧉ templates…** and **⧉ roles…** (the
[template](#templates) and [role-library](#roles-library) overlays), **⧉
profiles…** (spawn profiles), and **🔗 links…**. Toggles surface three
**virtual groups** below the real ones: **Ungrouped** (online agents in no
group),
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

### Spawn Profiles

Reusable launch presets for agents. A spawn profile can carry the harness,
model, effort, sandbox / permission-mode defaults, agent name, role, description,
initial message, dialog toggles, owner default, and per-slug permission
overrides. It deliberately does **not** carry a working directory or worktree:
those stay per-spawn.

Open the manager from the Groups tab cog (**⚙ → ⧉ profiles…**). The manager can
create/edit/delete profiles and now also **⇪ export** / **⤒ import** portable
profile bundles. Export opens a checklist of saved profiles so you can uncheck
anything that should not travel. Import reads a bundle, previews every profile,
lets you uncheck rows, and handles existing-name conflicts per profile by
renaming or overwriting.

### Templates

Reusable **group blueprints**. A template describes a whole team that does not
exist yet — unlike a group [export](#groups) it holds no conv-ids. Open the
templates overlay from the Groups tab's filter-bar cog (**⚙ → ⧉ templates…**).

**A minimal template is just a name, a roster, and per-agent briefs** — that
alone instantiates a working group. Everything below is an *optional advanced
layer* you add only when you want it, so don't read the list as a wall of
required concepts. A full template can carry:

- a **roster** of agent specs — name, role label, description, task brief, and
  an **owner** flag (which member leads the group);
- a **role reference** per agent (`role_ref`) into the [roles library](#roles-library),
  so the agent inherits that role's canonical brief and launch defaults beneath
  its own fields;
- **a launch profile per agent** — the agent's launch shape *and* its birth-time
  permissions are a single **pick a stored [spawn profile](#spawn-profiles)**: the
  profile's harness / model / effort / sandbox / approval and its
  grant/deny permission overrides all ride onto the spawned agent. The editor's
  launch row is a profile dropdown with **＋ new** (create one inline and use it)
  and **⧉ manage…** (open the real profiles manager) — there is no duplicated
  field set or permission-checkbox list in the template editor; a profile is the
  unit of launch config. The **owner** flag stays a separate per-agent checkbox
  because ownership is *structural* (which member leads), not launch config — at
  deploy it is **unioned** with the profile's own `is_owner` default (either one
  makes the agent an owner);
- an ordered, routed **work pattern** — briefing messages delivered, in order,
  once the whole roster has spawned (each routed to one agent or `all`);
- an advisory **process** — an ordered list of phases (the quest plan), tracked
  at runtime but never enforced (see [Steering a force](#steering-a-force));
- staged-spawn **waves** — agents tagged with a wave number spawn in ascending
  order, each wave holding until the previous one has come up and gone idle;
- **rhythms** — recurring nudges that become ordinary group cron jobs when the
  force is deployed (see [The rhythm model](#the-rhythm-model));
- an optional **per-agent worktrees** default — pre-checks the deploy dialog's
  “Give each agent its own worktree” option without locking it, so each spawn
  can still override the template preference.

> **Templates authored before the profile picker** may carry inline launch
> fields or an inline permission list on an agent. Those still apply when you
> deploy and are preserved when you re-save — nothing is silently dropped — but
> they can no longer be edited inline. The editor flags such an agent with a
> **⚠ legacy inline** notice and an **Extract to profile…** button that
> materializes the inline values into a reusable spawn profile and points the
> agent at it. (Bundled [starters](#starter-task-forces) that still list an
> inline `groups.spawn` grant on their lead deploy correctly for the same
> reason.)

Per-card actions: **🚀 deploy** (against a mission — see
[Task forces](#task-forces)), **⎘ instantiate** (create a group with no
mission), **edit**, **⇪ export** (a portable `<name>.task-force.json` file), and
**delete**. Each card also lists the **🚀 forces** already deployed from that
template. The overlay's own buttons are **+ new template** (from scratch),
**⤓ from a group** (snapshot an existing group's structure), **⤒ import** (read
an exported file back — see [Sharing task forces](#sharing-task-forces-as-a-file)),
and **⭐ starters** (see [Starter task forces](#starter-task-forces)).

> In 🧙 **wizard mode** these labels re-theme — a template is a "summoning
> circle", **🚀 deploy** reads **🧙 summon**, **⭐ starters** reads **⭐ conjure
> a preset party**, and so on. The affordances are identical; only the copy
> changes.

> **Editing circles by chat.** Everything this editor does is also a
> permission-gated CLI/daemon endpoint, so a **scribe agent** granted
> `templates.manage` can author and edit templates by conversation — no
> dashboard needed. Reads stay open, so any agent can discover and inspect
> circles. See [Agentic template editing](agent.md#templates) for the grant
> bundle and the bundled `agent-circles` skill. The **Config tab → Scribe
> defaults** selector picks which saved spawn profile a freshly summoned scribe
> launches with (harness / model / effort) — e.g. to run scribes on Codex; it
> applies to the next fresh summon and is stored as `scribe.profile` in
> config.json.

#### Sharing task forces as a file

A template can be exported to a single self-contained JSON file and imported on
another machine — the supported way to share a task force with a friend, a
coworker, or your own other computer. The file is a small versioned envelope
around the template:

```json
{
  "format": "tclaude-task-force",
  "format_version": 1,
  "exported_at": "2026-07-03T21:00:00Z",
  "template": { "name": "feature-team", "agents": [ ... ], "work_pattern": [ ... ] },
  "roles":    [ ... ],
  "profiles": [ ... ]
}
```

The `template` object is exactly the shape the editor and
`tclaude agent templates show --json` use, so every template field — agents,
launch-profile references, the work pattern — travels automatically. The
envelope also **embeds the full definition of every [role](#roles-library) and
[spawn profile](#spawn-profiles) the template references**, so a profile-driven
team reproduces its launch shape + permissions on another machine. The file
carries **no machine-local identity**: no database ids and no conversation
links, just the blueprint.

On import, the embedded roles and profiles are **materialized only if they are
missing** on the target machine — an existing role/profile of the same name is
never overwritten (your local edits are sacred; the import reports it kept the
local version). References that still can't be resolved **degrade with a
warning** rather than failing the whole import:

- a **spawn-profile reference** naming a profile that isn't defined here and
  wasn't embedded is dropped (the agent falls back to the group/harness default);
- an **unknown permission slug** on an agent's legacy inline list is dropped.

A **name collision** is refused unless you opt in: tick **Overwrite if it already
exists** (CLI `--update`) to replace the existing template in place, or set
**Import as** (CLI `--as <name>`) to store it under a different name. An export
written by a **newer tclaude** (higher `format_version`) is rejected with an
“upgrade tclaude” message.

From the CLI:

```bash
tclaude agent templates export feature-team --file feature-team.task-force.json
tclaude agent templates import --file feature-team.task-force.json          # errors on a name clash
tclaude agent templates import --file feature-team.task-force.json --as ft2  # import under a new name
tclaude agent templates import --file feature-team.task-force.json --update  # overwrite in place
```

#### Starter task forces

tclaude ships a small library of **curated, ready-to-run starters** so you can
deploy a working team without writing a template first. The templates overlay's
**⭐ starters** button opens a dialog listing them; each starter is a worked
example of the whole feature set — role references, per-agent launch tuning, a
process, staged-spawn waves, a seeded rhythm, and a routed work pattern.

Each row's **⤓ copy to my templates** button (**⭐ copy into my circles** in
wizard mode) **copies that starter into your own templates list** — it does
**not** spawn a team. Once copied it is an ordinary template you deploy or edit
from the list like any other. (This is a deliberate two-step: a starter is a
static, editable *blueprint* to adopt, not a one-click launch.)

| Starter | Team | Flow |
|---|---|---|
| `dev-squad` | lead · designer · dev · reviewer · tester | design → implement → review → test → ship (lead on `opus`, tester on `haiku`, reviewer reviews cold) |
| `research-pod` | coordinator · 3 researchers · critic | scope → research → adversarial verify → synthesize |
| `review-crew` | lead · 3 diverse-lens reviewers · synthesizer | scope → review (correctness / security / simplicity) → synthesize |

Copying a starter is **idempotent and never clobbers**: if a template of that
name already exists, the copy is skipped (your edited copy is sacred) — pass a
different name to copy in a fresh one. Starters work on a fresh empty install.

From the CLI:

```bash
tclaude agent templates starters ls                     # list the bundled starters
tclaude agent templates starters show dev-squad         # inspect one in full
tclaude agent templates starters install dev-squad      # install it as a local template
tclaude agent templates starters install dev-squad --as my-squad  # install a fresh copy
tclaude agent task-force deploy dev-squad --mission "…"  # then deploy it against a mission
```

### Roles library

Open it from the Groups tab's filter-bar cog (**⚙ → ⧉ roles…**; **⧉ classes…**
in wizard mode). A **role** is a named, reusable bundle of defaults a template
roster agent can point at: a canonical **role-brief** (folded into that agent's
startup context under a `## Role` block), a default **launch shape**
(spawn-profile reference, or inline harness / model / effort / sandbox /
approval), and a default **permission set**. A template agent references a role
by name in its `role_ref` field; the role fills whatever the agent leaves blank
and the agent's own fields always override it. This is distinct from the
freeform `role` **label** on an agent (e.g. `tech-lead`), which is just
display / routing text and carries no defaults.

Each saved role is fully **editable** — create, edit or delete any of them, over
every field above, from this dialog (**+ new role** / per-card **edit** /
**delete**). Every role picker (today: the templates editor's per-agent **Role
library** dropdown) shows an inline **inspect panel** beneath the selection —
the role's description, its launch shape (spawn-profile / harness / model /
effort / sandbox / approval), its granted permission **slugs**, and its brief
(expandable) — so picking a role is never blind. The same view is available from
the CLI with `tclaude agent roles show <name>`.

**Roles resolve at deploy time.** A template stores only a role's *name* in its
`role_ref`; the role's actual fields are read when the template is deployed. So
editing a role changes what **future** deploys inherit — already-deployed groups
are untouched (they captured the role's values at their own deploy). Because a
live reference matters, **deleting a role is refused while any template still
references it** — the dialog surfaces the referencing templates so you can edit
them to drop or repoint the reference first, then delete the role.

tclaude ships six **seed roles** — `po`, `lead`, `dev`, `designer`, `reviewer`,
and `tester` — as short, generic starting points. Their briefs are sensible
defaults, not policy, and their launch fields and permissions are deliberately
left blank (what a role launches on or is granted is your call). The seeds are
**self-healing**: they are re-checked on every daemon start, so a seed you
delete (once no template references it) reappears on the next open — but **your
edits are sacred**, never overwritten by the re-seed. Edit a seed to taste, or
add your own roles, and they stick. (The name `all` is reserved — it is the
work-pattern broadcast target — so you cannot create a role called `all`.)

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

The **Usage readout** section controls the top-bar Claude subscription bars.
By default agentd does **not** periodically call Anthropic's usage API; it uses
Claude Code's statusline callback when sessions run and otherwise shows the
last cached reading for `usage.idle_timeout` (default `72h`). Enable
`usage.poll_anthropic_api` there only if you want background API refreshes while
no statusline callback is active.

The **Default terminal** toggle (`dashboard.default_terminal`) chooses where
dashboard focus/open actions appear. Its default, `native`, opens or raises OS
terminal windows. Selecting web terminals routes per-agent focus, open-window,
open-terminal, and bulk focus from the **🪟 windows…** modal into panes in the
dashboard's **Terminals** tab. Bulk unfocus still detaches the selected terminal
clients and closes matching web panes; it never stops the agents.

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
on, focusing/​showing more than one native agent window (the 🪟 windows… modal or
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

## Task forces

A **task force** is a whole agent team deployed from a [template](#templates)
against a **mission** — a topic, problem, or epic. The journey runs in order:
pick a template → deploy it against a mission → watch and steer the live force
on its group → wind it down when the work is done. (The
[CLI](agent.md#task-forces-cli) drives the same journey headlessly.)

### Concepts: pattern, process & rhythms

Three things shape a deployed force, and they work **together**:

- the **work pattern** (*rite of command* in wizard mode) **briefs it once** —
  an ordered list of routed messages delivered a single time, after the whole
  team has spawned. It fires at deploy and does not repeat, but
  [Re-brief](#steering-a-force) re-delivers the template's *current* pattern
  to the live team on demand.
- the **process** (*quest plan*) gives it a **shared map of phases** to advance
  through. It is **advisory**: advancing records a transition and nudges the
  roles now active — nothing is blocked, no permissions change, nothing
  auto-advances.
- the **rhythms** (*drumbeats*) **keep it moving between phases** — recurring
  nudges materialized as group cron jobs at deploy (a **snapshot**; editing the
  template afterwards does not retune a force already in the field, see
  [The rhythm model](#the-rhythm-model)).

| Concept | Delivered | Repeats? | Enforced? | On stand-down |
|---|---|---|---|---|
| Work pattern | once, after the team is up | no — re-brief re-sends on demand | no — it is a briefing | already delivered (nothing to sweep) |
| Process | snapshot at deploy | advance by hand | no — advisory | phase history kept |
| Rhythms | cron jobs at deploy | yes, on a schedule | no — nudges | cron jobs deleted |

The template editor carries the same summary in a collapsible **“How deploying
works”** panel above its pattern / process / rhythms sections.

### Deploying a force

A template card's **🚀 deploy** button opens the deploy modal: pick the
template, state the **mission** (free text, or a Linear epic / issue link — it
is stored verbatim, tclaude pulls no title), and optionally set a working
directory and a **worktree** branch. The mission is folded into the new group's
shared context under a `## Mission` heading, so every spawned agent's startup
briefing carries it.

The **group name** is derived from the mission (slugged and made unique) and
pre-fills the field as you type; a bare-URL mission has no words to slug, so it
falls back to the template name. Type over the field to name the group
yourself. The group name is also the prefix for every agent — template agent
`PO` lands as `<group>-PO` (the modal previews the final names). The optional
**worktree** branch lands the whole force on its own branch in a git worktree,
which becomes the force's working directory. **Give each agent its own
worktree** instead fans that branch prefix out across the roster. A template
may pre-check this choice by default; changing it in the deploy modal affects
only that spawn and does not edit the template.

When creating a new force, the modal can also **mirror settings from an existing
group**: description and default cwd are copied into editable fields before
submit, and the startup context field shows the mirrored group's context
combined with the template's default context. Leave it top-level to create a
separate force with the same settings, or tick **Deploy as subgroup** to nest
the new force under the mirrored group. Dragging a template from the right dock
onto a group offers the same choices plus **Reinforce this group**, which keeps
the existing group and spawns the template roster directly into it.

Deploying does several things in one action:

- creates the fresh group (top-level or nested), recording the mission and the
  source template on it (this is what marks the group a *deployed force*);
- spawns **wave 0** synchronously, so the modal returns with real per-agent
  outcomes, and **defers** any higher waves to a background runner that spawns
  each as the previous wave settles (goes idle) or a max-wait backstop fires;
- **materializes the template's rhythms** as ordinary group cron jobs, armed
  the moment the team comes up (see [The rhythm model](#the-rhythm-model));
- **seeds the process** state at the first phase, if the template has one;
- delivers the template's **work pattern** — the ordered briefing messages —
  once the whole roster has spawned (immediately for a single-wave force, after
  the final wave settles for a staged one).

**⎘ instantiate** on the card is the same machinery without the mission framing
(no `## Mission`, no derived name — you name the group). Deploy is the
mission-framed twin; both spawn the whole team.

### The force block

Expand a deployed force's group on the Groups tab and its body leads with a
**force block** — a live glance at the deployment:

- the **mission** (labelled 🎯 Mission, or 🗺 Quest in wizard mode) and the
  **source template** it was deployed from. A force deployed with no mission
  reads "Deployed from template *X* — no mission recorded" instead.
- a **phase line** (◆ phase *N/M: name*) with a **history (*N*)** affordance;
  hover it for the transition log. Absent for a force with no process.
- a per-role **liveness rollup** — members grouped by role, each a pill showing
  a status glyph (● working, ○ idle, ✕ dead) and its context-window pressure
  (e.g. `62%`) when the snapshot carries it.
- a **⚠ stalling** hint next to the Roles heading. It fires **only when every
  live member is idle** — a conservative "nothing appears to be in flight"
  glance. A fully-offline force is dormant, not stalling, so the hint stays off
  when no member is live.
- a **↻ re-brief** button and a **⏻ stand down** button (see below).

The group's **summary line** also carries a **◆ phase** chip (with a **▸
advance** button when a next phase exists) and a **🌊 wave *N/M* pending** chip
while later waves are still deferred. Advancing lives on the summary chip and
retiring lives in the group's ⚙ cog, so the force block does not duplicate them.

This whole block has a CLI twin: **`tclaude agent task-force status <group>`**
prints the same mission, phase map, liveness rollup, waves and rhythms
headlessly (and **`task-force ls`** lists every deployed force) — see
[Task forces (CLI)](agent.md#task-forces-cli). The liveness classification is
shared, so the terminal and the dashboard never disagree about who is stalling.

### Steering a force

- **Advance the process.** The **▸ advance** button on the phase chip moves the
  group to the next phase, records the transition, and nudges the roles active
  in the phase it enters. The process is **advisory** — tracked and surfaced,
  never enforced. Advancing is gated server-side (the human always, group owners
  of the group, otherwise the `process.advance` slug); a non-permitted click
  just gets a 403 toast.
- **Re-brief.** **↻ re-brief** re-delivers the source template's **current**
  work pattern to the force's live members, with the group's recorded mission
  interpolated (`{{mission}}` / `{{task}}`). Reach for it when the roster has
  drifted or the original briefing has scrolled out of context. It is gated on
  the human, group owners, or the **`templates.instantiate`** slug. A force with
  no source template, a deleted template, or a template with no work pattern is
  refused cleanly (nothing is sent).
- **Stand down.** **⏻ stand down** winds the whole force down — the mirror of
  deploy (see [Winding a force down](#winding-a-force-down)). It is gated on the
  human, group owners, or the **`groups.retire`** slug.

### The rhythm model

A template's rhythms are a **deploy-time snapshot**. At deploy each rhythm is
materialized into an ordinary group cron job (named `<group>-<rhythm>`) and from
that point on the two are independent:

- **editing the template later does *not* change already-deployed jobs**, and
- **re-brief does *not* re-sync them** — re-brief only re-delivers the work
  pattern, never the rhythms.

To change a running force's cadence, edit its jobs directly in the **Cron** tab.
This is deliberate: a deployed force's live cron schedule is its own state, not a
mirror of the blueprint it came from.

### Winding a force down

Three verbs, with different blast radii:

- **Retire** (per-member status dot / the group ⚙ cog, or `groups retire`) is
  **non-destructive**: it demotes agents to plain conversations. The group and
  its history survive, and a retired conversation can be reinstated.
  - When a retire leaves the group with **no live members**, its group-target
    rhythms would otherwise keep firing every interval to nobody. So a retire
    that empties the group **auto-disables** those cron jobs (they stay visible
    and reversible in the **Cron** tab, marked *group-retired*) rather than
    leaving them running. A later **`groups resume`** on the group **re-enables
    exactly** the jobs that auto-disable turned off — never a job you paused by
    hand (once you flip a job's enabled state yourself, it stops being a
    candidate for the auto-re-enable).
- **Stand down** (the force block's **⏻ stand down** button, or
  `task-force stand-down`) is the **mirror of deploy** — the composed wind-down.
  It **retires the whole roster** *and* **sweeps** (deletes) the deploy-seeded
  runtime: the group-target **rhythm cron jobs** and any pending **wave
  choreography**. It **keeps the group row** as a dormant record — the mission,
  provenance, and process history all survive — so it is *not* a delete. Reach
  for it when a mission is done and you want the force wound down but its record
  kept. Gated on the human, group owners, or `groups.retire`.
- **Delete group** (the group ⚙ cog, or `groups rm`) is the **full sweep**: it
  removes the group and, in one transaction, its advisory **process state** and
  transition log, its staged-spawn **wave choreography**, and its group-target
  **cron jobs** (including the template-seeded rhythms). What each member *said*
  to the others is preserved as direct messages, and cron jobs that merely
  routed *through* the group (conv-targeted) still deliver and are left alone.

The difference between **stand down** and **delete group**: stand-down sweeps the
same rhythms + waves but **keeps the group** (mission and history preserved);
delete erases the group row entirely.

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
