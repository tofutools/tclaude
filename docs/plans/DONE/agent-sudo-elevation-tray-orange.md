# `tclaude agent sudo` — tray-icon orange state (v2 slice 4)

Shipped 2026-05.

The tray icon now flips **orange** when at least one sudo grant is
active anywhere — a passive ambient reminder that an elevation
window is open. The human can spot a forgotten grant from across
the desk without opening the dashboard, and the tooltip surfaces
the count + soonest expiry so the soonest forgotten window is
visible at a glance.

Yellow (pending approval) keeps priority — a blocking popup is more
time-critical than a passive reminder.

## Colour matrix

| State                                | Colour |
|--------------------------------------|--------|
| At least one `--ask-human` popup open | yellow |
| At least one active sudo grant only   | orange |
| Idle                                 | green  |

Tooltip:

- yellow → `tclaude agentd · N pending approval(s)`
- orange → `tclaude agentd · N active sudo grant(s) · soonest expires in <duration>`
- green  → `tclaude agentd`

The `· soonest expires in <dur>` suffix is dropped when the SELECT-
then-format race in `snapshotSudoTrayState` produces an already-
expired window — rare, but the partial index uses a wall-clock
cutoff so the row can slip past `expires_at` between the query and
the format. Better to render the count cleanly than emit "expires
in -1s".

## pickTrayIcon (pure function)

`pkg/claude/agentd/tray.go`:

```go
func pickTrayIcon(green, yellow, orange []byte,
                  pending, sudoActive int,
                  sudoExpiryHint string) ([]byte, string)
```

Pure: no DB / clock / systray references. Composition is testable
without spawning a real systray, exactly what the v1 unit tests
already do — the slice extends the same shape.

`pickTrayIcon` doesn't format `sudoExpiryHint` itself: the caller
formats the duration and passes the rendered string. That keeps the
test free of time math.

## snapshotSudoTrayState

`pkg/claude/agentd/tray.go`:

```go
func snapshotSudoTrayState() (count int, expiryHint string)
```

Polls `db.ListAllActiveSudoGrants()`, walks the rows for the global-
minimum `expires_at`, and renders the rendered hint (or "" when no
grants / on DB error / on the post-cutoff race). Called from the
tray's existing 200ms ticker — same cadence the pending-approval
poll uses, so the icon stays responsive without extra plumbing.

DB errors collapse to `(0, "")` — a transient SQLite hiccup
shouldn't flicker the icon orange when there's nothing to elevate.

## Poller integration

The 200ms ticker already updated on every `pendingCount` change.
Now it also tracks the sudoActive count + expiry hint:

```go
pending := approvals.pendingCount()
sudoActive, hint := snapshotSudoTrayState()
if pending == lastPending && sudoActive == lastSudo && hint == lastHint {
    continue
}
icon, tooltip := pickTrayIcon(greenIcon, yellowIcon, orangeIcon,
                              pending, sudoActive, hint)
```

The hint comparison is necessary so the icon updates when the
soonest-expiry rolls forward (e.g. a grant of 1 hour ticks down
without changing the count).

## Tests (3 new + 3 existing rewritten)

`pkg/claude/agentd/tray_test.go`:

Existing tests kept, signatures updated to the new 6-arg shape:

- `TestPickTrayIcon_GreenWhenIdle` — no pending, no sudo.
- `TestPickTrayIcon_YellowWhenPending` — pending only.
- `TestPickTrayIcon_YellowCountUpdatesWithMultiple` — N pending.

New for slice 4:

- `TestPickTrayIcon_OrangeWhenSudoActive` — pending=0, sudoActive=2,
  expiry hint present. Asserts icon flips orange, count + hint
  appear in tooltip.
- `TestPickTrayIcon_YellowBeatsOrange` — pending=1, sudoActive=3.
  Yellow wins (blocking > passive); tooltip mentions pending NOT
  sudo.
- `TestPickTrayIcon_OrangeWithoutExpiryHint` — pending=0,
  sudoActive=1, hint="". Asserts the icon still flips orange but
  the tooltip omits the expiry suffix cleanly. Pins the
  post-cutoff race fallback.

`snapshotSudoTrayState` is not separately unit-tested — it's a
thin SELECT + min-walk + duration format whose interesting branches
all surface through `pickTrayIcon`. Tested transitively by the
existing sudo flow tests (which exercise the underlying query +
state).

## Files

- `pkg/claude/agentd/tray.go` — `orangeIcon` constant; extended
  `pickTrayIcon` signature; new `snapshotSudoTrayState`; poller
  integration.
- `pkg/claude/agentd/tray_test.go` — 3 new test cases + signature
  rewrites of the 3 existing ones.

## Cross-references

- [`DONE/tray-icon-v1.md`](tray-icon-v1.md) — v1 yellow + green
  shape this slice extends.
- [`DONE/agent-sudo-elevation-v1.md`](agent-sudo-elevation-v1.md) —
  the grant model the new orange state surfaces.
- [`TODO/high-prio/agent-sudo-elevation.md`](../TODO/high-prio/agent-sudo-elevation.md)
  — slice 3 (dashboard panel) still open.
