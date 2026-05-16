# Retire + optional shutdown

Shipped 2026-05.

## What ships

Retiring an agent demotes it to a plain conversation (revokes group
memberships + permission/sudo grants, keeps the .jsonl, reinstatable)
but used to leave the agent's Claude Code session running — an idle
CC instance sitting in a tmux pane in retired state. A human who
retires an agent almost always wants that process gone too.

Every retire surface now also soft-stops the agent's running session,
**defaulting to ON**. Shutdown is a graceful soft exit (`stopOneConv`
with `force=false` — injects `/exit`, never `kill-session`). Retire
semantics are unchanged: the conversation is kept on disk and is
reinstatable; shutdown only ends the live process. A retired agent
whose session is already dead is a no-op (`skipped:already_offline`).

## Surfaces

1. **Backend.** `POST /v1/agent/{selector}/retire` (and the dashboard
   `POST /api/agents/{conv}/retire`, which routes to the same handler)
   take a `?shutdown=` query param. Absent or unparseable → ON, the
   documented default, so a forgetful caller fails safe. `?shutdown=0`
   opts out. The 200 response gains a `shutdown` object (the
   `stopOneConv` result: `soft_stopped` / `skipped:already_offline` /
   `error`) — present only when shutdown ran.

2. **Dashboard per-row retire button** (Agents tab). The old
   `confirmModal`-based prompt is replaced by a dedicated
   `retireConfirm` modal (`#retire-modal`) carrying an "Also shut down
   the running session" checkbox, checked by default. The choice rides
   to the endpoint as `?shutdown=1|0`.

3. **Dashboard drag-and-drop.** Dragging an agent row onto the virtual
   "Retired" group (`runDndRetire`) already showed a confirm modal; it
   now shares the exact same `retireConfirm` modal — checkbox and all
   — as the per-row button.

4. **Bulk cleanup modal, retire tier.** `POST /api/cleanup/agents`
   gains a `shutdown` field (`*bool`, nil → ON). The modal's retire
   tier shows one "Also shut down running sessions" checkbox governing
   the whole batch, locked to the retire tier via `syncTierLocks`. The
   per-item outcome detail notes `· session soft-stopped` when a pane
   was actually running.

5. **CLI.** `tclaude agent retire` gains `--no-shutdown` (default =
   shutdown ON, consistent with the UI). The CLI always sends
   `?shutdown=1|0` explicitly so behaviour is independent of the
   server-side default. The retire summary prints a `session:` line
   reporting the shutdown outcome. Command help text updated.

## Files

- `pkg/claude/agentd/enrollment_handlers.go` — `retireShouldShutdown`
  helper; `handleAgentRetire` soft-stops after the demotion.
- `pkg/claude/agentd/dashboard_cleanup.go` — `dashboardCleanupAgents`
  body gains `Shutdown *bool`; the retire tier soft-stops each
  retired agent's session unless opted out.
- `pkg/claude/agent/enrollment.go` — `retireParams.NoShutdown`; help
  text; `runRetire` sends `?shutdown=` and reports the outcome.
- `pkg/claude/agentd/dashboard.html` — `#retire-modal` overlay;
  `retireConfirm` helper; `retire-agent` case + `runDndRetire` use it;
  `cleanup-opt-shutdown` checkbox in the cleanup modal.

## Tests

`pkg/claude/agentd/retire_shutdown_flow_test.go` (testharness v2):

- `TestRetire_WithShutdownStopsRunningSession` — `?shutdown=1` leaves
  the agent in `retired[]` AND its session stopped.
- `TestRetire_WithoutShutdownKeepsSessionAlive` — `?shutdown=0` leaves
  the session alive; the response carries no shutdown outcome.
- `TestRetire_AbsentShutdownParamDefaultsToOn` — an omitted param
  defaults to shutdown ON.
- `TestRetire_CleanupTierShutdownToggle` — the cleanup retire tier
  honours `shutdown:true|false` (with `include_online` to reach a
  running agent).

`dashRetired` test struct gained an `Online` field to read the
snapshot's per-retired-agent liveness.

## Out of scope (deferred)

- **Force-kill on retire.** Retire always soft-exits — a retired
  agent's pane should close gracefully, not be killed. The per-row
  shut-down button still offers force kill for the cases that need it.
- **`--shutdown` as an explicit affirmative CLI flag.** Shutdown is
  the default, so only the opt-out (`--no-shutdown`) is meaningful —
  matches the `--no-copy-conv` precedent on `agent clone`.
