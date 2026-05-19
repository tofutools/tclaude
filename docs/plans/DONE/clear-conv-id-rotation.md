# /clear conv-id rotation — agent identity follows the rotation (2026-05)

Fixes issue #192: Claude Code's `/clear` rotates a conversation's
conv-id while keeping the same process alive. agentd keys group
memberships / ownerships / permission overrides on conv-id, so a raw
`/clear` stranded the live agent — its identity stayed on the dead old
conv-id, the dashboard showed it offline forever, it could not be
resumed, and the fresh conv showed up as a detached non-agent
conversation.

reincarnate already solved this exact problem (it migrates identity
old → new). The fix routes a raw `/clear` through the same migration.

## Hook sequence (the spike)

Confirmed against a real captured `/clear` hook recording:

```
SessionEnd    <old-conv-id>  reason=clear   — ends the old conv (NOT an exit; process lives)
SessionStart  <new-conv-id>  source=clear   — fresh conv; first hook carrying the new id
```

The same `TCLAUDE_SESSION_ID` / process spans both. The fix does NOT
key on the `source=="clear"` flag — see the trigger below — but the
recording was the spike that confirmed the rotation pattern.

## What shipped

### `db.MigrateAgentIdentity(oldConv, newConv, reason, granter)`

New shared primitive in `pkg/claude/common/db/agent_identity_migration.go`.
A SINGLE SQLite transaction that rekeys every conv-id-keyed identity
row old → new:

- `agent_group_members`, `agent_group_owners`, `agent_permissions`
  (grant AND deny overrides), `agent_cron_jobs` (owner/target refs)
- records the `agent_conv_succession` edge (powers `ResolveLatestConv`,
  so stale references to the old id route forward)
- enrolls `newConv`, drops `oldConv`'s `agent_enrollment` row (no
  offline ghost on the roster)
- carries the display name onto `newConv`'s `agent_enrollment.pending_name`
  (the rescan-immune fallback `agent.FreshTitle` consults), and
  returns it (`AgentIdentityMigration.CarriedName`) so the caller can
  also restore it as a real conversation title

Atomic + idempotent. reincarnate's inline step-3 migration was
replaced with a call to this function (`pkg/claude/agentd/reincarnate.go`).

### Trigger — `needsIdentityMigration`

`pkg/claude/session/hook_callback.go`'s migration trigger is a pure
DB-state predicate:

> oldConv is an active agent AND newConv is not already an agent AND
> no succession edge from oldConv yet.

Checked on **every** hook with a conv-id rotation, not just the
instant `SessionStart(source=clear)`. Three properties:

- **First hook (post-/clear `SessionStart`)** — predicate is true, the
  migration runs.
- **Retry** — `db.MigrateAgentIdentity` is atomic: a failure commits
  nothing, so no succession edge is recorded, so the predicate stays
  true. The NEXT hook (`UserPromptSubmit`, `Notification`, …) re-runs
  it. On success the edge is recorded and the predicate flips false,
  so the migration fires at most once per `/clear`.
- **`/resume` disambiguation** — an env-keyed tclaude session's conv-id
  only ever rotates mid-life via `/clear`. A resume is a fresh
  `tclaude session` with its own `TCLAUDE_SESSION_ID`, so its first
  hook records the conv-id from scratch (`oldConv == ""` — not a
  rotation). The new-conv-not-already-an-agent guard is
  belt-and-braces against a future in-session conversation switch
  landing on a conv that already owns an identity.

On migration failure the hook does NOT advance the session row's
`ConvID` — so the next hook still sees the rotation and the predicate
fires again.

### Title restoration — `/rename` injection + pending_name

`/clear` wipes CC's own conversation title. The fix restores it on
two layers:

1. **`pending_name`** carried onto the new conv by `MigrateAgentIdentity`
   — what the dashboard's `agent.FreshTitle` shows the instant the
   migration commits.
2. **A real `/rename`** injected into the agent's tmux pane by
   `restoreClearedTitle` — two `send-keys` calls (text, then Enter,
   matching the context-nudge pattern). The keystrokes queue in the
   pty and CC runs them once it settles after the `/clear` (the same
   mechanism auto-compact relies on for `/compact` from the Stop
   hook). That writes a real `customTitle` turn into the new conv's
   `.jsonl` — durable across rescans and visible in surfaces that
   don't consult `pending_name` (CC's own UI, `tclaude conv ls`).

The carried name source is the old conv's custom title, else its
spawn-time `pending_name`. The hook also runs a best-effort
`.jsonl` rescan of the old conv before the migration, so a `/rename`
the agent did itself just before the `/clear` (one that hadn't
been re-indexed yet) still surfaces. The injected title is
charset-gated (control chars rejected — a newline in a send-keys
payload would submit the line early and let the remainder inject as
a separate command).

### Reincarnate refactor

reincarnate's pre-spawn snapshot block + inline step-3 migration loop
were replaced with a single `db.MigrateAgentIdentity` call. Pure-rekey
UPDATEs preserve `agent_group_members.joined_at`; permission /
ownership rows continue to re-stamp `granted_by` / `granted_at` to
match the `system:reincarnate[:by=…]` audit convention. The response
`migrated` list shape changed from per-id (`group:3`) to counts
(`group_members:1`) — informational only; CLI just joins it.

## Tests

- `pkg/claude/agentd/clear_conv_rotation_flow_test.go` — flow tests:
  identity migration to the new conv-id (asserting membership /
  ownership / perms / online / carried title / `pending_name` set /
  `/rename` injected / succession edge recorded), resume of the
  pre-`/clear` id targeting the new conv, a message to the pre-`/clear`
  id reaching the agent, and a plain conversation NOT being promoted
  to an agent.
- `pkg/claude/session/hook_callback_test.go` — `TestNeedsIdentityMigration`
  (the trigger / retry predicate, including the "succession edge
  recorded → no re-fire" case that backs the failure-retry path) and
  `TestRunHookCallback_ClearMigratesAgentIdentity` (the full callback
  path).
- `pkg/testharness/` — `CCSim.clear()` models `/clear`: a conv-id
  rotation plus the real `SessionEnd(clear)` / `SessionStart(clear)`
  hook sequence driven through `session.ApplyHook`. `Flow.Clear(label)`
  drives it.

## Out of scope

The bigger refactor — a stable agent-identity key instead of conv-id —
remains parked in `docs/plans/TODO/future/stable-agent-identity-key.md`.
Migrate-on-rotation is the chosen approach.
