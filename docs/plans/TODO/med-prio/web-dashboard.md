# Web dashboard (browser UI) — v2 follow-ups

**v1 shipped** — read-only single-page dashboard served on the same
loopback port the approval popup uses. Tabs: Groups, Agents,
Permissions, Slug registry, Cron. Polls `/api/snapshot` every 5s.
Auth via per-process HttpOnly + SameSite=Strict cookie + Origin/
Referer pinned to the popup base URL.

Open with `tclaude agent dashboard` (or `dashboard --print` to just
emit the URL). Daemon discovers the URL via `/v1/info`.

This file tracks v2+ (the GCP-IAM-style edits view + direct
manipulation).

## Multiple perspectives

Switchable from the top nav.

- **Groups view** — root list of groups; expanding a group shows its
  members with online indicator, role/descr, and the group-
  scoped permissions each holds. Search at the top filters by group
  name / member name / permission slug. Owner badges shipped.
- **Agents view** — root list of conversations (members of any
  group + currently-online ones). Expanding an agent shows the
  groups it's in, its global permissions, and its per-group
  permission overrides. Same search box, scoped to the visible
  tree.
- **Permissions view** — invert the previous two: list of permission
  slugs, expanding shows every agent that holds it (globally or
  per-group). Useful for "who can spawn agents right now?".
- **Activity / inbox** — live list of agents (online/offline,
  current group, last activity, unread inbox count). Pending
  human-approval requests appear here with ack/approve/deny buttons
  (same UI as the standalone popup, just inline).

**Tree-style expand/collapse** for the first three views. Hover/
click on a permission slug surfaces a tooltip/sidebar explaining
what the slug authorises.

**Indicators alongside each row**:
- ● online / ○ offline
- ⚡ attached / ▷ active session in tmux
- inbox unread count
- count of granted permissions

## Edits

The dashboard should be the easiest place for the human to grant/
revoke permissions and group memberships. Buttons should call the
same daemon endpoints the CLI uses.

### Direct-manipulation interactions in the Groups view

- **Drag-and-drop members between groups + per-group `+ add member`
  button.** → **shipped.** See
  [`DONE/dashboard-dnd-move.md`](../../DONE/dashboard-dnd-move.md),
  [`DONE/dashboard-dnd-clone.md`](../../DONE/dashboard-dnd-clone.md),
  [`DONE/dashboard-add-member-overlay.md`](../../DONE/dashboard-add-member-overlay.md).

- **Per-member action buttons.** Far-right cell on each member row
  gets icon buttons:
  - **focus** (shipped): jump to the agent's tmux pane.
  - **clone**: one-click `agent clone` of this conv into the same
    group; uses the existing daemon orchestration. Same button on
    the Agents tab too.
  - **wake up / shut down** → **shipped.** See
    [`DONE/dashboard-agent-wake-shutdown.md`](../../DONE/dashboard-agent-wake-shutdown.md).
  - **make/revoke owner** (shipped).
  - **remove** (shipped).
  - Possible later: reincarnate / compact (manager-pattern verbs).

- **Add-member button** in each group's header → **shipped.** See
  [`DONE/dashboard-add-member-overlay.md`](../../DONE/dashboard-add-member-overlay.md).

- **Rename buttons (agents + groups).** Inline edit pattern: small
  input replaces the label cell on click, Enter saves / Esc
  cancels. Backed by `tclaude agent rename` (existing) and a
  `groups rename` we'd need to add.

- **Deprecation labels / soft-hide for groups + agents.** A generic
  "label" field per group / per (group, member), with `deprecated`
  (or `obsolete` / `archived` — bikeshed pending) as a well-known
  value. Default view filters out deprecated rows; toggle reveals
  them. Open: per-group only or per-agent too? One label or N?
  Stored where (new column or a tags table)?

## CRUD forms

The dashboard currently surfaces existing data + a few destructive
verbs; creation + edit are still CLI-only. Two equivalents to add:

- **Cron jobs.** "+ new cron job" + edit form → **shipped.** See
  [`DONE/dashboard-cron-create-form.md`](../../DONE/dashboard-cron-create-form.md).
- **Agents.** "+ new agent" button at the top of the Agents tab
  (and inside each group header — pre-fills the group). Form:
  name, role, descr, group(s) to join (multi-select), cwd
  (defaults to daemon's cwd; picker with file-system browse would
  be nice but optional v1). POST → existing `groups.spawn`
  endpoint per group.
- **Inline edit role / descr per member** → **shipped.** See
  [`DONE/dashboard-member-metadata-editing.md`](../../DONE/dashboard-member-metadata-editing.md).

## Framework migration trigger

Vanilla JS worked for v1 (read-only, ~300 lines). Re-evaluate
**before** adding any of:

- Drag/drop members between groups
- The `+ add member` search overlay
- Search/filter in the Groups view across many groups
- Activity / inbox tab (live message stream)

Candidates: React, Preact, Svelte, Solid. Build chain: vite +
esbuild keeps the embedded asset small. Trade-offs: a build step in
CI, bigger `//go:embed` blob, more JS to audit.

**Status (2026-05):** the "trigger" features all shipped anyway —
drag/drop, the add-member overlay, inline rename edits, the cron
create/edit form — and `dashboard.html` is now ~6.6k lines, still
vanilla JS with zero framework imports. Vanilla held up better than
this note expected. The migration is no longer a precondition for
new features; treat it as an optional refactor, weighed against the
CI build-step / `//go:embed` size / audit-surface costs above.

## Open questions

- Should the dashboard run only on demand or always when the daemon
  is up? Probably always-on, since the approval popup is also
  served there and we already pay the bind cost.
- How much richness does the tree need? Start with collapse/expand,
  add filtering and column sorts only if it gets heavy.

## Files
- `pkg/claude/agentd/dashboard.go` — handlers
- `pkg/claude/agentd/dashboard.html` — UI (vanilla JS today)
- `pkg/claude/agentd/dashboard_edit.go` — mutations
