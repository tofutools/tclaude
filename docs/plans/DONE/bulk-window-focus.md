# Bulk window focus / unfocus — shipped 2026-05

The dashboard can now bulk-control agent terminal WINDOWS — focus
(open / raise) or unfocus (detach) the windows of many agents at once.
It is a pure desktop-window concern: the agents themselves keep
running untouched. unfocus-all is the declutter button — after it the
desktop has fewer agent windows, but every agent is still alive.

- **focus** — raises the OS terminal window attached to each agent's
  tmux session, opening a fresh window when none is open. The bulk
  form of the per-agent `focus` button (`POST /api/jump/{conv}`).
- **unfocus** — detaches every tmux client from each agent's session,
  so the windows go away. The agent process is NEVER stopped, killed
  or signalled — only the windows are dismissed.

## UI — one trigger per scope, one selection modal

To keep the toolbars uncluttered there is exactly ONE trigger button
per scope; both open the same selection modal where the human picks
the direction AND the agent set.

- **Group level** — a `🪟 windows…` button in each group's
  `.group-actions` cluster (`data-act="window-modal-group"`).
- **Whole-dashboard level** — a `🪟 windows…` button in the header
  (`#window-all-btn`, `data-act="window-modal-all"`), left of the
  `⏻ shut down all` button.

No existing button was restyled or moved.

### Selection modal (`openWindowModal` in `dashboard.html`)

`#window-modal` reuses the `cleanup-modal` shell. It is built from the
last snapshot and offers:

- **Direction** — a `focus` / `unfocus` radio pair (defaults to
  `focus`). The hint line and the submit-button verb track it.
- **Selection, default-all** — every RUNNING agent in scope is listed
  and ticked by default, so the common "just do all of them" case is
  one click. Narrow by: per-agent checkbox, by-role chips
  (`.window-role-chip` — click toggles every agent with that role; an
  agent with no role lands in a `(no role)` chip), or the text filter.
- The submit button states the live count: `Focus N agents` /
  `Unfocus N agents`. Offline agents are not listed — they have no
  window to focus or detach.

The modal POSTs the explicit ticked conv-id list; the group modal is
always scoped to that group's agents, the dashboard modal to all.

## Endpoint

`POST /api/agent-windows` on the loopback dashboard mux — cookie +
Origin pinned, human-only, the same gate as the per-agent focus
endpoint (`/api/jump`) and `/api/emergency-shutdown`. Window focus has
no `/v1` twin and no permission slug (an agent never focuses another
agent's desktop window), so there is no shared permission-checked
handler to funnel through — `checkDashboardAuth` IS the gate, the
consistent pattern, not a divergent one. Registered in
`registerDashboardEditRoutes` (`dashboard_edit.go`).

Body:

```
{"direction":"focus"|"unfocus","scope":"group","group":"<name>"}
{"direction":"focus"|"unfocus","scope":"all"}
optional "convs": ["<conv-id>", …]   — the modal's explicit selection;
                                       absent → every agent in scope
```

Resolution (`handleAgentWindows`, `window_focus.go`):
- `scope` resolves the candidate universe — `group` →
  `db.ListAgentGroupMembers`, `all` → `db.ListActiveAgents`.
- `selectWindowTargets` narrows the universe to `convs` when present
  (intersection — a `convs` entry outside the scope is dropped, so the
  group modal can never reach an agent in another group). Empty
  `convs` → the whole scope (select-all default, and the pure
  scope-resolution path the flow test drives).
- Each target is resolved to a live tmux session via
  `pickAliveSession`; offline agents drop out with no outcome row.
- Every target is dispatched in PARALLEL (one slow tmux call can't
  delay the rest), WaitGroup-joined for the summary.

Response (`agentWindowsResp`): per-agent outcome list + summary counts
`focused` / `detached` / `no_window` / `failed` / `targeted`.

## Mechanics

- `focusAgentWindow` → `session.TryFocusAttachedSessionWithID` (the
  same per-platform AppleScript / wmctrl / PowerShell call the
  per-agent jump endpoint makes). Best-effort: it logs but never
  errors, so every focus outcome is `focused`.
- `detachAgentWindows` → `session.DetachSessionClients`, which now
  returns `(int, error)` — the count of tmux clients detached. `0`
  windows → outcome `no_window` (a clean no-op for an agent that had
  no window open); `>=1` → `detached`; a tmux error → `failed`.

Both are package-var seams (mirroring the `openTerminal` seam in
`dir.go`) so flow tests can record the dispatch set without popping
real OS windows.

## Files

- `pkg/claude/agentd/window_focus.go` — endpoint, scope resolution,
  parallel dispatch, the two seams.
- `pkg/claude/agentd/dashboard_edit.go` — route registration.
- `pkg/claude/session/session.go` — `DetachSessionClients` now returns
  `(int, error)`; `pkg/claude/session/watch.go` caller updated.
- `pkg/claude/agentd/dashboard.html` — triggers, modal, CSS, JS.
- `pkg/claude/agentd/testhooks_test.go` — `SetFocusAgentWindowForTest`
  / `SetDetachAgentWindowsForTest`.
- `pkg/claude/agentd/window_focus_flow_test.go` — flow coverage.

## Flow tests (`window_focus_flow_test.go`)

- `GroupScope_FocusesEveryAliveMember` — scope resolution: group →
  focus dispatched for every alive member.
- `AllScope_UnfocusHitsGroupedAndUngrouped` — all-scope covers
  grouped + ungrouped; the tmux sessions stay alive (window-only).
- `NarrowedSelectionIntersectsScope` — explicit `convs` is honoured,
  an unticked member and an out-of-group conv are both dropped.
- `SkipsOfflineAgents` — an offline member is never collected.
- `UnfocusNoWindowIsNoOp` — 0 clients → `no_window`, not `failed`.
- `RejectsBadDirectionAndScope` — malformed requests → 400.

## CLI

No CLI verb. Focusing windows is inherently a human-desktop operation
driven from the dashboard; the per-agent `tclaude session focus`
already covers the single-agent case from a terminal. A bulk CLI verb
was considered and skipped — the dashboard buttons were the explicit
ask and there is no agent-facing use case.
