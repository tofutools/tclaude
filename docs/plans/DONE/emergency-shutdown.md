# Emergency shutdown buttons — shipped 2026-05

The dashboard now has two emergency-shutdown controls that stop a
scope of running agents fast — and ONLY stop them. The op is
non-destructive: no conversation, enrollment, group membership or
permission row is touched, so every shut-down agent is reinstatable by
simply resuming its session.

- **Group level** — a `🛑 emergency shutdown` button in each group's
  `.group-actions` cluster: stops every alive member of that group.
- **Whole-dashboard level** — a `⏻ shut down all` button top-right in
  the header, to the right of the live indicator dot (`#status`):
  stops every alive agent on the dashboard's active roster (grouped
  and ungrouped alike). The request comes from the human's browser, so
  there is no "self" to exclude.

## Endpoint

`POST /api/emergency-shutdown` on the loopback dashboard mux —
cookie + Origin pinned, human-only, like every other `/api` mutation.
Registered in `registerDashboardEditRoutes` (`dashboard_edit.go`).

Body:

```
{"scope":"group","group":"<name>"}   — one group's alive members
{"scope":"all"}                      — every alive active agent
optional "grace_ms": <int>           — override the escalation window
```

Scope collection (`handleEmergencyShutdown`):
- `group` → `db.ListAgentGroupMembers` (owner-only rows are not
  members and are left alone — matches `tclaude agent groups stop`).
- `all` → `db.ListActiveAgents` (retired / superseded convs excluded
  by that query).
Both filtered through `aliveConvIDs` — only sessions with a live tmux
pane right now are collected and de-duplicated.

Response (`emergencyShutdownResp`): a per-agent outcome list plus
summary counts — `exited_gracefully`, `force_killed`,
`already_offline`, `failed`, and `targeted`.

## Escalation

`runEmergencyShutdown` fans every target out into its own goroutine
(parallel — one hung agent can't delay the rest) and the HTTP handler
WaitGroup-joins them so the response carries the full summary.

Per agent, `escalateShutdown`:
1. `stopOneConv(force=false)` — inject `/exit` (the existing soft stop).
2. `waitForConvOffline` polls tmux liveness for up to the grace
   window. An agent that honours `/exit` exits gracefully and is
   **never** force-killed.
3. Still alive when the window closes (or the `/exit` injection itself
   failed) → `stopOneConv(force=true)` — `tmux kill-session`.

`stopOneConv` is reused unchanged, so the op touches nothing but the
tmux session — no DB row, no `.jsonl`.

## Injectable grace

`emergencyShutdownGrace` (package var, default 10s) is the
soft→hard window. The per-request `grace_ms` body field overrides it,
clamped to `[0, emergencyShutdownGraceCap]` (2 min). The flow test
passes a few ms so it never sleeps for real seconds; the dashboard
omits the field and gets the 10s default.

## Dashboard

`dashboard.html`: the two buttons (both `data-act`, dispatched through
the existing `bindRowActions` delegate), the `emergencyShutdown(scope,
groupName)` JS helper, and styling. The helper counts the running
agents in scope from the last snapshot, pops the shared `confirmModal`
stating the count and that this is stop-only (no data deleted), POSTs,
then toasts the outcome summary (`N exited gracefully, M force-killed`).

## CLI parity — finding

No `tclaude agent` verb was added. The endpoint is dashboard-cookie-
auth on the loopback mux; the CLI talks to the `/v1` Unix socket.
`tclaude agent groups stop <group>` already gives the CLI a group
bulk-stop (soft only). A `/v1` emergency-shutdown surface — especially
an "all agents" scope — needs a permission-design decision (new slug?
human-only?), which is a scope question for the PO rather than
something to bolt on unilaterally. Left as a possible follow-up.

## Files

- `pkg/claude/agentd/emergency_shutdown.go` — endpoint, collection,
  parallel escalation, grace.
- `pkg/claude/agentd/dashboard_edit.go` — route registration.
- `pkg/claude/agentd/dashboard.html` — buttons, modal wiring, JS, CSS.
- `pkg/claude/agentd/emergency_shutdown_flow_test.go` — flow tests.

## Tests

`emergency_shutdown_flow_test.go` drives `POST /api/emergency-shutdown`
on the dashboard mux. `hangOnExit` registers a CCSim `/exit` handler
that writes a turn but never flips alive — the hung-agent quirk
encoded in the simulator. Scenarios:

- group scope force-kills ONLY the hung agent; the agent that honours
  `/exit` exits gracefully and is never killed; group membership +
  roster survive (stop-only).
- all scope reaches grouped + ungrouped agents.
- offline agents are never collected / targeted.
- a malformed scope (and `scope=group` with no group) is rejected 400.
