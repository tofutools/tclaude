# Conv-succession chain (schema v15, 2026-05)

Tracks the relationship between an old conv-id and its successor
when reincarnate (or future succession kinds — clone-replace,
etc.) replaces a conv.

## Schema v15

`agent_conv_succession` table. Columns: `old_conv_id`,
`new_conv_id`, `reason`, `created_at`. The `reason` column
distinguishes future succession kinds.

Recorded every time a conv is replaced (today: reincarnate).

## Cron-job migration

Reincarnate now also eagerly rewrites
`agent_cron_jobs.{owner_conv,target_conv}` from old → new via
`db.MigrateCronJobConvRef`, so jobs keep firing against the live
conv after a reincarnation.

Without this, scheduled jobs would target the dead pre-
reincarnate conv and silently fail (or worse, fire against an
archived conv-id no longer attached to a tmux session).

## Forward-walk lookup

`db.ResolveLatestConv(id)` walks the chain forward from any
historical conv-id to the current live one (cycle-protected at
32 hops).

Available to wire into `ResolveSelector` / other lookup paths
as a follow-up — useful for cases where a human (or an old
agent_messages row) refers to a conv-id that's since been
superseded.
