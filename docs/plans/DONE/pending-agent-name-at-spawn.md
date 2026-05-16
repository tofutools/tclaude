# Pending agent name at spawn — SHIPPED

## Problem

When the daemon spawns a new worker (`tclaude agent spawn <group> --name
<name>`), the dashboard showed `(unknown)` for the first few seconds —
until the agent's conversation title materialised. The `--name` value is
known at spawn time, so the dashboard could show the intended name
immediately instead.

## Investigation — the agent-naming flow

The name a listing surface (dashboard, `agent groups members`, `agent
ls`) shows for an agent is resolved by `agent.FreshTitle(convID)`:

```text
FreshTitle → FreshConvRowResolved → RefreshConvIndexEntry
           → conv_index row → displayTitle(row)
           → CustomTitle || Summary || FirstPrompt   (else "(unknown)")
```

`conv_index` is a **cache rebuilt from the conversation `.jsonl`**.
`RefreshConvIndexEntry` re-scans the file whenever its mtime/size
changed; `ScanAndUpsertFile → parseJSONLSession` reads `CustomTitle`
from a `custom-title` turn, and `UpsertConvIndex` writes the row.

So the displayed name is **derived from the `.jsonl` on every snapshot
refresh** — there is no standalone "agent name" DB column.

Timeline of a spawn (`handleGroupSpawn` + `runSpawnPostInit`):

1. `tclaude session new` launches a fresh CC instance.
2. The daemon polls the `sessions` table until the conv-id appears.
3. `AddAgentGroupMember` writes the group-member row (and, via
   `EnrollAgent`, the `agent_enrollment` row).
4. A background goroutine (`runSpawnPostInit`) waits for the pane to
   come online, then injects `/rename <name>` followed by a welcome
   `[system: …]` line.
5. CC processes `/rename`, writing a `custom-title` turn to the
   `.jsonl`. The next `conv_index` rescan picks it up.

The **gap** is steps 3→5: the member/enrollment rows exist, but the
`.jsonl` has no `custom-title` turn yet, so `FreshTitle` returns
`(unknown)` (or, once the welcome lands, the raw welcome line as a
first-prompt). That is the window the dashboard showed `(unknown)`.

## The open question — would a pending-name write race / auto-revert?

**Writing the name to `conv_index.custom_title` WOULD race.** That table
is rebuilt from the `.jsonl` on every mtime change. The very next turn
the spawned agent takes (the welcome line alone is enough) bumps the
file mtime, triggers a rescan, and `parseJSONLSession` finds no
`custom-title` turn → `UpsertConvIndex` writes `CustomTitle = ""`,
clobbering the pending name back to blank. Confirmed auto-revert.

**Writing it to a column the `.jsonl` scan never touches is stable.**
The scan only ever writes `conv_index`. `agent_enrollment` is written
by `EnrollAgent` (`INSERT OR IGNORE` — never updates an existing row),
`PromoteAgent`/`RetireAgent`/`ReinstateAgent` (retire fields only), and
the v30→v31 ghost cleanup. None recompute a name. A pending name parked
on `agent_enrollment` is therefore **stable until something explicitly
overwrites it** — and nothing does.

Conclusion: no genuine race, **provided the pending name lives outside
`conv_index`**. Approach judged clean and low-risk → implemented in the
same PR (per the task brief's conditional-implementation clause).

## What shipped

A pending name recorded at spawn time, used as a display fallback.

### Schema — migration v36→v37

`agent_enrollment` gains `pending_name TEXT NOT NULL DEFAULT ''`. Chosen
home because the row is 1:1 with the agent conv-id, already created at
spawn time (`AddAgentGroupMember → EnrollAgent`), never touched by the
`.jsonl` scan, and already cascade-deleted with the conversation.
Existing rows backfill to `''` (long-since-named agents — their title
resolves from `conv_index` as before).

### Write path — `handleGroupSpawn`

Right after `AddAgentGroupMember`, the handler calls
`db.SetEnrollmentPendingName(convID, body.Name)` (synchronous, before
the response returns). Best-effort: a failed write only costs the
`(unknown)` window — the pre-feature behaviour — so it is logged, not
bubbled. The name is stored even when it is not a valid `/rename` title
(the `/rename` injection is then skipped) so the dashboard can still
show the intended name.

### Read path — `agent.FreshTitle`

Resolution priority is now:

```text
custom title  >  pending name  >  summary  >  first prompt  >  "(unknown)"
```

- A **custom title** (the agent's own `/rename`, or the daemon's
  post-spawn injection) is the authoritative name and always wins.
- With no custom title yet, the **pending name** stands in. It
  deliberately outranks summary / first-prompt: the human-given
  `--name` is a stronger identity signal than an auto-generated summary
  or an uncleaned first prompt (often the spawn welcome line).

The pending-name lookup (`db.GetEnrollment`) runs **only** when there is
no custom title — i.e. for not-yet-renamed agents. Renamed agents (the
steady state) pay zero extra queries.

### Supersession & lifecycle

- **Self-rename supersedes cleanly.** Once the agent's `/rename` lands,
  `conv_index.custom_title` is set; `FreshTitle` returns it from the
  first branch and never consults `pending_name` again. No flicker.
- **The pending name is never cleared.** It is a pure fallback — once
  outranked it is inert. Leaving it avoids a write-in-the-read-path and
  is harmless (a few bytes per enrollment row).
- **Agent never renames itself** → the pending name persists as the
  display name indefinitely. That is the intended `--name`, strictly
  better than `(unknown)`.

## Files

- `pkg/claude/common/db/migrate.go` — `migrateV36toV37`, `currentVersion` → 37.
- `pkg/claude/common/db/agent_enrollment.go` — `AgentEnrollment.PendingName`
  field, `pending_name` in both SELECTs + `scanEnrollment`, new
  `SetEnrollmentPendingName`.
- `pkg/claude/agentd/lifecycle.go` — `handleGroupSpawn` records the
  pending name after the membership add.
- `pkg/claude/agent/lookup.go` — `FreshTitle` priority rewrite + the
  `pendingName` helper.
- `pkg/claude/agentd/spawn_pending_name_flow_test.go` — flow test.

## Test — `TestSpawn_PendingNameShownThenSupersededBySelfRename`

Spawns `alpha --name pending-reviewer`, then blocks the spawn's own
`/rename` injection with an hour-long `CCSim` command delay so the
conversation provably has no custom title for the whole window. Asserts:

- the group-members view **and** the `/api/snapshot` dashboard payload
  show `pending-reviewer` (not `(unknown)`, not the summary/welcome
  line) while no `/rename` has landed;
- after the agent self-renames to a **distinct** name (`real-reviewer`),
  the members view shows `real-reviewer` — the custom title supersedes
  the pending name.

## Risk assessment

Low. The change is additive: one nullable column (defaulted, backfills
cleanly), one new best-effort write, one localised read-path priority
change. The pending name lives outside the `.jsonl`-derived cache, so it
cannot race or auto-revert. The only behavioural judgement call is the
`pending name > summary > first-prompt` ordering, which is correct for
agent identity (a human-given `--name` beats an auto-summary).

## Out of scope / future

- **Clone / reincarnate** spawn via separate paths (`clone.go`,
  `reincarnate.go`) and derive their names from the parent. They have
  the same brief `(unknown)` window; the same `pending_name` mechanism
  could extend to them if it shows up as a real annoyance.
- The pending name is **not** surfaced by `/v1/whoami` (a freshly
  spawned agent asking `tclaude agent whoami` before its `/rename`
  lands still sees `(unnamed)`). The dashboard was the reported pain
  point; whoami can adopt the same fallback later if wanted.
