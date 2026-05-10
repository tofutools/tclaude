# Web dashboard v1 + Cron tab (2026-05)

Read-only single-page dashboard served on the same loopback port
the approval popup uses.

## Tabs (v1)

- **Groups** — list of groups, expandable to show members with
  online indicator, alias / role / descr, permissions.
- **Agents** — list of conversations (members of any group +
  online); expanding shows groups, global perms, per-group
  overrides.
- **Permissions** — list of permission slugs; expanding shows
  every agent that holds it.
- **Slug registry** — list of all known permission slugs with
  documentation.
- **Cron** — table of `agent_cron_jobs`: name / owner / target /
  interval / last-run / status pill / body summary. Per-row
  buttons: enable/disable toggle, run-now (with confirm),
  delete. Filter bar like Groups/Agents. Snapshot extended with
  a `cron[]` field including resolved owner/target labels and
  computed next-due timestamp. Mutations gated by the dashboard
  cookie; no permission slug since the dashboard is human-only.

Polls `/api/snapshot` every 5s.

## Auth

Per-process HttpOnly + SameSite=Strict cookie + Origin/Referer
pinned to the popup base URL (same threat model as the popup;
same-user /proc-leak limitation documented in
`future/popup-transport-hardening.md`).

## Implementation

- Static HTML + vanilla JS embedded via `//go:embed`.
- One HTML file (`dashboard.html`), polls `/api/snapshot` every
  5s, ~290 lines at v1 launch (~707 lines after Cron tab — the
  framework migration trigger has now fired).
- Reuses the loopback port already bound for the approval popup.
- Pages fetch from `/v1/...` on the same origin.
- Origin guard: only same-host. Ephemeral session cookie tied to
  the daemon's startup PID makes "another tab on the machine"
  attacks harder.

## Open with

`tclaude agent dashboard` (or `dashboard --print` to just emit
the URL). Daemon discovers the URL via `/v1/info`.

## Polish on top of v1

- **Owner badges** in the Groups view — members who are also
  owners get an "owner" badge in the role column; pure-owners
  surface as their own rows.
- **Agent state colours** (idle / working / awaiting / exited)
  mirroring `session/list.go`.
- **`<details>` open state persisted** in localStorage across
  polls.
- **(unknown) fix for fresh-spawned convs**:
  `agent.FreshConvRowResolved(convID)` falls back to a session-
  row cwd lookup when the conv has never been indexed, then runs
  the same .jsonl scan as `FreshConvRowAt`. All three dashboard
  `FreshConvRow` call sites switched to the resolver.
- **Per-group delete button** — header button (hover-reveal,
  full opacity when expanded) → confirm modal → `DELETE
  /api/groups/{name}` → `db.DeleteAgentGroup`. The DB helper has
  `ON DELETE RESTRICT` on `agent_messages`, so the modal warns
  the user to clear the inbox first; backend returns 409 on
  constraint failure and the toast surfaces the error.
- **Jump-to-terminal button** — `POST /api/jump/{conv}` resolves
  the conv to its alive tmux session row daemon-side and calls
  `session.TryFocusAttachedSession`. UI shows a "focus" button
  per row (Agents tab + Groups members), only when the agent is
  online. Non-destructive, no confirm modal — fire + toast.

## Files

- `pkg/claude/agentd/dashboard.go` — handlers
- `pkg/claude/agentd/dashboard.html` — UI
- `pkg/claude/agentd/dashboard_edit.go` — mutations

## Open follow-ups

See `med-prio/web-dashboard.md` for v2+ wishlist (drag/drop
membership UX is carved out to
`high-prio/dashboard-group-membership-ux.md`).
