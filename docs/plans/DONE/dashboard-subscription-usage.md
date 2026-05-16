# Dashboard subscription-usage readout

Shipped. The agent dashboard's top bar now shows the account's
subscription usage limits — the 5-hour and 7-day rolling windows —
mirroring the tclaude statusbar's `5h [bar] 17% (2h16m) | 7d ...`
format. The readout sits in the top-right, left of the `● live` dot.

## Where the data comes from

agentd reads the SQLite `usage_cache` table it already owns — no new
credential plumbing, no migration (the table predates this feature).

- The `statusbar` command already persists every rate-limit snapshot
  there: whenever a Claude Code session renders its statusline,
  `tclaude status-bar` receives the 5h/7d buckets and calls
  `usageapi.UpdateFromStatusLine`, which writes a `CachedUsage` blob to
  `usage_cache`.
- `usageapi.GetCached` reads that same row (5-min TTL) and only on a
  miss refreshes via the Anthropic OAuth usage API
  (`~/.claude/.credentials.json`). So it already unifies
  "statusbar-writes-to-DB" and "API-poll".

Two paths in agentd:

- **Snapshot read (cheap, zero network):** `collectUsageSnapshot` →
  `usageapi.Peek` (new) → `db.LoadUsageCache`. A pure DB read on every
  `/api/snapshot`.
- **Background poller (side-effect only):** `startUsagePoller` ticks
  every 3 min and calls `usageapi.GetCached` purely to keep the
  `usage_cache` row fresh when no statusbar is running. `GetCached`'s
  own TTL keeps API hits rare; a failed refresh is logged at debug and
  never reaches the snapshot.

## Graceful degradation

The readout degrades to a muted `usage: n/a` (never a broken/error
state) when `collectUsageSnapshot` returns `Available: false`:

- nothing cached yet (cold start),
- the cached reading is older than `usageStaleAfter` (30 min),
- the cache carries no rolling-limit buckets (e.g. an API-billing
  account, which has cost but no 5h/7d windows).

## Surface

- `/api/snapshot` payload gains a `usage` object:
  `{ available, five_hour?, seven_day? }`, each window
  `{ pct, resets_at, remaining }`. `remaining` is pre-formatted
  (`2h16m`, `5d9h`, `reset`) mirroring the statusbar's `resetTimer`.

## Files

- `pkg/claude/common/usageapi/usageapi.go` — `Peek()`, the
  network-free cached-read accessor.
- `pkg/claude/agentd/usage.go` — poller, `collectUsageSnapshot`, wire
  types (`dashboardUsage`, `usageWindow`), format helpers.
- `pkg/claude/agentd/dashboard.go` — `snapshotPayload.Usage` field +
  `out.Usage = collectUsageSnapshot()`.
- `pkg/claude/agentd/serve.go` — `startUsagePoller(cronStop)`.
- `pkg/claude/agentd/dashboard.html` — `#usage` top-bar element, CSS,
  and the `renderUsage` JS (mini bar + percent + remaining per window).

## Tests

`pkg/claude/agentd/dashboard_usage_flow_test.go` — flow tests over the
real `/api/snapshot` surface:

- `TestDashboardUsage_SurfacedInSnapshot` — fresh 5h + 7d cache →
  snapshot carries both windows with percent, remaining-time, reset
  timestamp behind `available: true`.
- `TestDashboardUsage_UnavailableDegradesGracefully` — all three
  unavailable cases (cold start, stale, no buckets) → `available:
  false`, no windows.
