# Snapshot-shaped tmux liveness via one `list-sessions` — SHIPPED

Follow-up to the fsnotify conv_index monitor (`fsnotify-monitor.md`).
The dashboard poll and the snapshot-shaped CLI list handlers stopped
firing one `tmux has-session` subprocess per session row; they now
issue ONE `tmux list-sessions` per request and test each row's
liveness via a map lookup against the resulting alive set.

## What shipped (one paragraph)

`clcommon.Tmux` gains a `ListSessions() (map[string]struct{}, error)`
method. `LiveTmux.ListSessions` forks one
`tmux -L tclaude list-sessions -F '#{session_name}'`, parses one name
per line, and collapses a non-zero "no server running" exit to an empty
set (= "everything offline" — the same semantics the old per-row probes
gave when the server was down). A thin `session.LiveTmuxSessions()`
wrapper keeps the agentd handlers off the `clcommon` boundary. Each
snapshot-shaped HTTP handler fetches the alive set ONCE at the top and
passes it down to new `isConvOnlineIn(convID, alive)` /
`stateForConvIn(convID, alive)` helpers — both do the per-row test via
map lookup, never spawn a subprocess. Single-target callers (delivery,
lifecycle, reaper, conv watchers) stay on the existing
`isConvOnline` / `session.IsTmuxSessionAlive` — those fire one probe
each by design and the batch overhead would be wasted.

## Boundary widening — `pkg/claude/common/tmux.go`

The `Tmux` interface (the one of two production-test seams flow tests
inject through) gains `ListSessions`. Sim and live impl both
implement it; the rest of the test harness is unchanged.

- `LiveTmux.ListSessions` — production: `tmux ls -F '#{session_name}'`
  → split lines → trim → set. `*exec.ExitError` is folded to an empty
  set with nil error (the normal "no server" state, not a probe
  failure); other errors propagate.
- `TmuxSim.ListSessions` — testharness: walks its in-memory
  `sessions` map under the lock, applies the same `IsAlive` predicate
  per name, returns the surviving set. Identical to N `IsAlive`
  calls — just routed through the bulk API.

## Variant helpers — `pkg/claude/agentd/`

Two new functions named with the `-In` suffix to mark them as
"snapshot-shaped, caller supplies the alive set":

- `isConvOnlineIn(convID, alive map[string]struct{}) bool` (handlers.go) —
  walks the conv's session rows, returns true on first alive map hit.
- `stateForConvIn(convID, alive map[string]struct{}) agentState`
  (dashboard.go) — rename of the old `stateForConv`. All its callers
  were dashboard-only, all in snapshot loops; the rename forces every
  current AND future caller to pass an alive set.

`isConvOnline` (without the suffix) stays for single-target callers
(`nudgeIfAlive`, lifecycle / reincarnate / reaper / dashboard cleanup).
Its doc was tightened to point future readers to `isConvOnlineIn` for
loop-shaped callers.

## Converted call sites

Every per-row tmux probe inside a snapshot-shaped loop now uses the
batched path. The handlers fetch the alive set once at the top:

- `handleDashboardSnapshot` (dashboard.go) — the 5-second dashboard
  poll. Single biggest win. Threads `aliveSessions` to
  `collectConversationsSnapshot` and `collectRetiredSnapshot`.
- `handlePeers` (handlers.go) — `tclaude agent peers`.
- `handleGroups` (handlers.go) — `tclaude agent groups ls` member
  expansion.
- `handleGroupMembersList` (handlers.go) — `tclaude agent groups
  members <name>`.
- `handleGroupOwnersList` (handlers.go) — `tclaude agent groups
  owners <name>`.

Single-target paths kept on the per-call subprocess — they fire one
probe at most each, and bulk-fetch overhead would dominate:

- Message delivery (`nudgeIfAlive`, `agent/message.go`).
- Slash injection (`injectSlashCommand`).
- Lifecycle: kill / focus / attach / resume.
- `dashboard_cleanup.go` per-target double-checks before mutating.
- `reincarnate.go`, `reaper.go`, `flush.go`.
- `conv/watch.go`, `session/watch.go` periodic per-session checks.

## Test — `pkg/claude/agentd/dashboard_tmux_batch_flow_test.go`

`TestDashboardSnapshot_OneTmuxListZeroHasSession`: sets up a four-
member group with mixed alive / killed tmux sessions, snapshots the
`TmuxSim.CommandCount` baseline, fetches `/api/snapshot`, asserts:

1. **Count invariant**: exactly ONE `list-sessions` and ZERO
   `has-session` calls fired by the snapshot. The regression that
   reintroduces per-row probing in the snapshot path fails this
   assertion long before it becomes a perf incident.
2. **Correctness**: every member's `Online` flag matches its actual
   tmux liveness across the alive/offline mix — proving the bulk path
   has not diverged from the per-row semantics.

The `TmuxSim.CommandCount(verb)` accessor is new — added to
`pkg/testharness/tmux_sim.go` alongside `ListSessions`. It records
every `Command(args...)` invocation by `args[0]`, with `ListSessions`
counting under the synthetic verb `"list-sessions"`.

## Out of scope — separate later PRs

- `dashboard_cleanup.go` per-target double-checks (`isConvOnline` at
  lines ~156, ~293) — different shape (per-target action guards, not
  snapshot loops) and lower volume.
- SSE / browser push so the dashboard updates without the 5s poll —
  `docs/plans/TODO/med-prio/dashboard-realtime-push.md`. Now that
  both `.jsonl` reads and tmux probes are essentially free, the poll
  is cheap; SSE remains a latency win, not a CPU one.

## Files

- `pkg/claude/common/tmux.go` — `Tmux.ListSessions` on the interface +
  `LiveTmux.ListSessions` impl.
- `pkg/claude/session/session.go` — `LiveTmuxSessions()` wrapper.
- `pkg/claude/agentd/handlers.go` — `isConvOnlineIn` + converted
  `handlePeers`, `handleGroups`, `handleGroupMembersList`,
  `handleGroupOwnersList`.
- `pkg/claude/agentd/dashboard.go` — `stateForConvIn` rename, alive
  set threaded through `handleDashboardSnapshot` and both collectors.
- `pkg/testharness/tmux_sim.go` — `ListSessions` impl, command-count
  tracking, `CommandCount(verb)` accessor.
- `pkg/claude/agentd/dashboard_tmux_batch_flow_test.go` — NEW: pins
  the one-call / zero-probe invariant.
