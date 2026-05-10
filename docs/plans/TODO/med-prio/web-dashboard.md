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
  members with online indicator, alias/role/descr, and the group-
  scoped permissions each holds. Search at the top filters by group
  name / member alias / permission slug. Owner badges shipped.
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

- **Drag-and-drop members between groups.** Modifier-key matrix:
  - **No modifier (move)**: drag a member row from group A onto
    group B's header → POST add to B + DELETE from A, in that order
    so the conv is never groupless mid-drag.
  - **Ctrl+drag (clone)**: drops a `agent clone` of the source row
    into B, leaving the original in A untouched. Uses the existing
    `clone` daemon endpoint with target group set to the drop
    target.
  - **Shift+drag (multi-membership)**: adds the conv to B without
    removing it from A.
  Drop targets pulse on hover; modifier hint pill ("→ move", "→
  clone", "→ multi") appears near the cursor.

- **"Ungrouped" virtual group.** Pinned row at the bottom (or top)
  of the Groups tab that surfaces every conv-id that's not currently
  a member of any group but is online / has a recent session. Acts
  as a drag SOURCE: drag an ungrouped agent into a real group to
  add it. Drop ON the ungrouped row removes the conv from all
  groups (= "kick from every group I'm in"). Empty when every known
  agent already has at least one group membership.

- **Per-member action buttons.** Far-right cell on each member row
  gets icon buttons:
  - **focus** (shipped): jump to the agent's tmux pane.
  - **clone**: one-click `agent clone` of this conv into the same
    group; uses the existing daemon orchestration. Same button on
    the Agents tab too.
  - **wake up**: only shown when the agent is OFFLINE. Spawns a
    fresh tmux session resumed onto this conv. Daemon endpoint
    `POST /api/agents/{conv}/wake` (or hook the existing
    `groups.resume` slug into a single-conv path).
  - **shut down**: only shown when the agent is ONLINE. Soft
    `/exit` injection (or `--force` kill-session). Confirmation
    modal.
  - **make/revoke owner** (shipped).
  - **remove** (shipped).
  - Possible later: reincarnate / compact (manager-pattern verbs).

- **Add-member button** in each group's header. Opens a search
  overlay listing candidate convs. Selecting one calls `groups
  add` against the current group.

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

- **Cron jobs.** "+ new cron job" button on the Cron tab opens a
  form modal: name, owner (default = "<dashboard-human>"), target
  (group:<name> or solo conv via search), interval (preset chips:
  1m / 5m / 15m / 1h / custom duration string), subject, body.
  POST `/api/cron`. On row hover: an "edit" icon opens the same
  form pre-filled; PATCH `/api/cron/{id}` (new endpoint to add).
  Editing the interval should NOT bump `last_run_at`.
- **Agents.** "+ new agent" button at the top of the Agents tab
  (and inside each group header — pre-fills the group). Form:
  alias, role, descr, group(s) to join (multi-select), cwd
  (defaults to daemon's cwd; picker with file-system browse would
  be nice but optional v1). POST → existing `groups.spawn`
  endpoint per group. For "edit": surfaces the existing
  `groups update-member` verb as inline edits on the row.

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

**Status (2026-05):** Cron tab landed pushing the script section to
~707 lines — the trigger has now fired. Next dashboard feature
(drag/drop, add-member overlay, rename inline edits) should do the
framework migration first.

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
