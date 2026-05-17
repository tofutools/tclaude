# Dashboard: confirm every drag-and-drop agent move

Shipped 2026-05 — PR #135 (`feat(dashboard): confirm every drag-and-drop
agent move`). This doc was added by the follow-up PR (see end).

## What ships

Every drag-and-drop agent operation on the dashboard's Groups tab now
pops a confirmation modal *before* it mutates anything, so an
accidental drag can no longer move, clone, retire, or reinstate an
agent. Drag-and-drop is a low-friction gesture — easy to trigger by
mistake — and before this change a stray drop changed group
membership or retired an agent with no undo.

The seven drag operations and their gates:

| Operation | runDnd\* function | Gate |
|-----------|-------------------|------|
| Clone into group | `runDndClone` | `confirmModal` |
| Move between groups | `runDndMove` | `confirmModal` |
| Add ungrouped → group | `runDndAddToGroup` | `confirmModal` |
| Remove from group | `runDndRemoveFromGroup` | `confirmModal` |
| Promote to ungrouped | `runDndPromoteToUngrouped` | `confirmModal` |
| Reinstate retired agent | `runDndReinstate` | `confirmModal` |
| Retire (drag to Retired) | `runDndRetire` | `retireConfirm` |

Six operations gate on the shared `confirmModal` helper. `runDndRetire`
was *already* gated — it predates this change and keeps its richer
`retireConfirm` modal (the optional "also shut the session down"
checkbox), so a retire-by-drag asks the identical question as the
per-row retire button. The net effect: all seven drag operations are
now uniform — nothing mutates state without an explicit confirm.

## Design — the gate lives inside each runDnd\* function

The confirm is the *first step inside* each `runDnd*` function, not a
wrapper at the drop-dispatch site. Each function:

1. `await confirmModal({title, body, meta, okLabel})` — the modal
   names the agent and the source/target groups in plain language.
2. `if (!confirmed) { await refresh(); return; }` — the cancel path
   bails *before* any daemon round-trip or optimistic snapshot
   mutation.
3. Runs the operation.
4. `finally { await refresh() }` — every exit path (cancel, a
   guard-clause return, partial failure, success, a thrown error)
   funnels through one resync. This matters because the confirm modal
   suspends the 5s auto-refresh while open, and the `dragend`-fired
   `refresh()` bailed for the same reason (`refreshSuspended()` saw
   the modal) — so without the `finally` a confirmed-then-aborted op
   would leave the dashboard showing stale state until the next tick.

`runDndMove` is the only *optimistic* operation — it splices
`lastSnapshot` to render the move instantly before the POST→DELETE
round-trip. Both the confirm and its cancel guard precede that splice,
so a cancelled move leaves the snapshot and the render completely
untouched (no flicker).

## Tests — a structural guard

The gates live entirely in `dashboard.html`'s embedded JS — each
`runDnd*` function awaits a modal — so there is no server code path a
flow test can exercise, and the repo has no JS test runner. The seven
operations were exercised by hand in a browser (confirm AND cancel).

`pkg/claude/agentd/dashboard_dnd_confirm_test.go` is therefore a
**structural guard**, following the established pattern of the other
`dashboard_*_test.go` guards (refresh-guard, context-meter, sort,
wtsync). A `dndFuncBody` helper slices a single `runDnd*` function's
source span out of the embedded `dashboardHTML` string by index, and
`TestDashboardHTML_DndOperationsConfirm` asserts, per function:

- `await confirmModal({` is present — the operation is gated;
- both the confirm and its `if (!confirmed)` cancel guard precede the
  first `await fetch(` — a refactor that fetched, then checked
  `!confirmed`, would not pass;
- a `finally {` resync block exists;
- for `runDndMove` specifically, the confirm **and** the cancel guard
  precede the optimistic `.splice(` (the cancel-before-splice half was
  added by the follow-up PR — see below);
- `runDndRetire` stays gated behind `retireConfirm`.

It pins the *shape* of the gates so a future refactor of the file
cannot silently drop one.

## Files

- `pkg/claude/agentd/dashboard.html` — the six `runDnd*` functions
  gain a leading `confirmModal` await, cancel guard, and `finally`
  resync. `confirmModal` itself is a pre-existing shared helper
  (also used by the bulk shutdown/power-on buttons); `retireConfirm` and
  `runDndRetire` were already in place.
- `pkg/claude/agentd/dashboard_dnd_confirm_test.go` — the structural
  guard test.

## Follow-up PR (this doc's PR)

PR #135 was merged before its assigned follow-up reached the worker.
A combined follow-up PR carried three items:

1. **Fixed `runDndClone`'s confirm body text.** It described the clone
   as "a fresh conversation that inherits the original's identity" —
   wrong. `runDndClone` POSTs an empty body to
   `/api/agents/{conv}/clone`, so `no_copy_conv` defaults to `false`
   and the daemon copies the source `.jsonl`; the clone resumes with
   the source's full conversation history. The text now says the
   clone inherits the identity **and a copy of its conversation
   history** — matching the existing clone-modal checkbox wording.
2. **Closed a test gap.** The `runDndMove` block pinned confirm-before-
   splice but not cancel-before-splice; a refactor moving the cancel
   guard between the splice and the fetch would have passed both
   existing checks while a cancelled move still ran the optimistic
   splice. Added the cancel-before-splice assertion.
3. **Added this DONE doc** and the cross-reference below.

## Cross-references

- [`DONE/dashboard-dnd-move.md`](dashboard-dnd-move.md),
  [`DONE/dashboard-dnd-clone.md`](dashboard-dnd-clone.md) — the drag
  operations these gates protect.
