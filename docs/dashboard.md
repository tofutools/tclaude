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
- **On startup** — `tclaude agentd serve --auto-launch-dashboard` (or
  `agent.auto_launch_dashboard: true` in `~/.tclaude/config.json`) pops the
  dashboard automatically when the daemon comes up. Off by default — a fresh
  daemon doesn't open a browser tab uninvited.

The `--print` URL carries a single-use token that expires in ~60 seconds, so
use it immediately.

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
seven tabs. Common affordances across the data tabs:

- **Click-to-sort** — column headers toggle ascending/descending.
- **Search box** — per-tab text filter. On Groups and Agents it also matches
  role, description, conv-id, and working directory; on Cron it matches the
  job subject and body.
- **Show offline** — Groups and Agents have a toggle that hides agents whose
  tmux pane isn't alive. Groups additionally has a per-group override
  (`inherit → always show → always hide`).
- **Expandable rows** — `<details>` open/closed state persists in
  `localStorage` across polls.
- All edits are **optimistic**: the UI applies the change locally, fires the
  API call, and rolls back on failure; the next 5s poll reconciles to
  canonical state.

### Groups

Every group, expandable to its members. Each member row shows the online
indicator, alias / role / description, working directory, git branch or
worktree, effective permissions, and an **owner** badge where applicable.

The **working-directory** cell is clickable — clicking a path opens a terminal
window there (the same out-of-sandbox spawn the **term** button does, minus the
dir picker). The **branch** cell links to the branch's GitHub compare view, and
when the branch has a pull request a `#<num>` link to it is shown alongside.
Branch/PR links resolve in the background (cached, best-effort) and are simply
absent for a non-GitHub repo or when `gh` is unavailable.

Per-member actions: edit alias/role/descr, **wake** / **shut down** / **focus**
the session, schedule a **cron** job, toggle ownership, and remove from the
group. Per-group actions live in the group header: rename, **+ add member**
(a searchable keyboard-navigable overlay), **+ spawn agent**, **🧹 cleanup**
(bulk-remove confirmed-offline members — see [Cleanup](#cleanup)), delete the
group, **⏰ multicast** cron, a click-to-edit **start-dir** chip (the
default working directory for agents spawned into that group), and a
click-to-edit **👥 member-cap** chip (`agent_groups.max_members` — a spawn
that would exceed it is refused; the chip turns orange when the group is
full). The tab's filter bar also carries a **🧹 clean up** button that
sweeps every group at once.

**Drag-and-drop.** Drag a member row onto another group's header to **move**
it; hold **Ctrl** (**Cmd** on macOS) while dragging to **clone** it into the
target group instead, leaving the original in place. A hint pill follows the
cursor and the drop target's outline flips colour to show which effect is
armed.

### Agents

Every conversation the daemon knows about — group members, holders of explicit
permission grants, and online ungrouped sessions. Columns include the title,
conv-id, online state, working directory, and git branch; expand a row for its
group memberships and effective permissions.

Per-row actions: wake / shut down / focus, open a terminal in the agent's
working directory, **clone**, **reincarnate**, rename, schedule a cron job, and
delete. The delete confirm also offers to remove the agent's git worktree
(see [Cleanup](#cleanup)). **+ new agent** spawns a fresh standalone session;
the filter bar's **🧹 cleanup** button bulk-deletes confirmed-offline agents.

### Cron

The scheduled-job table — name, owner, target, interval, last run, status pill,
and body summary. Per-row buttons: enable/disable, **run now**, edit, delete.
**+ new cron job** opens a create form (also reachable pre-filled from the ⏰
buttons on the Groups and Agents tabs). See
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

## Spawning agents from the dashboard

The **+ spawn agent** (into a group) and **+ new agent** (standalone) buttons
open a modal: alias, role, description, and a working-directory field with a
git-worktree picker. The group's start-dir default pre-fills the directory when
present.

The modal has an **Auto focus** checkbox (default on): when checked, the daemon
opens a terminal window attached to the freshly-spawned session — via
`tclaude session attach`, so the reattached session keeps its status bar and
focus/notify wiring — so you can watch and talk to the new agent immediately. A
detached spawn otherwise has no window of its own.

## Cleanup

Long-running coordination sessions accumulate dead agents — exited workers,
abandoned experiments. The **🧹 cleanup** affordances bulk-prune them.

Three entry points, all opening the same editable modal:

- **Per group** (group header → 🧹 cleanup) — removes confirmed-offline
  *members* from that one group. The conversations keep running and stay on
  disk; only the membership is dropped.
- **All groups** (Groups tab → 🧹 clean up) — removes confirmed-offline agents
  from *every* group they belong to. An optional **permanently delete**
  checkbox escalates to a full purge.
- **Agents** (Agents tab → 🧹 cleanup) — **permanently deletes**
  confirmed-offline agents: wipes the conversation history from disk and drops
  every group / owner / permission row.

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
- `docs/plans/agentd.md` — daemon design (peer-cred identity, socket layout).
