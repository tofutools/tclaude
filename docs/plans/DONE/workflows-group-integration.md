# workflows: agent group integration (start / attach / approve) — SHIPPED

Step 4 of the **Workflows** feature (see `docs/plans/workflows.md`). A workflow
instance binds to a regular tclaude agent group, and AI nodes map to agents in
it: the dashboard can spawn or attach the agent doing a node, and a human can
gate a node through an approval. All backend, in the dashboard cookie-auth
`/api/workflows/...` surface Step 3 established — **no `/v1/` socket API** (the
original brief's `/v1/` + `workflows_api.go` + `advanceInstance` contract was a
stale-plan fiction; reality is `dashboard_workflows.go` + the pure
`workflow.Advance`).

Shipped in **PR #<TBD>** (branch `workflows-group-integration`, squash-merged
into `agent-workflows`).

## What shipped

### Instance ↔ group binding
- `POST /api/workflows` accepts an optional `group` (the group **name**),
  resolved via `db.GetAgentGroupByName` → stored as `workflow_instances.group_id`
  (the soft link already in schema **v48**). Unknown name → 400 (no auto-create);
  omitted → unbound (`group_id` 0). Create response echoes `group_id`.
- The snapshot already resolves `group_name` from `group_id`
  (`collectWorkflowsSnapshot`, Step 3) — unchanged.

### start / attach (replaced Step 3's 501 stubs)
- `POST /api/workflows/{id}/nodes/{nodeId}/start` — for a `ready` **ai** node,
  spawns a fresh agent into the instance's bound group via the shared
  `executeSpawn` core (the same path `/v1/groups/{name}/spawn` and the
  group-template instantiator use — not reimplemented). The node's snapshotted
  executor supplies the agent name/role hint (`executor.Agent`) and the task
  prompt (`executor.Prompt` → the agent's inbox briefing). Node → `running`,
  `assignee` = spawned conv-id; appends `node_started`. Returns conv-id + label +
  `attach_cmd`.
- `POST .../nodes/{nodeId}/attach` — assigns an **existing** member of the bound
  group (`{conv_id}`) to a `ready` ai node and delivers the node task to that
  member's inbox (best-effort, gated on `isValidInitialMessage`). No new process.
  Node → `running`, `assignee` = conv-id.
- Guards (shared `reloadReadyAINode` + `boundGroup`): instance running; node
  exists; node is ai-executor (400 otherwise); node is `ready` (409 if pending/
  running/terminal); instance has a bound group (400 otherwise); attachee is a
  current group member (400 otherwise).

### Human-approval gate
- `POST .../nodes/{nodeId}/approve` body `{decision, note?}`, `decision` =
  `approve` | `reject`. Only for nodes whose `verify.kind` is `human` (400
  otherwise). The node must be `running`/`awaiting_verify` (409 if not started or
  already settled).
  - **approve** → settle the node `done` on `OutcomePass` (a human-verified
    node's success continuation is its unlabeled edge), then advance via the
    SAME `workflow.Advance` + `applyWorkflowAdvance` + `recomputeWorkflowInstanceStatus`
    the manual settle uses (no duplicated frontier logic). Audit: `node_approved`.
  - **reject** → append `node_rejected` (with note), no status change, no advance
    — the node stays for re-work.

### Wire fields Step 5 needs (cross-dep from PO msg #54)
- `workflowNodeJSON` now carries `agent` (`executor.Agent`, the intended-agent
  hint Step 5 overlays vitals on) and `verify_kind` (drives the approve
  affordance).
- Template topology warnings (Step 2b) are exposed: `workflow.ListEntry.Warnings`
  → `dashboardWorkflowTemplate.warnings` (templates snapshot), and the
  `GET /api/workflows/{id}` detail payload gains `warnings[]`, recovered off the
  rebuilt snapshot via the new exported `workflow.Template.Analyze()`
  (`RebuildFromSnapshot` deliberately skips analysis).

### Cold-review #230 hardening folded in
- **Per-instance mutex** (`workflowInstanceLocks`, keyed by instance id):
  PATCH / start / attach / approve / cancel / **delete** all serialise their
  read-modify-write so two concurrent drives (double "start", start racing a
  settle, delete mid-spawn) can't act on stale node state or break mutual
  exclusion. Handlers re-read instance+node fresh under the lock.
- **Manual PATCH restricted to running/done/failed** (`isManualDriveStatus`): a
  direct hop to `skipped` would settle a node WITHOUT running Advance and strand
  the sub-tree behind it; pending/ready/awaiting_verify are engine-internal.
  Skipping a branch is reached by cancelling the instance.

## Endpoints (Step 4 additions / changes)

| Method | Path | Effect |
|--------|------|--------|
| POST   | `/api/workflows` | now accepts `group` (name) → binds `group_id` |
| POST   | `/api/workflows/{id}/nodes/{nodeId}/start`   | spawn ai agent into bound group |
| POST   | `/api/workflows/{id}/nodes/{nodeId}/attach`  | assign existing member + deliver task |
| POST   | `/api/workflows/{id}/nodes/{nodeId}/approve` | human-verify gate (approve/reject) |

## Files

- `pkg/claude/agentd/dashboard_workflows.go` — all of the above.
- `pkg/claude/agentd/dashboard_workflows_flow_test.go` — flow tests (below).
- `pkg/claude/common/db/workflows.go` — `WorkflowEventNodeApproved` /
  `WorkflowEventNodeRejected` event-kind constants. **No migration** — `group_id`
  was already in v48; schema version unchanged at **48**.
- `pkg/claude/workflow/discover.go` — `ListEntry.Warnings` (populated from the
  loaded template).
- `pkg/claude/workflow/analyze.go` — exported `Template.Analyze()`.

## Test scenarios (`dashboard_workflows_flow_test.go`)

`GroupBindingOnCreate` (bound/unknown-400/unbound + snapshot group_name),
`StartSpawnsAgentIntoGroup` (+ membership), `StartGuards` (unbound-400, non-ai-400,
not-ready-409, double-start-409), `AttachExistingMember` (empty-400, non-member-400,
member-200 + inbox delivery), `HumanApproveAdvancesFrontier`,
`HumanRejectRecordsNoAdvance` (+ audit), `ApproveGuards` (not-running-409,
non-human-400, bad-decision-400), `ManualSkipRejected`,
`NodeJSONAgentVerifyAndWarnings`, `Warnings` (snapshot + detail).

## Deferred (out of Step 4)

- Auto-driving / auto-spawn-and-advance — Step 6 execution engine
  (`workflows-execution-engine.md`). `start`/`attach` here are manual triggers;
  the engine that decides *when* to start nodes is separate. `workflow.Advance`
  stays pure + single-step.
- One-shared-agent-per-instance (resume-style) vs one-agent-per-node — start
  always spawns fresh; reuse-across-nodes is a Phase 2 call.
- Group cleanup on instance completion/cancel — operator's call per the
  group/worker policy; not auto-retired.
- Live-vitals overlay rendering — Step 5 (front-end) consumes the `agent` /
  `verify_kind` / `warnings` fields exposed here.
- `workflowInstanceLocks` entries are freed on instance delete; a terminal-but-
  kept instance retains a pointer-sized entry until deleted (bounded by
  instances-created, negligible) — accepted, documented in code.
