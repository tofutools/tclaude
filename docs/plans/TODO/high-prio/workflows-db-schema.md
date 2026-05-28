# workflows: SQLite schema + CRUD

Part of the **Workflows** feature — see `docs/plans/workflows.md`. Persists
workflow **instances** and **per-node state** (templates stay on disk). Designed
so a remote sync backend can be layered on later without reshaping it.

## Open / to build

1. **Migration** `migrateV46toV47` (bump `currentVersion` in
   `pkg/claude/common/db/migrate.go`, currently 46) creating three tables:

```sql
CREATE TABLE IF NOT EXISTS workflow_instances (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  template_ref  TEXT NOT NULL,                    -- "user:foo" | "example:foo" | path
  template_name TEXT NOT NULL,
  title         TEXT NOT NULL DEFAULT '',
  status        TEXT NOT NULL DEFAULT 'running',  -- running|completed|failed|cancelled
  mermaid       TEXT NOT NULL DEFAULT '',         -- snapshot of chart at instantiation
  params        TEXT NOT NULL DEFAULT '{}',       -- JSON
  vars          TEXT NOT NULL DEFAULT '{}',       -- JSON captured vars
  group_id      INTEGER NOT NULL DEFAULT 0,       -- 0 = no linked group
  created_at    TEXT NOT NULL,
  updated_at    TEXT NOT NULL,
  completed_at  TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS workflow_nodes (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  instance_id   INTEGER NOT NULL REFERENCES workflow_instances(id) ON DELETE CASCADE,
  node_id       TEXT NOT NULL,
  label         TEXT NOT NULL DEFAULT '',
  executor_kind TEXT NOT NULL DEFAULT '',
  status        TEXT NOT NULL DEFAULT 'pending',  -- pending|ready|running|awaiting_verify|done|failed|skipped
  outcome       TEXT NOT NULL DEFAULT '',         -- enum value chosen (drives branch)
  detail        TEXT NOT NULL DEFAULT '{}',       -- node-def snapshot JSON
  output        TEXT NOT NULL DEFAULT '',         -- captured I/O summary
  assignee      TEXT NOT NULL DEFAULT '',         -- agent conv id / human name
  visits        INTEGER NOT NULL DEFAULT 0,
  started_at    TEXT NOT NULL DEFAULT '',
  finished_at   TEXT NOT NULL DEFAULT '',
  updated_at    TEXT NOT NULL,
  UNIQUE(instance_id, node_id)
);
CREATE INDEX IF NOT EXISTS idx_workflow_nodes_instance ON workflow_nodes(instance_id);

CREATE TABLE IF NOT EXISTS workflow_events (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  instance_id  INTEGER NOT NULL REFERENCES workflow_instances(id) ON DELETE CASCADE,
  node_id      TEXT NOT NULL DEFAULT '',
  kind         TEXT NOT NULL,                     -- instance_created|node_ready|node_started|node_done|node_failed|node_skipped|...
  message      TEXT NOT NULL DEFAULT '',
  at           TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_workflow_events_instance ON workflow_events(instance_id);
```

2. **CRUD** in `pkg/claude/common/db/workflows.go` mirroring `agent_cron.go`
   (every fn calls `Open()`; RFC3339 UTC timestamps; `scan*` helpers):
   - instances: `InsertWorkflowInstance`, `GetWorkflowInstance`,
     `ListWorkflowInstances`, `UpdateWorkflowInstanceStatus`,
     `UpdateWorkflowInstanceVars`, `DeleteWorkflowInstance`.
   - nodes: `InsertWorkflowNode` (bulk insert at instantiation),
     `ListWorkflowNodes(instanceID)`, `GetWorkflowNode(instanceID, nodeID)`,
     `UpdateWorkflowNode` (partial-update via pointer-in-struct patch, like
     `UpdateAgentCronJobFields`).
   - events: `AppendWorkflowEvent`, `ListWorkflowEvents(instanceID[, nodeID])`.
3. **Tests** next to the code: insert→get→list→update→delete round-trips;
   `ON DELETE CASCADE` removes nodes + events with the instance; node partial
   update touches only set fields.

## Shipped context

DB is `~/.tclaude/db.sqlite`, WAL, `foreign_keys(1)` on (so CASCADE works),
pure-Go `modernc.org/sqlite`. Singleton via `db.Open()`. Migration chain in
`migrate.go` is create-if-not-exists per version with `UPDATE schema_version SET
version = N`. Closest existing analogues: `agent_cron_jobs` (template→runs, state
tracking) and `group_templates` (v42: parent table + child rows + index).

## Relevant source files

- `pkg/claude/common/db/migrate.go` — `currentVersion`, add `migrateV46toV47`
- `pkg/claude/common/db/agent_cron.go` — CRUD pattern to mirror (incl. partial update)
- `pkg/claude/common/db/db.go` — `Open()` singleton, DSN/pragmas
- NEW: `pkg/claude/common/db/workflows.go` (+ `workflows_test.go`)

## Open questions

- Store parsed edges in a table, or re-parse the snapshotted `mermaid` when
  advancing? Lean: re-parse (mermaid snapshot is the source of truth; no edge
  table to keep in sync). Phase 2 decides.
- Keep `done` instances forever or prune? Add retention later; keep for now.
