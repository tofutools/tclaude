# Context nudges (opt-in)

Shipped 2026-05.

Opt-in "consider reincarnating" nudge that fires as a long-running
agent's context fills. The agent sees a transient `[system: …]` line
in its pane, picked up on the next turn — no inbox surface
pollution, no forced action.

The next-level evolution (per-group thresholds, UI configuration,
auto-trigger) lives in
[`per-group-reincarnate-thresholds`](../TODO/med-prio/per-group-reincarnate-thresholds.md).

## What shipped

### Config

`~/.tclaude/config.json` gained an `agent.context_nudge` block:

```jsonc
{
  "agent": {
    "context_nudge": {
      "enabled": true,             // off by default
      "min_pct": 30,               // first threshold; default 30
      "interval_pct": 10           // step; default 10
    }
  }
}
```

Threshold ladder: `min_pct`, `min_pct + interval_pct`, … capped at 90.
Defaults give 30, 40, 50, 60, 70, 80, 90 — six taps before the
agent's window is critically full.

`(*ContextNudgeConfig).Resolved()` returns `(enabled, min, step)`
with sensible fallbacks so callers don't repeat the "0 → default"
dance.

### DB

New `sessions.nudged_pct REAL NOT NULL DEFAULT 0` column (schema v24).
Tracks the highest threshold the daemon has already fired for that
session. `db.GetNudgedPct(sessionID)` + `db.SetNudgedPct(sessionID,
pct)` are the read/write helpers.

`ResetCompact` now also zeros `nudged_pct` — a post-compact session
starts fresh and gets re-nudged on its next climb.

### Stop-hook path

`handleContextNudge` runs alongside `handleAutoCompact` after every
Stop hook. The order matters: `handleAutoCompact` runs first so its
`compact_pending` flag suppresses the nudge that would otherwise tell
an about-to-compact agent to reincarnate.

Decision flow:

1. `TCLAUDE_SESSION_ID` is set; otherwise skip.
2. `agent.context_nudge.enabled` is true; otherwise skip.
3. `compact_pending > 0` → skip (auto-compact wins).
4. `nextNudgeTarget(context_pct, min, step)` returns a non-zero
   target.
5. Target > stored `nudged_pct`; otherwise the threshold already
   fired, skip.
6. Resolve current tmux session. If absent, still stamp
   `nudged_pct` so a later run doesn't re-send the same threshold
   for the same climb.
7. `tmux send-keys` the bracketed-paste message + `Enter`.
8. Stamp `nudged_pct = target`.

### Message shape

```
[system: context at 50%. Consider /reincarnate at the next breakpoint
to avoid running out of room mid-task — fresh CC inherits identity
but starts with a clean window.]
```

`[system: …]` prefix matches the convention every other agent-message
nudge uses. The agent's pane scrollback shows the nudge as user-typed
text but the framing makes intent clear.

### Pure helpers

- `nextNudgeTarget(pct, minPct, intervalPct) int` — pure ladder
  math, exhaustively unit-tested.
- `formatContextNudgeMessage(target) string` — message templating,
  pinned to include the `%` and `/reincarnate`.

## Files

- `pkg/claude/common/config/config.go`:
  - `AgentConfig.ContextNudge *ContextNudgeConfig`
  - `ContextNudgeConfig{Enabled, MinPct, IntervalPct}`
  - `Resolved() (bool, int, int)` accessor
- `pkg/claude/common/db/migrate.go`:
  - `currentVersion = 24`
  - `migrateV23toV24` — `ALTER TABLE sessions ADD COLUMN nudged_pct`
- `pkg/claude/common/db/sessions.go`:
  - `GetNudgedPct(id)`, `SetNudgedPct(id, pct)`
  - `ResetCompact` also zeros `nudged_pct`
- `pkg/claude/common/db/db_test.go`:
  - `TestNudgedPct` — default 0, stamp, ResetCompact zeroes
- `pkg/claude/session/hook_callback.go`:
  - `nextNudgeTarget`, `formatContextNudgeMessage` (pure)
  - `handleContextNudge` (full Stop-hook integration)
  - Stop-hook path now calls both `handleAutoCompact` (first) and
    `handleContextNudge` (second).
- `pkg/claude/session/context_nudge_test.go`:
  - `TestNextNudgeTarget` (13 table-driven cases)
  - `TestFormatContextNudgeMessage`

## Transport decision (why send-keys not inbox)

The TODO floated three transport options: CC hooks, skill-side pull
via the statusbar, tmux send-keys with a sentinel, and an
agent_messages fallback. **Send-keys with a `[system: …]` prefix**
won because:

- Zero inbox pollution. `inbox ls` stays a real-message surface.
- Trivial to implement — same path the cron scheduler and
  auto-compact already use.
- Visible in scrollback so a human grepping the transcript can find
  the nudge.
- No new transport plumbing to maintain.

The downside (transcript pollution) is mild: a six-line nudge total
over a session's life, all clearly marked `[system: …]`.

## Avoiding double-firing

`sessions.nudged_pct` tracks the highest threshold already fired.
The Stop-hook path only sends when the computed target strictly
exceeds the stored value. Context flicker around a boundary (49.5
→ 50.1 → 49.8 → 50.2) only fires the 50 nudge once.

`ResetCompact` resets `nudged_pct` to 0 so a post-compact session
can be nudged again on its next climb. Reincarnation creates a
fresh session row with `nudged_pct = 0` (DEFAULT clause), same
result.

## Cross-references

- [`per-group-reincarnate-thresholds`](../TODO/med-prio/per-group-reincarnate-thresholds.md)
  — the planned next-step layer: per-group thresholds, UI config,
  optional auto-trigger.
- [`hook-driven-agent-actions`](../TODO/med-prio/hook-driven-agent-actions.md)
  — the planned general-case rule engine; context-pct-crossed events
  would be one event source.
