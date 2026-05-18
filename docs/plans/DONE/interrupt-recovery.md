# Interrupt recovery — un-stick a session after a user-interrupt

## Problem

When the user cancels an in-flight turn with Escape, Claude Code fires
**no hook** — not `Stop`, not anything (the feature request for an
interrupt hook, anthropics/claude-code#11189, was closed as
not-planned). The session row stays frozen at whatever the last hook
left it — typically `status='working', status_detail='UserPromptSubmit'`
— and the dashboard shows "working: UserPromptSubmit" indefinitely.

PR #185 (`fix(hooks)`, predecessor) tried to recover via the
`Notification idle_prompt` hook. Empirically that does not work: agentd
logs show CC emits `idle_prompt` only ~60–70s **after a clean `Stop`**
(when the session is already `idle` — a no-op), and **never** in the
post-interrupt `working`-stuck state it was meant to fix. `idle_prompt`
is the wrong signal. PR #185 is kept as a harmless backstop.

## What shipped

The only reliable signal is the one CC writes to the conversation
`.jsonl`: a `[Request interrupted by user]` user turn (CC also writes
the `for tool use` variant). agentd already rescans a conv's `.jsonl`
whenever the dashboard poll resolves it and the file's mtime/size
changed (`RefreshConvIndexEntry` → `ScanAndUpsertFile` →
`parseJSONLSession`). An interrupt grows the file, so the next poll
already rescans it — the recovery rides that existing rescan, with **no
new poller / watcher / goroutine**.

- `parseJSONLSession` tracks the last *conversation* turn and sets the
  transient `SessionEntry.LastTurnInterrupted` when it is an interrupt
  marker. Only user/assistant records count as turns: sidecar records
  (`file-history-snapshot`, `custom-title`, …) and text-less `user`
  records (tool_result carriers — what CC writes to close a cancelled
  tool call) can trail a real interrupt yet must not reset the flag.
  The marker is matched **exactly** against the known strings
  (`interruptMarkers`), never by prefix — a genuine prompt that merely
  begins with `[Request interrupted` must not be misread.
- `ScanAndUpsertFile`, after a complete scan, calls
  `db.MarkSessionsIdleAfterInterrupt(convID)` when the flag is set.
- `MarkSessionsIdleAfterInterrupt` flips every `working` session row of
  the conv to `idle` with a cleared `status_detail`. conv-scoped (a
  conv can own several session rows); only `working` rows are touched,
  so it is idempotent and never disturbs `exited` / `awaiting_*` /
  already-`idle` rows.

Gated on `scanComplete`: a truncated scan never reached the real last
turn, so its `LastTurnInterrupted` is not authoritative.

Recovery latency = one dashboard poll. No schema migration —
`LastTurnInterrupted` is transient, never persisted to `conv_index`.

## Why not fsnotify

An fsnotify watcher in agentd was considered (and named as the
follow-up in PR #185). Rejected: it adds watcher lifecycle (start/stop
per active session, inotify-watch ceilings, file rotation) for a
marginal latency win on a non-hot path. Piggybacking the rescan the
dashboard poll already performs is strictly simpler. (A general
fsnotify monitor remains an open, unrelated idea —
`TODO/med-prio/fsnotify-monitor.md`.)

## Files

- `pkg/claude/common/convops/convops.go` — `SessionEntry.LastTurnInterrupted`,
  detection in `parseJSONLSession`, action in `ScanAndUpsertFile`.
- `pkg/claude/common/db/sessions.go` — `MarkSessionsIdleAfterInterrupt`.
- `pkg/claude/common/convops/convops_test.go` — tests.

## Tests

- `TestParseJSONLSession_LastTurnInterrupted` — table: marker last →
  true; `for tool use` text variant; marker as a content block array;
  marker as the only record; sidecar records and a tool_result carrier
  after the marker keep it true; a real user/assistant turn after the
  marker clears it; a prompt that merely *starts with* the marker text
  → false (exact match, not prefix); no marker → false.
- `TestRefreshConvIndexEntry_RecoversInterruptedSession` — the dashboard
  path: index a normal turn, append the marker, `RefreshConvIndexEntry`
  → the stuck `working` row recovers to `idle`; `exited` and
  `awaiting_*` sibling rows are left alone.
- `TestRefreshConvIndexEntry_GenuineWorkLeavesSessionWorking` — mirror:
  a real assistant turn lands → a `working` session is NOT flipped.
