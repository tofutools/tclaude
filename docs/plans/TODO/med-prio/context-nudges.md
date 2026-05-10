# Context nudges (opt-in)

Periodic "consider reincarnating" nudges to a long-running agent as
its context fills (30%, 40%, …, 90%). Goal: surface the choice into
the agent's workflow *before* it runs out of room, without forcing it
to reincarnate mid-task. Already-reincarnating agents and agents that
just reincarnated should not get pinged.

> **Pairs with**
> [`per-group-reincarnate-thresholds.md`](per-group-reincarnate-thresholds.md)
> — that file extends this one with **per-group UI-configurable
> thresholds** and **optional auto-reincarnate**. Ship this simpler
> global-threshold version first; the per-group / UI / auto-trigger
> evolution layers on top.

## Configuration (opt-in)

Per-agent (or default) in `~/.tclaude/config.json`, probably under an
`agent.context_nudge` block. Three knobs:

- `enabled` (bool) — default off so the nudge doesn't surprise
  anyone running daemon for the first time.
- `min_pct` (int) — first threshold to fire at (default 30 or 50;
  decide once we've actually felt the cadence).
- `interval_pct` (int) — step between subsequent nudges (default
  10). Combined with `min_pct` this defines 30, 40, 50, … 90 as the
  nudge points.

## Transport — explore back-channel first

Regular agent messages work but pollute the receiver's inbox +
`inbox ls` view, which is the wrong shape for an ambient "tap on the
shoulder". Things to explore before falling back to messages:

- **CC hooks.** `tclaude setup` already wires a hook into
  `~/.claude/settings.json`. Could the daemon emit a side-channel
  signal (a hidden file the hook polls, a Unix socket the hook reads
  on its next invocation, …) that the hook converts into a
  transient `[system: …]` line without writing anything to
  `agent_messages`? Advantage: the receiver only sees the nudge in
  their transcript, never in inbox surfaces; the daemon never has
  to poke tmux directly for these background pings. Challenge:
  hooks fire on PostToolUse/Notification, not on a timer — we'd
  need an existing hook firing soon enough after the threshold
  crosses.
- **Skill-side pull.** Have the agent's status-bar skill consult
  the daemon for "should I be nudged?" on every statusline update.
  The skill prints the nudge inline rather than the daemon pushing
  it. Bypasses the hook timing issue entirely; still respects opt-in.
- **tmux send-keys with a sentinel marker.** Same delivery path as
  agent_messages but with a marker that the receiver's `inbox ls`
  filters out. Cheapest to ship; downside is the marker leaks into
  the transcript and pollutes scrollback.

## Fallback

If none of the back-channel options shake out cleanly, ship via the
existing message path with a distinguishing subject (e.g.
`__context_nudge`) so receivers can filter.

## Avoid double-firing

Whatever transport: record per-conv the highest threshold last fired
so a brief context fluctuation around a boundary doesn't ping twice.

## Files
- `pkg/claude/common/config/config.go` — config schema
- `pkg/claude/agentd/identity.go` — context_pct accessible per conv
- `~/.claude/settings.json` (hook target)
