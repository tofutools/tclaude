# Clean-exit vs unexpected-death indicator

The dashboard could not tell an agent that exited cleanly (a deliberate
`/exit`, a logout, a normal quit) from one whose process died
unexpectedly (a crash, an OOM kill, `tclaude session kill`, a reboot) —
both collapsed to a grey "offline" pill. This was the follow-up the
error-status work (`error-agent-status.md`) proposed and the human
greenlit.

## How it works

A graceful Claude Code shutdown fires a **SessionEnd** hook carrying a
`reason`. An unexpected death fires **no** SessionEnd at all. That
asymmetry is the whole signal:

- **SessionEnd hook** (`session/hook_callback.go`) records the `reason`
  into the new `sessions.exit_reason` column via
  `db.SetSessionExitReason`. A `/clear` (process survives) still
  records nothing — it is not an exit.
- **SessionStart hook** clears `exit_reason` back to NULL
  (`db.ClearSessionExitReason`) — a resumed session is alive again, so
  a stale reason must not linger to mislabel a later death.
- **The session reaper** stamps `exit_reason='unexpected'` when it
  reaps a dead row that recorded no reason. This is done atomically
  inside `MarkSessionExitedIfUnchanged` as
  `exit_reason = COALESCE(exit_reason, 'unexpected')` — so a reason a
  real SessionEnd already recorded is preserved (the narrow
  reaper-vs-hook race).

**Point-2 decision (reaper stamps vs dashboard infers):** the reaper
explicitly stamps. `exit_reason` is then a single stored source of
truth every reader consumes verbatim — no join-time inference, no two
places encoding "what is a crash". The ~30s reaper cadence before a
crash flips from plain "offline" to "crashed" is imperceptible for a
coarse offline state.

## Schema

- Migration **v41→v42**: `ALTER TABLE sessions ADD COLUMN exit_reason
  TEXT` — **nullable**. NULL means "no reason recorded": a live
  session, or a row that exited before this column existed.

## Dashboard rendering

`agentd/dashboard.go` `stateForConv` reads `exit_reason` (via
`db.GetSessionExitReason`) into `agentState.ExitReason` for an offline
agent; the snapshot carries it. `dashboard.html` `statePill`:

- `exit_reason == 'unexpected'` → an amber **"crashed"** pill
  (`.state-crashed`), tooltip "process ended without a clean exit —
  crash, kill, or reboot".
- any other value, **including empty/NULL** → the plain grey "offline"
  pill, unchanged.

**Self-healing:** only an explicit `'unexpected'` renders as crashed. A
NULL `exit_reason` — a pre-migration corpse, or a death the reaper has
not swept yet — renders as a plain offline. Old data is never
retroactively mislabelled a crash.

## Files

- `pkg/claude/common/db/migrate.go` — `migrateV41toV42`, `currentVersion=42`.
- `pkg/claude/common/db/sessions.go` — `SetSessionExitReason`,
  `ClearSessionExitReason`, `GetSessionExitReason`; `MarkSessionExitedIfUnchanged`
  extended with the COALESCE stamp.
- `pkg/claude/session/hook_callback.go` — SessionEnd records, SessionStart clears.
- `pkg/claude/agentd/dashboard.go` — `agentState.ExitReason`, `stateForConv`.
- `pkg/claude/agentd/dashboard.html` — `.state-crashed` CSS, `statePill`.

## Tests

- `db/migrate_v42_test.go` — the migration adds a nullable column;
  pre-existing rows stay NULL; fresh-schema wiring.
- `db/sessions_exit_reason_test.go` — set/get/clear round-trip; the
  reaper COALESCE stamps `'unexpected'` on a no-reason row and preserves
  an existing reason.
- `session/hook_callback_test.go` — SessionEnd records the reason; a
  `/clear` records nothing; SessionStart clears a stale reason.
- `agentd/session_reaper_flow_test.go` — a reaped no-SessionEnd session
  is stamped `'unexpected'` end to end.
- `agentd/dashboard_exit_reason_flow_test.go` — the snapshot surfaces
  clean / crashed / legacy-NULL / live distinctly.
- `agentd/dashboard_crashed_pill_html_test.go` — guards the `statePill`
  crashed-pill JS contract.

## Notes / scope

- An abrupt termination — `tclaude session kill`, SIGKILL, a reboot —
  fires no SessionEnd, so it lands in `'unexpected'`. This matches the
  brief, which grouped "crashed / killed" together; the "crashed"
  tooltip says "crash, kill, or reboot" so a deliberate kill is not
  mystifying. A dedicated `'killed'` reason for deliberate kills is a
  possible future refinement, deliberately out of scope here.
- The SessionEnd `reason` value set per the live CC docs is `clear`,
  `resume`, `logout`, `prompt_input_exit`, `bypass_permissions_disabled`,
  `other` — stored verbatim. The CLI (`session ls`) still shows a plain
  `exited`; surfacing `exit_reason` there was not in scope.
- Out of scope (deferred): the long-idle "stalled?" hint.
