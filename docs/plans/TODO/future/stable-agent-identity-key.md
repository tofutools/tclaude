# Stable agent-identity key — decouple agent identity from conv-id

Deferred / parking-lot. Recorded so we don't forget it; revisit if
conv-id-rotation bugs recur.

## The problem

agentd keys agent identity entirely on **conv-id**: group memberships,
ownership, permissions, cron jobs, messages, succession — all
`WHERE conv_id = …` (`db/agent*.go`). But conv-id is **not stable**:
Claude Code rotates it on `/clear`, and a fresh conv-id is minted on
reincarnate / clone / spawn.

So every conv-id rotation needs an explicit "migrate identity old→new"
step:

- **reincarnate** already does this migration (its step 3 — see the
  header comment in `agentd/reincarnate.go`).
- a raw **`/clear`** did *not* — that was bug #192, fixed by
  migrate-on-rotation in the SessionStart hook (see that fix's DONE doc
  once shipped).

The current model is therefore "conv-id is the identity key, and every
rotation path must be individually caught and migrated." Fragile by
construction: any new conv-rotation path — or a single missed hook —
silently orphans an agent's identity (live agent under the new
conv-id, group/perms/ownership stranded on the old one).

## The alternative

Give each agent a **stable identity key that is not the conv-id**.
Conv-id becomes a *mutable attribute* of the agent row, not its
primary key.

Then:

- group memberships / ownership / permissions / cron / messages key on
  the stable agent id.
- a conv-id rotation is a **one-field UPDATE on the agent row** — no
  fan-out migration across N tables, no way to miss one.
- dashboard / resume always resolve agent → current conv-id through
  the agent row.

Candidate stable keys:

- **`TCLAUDE_SESSION_ID`** — minted once at `session/new.go:214`,
  lives in the tmux pane env, survives `/clear` (the CC process never
  restarts). Human-meaningful and already exists. Most likely choice.
- **A dedicated agent UUID** — cleaner if session ids can ever be
  reused / collide; costs a new column.
- **CC process PID** — corroborating *live* signal only. Unsuitable as
  the *persistent* key: PIDs are reused, and a PID does not survive a
  resume / reboot. Useful as a runtime cross-check, not as identity.

## Why it is deferred — blast radius

Every agentd table with a `conv_id` column would need re-keying to the
stable id, plus a schema migration of existing rows, plus rewriting
every `WHERE conv_id` query and every API surface that accepts a
conv-id. Touches `db/agent*.go`, `agentd/handlers.go`,
reincarnate/clone, the dashboard, and the `tclaude agent` CLI's
conv-id-prefix addressing.

Migrate-on-rotation (what reincarnate does, and what the #192 fix adds
for `/clear`) is consistent, far smaller, and good enough until a
rotation path is missed often enough to justify this refactor.

## Relevant source

- `pkg/claude/common/db/agent.go` — every `conv_id`-keyed table + func.
- `pkg/claude/agentd/reincarnate.go` — the existing old→new migration.
- `pkg/claude/session/new.go:214` — where `TCLAUDE_SESSION_ID` is minted.
- `pkg/claude/session/hook_callback.go` — the session row is *already*
  keyed on `TCLAUDE_SESSION_ID`, with conv-id as a mutable field; that
  layer is the model the agentd layer would adopt.

## Open questions

- `TCLAUDE_SESSION_ID` vs a dedicated agent UUID for the persistent key.
- Backward-compat: existing groups reference conv-ids; a one-time
  migration maps each to its agent row.
- The `tclaude agent` CLI addresses agents by conv-id prefix / title —
  it would gain a stable-id addressing form.
