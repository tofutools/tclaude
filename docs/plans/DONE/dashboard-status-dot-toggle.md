# Clickable on/off toggle on the agent status-light dot

Shipped 2026-05.

## What shipped

Each agent row's far-left status light (green ● = online, grey ○ =
offline) is now a clickable on/off toggle:

- **Green dot click → turn the agent off** (soft `/exit`). If the agent
  is non-idle (working / awaiting), a confirm modal pops first; if it is
  idle, it stops immediately with just a toast. The human explicitly
  scoped the confirm to non-idle — a misclick on an idle agent only
  halts a non-busy, resumable session.
- **Grey dot click → turn the agent on** (resume / wake). No confirm —
  starting a session is non-destructive.

The toggle does NOT delete or retire anything; it only stops / starts
the running tmux session.

## Implementation — frontend only

Entirely in `pkg/claude/agentd/dashboard.html`. No daemon, schema, or
endpoint changes — the toggle reuses the existing
`POST /api/agents/{conv}/stop` and `POST /api/agents/{conv}/resume`
endpoints (the same ones the per-row "shut down" / "wake" buttons hit).

- `agentStatusDot(m)` — renders the dot as a real `<button>` (so it is
  keyboard-reachable via Tab + Enter/Space) carrying
  `data-act="dot-toggle"`, `data-online`, `data-idle`, and a `title` /
  `aria-label` describing what a click will do. Replaces the plain
  `onlineDot(...)` at the two agent-row render sites: `memberRowHTML`
  (Groups tab, real + ungrouped members) and `renderAgents` (Agents
  tab). Non-agent rows (Conversations, Retired — no wake/shutdown path)
  keep the plain `onlineDot`.
- `isIdleState(state)` — mirrors `session/status_callback.go`'s
  `StatusIdle`: idle is exactly `state.status === 'idle'`. Everything
  else an online agent can be in (working, main_agent_idle,
  awaiting_permission, awaiting_input, unreported) counts as non-idle
  and gets the confirm.
- `resumeAgentReq` / `stopAgentReq` — extracted shared helpers. The
  existing `wake-agent` / `shutdown-agent` row-button cases were
  refactored onto them, and the new `dot-toggle` delegated-click case
  reuses them too, so there is exactly one resume path and one stop
  path in the dashboard JS.
- `dot-toggle` case in `bindRowActions`: offline → `resumeAgentReq`;
  online + idle → `stopAgentReq(force=false)` straight away; online +
  non-idle → `confirmModal` then `stopAgentReq(force=false)`. The dot
  always sends a SOFT stop — force-kill stays behind the explicit
  "shut down" button's confirm.
- CSS: `.status-dot` (chrome-free button), `.status-dot-online`
  (green), `.status-dot-offline` (grey), hover background +
  `:focus-visible` outline.

## Test coverage

`pkg/claude/agentd/dashboard_dot_toggle_flow_test.go` (testharness v2)
— the confirm-vs-immediate branch is frontend, so these pin the
backend effect a dot click produces:

- `TestDotToggle_OnlineDotSoftStopsAgent` — green-dot click reaches
  `POST /stop` with `force=false`; session soft-exits, snapshot flips
  the agent offline.
- `TestDotToggle_OfflineDotWakesAgent` — full off→on cycle: stop, then
  grey-dot click reaches `POST /resume`; snapshot flips the agent back
  online.
- `TestDotToggle_IdempotentBothDirections` — a stale-dot click is safe:
  `/resume` on an online agent → `skipped:already_online`, `/stop` on
  an offline agent → `skipped:already_offline`.

## Files

- `pkg/claude/agentd/dashboard.html` — CSS + `agentStatusDot`,
  `isIdleState`, `resumeAgentReq`, `stopAgentReq`, `dot-toggle` case.
- `pkg/claude/agentd/dashboard_dot_toggle_flow_test.go` — 3 flow tests.

## Out of scope

- The dot stays a plain non-interactive `onlineDot` on non-agent rows
  (the virtual Conversations / Retired groups and their Agents-tab
  lists) — those rows have no wake/shutdown path to reuse.
- The candidate-picker dots in the add-member / link modals are not
  toggles — clicking those rows already means "add this member".
