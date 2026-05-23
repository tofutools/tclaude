# fsnotify-based live conv_index monitor in agentd — SHIPPED

PR 1 of the conv_index-freshness work. `agentd` now runs one global
fsnotify watcher over `~/.claude/projects/` that keeps the `conv_index`
SQLite cache continuously fresh, and the dashboard's recent-conversations
list reads straight from the cached rows instead of re-stat+reparsing
each `.jsonl` on every poll.

## What shipped

### The monitor — `pkg/claude/agentd/fsnotify.go` (NEW)

`convMonitor`: ONE `fsnotify.Watcher` over `~/.claude/projects/`.
fsnotify is not recursive on any platform, so the watcher `Add()`s the
projects root (to catch new project dirs) plus every existing project
subdir (to catch `.jsonl` writes within).

- `startConvMonitor(stop <-chan struct{}) *convMonitor` — resolves
  `convops.ClaudeProjectsDir()`, `os.MkdirAll`s it (so a cold machine
  where Claude Code never ran is still watchable), creates the watcher,
  `Add()`s the root + every subdir synchronously, launches the
  event-loop goroutine. Returns `nil` (after a warning) if the watcher
  can't be created — the daemon still runs, conv_index just isn't
  self-maintaining.
- Event loop (`loop`): a single goroutine. Every `ScanAndUpsertFile`
  call happens on it — debounce timers only enqueue a path onto an
  internal `fireCh`, they never touch the DB — so once the loop returns
  no re-index is in flight. That is what makes shutdown / test teardown
  race-free.
- Startup scan (`startupScan` / `reindexDir` / `reindexIfStale`):
  fsnotify only delivers *future* events, so the loop first walks every
  existing `.jsonl` under the projects root once. Each file goes
  through `RefreshConvIndexEntry`'s freshness guard (mtime + size match
  against the cached row): a conv whose on-disk file is unchanged
  since the last index pass is skipped, a conv whose file moved past
  the cached row gets re-parsed, and a conv with no cached row at all
  is scanned fresh. So on a daemon restart, only the convs that
  actually changed (or are brand-new) hit the parser — boot cost
  scales with churn, not with total conv count. Honours the stop
  channel so a shutdown mid-scan doesn't drag boot out.
- `handleEvent`: a new dir directly under the root → `Add()` it +
  `scanDir` it (catches `.jsonl` already inside, narrowing the
  dir-created-then-file-written race). `.jsonl` create / remove /
  rename → re-index immediately. `.jsonl` write → debounce per-file
  (`convMonitorDebounce`, 500ms — well under the 5s dashboard poll, so
  a change is visible on the very next render, while still coalescing a
  response's burst of turn writes; CC streams turns at sub-500ms gaps).
- Depth gate `filepath.Dir(filepath.Dir(path)) == projectsDir` rejects
  the CC-internal `<convID>/` and `memory/` subdirs; flat sub-agent
  `<convID>.jsonl` files still pass.
- `reindex` calls `convops.ScanAndUpsertFile` — idempotent and
  self-cleaning (upserts on a change, deletes the row when the file is
  gone). It is the single seam where the future SSE / dashboard-push PR
  publishes a "conv changed" event; PR 1 deliberately builds no
  broadcaster / fan-out around it.
- Partial-write safety: a `.jsonl` read while Claude Code is mid-append
  cannot corrupt or wipe the cache. `parseJSONLSession` skips any line
  that is not complete valid JSON; the destructive branch-history
  rebuild is gated on `scanComplete` (false for a truncated scan); and
  a row is only deleted on an `os.Stat` not-exist. The debounce also
  means the file is read well after the writes settle.

### Lifecycle wiring — `pkg/claude/agentd/serve.go`

`runServe` calls `startConvMonitor(cronStop)` alongside the other
background goroutines (cron scheduler, session reaper, usage poller).
Shares the daemon-wide stop channel.

### Dashboard poll-on-read removal — `dashboard.go` + `agent/lookup.go`

The dashboard's 5s snapshot poll re-`os.Stat`+reparsed each conv's
`.jsonl` to render its title — across the recent-conversations list,
the agent rows, group members/owners, retired agents, sudo grants and
cron-job labels (~75+ stat calls per poll). Now that the monitor keeps
`conv_index` live, the whole poll reads titles straight from the cached
rows — the dashboard snapshot touches the filesystem for titles not at
all:

- `collectConversationsSnapshot` (non-agent list) renders via
  `convindex.FormatConvTitle` on the row from `db.ListRecentConvIndex`.
- `agent.CachedTitle` was added — `FreshTitle`'s cache-only twin: same
  custom-title > pending-name > summary > first-prompt > UnknownTitle
  priority, but reads `db.GetConvIndex` instead of rescanning the
  `.jsonl`. The dashboard's agent rows, group members/owners, retired
  agents, sudo titles and cron labels all resolve through it. The
  pending-name tier (from `agent_enrollment`) still names a
  just-spawned agent before its first index event lands. The
  rescan-backed `Fresh*` functions stay for the CLI and other callers
  that may run with no monitor live.

This is the previously-reverted `fix/dashboard-skip-nonagent-stat`
change, generalised to the whole poll and now safe.

## Tests — `pkg/claude/agentd/fsnotify_test.go`

Flow-test style, against a real fsnotify watcher over the test HOME's
`~/.claude/projects`. Assert at the real read surface — `db.GetConvIndex`
— with no explicit `conv ls`; the monitor is the only thing touching
the index. Events are async, so assertions poll via `require.Eventually`.

- `TestConvMonitor_StartupScanIndexesExistingConvs` — a conv that
  existed before the monitor started (no cached row at all) is indexed
  by the startup scan.
- `TestConvMonitor_StartupScanRefreshesChangedConv` — a stale cached
  row (`.jsonl`'s on-disk mtime/size moved past the cache while tclaude
  was down) is re-parsed on restart. The repair-on-restart path.
- `TestConvMonitor_StartupScanSkipsUnchangedConv` — the freshness
  guard's complementary property: a cached row whose mtime/size match
  the on-disk file is NOT re-parsed (proved via a sentinel `Summary`
  the real file would never produce, asserted to survive the scan).
- `TestConvMonitor_WriteRefreshesConvIndex` — a `/rename`-style write to
  a live `.jsonl` lands in `conv_index` on its own.
- `TestConvMonitor_NewConvIsIndexed` — a brand-new conversation under a
  project dir created after the monitor started is still indexed.
- `TestConvMonitor_RemoveDeletesConvIndexRow` — deleting a `.jsonl`
  drops its `conv_index` row.

Test hook `StartConvMonitorForTest(t, debounce)` in `testhooks_test.go`
starts the monitor against the test HOME with a shrunk debounce and
registers a synchronous `t.Cleanup` stop (closes the stop channel and
blocks on the event-loop goroutine exiting).

## Out of scope — separate later PRs

- SSE / browser push so the dashboard updates without the 5s poll —
  `docs/plans/TODO/med-prio/dashboard-realtime-push.md`. The `reindex`
  seam is the hook point.
- The ~150 `tmux has-session` subprocess spawns per dashboard poll
  (`isConvOnline` + `stateForConv`, still per-row in
  `collectConversationsSnapshot`).
- Replacing the spawn-wait polling loops in `handleGroupSpawn` /
  `handleReincarnate` with monitor events.

## Files

- `pkg/claude/agentd/fsnotify.go` — NEW: the monitor.
- `pkg/claude/agentd/serve.go` — `startConvMonitor(cronStop)` wiring.
- `pkg/claude/agentd/dashboard.go` — cached-row title rendering across
  the whole snapshot poll.
- `pkg/claude/agent/lookup.go` — NEW `CachedTitle`, the cache-only
  twin of `FreshTitle`.
- `pkg/claude/agentd/fsnotify_test.go` — NEW: scenario tests.
- `pkg/claude/agentd/testhooks_test.go` — `StartConvMonitorForTest`.
- REFERENCE (unchanged): `pkg/claude/conv/watch.go` —
  `startFSWatcher` + `fsDebounceLoop`, the fsnotify pattern ported from.
