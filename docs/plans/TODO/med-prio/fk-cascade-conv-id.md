# FK + ON DELETE CASCADE from every `conv_id` column → `conv_index.conv_id`

## Why this is on file

`conv_index` is already the system-of-record for "a conv exists." Every
other table that mentions a conv-id is logically a child of that row.
Today nothing in SQLite enforces that relationship: rows in
`agent_group_members`, `agent_permissions`, `agent_sudo_grants`,
`agent_cron_jobs`, `agent_conv_succession`, `agent_clone_history`,
`agent_head_aliases`, `conv_embeddings`, `agent_messages`, and `sessions`
can all be inserted for a `conv_id` that has never been indexed, and
they survive (as orphans) if the index row is deleted out from under
them.

The current cleanup story is `db.DeleteAgentByConvID`
(`pkg/claude/common/db/agent.go:561`), a hand-maintained transaction
that DELETEs from every child table in sequence. It works, but every
new conv-id-referencing table is a chance to forget to add it to that
function — and there's no DB-level safety net.

The user wants the schema to enforce this: real FOREIGN KEY
constraints, ON DELETE CASCADE, with `conv_index.conv_id` as the
parent. Once that ships, `DeleteAgentByConvID` collapses to "DELETE FROM
conv_index WHERE conv_id = ?" and the DB handles the rest.

PRAGMA `foreign_keys` is already on (`pkg/claude/common/db/db.go:48`),
so the enforcement layer is in place — only the constraints are
missing.

## Tables that need the FK

All point at `conv_index(conv_id)` with `ON DELETE CASCADE`.

| Table                       | Column(s)                                  | Notes                                                                                          |
|-----------------------------|--------------------------------------------|------------------------------------------------------------------------------------------------|
| `sessions`                  | `conv_id`                                  | Empty string allowed today (DEFAULT ''); FK must permit "" or migration must fill it.          |
| `agent_group_members`       | `conv_id`                                  | PRIMARY KEY (group_id, conv_id). Straightforward CASCADE.                                       |
| `agent_group_owners`        | `conv_id`                                  | Straightforward CASCADE.                                                                       |
| `agent_permissions`         | `conv_id`                                  | Straightforward CASCADE.                                                                       |
| `agent_sudo_grants`         | `conv_id`                                  | Straightforward CASCADE.                                                                       |
| `agent_head_aliases`        | `anchor_conv_id`                           | Aliases pointing at a deleted anchor should go.                                                |
| `agent_cron_jobs`           | `owner_conv`, `target_conv`                | Two FKs on one row. Owner deletion drops their jobs; target deletion drops jobs aimed at them. |
| `agent_conv_succession`     | `old_conv_id`, `new_conv_id`               | Audit-ish, but the dashboard treats it as live routing. CASCADE both sides.                    |
| `agent_clone_history`       | `source_conv_id`                           | Audit table. CASCADE acceptable (the user's whole point is "reincarnate the schema").          |
| `agent_messages`            | `from_conv`, `to_conv`, `original_to_conv` | `to_recipients` + `cc_recipients` are JSON arrays — left alone (not relational).               |
| `conv_embeddings`           | `conv_id`                                  | PRIMARY KEY (conv_id, chunk_index). CASCADE drops all chunks at once.                          |

Tables that DON'T get the FK (intentional):

- `agent_group_audit.by_conv` — empty for human-driven renames. Allowing
  the by_conv to outlive the conv that wrote the audit row is the
  correct audit-table behavior. Keep as plain TEXT.
- `agent_messages.to_recipients` / `cc_recipients` — JSON arrays of
  conv-ids; not relational in the schema sense. Stale entries are
  cosmetic.

## Spawn-time stub-row ordering

The FK changes the spawn-time invariant: `conv_index` row MUST exist
**before** any session, group-member, permission, cron job, etc. is
inserted for that conv-id. Today the order is loose — multiple paths
write the session row (or auto-register a hook) before any indexer
walks the jsonl.

Concrete spawn ordering (post-FK):

1. `tclaude agent spawn` / `tclaude session new`
2. Daemon (or CLI) inserts a **stub row** into `conv_index` with the
   conv-id, project_dir, full_path, and indexed_at = now. Other fields
   (`first_prompt`, `summary`, `message_count`, etc.) stay empty — the
   walker fills them on the next indexing pass.
3. Only AFTER the stub commits: insert `sessions`, group_member, any
   permissions the spawn carries, etc.

Hot spots to audit for ordering:

- `pkg/claude/session/new.go` — the spawn entry point.
- `pkg/claude/agentd/spawn.go` (or wherever the daemon writes
  SessionRow) — must stub conv_index first.
- The PreToolUse / auto-register hook callback path (`sessions` row
  inserted from hooks) — needs to either stub conv_index in the same
  transaction or be guaranteed to fire after the spawn path's stub.
- Tests: `pkg/testharness/simSpawner` already writes the SessionRow the
  production hook would have written — it needs to write the conv_index
  stub too, or invoke a shared helper that does both.

A single helper `EnsureConvIndexStub(convID, projectDir, fullPath)` —
idempotent, runs inside any transaction — keeps the call sites honest.

## Migration approach

SQLite does NOT support `ALTER TABLE … ADD CONSTRAINT`. To add an FK
to an existing table, the standard recipe is:

```
PRAGMA foreign_keys = OFF;            -- per-migration; restored at end
BEGIN;
  CREATE TABLE <name>_new ( … , FOREIGN KEY (col) REFERENCES conv_index(conv_id) ON DELETE CASCADE );
  INSERT INTO <name>_new SELECT … FROM <name>;
  DROP TABLE <name>;
  ALTER TABLE <name>_new RENAME TO <name>;
  -- recreate every index that existed on <name>
COMMIT;
PRAGMA foreign_keys = ON;
```

The wrinkle is **dangling rows**: any existing row whose `conv_id`
points at a non-existent `conv_index` row will fail the `INSERT INTO
… SELECT` after FKs come back on. Two options:

1. **Pre-clean.** Before the per-table rebuild, run a one-shot pass
   that deletes orphans. `DELETE FROM agent_group_members WHERE conv_id
   NOT IN (SELECT conv_id FROM conv_index);` etc. for every child
   table. Safe because orphans are by definition unreachable through
   the union delete path that already exists.
2. **Backfill stubs.** For every child table, INSERT OR IGNORE a
   minimal `conv_index` row (conv_id only, other columns left at
   defaults) for any conv-id that appears as a child but not as a
   parent. Slightly more forgiving — preserves the child rows for
   forensic inspection — at the cost of polluting `conv_index` with
   stubs whose jsonl files may not exist.

Recommend **option 1 (pre-clean)** — matches the existing
`DeleteAgentByConvID` mental model and avoids `conv_index` rows whose
`full_path` points at nothing.

Migration steps for the actual `migrateVNtoVN+1`:

1. PRAGMA foreign_keys = OFF.
2. Run pre-clean DELETEs for every child table (each filtered by `conv_id
   NOT IN (SELECT conv_id FROM conv_index)`, with the appropriate column
   name per table).
3. For each child table in the list above, run the rebuild recipe.
   Keep every existing index; some tables have composite PRIMARY KEY
   declarations that need to be reproduced exactly.
4. After all rebuilds: `PRAGMA foreign_key_check` — any returned rows
   indicate a missed orphan and should fail the migration.
5. PRAGMA foreign_keys = ON.
6. UPDATE schema_version.

Watch out for:

- `sessions.conv_id DEFAULT ''`: the FK will reject `''` because `''`
  isn't a `conv_index.conv_id`. Either change the column to allow
  NULL (and rewrite reads), or drop the default and require callers
  to supply a real conv-id (preferred, but couples the migration to
  the stub-row-first ordering above being already in place).
- `agent_conv_succession`: `old_conv_id` is PRIMARY KEY. Reproduce
  exactly when rebuilding. `new_conv_id` is indexed — recreate the
  index.
- `agent_messages.original_to_conv` has `DEFAULT ''`; the FK must
  permit the empty string OR migration must skip the FK on that
  column. (Empty string means "no rewrite," which is the common case.)
  Easiest: leave `original_to_conv` without an FK; treat it like
  `to_recipients` (cosmetic).
- `conv_embeddings` has a composite PRIMARY KEY (conv_id,
  chunk_index). Must be reproduced. CASCADE will delete all chunks at
  once when the parent goes, which is what we want.

## Cleanup that becomes possible after this lands

`db.DeleteAgentByConvID` in `pkg/claude/common/db/agent.go:561`
collapses to:

```go
func DeleteAgentByConvID(d ExecCtx, convID string) (DeleteAgentSummary, error) {
    if convID == "" { return DeleteAgentSummary{}, errors.New("conv_id required") }
    res, err := d.Exec(`DELETE FROM conv_index WHERE conv_id = ?`, convID)
    // summary derives from ExecResult.RowsAffected, etc.
}
```

The hand-maintained list of child tables in that function — and the
risk of forgetting one when a new conv-id-referencing table ships —
goes away. The unit test for `DeleteAgentByConvID` should grow a check
that asserts CASCADE actually fired for each child table.

Sites that delegate to the union delete (and therefore inherit the
simplification automatically):

- Daemon `handleAgentDelete`
- Dashboard `DELETE /api/agents/{conv}`
- `tclaude conv rm` (via `conv.DeleteConvByID`)

## Open questions

- **Should `agent_clone_history` and `agent_conv_succession` really
  CASCADE, or stay as immutable audit?** The user's framing is
  "reincarnate the schema" — i.e., when a conv is gone, every trace
  goes. That's the simplest mental model and matches what
  `DeleteAgentByConvID` already does today, so the plan above goes
  with CASCADE. If audit-retention rules ever appear, those tables
  can be lifted out as a follow-up.

- **`sessions.conv_id = ''` rows in the wild?** Need a count from a
  real DB before the migration ships. If there are any, decide
  whether to delete them or leave the FK off `sessions` (less ideal —
  the whole point is symmetry).

- **What about FK on `agent_groups.id` from `agent_messages.group_id`
  / `agent_group_members.group_id` / `agent_group_owners.group_id` /
  `agent_group_audit.group_id`?** Those already exist (mix of
  RESTRICT and CASCADE). Out of scope for this plan but worth a
  consistency pass at the same time — currently `agent_messages.group_id`
  is RESTRICT while the others CASCADE, which means deleting a group
  with messages errors out. Probably wanted; flag it for a separate
  decision.

## Source files to touch (rough)

- `pkg/claude/common/db/migrate.go` — new `migrateV24toV25` (or
  whatever's next) implementing the pre-clean + rebuild.
- `pkg/claude/common/db/agent.go` — simplify `DeleteAgentByConvID`
  post-FK.
- `pkg/claude/common/db/convindex.go` — add `EnsureConvIndexStub`
  helper.
- `pkg/claude/session/new.go` + daemon spawn path — call
  `EnsureConvIndexStub` before any child insert.
- `pkg/testharness/sim_spawner.go` (whatever the simSpawner file is
  called) — same.
- Hook callbacks that auto-register a session (search for
  `InsertSession` outside the spawn path).
- Tests in `pkg/claude/common/db/agent_test.go` — assert CASCADE fires.

## Not part of this work

- Doing the spawn-time stub-row ordering refactor as a separate first
  step is fine. The FK migration must come AFTER ordering is in place,
  not before — otherwise the migration will pass on a fresh DB but
  immediately fail at runtime on the first spawn.
