# fsnotify-based live `.jsonl` monitor in agentd

**Hypothesis:** CC rewrites the `.jsonl` immediately on `/rename`
(and on every turn generally), so a filesystem watcher on
`~/.claude/projects/...` would let the daemon detect title changes /
new conv files / etc. in real time — replacing the current
poll-on-read pattern.

## Validate first

Before building anything, hook a transient fsnotify watcher up to a
single project dir during a normal session. Confirm `/rename`
produces an immediate `Write` event (and that the new title is in the
file at that moment, not on the *next* turn). If the write is
buffered until end-of-turn, the watcher is less useful and we keep
poll-on-read.

## Use cases (after validation)

1. **Rename propagation.** Reincarnate's `uniqueReincarnateTitle`
   reads the parent's CustomTitle from `conv_index`; a watcher would
   push fresh values into `conv_index` without waiting for `tclaude
   conv ls` or the watch model.
2. **Dashboard live-refresh.** Title / context-pct changes show up
   in the dashboard without a manual refresh. **Pairs with**
   [`dashboard-realtime-push.md`](dashboard-realtime-push.md) —
   fsnotify is the event SOURCE; the push TODO is the browser-
   delivery TRANSPORT. Best to design + ship them together.
3. **Cheap "new conv spawned" detection.** Replaces the polling loop
   in `handleGroupSpawn` / `handleReincarnate` that waits for the
   new `.jsonl` to appear.

## Library

`github.com/fsnotify/fsnotify` (cross-platform, Go-native). One
watcher per `~/.claude/projects/<sanitised>` dir, started lazily when
the daemon first sees a request referencing that project — avoids
holding watchers on long-archived projects.

## Resource ceiling

fsnotify on Linux uses inotify watches, capped per-user
(`fs.inotify.max_user_watches`). Cap our watchers at e.g. 64 active
project dirs, evict LRU.

## Files
- `pkg/claude/agentd/` — new file `fsnotify.go` once validated
- `pkg/claude/agent/lookup.go` — fresh-conv-row resolution (current
  poll-on-read site)
