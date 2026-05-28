# workflows: agentd HTTP API + snapshot — SHIPPED

Step 3 of the **Workflows** feature (`docs/plans/workflows.md`). The
dashboard-facing CRUD + manual-driving surface for workflow instances, plus the
pure successor-advance/branch-skip helper that Step 6's engine will reuse.
Builds on Step 1 (`pkg/claude/workflow`) and Step 2 (`pkg/claude/common/db`).

## What shipped

### Pure advance helper — `pkg/claude/workflow/advance.go`
The shared "what happens when a node settles" brain (no DB, fully unit-tested in
`advance_test.go`):

- `Advance(t *Template, settledID, outcome string, state map[string]NodeRunState) AdvanceResult`
  → `AdvanceResult{Ready, Skipped []string}`. Follows the taken edges (label ==
  outcome; unlabeled edges are the `pass` path) to ready successors, **respecting
  joins** (single-pred / `JoinAny` fire immediately; `JoinAll` waits until every
  other predecessor has settled). Skips are **reachability-based**: any pending
  node no longer forward-reachable from a live node (live ∪ freshly-readied) is
  dead — so loop-backs and still-fed joins are never wrongly skipped, and a whole
  abandoned sub-branch is skipped transitively. Single-step (documented: a settle
  that both supplies a join's last arrival and orphans another of its
  predecessors in the same step needs the next settle to converge; Step 6's
  engine will iterate to a fixpoint).
- `NodeRunState` (`NodePending` / `NodeLive` / `NodeSettled`) — the db-agnostic
  state Advance reasons over; agentd maps storage statuses onto it.
- `RebuildFromSnapshot(mermaid, nodes)` — reconstructs the topology-relevant
  Template from an instance snapshot (re-parses the stored chart, rehydrates node
  defs) so a running instance never re-reads its source template from disk.
- `(*Template) AllowedOutcomes(nodeID)` and `(*Template) FailHalts(nodeID)` —
  helpers for outcome validation and instance-status recompute, reused by Steps
  5/6.

This file is additive — `template.go` was left untouched (graph-analysis #228
owns it); rebased onto `agent-workflows` after #228/#229 merged.

### agentd HTTP API — `pkg/claude/agentd/dashboard_workflows.go`
`registerDashboardWorkflowsRoutes(mux)`, wired from `registerDashboardEditRoutes`
(`dashboard_edit.go`). Every handler is cookie-gated (`checkDashboardAuth`) and
responds via `writeJSON`, mirroring `dashboard_cron.go`. Endpoints:

| Method | Path | Behaviour |
|--------|------|-----------|
| POST   | `/api/workflows` | `{template_ref, title, params}` → `workflow.Resolve` + **snapshot** mermaid & node defs into the instance, insert instance + all nodes (entry → `ready`, rest → `pending`), append `instance_created` + `node_ready` events. Validates required params. Returns `{id}`. |
| GET    | `/api/workflows/{id}` | full detail: instance + nodes (with `allowed_outcomes`) + params/vars (raw JSON) + snapshot mermaid + recent events (capped 200). |
| PATCH  | `/api/workflows/{id}/nodes/{nodeId}` | `{status?, outcome?, output?, assignee?}` manual update. On `done`/`failed` → runs `workflow.Advance`, applies ready/skip, recomputes instance status. Stamps started/finished. Validates outcome against the node's allowed set (enum nodes require an explicit outcome). Returns `{ok, status, instance_status, ready, skipped}`. |
| GET    | `/api/workflows/{id}/nodes/{nodeId}/audit` | `{events:[...]}` scoped to that node. |
| POST   | `/api/workflows/{id}/cancel` | skip non-terminal nodes + instance → `cancelled`. |
| DELETE | `/api/workflows/{id}` | delete (cascades nodes+events). |
| POST   | `/api/workflows/{id}/nodes/{nodeId}/start` \| `/attach` | **501** — Step 4 (group integration) owns these. |

Template resolution/discovery dirs come from `workflowProjectDirsFn` (default:
the daemon's cwd-derived `.tclaude/workflows`; overridable in tests via
`SetWorkflowProjectDirsForTest`).

### Snapshot integration — `dashboard.go`
`snapshotPayload` gained `Workflows []dashboardWorkflowInstance` (id, title,
template ref/name, status, node counts total/done/failed/running, group id/name,
timestamps) and `WorkflowTemplates []dashboardWorkflowTemplate` (ref, name,
description, node count, source, err). Both assembled in `handleDashboardSnapshot`
via `collectWorkflowsSnapshot` / `collectWorkflowTemplatesSnapshot`
(`workflow.List`). Data rides the existing 2s `/api/snapshot` poll (no SSE).

## Tests
- `pkg/claude/workflow/advance_test.go` — linear, enum branch + sibling skip,
  JoinAll (waits then fires), JoinAny, loop-back-not-skipped, fail-follows-fail-
  edge, transitive sub-tree skip, AllowedOutcomes, FailHalts, RebuildFromSnapshot.
- `pkg/claude/agentd/dashboard_workflows_flow_test.go` — instantiate example +
  walk to `completed`; diamond (temp project dir) branch→ready + sibling→skipped
  + join fires; enum-requires-outcome 400; missing-param 400; cancel+delete;
  node audit; start/attach 501; snapshot carries instances+templates; auth gate.

## Gates
`go build ./...` · `go test ./...` · `golangci-lint run ./...` — all clean.

## Deviations / cross-step notes
- **Group creation deferred.** POST does not create/link a group (GroupID stays
  0); the snapshot already resolves group name when set. Group linking belongs
  with node start/attach in Step 4.
- **Node Detail JSON** is `encoding/json` of the yaml-tagged `workflow.Node`
  (Go field names), round-tripped by `RebuildFromSnapshot`. If Step 5 wants a
  prettier shape it can remap; the dashboard mainly reads the dedicated columns.
- **Advance is single-step** (see helper notes) — Step 6's engine should iterate
  it to a fixpoint for the rare simultaneous skip-unblocks-join case.
- Snapshot does an N+1 `ListWorkflowNodes` per instance — fine at expected
  instance counts; cache by mtime/count if profiling flags it.
