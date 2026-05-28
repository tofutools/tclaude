# workflows: SQLite schema + CRUD — SHIPPED

Part of the **Workflows** feature — see `docs/plans/workflows.md`. This step
(Step 2) landed the SQLite persistence layer: workflow **instances** and
**per-node state** live in the DB; templates stay on disk (parsed mermaid).
Designed so a remote sync backend can be layered on later without reshaping it.

## What shipped

**Schema version: 46 → 47** (`migrateV46toV47` in
`pkg/claude/common/db/migrate.go`; `currentVersion` bumped to 47).

Three new tables, all created CREATE-IF-NOT-EXISTS in one migration:

- `workflow_instances` — one row per instantiation. `template_ref`,
  `template_name`, `title`, `status` (running|completed|failed|cancelled),
  `mermaid` snapshot, `params` JSON, `vars` JSON, `group_id` (soft link to a
  tclaude agent group, 0 = none — NOT a foreign key, so a finished instance
  keeps its history if the group is later deleted), `created_at`, `updated_at`,
  `completed_at`.
- `workflow_nodes` — one row per node per instance. Synthetic `id` PK +
  `UNIQUE(instance_id, node_id)` so the engine addresses a node by its mermaid
  id. `label`, `executor_kind`, `status`
  (pending|ready|running|awaiting_verify|done|failed|skipped), `outcome`,
  `detail` (node-def snapshot JSON), `output`, `assignee`, `visits` (loop
  re-entry count), `started_at`, `finished_at`, `updated_at`.
  `instance_id REFERENCES workflow_instances(id) ON DELETE CASCADE` +
  `idx_workflow_nodes_instance`.
- `workflow_events` — append-only audit/timeline. `instance_id`, `node_id`
  ('' for instance-level), `kind`, `message`, `at`. Same
  `ON DELETE CASCADE` + `idx_workflow_events_instance`.

CASCADE fires because the DB DSN enables `foreign_keys(1)` per connection.

## CRUD — `pkg/claude/common/db/workflows.go`

Mirrors `agent_cron.go` conventions: every fn calls `Open()`; RFC3339 UTC
timestamps stamped server-side; `scan*` helpers; partial-update via a
pointer-in-struct patch (like `UpdateAgentCronJobFields`). `params` / `vars` /
`detail` are stored as opaque JSON TEXT (`'{}'` when blank) — the DB layer is
deliberately ignorant of their shape; the engine (Step 6) owns marshalling.

Status / node-status / event-kind values are exported constants
(`WorkflowStatus*`, `WorkflowNodeStatus*`, `WorkflowEvent*`).

- **instances**: `InsertWorkflowInstance`, `GetWorkflowInstance` (nil if
  missing), `ListWorkflowInstances` (id asc), `UpdateWorkflowInstanceStatus`
  (stamps `completed_at` on a terminal status, clears it on a return to
  running), `UpdateWorkflowInstanceVars`, `DeleteWorkflowInstance` (idempotent;
  cascades).
- **nodes**: `InsertWorkflowNode` (single), `InsertWorkflowNodes` (bulk,
  transactional — the instantiation path; overrides each node's InstanceID and
  rolls the whole batch back on any failure), `ListWorkflowNodes(instanceID)`
  (id asc), `GetWorkflowNode(instanceID, nodeID)` (nil if missing),
  `UpdateWorkflowNode(instanceID, nodeID, WorkflowNodePatch)` (partial update
  keyed by the mermaid identity; only non-nil fields written; `updated_at`
  bumped only when something changes; empty patch = 0-row no-op).
- **events**: `AppendWorkflowEvent` (stamps `at` when zero, honours an explicit
  one), `ListWorkflowEvents(instanceID[, nodeID])` (oldest first; optional
  variadic nodeID filter for the per-node "open audit data" action).

## Tests

- `pkg/claude/common/db/workflows_test.go` — instance insert→get→list→
  update(status/vars)→delete round-trips; completed_at stamp/clear on status
  transitions; node single + bulk insert (incl. transactional rollback on a
  dup), UNIQUE(instance_id, node_id) violation, get-by-identity, list ordering;
  partial node update touches only set fields + bumps updated_at; zero-time
  pointer clears a timestamp; empty/missing patch is a 0-row no-op; CASCADE
  removes nodes + events with the instance (through the real `Open()` DSN);
  event append/list/node-filter + explicit-`at` preservation.
- `pkg/claude/common/db/migrate_v47_test.go` — bare-v46 → v47 migration (table
  creation, defaults, UNIQUE, raw CASCADE) and a fresh-schema end-to-end check.
  Carries the `currentVersion == 47` pin (the tripwire moved forward from the
  v46 test); the next migration's author moves it into a v48 test.

## Quality gates

`go build ./...` clean · `go test ./...` clean · `golangci-lint run ./...` →
0 issues.

## Out of scope (other steps)

agentd HTTP API (Step 3), group integration (Step 4), dashboard tab (Step 5),
the engine (Step 6). The CRUD surface is intentionally dumb so those steps
consume it without reshaping.

## Carried-forward open questions

- Store parsed edges in a table, or re-parse the snapshotted `mermaid` when
  advancing? Lean: re-parse (mermaid snapshot is the source of truth; no edge
  table to keep in sync). Phase 2 / Step 6 decides.
- Keep `done` instances forever or prune? Add retention later; keep for now.
