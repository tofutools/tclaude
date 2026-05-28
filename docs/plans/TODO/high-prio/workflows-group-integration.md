# workflows: agent group integration (start / attach)

Part of the **Workflows** feature — see `docs/plans/workflows.md`. The
tclaude-native angle: a workflow **instance can own a regular tclaude agent
group**, and **AI nodes map to agents** in it. This is how monitoring gets
"start / attach the agent doing this node" for free, and the bridge to Phase 2
auto-execution.

## Open / to build

1. **Instance ↔ group link** — `workflow_instances.group_id` (already in the
   schema). On instantiate, optionally create a group named after the instance
   (e.g. `wf-<id>-<slug>`) via the existing group-creation path. Decide policy:
   always / opt-in / only-if-the-template-has-an-AI-node (lean: opt-in at
   instantiate time, default on when ≥1 AI node).
2. **Node ↔ agent link** — `workflow_nodes.assignee` stores the agent conv id
   driving an AI node.
3. **Start** (`POST /api/workflows/{id}/nodes/{nodeId}/start`) — for a `ready` AI
   node: spawn an agent into the instance's group (reuse the existing
   `groups.spawn`/spawn machinery — do NOT reimplement), seed it with the node's
   interpolated prompt + captured inputs, set `assignee`, move node → `running`,
   append `node_started`. If `assignee` already set, no-op / focus.
4. **Attach** (`POST /api/workflows/{id}/nodes/{nodeId}/attach`) — attach to the
   assignee's tmux session via the existing attach path (out-of-sandbox, the
   daemon does it). Surfaces in the node context menu.
5. The dashboard node context menu (in the dashboard-tab step) calls these.
6. **Live agent vitals** — expose each AI node's assignee so the dashboard (Step 5)
   can overlay that agent's live status (working/idle/crashed, context %). The data
   already rides the snapshot per agent; this step maps node → assignee → vitals.
7. **Node approval gates** — a node may require explicit human approval before it
   runs, gated on the existing permission/sudo machinery. Important for `tool`/
   `program` nodes that execute commands, and essential once Step 7 allows
   `git:`-sourced templates (third-party remote code execution). A gated node sits
   blocked/`awaiting_verify` until approved in the dashboard.

MVP keeps it shallow: link/create the group, allow manual association of an agent
to a node, support attach. Full "spawn the right agent automatically and advance
when it's done" is Phase 2 (`workflows-execution-engine.md`).

## Shipped context

Groups, spawn, and attach already exist and are the machinery to reuse:
- group create / membership: `pkg/claude/common/db` group tables + agentd group
  routes (`registerDashboardGroupRoutes`), `tclaude agent groups …`.
- spawn: `agentd.SpawnSpawner` boundary + `tclaude session new`; dashboard spawn
  modal (`js/modal-spawn.js`) + handlers.
- attach: existing tmux attach path (the daemon nudges/attaches out-of-sandbox).
This step is mostly *wiring* workflow nodes to those, not new infra.

## Relevant source files

- `pkg/claude/agentd/dashboard_workflows.go` — start/attach handlers (api step)
- `pkg/claude/agentd/` — group routes, spawn handlers, attach path to reuse
- `pkg/claude/common/db/workflows.go` — `assignee`, `group_id` updates
- `pkg/claude/agent/` — `tclaude agent` group/spawn client surface

## Open questions

- One agent per AI node, or one shared agent per instance that gets re-prompted
  per node (resume-style)? agent-runner's `session: resume/inherit` suggests a
  shared session can be valuable (fixes happen in the context that wrote code).
  Lean: support both — default one-agent-per-instance reused across its AI nodes,
  with an opt-out for fresh-per-node. Decided properly in Phase 2.
- Group cleanup when an instance completes/cancels (retire agents? keep idle per
  the group/worker policy?). Operator's call — surface, don't auto-retire.
