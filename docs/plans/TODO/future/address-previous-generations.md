# Address an agent's previous generation as itself (succession opt-out)

Deferred / parking-lot. Recorded so we don't forget. The use case is "ping
the previous generation of an agent for what it knew."

## Context — what already exists

After `/clear` or `tclaude agent reincarnate`, an agent's conv-id rotates.
`db.MigrateAgentIdentity` carries the agent's identity (memberships,
ownership, permissions, cron refs) onto the new conv-id and records a
**succession edge** in `agent_conv_succession` (`old → new`). The old conv is
**retired** — its enrollment row is retire-stamped (`retired_at`,
`retired_by`, `retire_reason`) so it shows up in the dashboard's "Retired"
virtual group and can be dragged back into a real group to reinstate. See
`docs/plans/DONE/clear-conv-id-rotation.md` for the full mechanism.

The succession edge is what keeps existing addresses alive across the
rotation: `tclaude agent message <old-conv-id>` walks
`db.ResolveLatestConv()` and routes forward to the new conv. That is exactly
the right default — most callers don't track conv-id rotations and want their
message to reach "the agent."

## The gap

If you *do* want to reach the old generation as itself — to ask the previous
incarnation what it knew before the context was wiped — there is no way to.
Every addressing path runs through `ResolveLatestConv`. Reinstating the old
conv (dragging it out of the Retired tray) only clears `retired_at`; the
succession edge persists, so `agent message <old-id>` still routes to the
new conv. You can see the old generation, reinstate it, and resume it as a
live CC process (a separate session running on its `.jsonl`), but you can't
*address* it.

## Sketch

Add an addressing form that bypasses succession resolution. Two natural
shapes:

- **A flag**: `tclaude agent message <id> --no-follow` (and `agent reply
  ... --no-follow`, `agent send`). Resolves exactly the given conv-id, no
  `ResolveLatestConv` walk.
- **A namespace prefix**: `tclaude agent message exact:<id>` for the same
  intent, more visible at the call site.

The flag form is lighter; the namespace form is louder. Probably the flag,
with a clear error when it would target a retired-but-non-resumable conv.

The CLI verbs map to agentd endpoints (`POST /v1/agent/messages` and
friends) — those need a matching `follow_succession=false` query param /
body field so the daemon doesn't walk the chain server-side.

## Issues / open questions

1. **Data survives both rotations — verified 2026-05.** Reincarnate `/exit`s
   the old pane cleanly, leaving the `.jsonl` intact. `/clear` is CC's own
   behaviour and *also* preserves the old `.jsonl` (confirmed empirically by
   the human). So the previous generation is fully reachable + resumable
   regardless of which rotation produced it — no audit-only edge case.
2. **Reply routing.** A reply to a message that came from the old
   generation — does it follow succession (reach the live thing) or address
   back to the old generation exactly? Probably follow (the live agent is
   the one you're conversing with); but a `--no-follow` reply gives the
   precise option. Default-follow seems right.
3. **Dashboard symmetry.** The dashboard's "send message" modal needs an
   "address exactly this conv" toggle so the same option exists there.
4. **Discoverability.** When does the human / an agent learn this flag
   exists? Probably surface it on the dashboard's Retired tray
   ("`tclaude agent message --no-follow <id>` to ping this generation").

## Relevant source

- `pkg/claude/common/db/agent_succession.go` — `ResolveLatestConv`,
  `RecordConvSuccession`.
- `pkg/claude/agent/lookup.go` — addressing / conv selector resolution.
- `pkg/claude/agentd/handlers.go` — message endpoints.
- `pkg/claude/agentd/reincarnate.go`, `pkg/claude/session/hook_callback.go`
  — the two conv-id rotations that populate the succession table.
- `docs/plans/DONE/clear-conv-id-rotation.md` — the rotation/retirement
  mechanism this builds on.
