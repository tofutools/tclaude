# `tclaude agent sudo` — periodic cleanup (housekeeping)

Shipped 2026-05.

The `agent_sudo_grants` table grows monotonically: every grant
ever issued lives forever after it expires (the active-grants
probe filters by `expires_at > now`, so correctness was never at
stake). Long-running daemons accumulated rows the audit trail
didn't really need beyond ~30d.

This slice wires `db.PurgeExpiredSudoGrants` (already shipped in
v1) into a daemon-internal periodic sweep, mirroring the existing
`startCronScheduler` shape.

## Sweep

`pkg/claude/agentd/cron.go`:

```go
const sudoGrantsCleanupInterval = 1 * time.Hour
var sudoGrantsRetention = 30 * 24 * time.Hour

func startSudoGrantsCleanup(stop <-chan struct{})
func runSudoGrantsCleanup(now time.Time)
```

- Hourly tick. Correctness doesn't depend on prompt purging, so
  hourly keeps log noise low and avoids burning CPU on an empty
  table.
- 30-day retention. Recent forensic context ("what did agent X do
  yesterday?") stays queryable; ancient rows get purged.
- First sweep fires immediately on daemon startup so a restart
  doesn't have to wait the full hour. Subsequent ones are timer-
  driven.
- Active grants are NOT touched. PurgeExpiredSudoGrants filters by
  `expires_at < cutoff`, so an in-window grant survives even if
  the row is years old (which it can't be — duration cap is 1h —
  but the rule is robust either way).

`runSudoGrantsCleanup` is split out from `startSudoGrantsCleanup`
so unit tests can call it directly with a synthetic `now`.

## Wiring

`pkg/claude/agentd/serve.go` — `startSudoGrantsCleanup(cronStop)`
runs alongside `startCronScheduler`. Shares the daemon's quit
channel; both shut down together.

## Tests

`pkg/claude/agentd/sudo_cleanup_test.go`:

- `TestSudoGrantsCleanup_PurgesAgedExpiredRows` — three rows:
  expired-90d-ago (purged), expired-1h-ago (kept, inside the 30d
  retention), active (kept, in-window). Pins both ends of the
  `expires_at < cutoff` rule.
- `TestSudoGrantsCleanup_QuietWhenNothingToPurge` — active row,
  cleanup runs, row survives. No-op path correctness.

Both call `runSudoGrantsCleanup` directly (synthetic `now`)
instead of kicking the timer-driven goroutine — keeps the test
deterministic.

## Files

- `pkg/claude/agentd/cron.go` — `sudoGrantsCleanupInterval`,
  `sudoGrantsRetention`, `startSudoGrantsCleanup`,
  `runSudoGrantsCleanup`.
- `pkg/claude/agentd/serve.go` — `startSudoGrantsCleanup(cronStop)`
  call.
- `pkg/claude/agentd/sudo_cleanup_test.go` — 2 unit tests.

## Cross-references

- [`agent-sudo-elevation-v1.md`](agent-sudo-elevation-v1.md) — v1
  shipped `PurgeExpiredSudoGrants`; this slice wires the call.
