# agentd: size-based log rotation for ~/.tclaude/output.log

Shipped 2026-05.

`~/.tclaude/output.log` grew unbounded. agentd now rotates it: the
active file is capped at a configured size; when it is exceeded the
file is renamed aside and a fresh one is opened; a bounded number of
rotated files is kept and the oldest dropped.

## Writer model — investigated first (one process or many?)

Many processes write the log. `pkg/common/logging.go`'s `logFileHandler`
is the *only* writer location — `os.OpenFile(logPath, O_APPEND|O_CREATE|
O_WRONLY)` — and it is reached by `common.SetupLogging`, called both in
`main.go` and in cobra's `PersistentPreRun`. So **every** `tclaude`
process (the long-lived `agentd` daemon plus every transient CLI
invocation) appends to the same file.

`O_APPEND` makes concurrent appends atomic per write, so the human's
call was to **not** add cross-process locking — rotation is best-effort:

- Writers just open + append. No cross-process lock protocol; the
  `pkg/common` file-locking util is *not* used here.
- Only agentd rotates. A transient CLI process that holds an open fd
  across a rotation may land its last few lines in the just-rotated
  file instead of the fresh one — fine for a log, not worth a lock.

The crux: agentd opens the log fd **once at startup and never closes
it**. A plain `mv output.log output.log.1` does not redirect that fd —
on POSIX (and Windows) the fd follows the inode — so the daemon would
keep writing into the rotated-away file forever. Rotation therefore
**renames the file AND reopens a fresh one in-process**, swapping the
writer's fd under an in-process mutex (agentd's logger writes from many
goroutines, so the swap must be race-free). That in-process mutex is the
only lock; cross-process locking stays de-scoped.

## Atomic-rotation mechanism

`common.RotatingWriter` (`pkg/common/logrotate.go`) is the `io.Writer`
slog writes through. It wraps the active `*os.File` behind a mutex.

`rotate()` (mutex held for the whole sequence, so a record write never
splits across it):

1. Cascade — drop `output.log.<keep>`, then shift `.(keep-1)…1` up by
   one slot (`.i` → `.i+1`), oldest-first so nothing is clobbered.
2. Sweep — remove any `output.log.N` for `N > keep`, so lowering `keep`
   between runs does not leak the now-excess rotated files.
3. `os.Rename(output.log, output.log.1)`.
4. Reopen a fresh `output.log` and swap the fd; close the old one.

Every rename is **within `~/.tclaude/`** — same directory, same
filesystem — so each `os.Rename` is atomic on POSIX and replace-existing
on Windows. If the reopen fails the old fd is left in place (the daemon
keeps logging) and the active file is rolled back to its path so the
next tick retries. Cascade-rename failures are collected and returned
but do not abort the rotation. (Known edge, documented in `rotate()`: a
*persistently* unwritable log dir re-runs the cascade each retry tick,
slowly shifting history out — accepted as a broken-host case.)

`MaybeRotate()` is the size policy: one `os.Stat`, rotate if over. It
also reopens the file if it vanished out from under the daemon. A
pre-existing oversized log (the first run of this feature on a
long-lived install) is rotated by the same path — agentd fires an
immediate first check at startup, then ticks every 30s
(`logRotationInterval`, a dedicated ticker alongside the cron scheduler
/ session reaper / usage poller, sharing the daemon-wide stop channel).

No method in `logrotate.go` calls `slog` — `RotatingWriter` *is* the
slog sink, so logging while the mutex is held would deadlock. Errors
bubble to agentd's ticker (`rotateLogOnce`), which logs them after the
lock is released.

## Config — `log_rotation` block in ~/.tclaude/config.json

```json
"log_rotation": { "max_size": "10MiB", "keep": 5 }
```

- `max_size` — active-log cap, a human-friendly size string parsed by
  `common.ParseSize`. Empty/absent → default **10 MiB**. An explicit
  `"0"` is a valid zero size and **disables** rotation.
- `keep` — rotated files to retain (`output.log.1 … .keep`). `<= 0` →
  default **5**.

A config file lacking the block behaves on defaults — `Config.
ResolvedLogRotation()` is nil-safe end to end. A nested struct (not two
flat keys) leaves room for a future time/date-based mode without
reshaping config.json; `rotate()` is already decoupled from the
size trigger so a `MaybeRotateByAge` sibling could drive it.

`Config.Validate()` rejects an unparseable `max_size` and a negative
`keep` (for the dashboard's visual config editor); `Load()` stays
lenient and falls back to defaults.

`common.ParseSize` was extended to accept the IEC `i` infix (`KiB`,
`MiB`, `GiB`, `TiB`) so the conventional spelling parses — tclaude's
units are already binary, so `"10m"` and `"10MiB"` are equal.

## lumberjack vs in-repo

`gopkg.in/natefinch/lumberjack` was considered and rejected — tclaude
leans low-dependency, and the in-repo `RotatingWriter` is ~230 lines
fully under our control. Crucially, lumberjack's `Write` triggers
rotation inline; tclaude needs rotation driven by a daemon ticker and
must support the **fd-swap / reopen** semantics for the long-lived
agentd fd — straightforward to own directly.

## Files

- `pkg/common/logrotate.go` — `RotatingWriter`, `OpenRotatingWriter`,
  the cascade + atomic-rename + fd-swap logic.
- `pkg/common/logging.go` — `SetupLogging` now logs through a
  `RotatingWriter` via `fileWriter()`, which opens the log once and
  reuses it across the two startup calls (main.go + cobra
  PersistentPreRun) — one fd for the process, no leak — and exposes it
  through `ActiveLogRotator()` for agentd.
- `pkg/common/common.go` — `ParseSize` regex accepts the IEC `i` infix.
- `pkg/claude/common/config/config.go` — `LogRotationConfig`, the
  `Config.LogRotation` field, `ResolvedLogRotation()`, `Validate` rules.
- `pkg/claude/agentd/logrotate.go` — `startLogRotation` (resolve config,
  configure the writer, run the rotation ticker); wired into
  `serve.go`'s daemon goroutine startup.

## Tests

- `pkg/common/logrotate_test.go` — rotates past max size; no-rotate
  under it; cascade order; oldest dropped past keep-count; rotated files
  are in-dir siblings; pre-existing oversized file; writes reach the
  fresh fd after rotation; `max_size 0` disables; vanished-file reopen;
  `keep 0` discard; concurrent writes + rotation (`-race`); path stable;
  **reopen-failure rollback** (injected open failure — old fd stays
  usable, active file rolled back); **orphan sweep** when `keep` is
  lowered; `SetupLogging` reuses one rotator across startup calls.
- `pkg/common/common_test.go` — `ParseSize` IEC `i`-infix cases + new
  invalid cases.
- `pkg/claude/common/config/config_test.go` — `ResolvedLogRotation`
  defaults / explicit values / `"0"` disable / bad-value fallback;
  `Validate` accept + reject cases.
- `pkg/claude/agentd/logrotate_test.go` — `startLogRotation` rotates an
  oversized log at startup and keeps rotating on the ticker; `max_size
  0` leaves the writer unconfigured; a nil writer is safe.

No `*_flow_test.go` was added: log rotation touches neither tmux nor the
spawner nor the daemon HTTP mux — the surfaces the flow harness models.
The rotation engine is exercised directly in `pkg/common`, and the
daemon glue (`startLogRotation` + its ticker) in the agentd test above.
