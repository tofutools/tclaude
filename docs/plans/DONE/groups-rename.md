# `tclaude agent groups rename <old> <new>`

Shipped 2026-05.

## CLI

```
tclaude agent groups rename <old> <new> [--ask-human <duration>]
```

Tab-completion suggests existing group names for `<old>`. Same-name
rename is a no-op (200 OK + audit row) so the human can safely
re-run a script.

## Daemon orchestration

`POST /v1/groups/{old}/rename` with body `{"new_name": "<new>"}`.
Single transaction on `agent_groups`:

1. Validate `<new>` against `validateGroupName`: non-empty, no
   slashes/backslashes, no control characters, no leading/trailing
   whitespace. Reject 400 on bad input.
2. Resolve source via the existing `/v1/groups/{name}` dispatcher
   prefix → 404 if the group doesn't exist.
3. `db.RenameAgentGroup` opens a transaction:
   - Resolves source by name (404 surfaces as `(nil, nil)`).
   - Pre-checks the new name for collision → 409 if taken.
   - `UPDATE agent_groups SET name = ? WHERE id = ?`.
   - Inserts an `agent_group_audit` row recording
     `(group_id, old_name, new_name, by_conv, at)`.
   - Commits.

**Schema is integer-FK throughout** (`agent_group_members.group_id`,
`agent_group_owners.group_id`, `agent_messages.group_id`,
`agent_cron_jobs.group_id`), so renaming is a single-row UPDATE — no
cascades required. The `group:` selector prefix in `agent_messages`
is parsed at request time, never stored, so it's also unaffected.

Auth: `groups.rename` slug, default human-only. Same dispatcher as
the other `groups.*` verbs.

## Schema bump (v20)

```sql
CREATE TABLE agent_group_audit (
  id        INTEGER PRIMARY KEY AUTOINCREMENT,
  group_id  INTEGER NOT NULL REFERENCES agent_groups(id) ON DELETE CASCADE,
  old_name  TEXT NOT NULL,
  new_name  TEXT NOT NULL,
  by_conv   TEXT NOT NULL DEFAULT '',
  at        TEXT NOT NULL
);
CREATE INDEX idx_agent_group_audit_group ON agent_group_audit(group_id, at);
```

`db.ListAgentGroupRenames(groupID)` returns the rename history. Not
surfaced via CLI yet; cheap to expose later if `groups ls --history`
becomes useful.

## Test coverage

`pkg/claude/agentd/groups_rename_flow_test.go`:

- `TestGroupsRename_BasicMembersSurvive` — members + owners + audit row
  all attached after rename via stable id
- `TestGroupsRename_NameCollisionIsConflict` — 409, no mutations
- `TestGroupsRename_RejectsInvalidNames` — 400 on empty / slashes /
  backslashes / leading-or-trailing whitespace / control chars
- `TestGroupsRename_SameNameIsNoop` — 200, audit row still recorded
- `TestGroupsRename_MissingSourceIs404` — dispatcher 404
- `TestGroupsRename_PreservesArchivedState` — `archived_at` column
  survives the rename

## Files

- `pkg/claude/common/db/migrate.go` — `migrateV19toV20` adds the audit table
- `pkg/claude/common/db/agent.go` — `RenameAgentGroup`,
  `ListAgentGroupRenames`, `AgentGroupAudit` type, `ErrGroupNameTaken`
- `pkg/claude/agentd/identity.go` — `PermGroupsRename` slug
- `pkg/claude/agentd/handlers.go` — dispatcher branch for `/rename`
- `pkg/claude/agentd/groups_rename.go` — handler + `validateGroupName`
- `pkg/claude/agent/groups.go` — `groupsRenameCmd` / `runGroupsRename`
- `pkg/claude/agentd/groups_rename_flow_test.go` — 6 flow tests

## Out of scope (deferred)

- **Reserved old names** (history of "this group used to be called X")
  — the audit table makes this debuggable; `groups ls --history` would
  be a future surface if needed.
- **Auto-redirect old name in CLI selectors** — would require a soft
  redirect via the audit table. Defer.
- **Dashboard inline rename** — calls the same daemon endpoint; ship
  with the framework migration sketched in
  `dashboard-group-membership-ux.md`.
- **Strike the cross-reference in `web-dashboard.md`** — that file is
  reserved for another agent right now per the parallel-work handoff,
  so the "Rename buttons (agents + groups)" bullet stays as-is. Whoever
  next touches `web-dashboard.md` should mark the daemon side shipped.
