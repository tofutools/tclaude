# Dashboard: real-time push via SSE — SHIPPED

The dashboard now reacts to a server-sent-event stream the moment the
daemon writes something interesting, instead of waiting up to 2s for
the next snapshot poll. The 2s poll stays in place as a safety net (it
also continues to catch the things the daemon can only learn by polling
upstream — tmux liveness, PR badge state, subscription usage).

## What shipped

### Broadcaster — `pkg/claude/agentd/events.go` (NEW)

`dashboardEvents` (`*broadcaster`) is a process-wide debounced fan-out.
Any writer that wants to nudge the dashboard calls `Publish()`; one
debounce timer drives the fan-out to every `Subscribe()`d channel.

Debounce timings (`eventsBroadcastMinQuiet` / `eventsBroadcastMaxWait`):

- **100ms quiet window** — an isolated write lands ~100ms after the
  call. A burst of writes inside the window keeps re-arming the timer
  so the fan-out fires once after the writes go quiet, not per write.
- **1s max wait** — sustained churn (a hook-callback flurry mid-turn)
  can't suppress fan-out indefinitely; the timer fires at the max-wait
  deadline measured from the FIRST queued Publish, then resets.

All state runs under one `sync.Mutex`. Subscriber channels are
buffered 1; a non-blocking send drops a redundant nudge rather than
stalling fan-out, since a queued nudge already says "go re-fetch."

The vars are exported as `var` (not const) so tests can shrink them.
`newTestBroadcaster()` uses 20ms / 100ms.

### SSE handler — `pkg/claude/agentd/sse.go` (NEW)

`handleDashboardEvents` serves `GET /api/events` as
`text/event-stream`:

- Same auth as the rest of `/api/*` — cookie + Origin/Referer pin via
  `checkDashboardAuth`.
- Initial `: connected` comment + flush so `EventSource.onopen` fires
  immediately, even on a quiet daemon.
- Subscribes to `dashboardEvents`. Each fan-out → writes
  `event: snapshot\ndata: <ts ms>\n\n` + flushes.
- 25s heartbeat comment line keeps the connection alive through
  paranoid proxies / browser tab throttlers.
- Streams until `r.Context().Done()` (client closes / shutdown).

The wire payload is intentionally MINIMAL — clients always react by
re-fetching `/api/snapshot`. Decoupling the event from the payload
keeps the SSE protocol stable across snapshot-shape changes; only the
debounced "something changed" signal travels the wire.

### Writer-side publish wiring

Three classes of writer, three publish strategies:

1. **HTTP-mux mutations.** `publishOnSuccessfulWrite` (in events.go)
   wraps both the popup mux (`/api/*`) in `popup.go` and the daemon's
   Unix-socket mux (`/v1/*`) in `serve.go`. Non-GET requests with a
   2xx response trigger `dashboardEvents.Publish()`. GET / HEAD /
   OPTIONS pass through unmodified, which is load-bearing: the SSE
   handler itself (GET `/api/events`) needs an unwrapped
   `http.Flusher` to stream. One wrap covers every current and future
   mutation route — no risk of a new `/api/foo` POST shipping
   without firing dashboard events.

2. **fsnotify reindex.** `convMonitor.reindex` (`fsnotify.go`) now
   calls `Publish()` after `ScanAndUpsertFile`. A `.jsonl` write —
   typical of every Claude turn — triggers an SSE event after the
   debouncer settles. (This implicitly covers hook-callback writes
   too: the hook runs in a subprocess and writes the `sessions` table
   directly, but Claude Code is writing the same conversation's
   `.jsonl` at the same moment, so fsnotify catches the change and
   the subsequent snapshot rebuild sees the fresh `sessions` row.)

3. **Background daemon goroutines.** Cron firings
   (`runCronTick` in `cron.go`) and session reaping (`sessionReaper.tick`
   in `reaper.go`) bypass HTTP entirely. Each calls `Publish()` directly
   when it actually changed something (cron fired ≥1 job, reaper
   marked ≥1 session exited).

### Client — `dashboard/js/refresh.js` + `dashboard.js`

`startEventStream()` opens a `new EventSource('/api/events')` and
binds the `snapshot` event to `refresh()`. The 2s `setInterval`
safety-net poll stays as a fallback for both transport failures and
the upstream-poll changes the daemon can't push.

A single-slot reentrancy guard inside `refresh()` coalesces overlapping
triggers:

- `refreshInFlight` blocks reentry while a fetch / decode / render is
  in flight.
- `refreshQueued` collects any number of mid-flight triggers into one
  chained follow-up on completion.

So three triggers landing on top of one in-flight refresh produce at
most ONE extra round-trip, not three. Manual post-mutation `refresh()`
calls flow through the same guard — a button click that fires while
a poll is mid-flight stacks up exactly one queued chain refresh.

There is NO additional FE time-based throttling. The backend
debouncer already coalesces the rapid-write case (100ms quiet / 1s
max), so a FE timing gate would add latency without value and could
actually block a legitimate fresh event arriving 200ms after a manual
refresh.

## Tests

- `events_test.go`:
  - `TestBroadcaster_SingleFiresAfterQuiet` — one Publish lands one
    event, after minQuiet, before maxWait.
  - `TestBroadcaster_BurstCoalesces` — 10 publishes in the quiet
    window fan out exactly once.
  - `TestBroadcaster_SustainedHitsMaxWait` — publishes faster than
    minQuiet, sustained past maxWait, fire on the maxWait cadence
    (≥ 2 events over 3 maxWait windows; the first lands at the
    maxWait boundary, not earlier).
  - `TestBroadcaster_NoPublishMeansNoFire` — defensive: no auto-tick
    without a Publish call.
  - `TestBroadcaster_CancelRemovesSubscriber` — `cancel()` from
    `Subscribe()` stops further delivery.
  - `TestBroadcaster_ConcurrentPublishesSafe` — 32 goroutines × 50
    Publishes each; no panic, ≥1 fan-out.
  - `TestPublishOnSuccessfulWrite` — (method × status) matrix pins
    the mux-level publish wrapper: POST/DELETE/PATCH 2xx fire;
    GET/HEAD don't; 4xx/5xx don't.
  - `TestPublishOnSuccessfulWrite_PreservesFlusher` — GET bypasses
    the wrap so `http.Flusher` survives for the SSE handler.

- `sse_test.go`:
  - `TestSSE_DeliversBroadcastEvent` — end-to-end: connect, Publish,
    receive `event: snapshot` on the wire.
  - `TestSSE_AuthRequired` — unauthenticated connect is 403.
  - `TestSSE_DebouncedFanout` — 20 Publishes in the quiet window
    deliver as one SSE event to the client.

## Files

- `pkg/claude/agentd/events.go` — NEW: broadcaster + wrap middleware.
- `pkg/claude/agentd/sse.go` — NEW: `/api/events` handler.
- `pkg/claude/agentd/events_test.go` — NEW: broadcaster + middleware
  tests.
- `pkg/claude/agentd/sse_test.go` — NEW: end-to-end SSE tests.
- `pkg/claude/agentd/dashboard.go` — `/api/events` route registration.
- `pkg/claude/agentd/popup.go` — wraps the popup mux with
  `publishOnSuccessfulWrite`.
- `pkg/claude/agentd/serve.go` — wraps the daemon mux with
  `publishOnSuccessfulWrite`.
- `pkg/claude/agentd/fsnotify.go` — `reindex` calls `Publish()`.
- `pkg/claude/agentd/cron.go` — cron tick calls `Publish()` when
  ≥1 job fired.
- `pkg/claude/agentd/reaper.go` — reaper tick calls `Publish()` when
  ≥1 session reaped.
- `pkg/claude/agentd/dashboard/js/refresh.js` — `refresh()` reentrancy
  guard + `startEventStream`.
- `pkg/claude/agentd/dashboard/js/dashboard.js` — `startEventStream()`
  on load; `setInterval(refresh, 2000)` retained as safety net.

## Follow-ups (separate PRs)

- Promote upstream pollers (tmux `has-session`, gh PR state, usage)
  to background goroutines that publish on delta. Would let us drop
  the safety-net poll cadence further. The current 2s polling is
  fine; this is purely an efficiency play.
- Per-tab event-type subscription (`event: groups` vs `event: messages`)
  if and when fan-out load becomes measurable. v1 ships ONE event
  type — "something changed, refetch."
- Server-side `If-None-Match` / ETag on `/api/snapshot` so a 2s poll
  arriving in a quiescent window returns 304 instead of rebuilding
  the snapshot.
