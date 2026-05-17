# Dashboard power buttons — shipped 2026-05

The dashboard has a matched pair of bulk power controls — **Shutdown**
and **Power On** — at two scopes each (one group, the whole
dashboard). Plus a cleanup: the redundant per-agent wake/shutdown row
buttons were removed in favour of the row's status dot.

This shipped in three coherent parts:

1. **Rename** — the old "emergency shutdown" buttons/endpoint/code
   dropped the "emergency" prefix; it is just **Shutdown** now.
2. **Power On** — the inverse of Shutdown was added: resume every
   OFFLINE agent in scope.
3. **Per-agent declutter** — the dedicated per-row `wake` / `shut down`
   buttons were removed; the row's status dot is now the sole
   per-agent power control.

## Shutdown

Two `🛑 shutdown` controls that stop a scope of running agents fast —
and ONLY stop them. Non-destructive: no conversation, enrollment,
group membership or permission row is touched, so every shut-down
agent is reinstatable by simply resuming its session.

- **Group level** — a `🛑 shutdown` button in each group's
  `.group-actions` cluster: stops every alive member of that group.
- **Whole-dashboard level** — a `🛑 shutdown all` button top-right in
  the header: stops every alive agent on the dashboard's active roster
  (grouped and ungrouped alike). The request comes from the human's
  browser, so there is no "self" to exclude.

`POST /api/shutdown` on the loopback dashboard mux — cookie + Origin
pinned, human-only, like every other `/api` mutation. Body:

```
{"scope":"group","group":"<name>"}   — one group's alive members
{"scope":"all"}                      — every alive active agent
optional "grace_ms": <int>           — override the escalation window
```

Scope collection (`handleShutdown`) resolves through the shared
`resolvePowerScope`:
- `group` → `db.ListAgentGroupMembers` (owner-only rows are not
  members and are left alone — matches `tclaude agent groups stop`).
- `all` → `db.ListActiveAgents` (retired / superseded convs excluded
  by that query).
Then filtered through `aliveConvIDs` — only sessions with a live tmux
pane right now are collected and de-duplicated.

Response (`shutdownResp`): a per-agent outcome list (`powerAgentOutcome`)
plus summary counts — `exited_gracefully`, `force_killed`,
`already_offline`, `failed`, and `targeted`.

### Escalation

`runShutdown` fans every target out into its own goroutine (parallel —
one hung agent can't delay the rest) and the HTTP handler WaitGroup-
joins them so the response carries the full summary.

Per agent, `escalateShutdown`:
1. `stopOneConv(force=false)` — inject `/exit` (the existing soft stop).
2. `waitForConvOffline` polls tmux liveness for up to the grace
   window. An agent that honours `/exit` exits gracefully and is
   **never** force-killed.
3. Still alive when the window closes (or the `/exit` injection itself
   failed) → `stopOneConv(force=true)` — `tmux kill-session`.

`stopOneConv` is reused unchanged, so the op touches nothing but the
tmux session — no DB row, no `.jsonl`.

`shutdownGrace` (package var, default 10s) is the soft→hard window.
The per-request `grace_ms` body field overrides it, clamped to
`[0, shutdownGraceCap]` (2 min). The flow test passes a few ms so it
never sleeps for real seconds; the dashboard omits the field and gets
the 10s default.

## Power On

Two `🟢 power on` controls — the inverse of Shutdown. For each OFFLINE
agent in scope they resume its session into a fresh detached tmux
pane. Resume-only: nothing is created that wasn't already a recorded
conversation.

- **Group level** — a `🟢 power on` button in the group's
  `.group-actions` cluster: resumes every offline member.
- **Whole-dashboard level** — a `🟢 power on all` button in the header.

`POST /api/power-on` — same cookie + Origin gate, same two scopes (no
`grace_ms` — there is no escalation). Body:

```
{"scope":"group","group":"<name>"}   — one group's offline members
{"scope":"all"}                      — every offline active agent
```

`handlePowerOn` resolves scope through the same `resolvePowerScope`,
then filters through `offlineConvIDs` — the mirror of `aliveConvIDs`:
only convs with NO live tmux pane are collected (already-online agents
are skipped at collection, exactly as shutdown skips already-offline
ones; empty placeholder conv-ids are dropped — nothing to resume).

`runPowerOn` resumes each target via `resumeOneConv` — the existing
per-agent resume primitive shared with `tclaude agent groups resume`
and the status-dot wake. It is a sequential loop (resume only spawns a
detached subprocess; there is no grace window to parallelise around).

Response (`powerOnResp`): a per-agent outcome list (the shared
`powerAgentOutcome`) plus summary counts — `resumed`,
`already_online`, `failed`, and `targeted`.

## Per-agent declutter — status dot is the sole power control

The per-row status dot (`agentStatusDot`, `data-act="dot-toggle"`) was
already a context-aware on/off toggle. The dedicated per-row `wake` and
`shut down` buttons duplicated it, so they were removed
(`lifecycleAndFocusButtons` now renders only `focus` + `term`).

To avoid losing functionality, the dot's online-click confirm was
upgraded from a plain soft-only confirm to the existing 3-way
`shutdownConfirm` modal (**Cancel / Soft exit / Force kill**) — so the
dot now reaches both the soft `/exit` and the `tmux kill-session`
force path the removed `shut down` button used to own. The offline-dot
click still resumes with no confirm.

## Dashboard

`dashboard.html` — the two header buttons (`🟢 power on all`,
`🛑 shutdown all`). `dashboard/js/render.js` — the two per-group
buttons. `dashboard/js/row-actions.js` — the `shutdown-{group,all}`
and `power-on-{group,all}` `data-act` cases, dispatched through the
existing `bindRowActions` delegate. `dashboard/js/refresh.js` — the
`shutdownScope` / `powerOnScope` helpers: each counts the agents in
scope from the last snapshot, pops the shared `confirmModal` stating
the count, POSTs, then toasts the outcome summary. `dashboard.css` —
`#shutdown-all-btn` (danger red) and `#power-on-all-btn` (calm green).

## CLI parity — finding

No `tclaude agent` verb was added. The endpoints are dashboard-cookie-
auth on the loopback mux; the CLI talks to the `/v1` Unix socket.
`tclaude agent groups stop`/`resume <group>` already give the CLI a
group bulk-stop/resume. A `/v1` whole-dashboard "all agents" scope
would need a permission-design decision (new slug? human-only?), left
as a possible follow-up.

## Files

- `pkg/claude/agentd/power.go` — both endpoints, shared scope
  resolution, the alive/offline collectors, parallel shutdown
  escalation, sequential power-on, grace.
  (Renamed from `emergency_shutdown.go`.)
- `pkg/claude/agentd/dashboard_edit.go` — route registration
  (`/api/shutdown`, `/api/power-on`).
- `pkg/claude/agentd/dashboard/dashboard.html` + `dashboard.css` —
  header buttons + styling.
- `pkg/claude/agentd/dashboard/js/{render,row-actions,refresh,helpers}.js`
  — per-group buttons, action cases, the scope helpers, and the
  per-agent button removal + dot-toggle force-kill wiring.
- `pkg/claude/agentd/power_flow_test.go` — flow tests.
  (Renamed from `emergency_shutdown_flow_test.go`.)
- `pkg/claude/agentd/dashboard_dot_toggle_flow_test.go` — extended
  with the dot's force-kill scenario.

## Tests

`power_flow_test.go` drives `POST /api/shutdown` and
`POST /api/power-on` on the dashboard mux. `hangOnExit` registers a
CCSim `/exit` handler that writes a turn but never flips alive — the
hung-agent quirk encoded in the simulator. Scenarios:

- **Shutdown** — group scope force-kills ONLY the hung agent (the
  agent that honours `/exit` exits gracefully and is never killed;
  group membership + roster survive); all scope reaches grouped +
  ungrouped agents; offline agents are never collected; a malformed
  scope is rejected 400.
- **Power On** — group scope resumes the offline member and skips the
  already-online one (no outcome row); all scope resumes grouped +
  ungrouped offline agents and skips an online one; a malformed scope
  is rejected 400. Offline state is set up with `Flow.MarkOffline`;
  online-after-resume is asserted at the dashboard snapshot surface.
- **Status dot** (`dashboard_dot_toggle_flow_test.go`) — the online
  dot's soft path (`{"force":false}` → soft `/exit`) and force path
  (`{"force":true}` → `tmux kill-session`), plus the offline dot's
  resume.
