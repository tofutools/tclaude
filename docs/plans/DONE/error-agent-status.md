# Error agent status on the dashboard

When an agent's turn ends in an API/auth/billing error, the dashboard
used to stay frozen on the agent's last successful status (e.g.
"working") — tclaude did not listen for the error event at all. It now
surfaces an explicit, transient `error` status.

## What shipped

- **`StopFailure` hook registered.** `session/hooks.go` `RequiredHooks`
  now includes `StopFailure`, so Claude Code invokes
  `tclaude session hook-callback` when a turn ends in an error (a
  separate event from `Stop`). Existing installs pick it up via the
  normal `EnsureHooksInstalled` auto-install path.

- **`StatusError = "error"`** — new status constant in
  `session/status_callback.go`.

- **Hook callback.** `HookCallbackInput` gained `error_type` /
  `error_message` fields. The new `StopFailure` case in
  `session/hook_callback.go` sets `status=error` and puts `error_type`
  into `status_detail` (falling back to `"unknown"` if absent), and
  logs `error_message` at warn level. It deliberately does NOT set
  `stopped=true`: the `stopped` branch drives auto-compact, the context
  nudge and the task-runner signal — all of which would *act on* the
  error (out of scope). The status transition + notification fire
  regardless.

- **Transient, not sticky.** Every other hook case sets `state.Status`
  unconditionally, so the next normal event (UserPromptSubmit, a tool
  event, a later Stop) clears `error` back to working/idle on its own.
  Verified by test — nothing blocks the clear.

- **Dashboard.** `agentd/dashboard.html`: `statusPillClass` maps
  `error` → a new bold-red `.state-error` pill ("error: rate_limit");
  `agentStatusDot` renders a red `.status-dot-error` dot for an online
  errored agent (still a working toggle — its CC process is alive).
  `stateForConv` needed no change: its `exited` override is keyed on
  tmux liveness, not the status string, so a live errored agent keeps
  `error` and only a dead session is overridden to `exited`.

- **CLI surfaces.** `error` is wired into `getStatusColorFunc`,
  `getRowStyle`, `statusPriority` (red / needs-attention / sorts
  first), the `session` watch filter list, and `normalizeStatusFilter`
  (`--show error`, and `attention` now includes it).

## Follow-up fix (post-merge cold review)

The PO's independent cold review of #137 caught one defect that both
the author's cold review and the brief missed:

- **Watch-TUI `attention` filter omitted errors.** `session/watch.go`
  has its own `matchesShowFilter` / `matchesHideFilter` (model methods,
  separate from `list.go`'s) that expand the `attention` group inline —
  they listed only `awaiting_permission` / `awaiting_input`. So
  `session ls --show attention` surfaced an errored agent on the CLI
  but the interactive watch TUI silently did not. Fixed: both now
  include `StatusError`. Also added `StatusError` to the
  `runStatusCallback` validation switch (`status_callback.go`) — a
  latent trap, not a live bug (StopFailure routes via `hook-callback`,
  never the positional `status-callback`). Test coverage added for the
  previously-untested watch filter methods and the needs-attention
  rendering switches.

- **Notifications.** Default notification rules
  (`config.DefaultConfig`) gained `{from:"*", to:"error"}`, and
  `notify.formatStatus` renders it as "Error" — so a transition into
  the error status fires a desktop notification.

## Tests

- `session/hook_callback_test.go` — StopFailure → `status=error` +
  `error_type` in detail; missing `error_type` → `"unknown"`; the
  transient clear (StopFailure then UserPromptSubmit → back to working).
- `session/hooks_test.go` — `RequiredHooks` registers `StopFailure`.
- `common/config/config_test.go` — a `*→error` transition is
  notify-worthy by default.
- `agentd/dashboard_error_state_flow_test.go` — flow test: errored +
  alive surfaces `online + status=error` through `/api/snapshot`;
  healthy control unaffected; errored + dead is overridden to `exited`
  (no stale `error` for a gone process).

## Out of scope (deliberate)

Error nudge / auto-retry / multicast-on-error — a deferred future idea.

## Follow-up considered: a non-error "stopped" indicator

The human also asked, unsure, for "a regular stopped indicator if
stopped for other reasons." Investigation:

- **Dead vs healthy-idle is already solved.** A dead agent renders as a
  grey `offline` pill + grey `○` dot — clearly distinct from a green
  `●` + yellow `idle`. The earlier offline-state fix (`stateForConv`'s
  `exited` override) already stops a dead agent masquerading as idle.
- **Clean `/exit` vs unexpected crash is NOT distinguishable** — both
  render as `offline`. It cannot be inferred from the frozen row
  status: `RefreshSessionStatus` (the reaper, `session.go:316`) flips a
  dead session to `exited` regardless of cause, racing the SessionEnd
  hook. Reliably telling them apart needs an explicit `exit_reason`
  recorded by the SessionEnd hook (one nullable column) and left unset
  by the reaper.
- **A long-idle "stalled?" hint is not recommended** as a first step —
  an idle agent waiting for input is normal, not stalled; flagging it
  would be noise.

Recommendation surfaced to the PO: if anything beyond the error status
is wanted, ship just the clean-exit-vs-crash distinction as a separate
PR; defer the stalled hint to `future/`.
