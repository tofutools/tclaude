# workflows: agentd HTTP API

Part of the **Workflows** feature — see `docs/plans/workflows.md`. The
dashboard-facing endpoints. Mirrors the cron handler patterns exactly.

## Open / to build

New file `pkg/claude/agentd/dashboard_workflows.go`:

1. `registerDashboardWorkflowsRoutes(mux)` — call it from
   `registerDashboardEditRoutes` in `dashboard_edit.go`:
   - `mux.HandleFunc("/api/workflows", handleDashboardWorkflowsCreate)`
   - `mux.HandleFunc("/api/workflows/", handleDashboardWorkflowsAPI)`
2. Every handler starts with `if !checkDashboardAuth(w, r) { return }` and
   responds via `writeJSON`. Path-dispatch like `dashboard_cron.go`
   (trim prefix, `SplitN` on `/`, parse id, switch on method + subpath).
3. Endpoints:
   - `POST /api/workflows` `{template_ref, title, params}` → resolve+load
     template (discovery), snapshot mermaid+node defs, insert instance + all
     nodes (entry/no-incoming → `ready`, rest `pending`), optionally create the
     linked group, append `instance_created` event. Returns `{id}`.
   - `GET /api/workflows/{id}` → full detail: instance + nodes + vars + mermaid +
     recent events.
   - `PATCH /api/workflows/{id}/nodes/{nodeId}` `{status?, outcome?, output?,
     assignee?}` → manual node update (MVP driving). On `done`+outcome, mark the
     matching successor(s) `ready` and skip non-taken branches; recompute
     instance status (all terminal → completed/failed). Append events.
   - `POST /api/workflows/{id}/nodes/{nodeId}/start` → spawn/associate the AI
     agent (see group-integration step).
   - `POST /api/workflows/{id}/nodes/{nodeId}/attach` → attach to the node's
     agent tmux session.
   - `GET /api/workflows/{id}/nodes/{nodeId}/audit` → `{events:[...]}` for that node.
   - `POST /api/workflows/{id}/cancel`, `DELETE /api/workflows/{id}`.
4. **Snapshot integration** — add to `snapshotPayload` (in `dashboard.go`):
   - `Workflows []dashboardWorkflowInstance` (id, title, template_name, status,
     counts: total/done/failed/running, group_id/name, timestamps)
   - `WorkflowTemplates []dashboardWorkflowTemplate` (ref, name, description,
     node count, source: project/user/example)
   Build both in `handleDashboardSnapshot` (`out.Workflows = …`). Template
   discovery is cheap (few files); cache by mtime if it shows up in profiling.
5. The successor-advance + skip-branch logic is shared with Phase 2's engine —
   put it in a small reusable helper (`pkg/claude/workflow` advance fn) so the
   manual PATCH path and the future auto-engine use the same code.
6. **Flow tests** in `*_flow_test.go` driving the mux: instantiate → assert nodes
   created with right initial statuses → PATCH a node done with an enum outcome →
   assert correct successor `ready` + sibling `skipped` → finish → instance
   `completed`.

## Shipped context

Route registration: `registerDashboardEditRoutes` (`dashboard_edit.go:50`) is
called from `registerDashboardRoutes` (`dashboard.go:101`). Auth gate
`checkDashboardAuth` (`dashboard.go:285`), JSON helper `writeJSON`
(`handlers.go`). Dispatch exemplar: `dashboard_cron.go:19-136`. Snapshot struct +
assembly: `dashboard.go:312-365` / `handleDashboardSnapshot` (~616+). No SSE in
this branch — data rides the 2s `/api/snapshot` poll.

## Relevant source files

- NEW: `pkg/claude/agentd/dashboard_workflows.go` (+ `*_flow_test.go`)
- `pkg/claude/agentd/dashboard_edit.go` — register routes here
- `pkg/claude/agentd/dashboard_cron.go` — dispatch pattern
- `pkg/claude/agentd/dashboard.go` — `snapshotPayload`, `handleDashboardSnapshot`, `checkDashboardAuth`
- `pkg/claude/common/db/workflows.go` — CRUD (db-schema step)
- `pkg/claude/workflow/` — load/discover/advance (template-format step)

## Open questions

- Should `start`/`attach` live here or in the agent dispatch layer? Reuse the
  existing spawn/attach code paths rather than duplicating.
