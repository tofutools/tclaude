# workflows: idea / enhancement backlog

PO-curated ideas for the Workflows feature (see `docs/plans/workflows.md`). Each
notes where it fits and a rough cost. Promote into a step (or a step's brief)
when we decide to do it. Several are uniquely enabled by tclaude's existing
agent/group/inbox/usage machinery — that's where the differentiation is.

## Promoted (no longer just ideas)

- Static graph analysis → its own high-prio step `workflows-graph-analysis.md`.
- Live agent vitals on nodes → folded into Steps 4 (`group-integration`) + 5 (`dashboard-tab`).
- Stuck/SLA escalation + inbox handoffs → folded into Step 6 (`execution-engine`).
- Node approval gates → folded into Step 4 (`group-integration`).
- Sub-workflow nodes + foreach fan-out → `workflows-dynamic-subgraphs.md` (Phase 3).
- Composite nodes → `workflows-composite-nodes.md` (Phase 3).

The rest below remain open ideas.

## Cheap wins — fold into existing steps

- **Static graph analysis in the validator** (extends Step 1, `pkg/claude/workflow`).
  Beyond the current 1:1 chart↔node checks, add: every node **reachable** from an
  entry; every node can **reach a terminal** (no dead pockets); flag **unreachable**
  nodes and **dead-end** non-terminal nodes; enum value with no outgoing edge as a
  *warning* (currently silently allowed). Cheap (pure graph walk), catches real
  authoring bugs before a run. Cost: S.

- **Live agent vitals on AI nodes** (Steps 4+5). tclaude already tracks per-agent
  online/idle/working/**crashed** state, context-meter %, and exit reason. Paint
  that onto the AI node in the mermaid render — not just workflow status but the
  *actual agent's* vitals. This is the headline thing agent-runner can't do: the
  workflow graph becomes a live ops view of your agent fleet. Data is already in
  the snapshot. Cost: S–M.

## Enhancements — bake into the upcoming step briefs

- **Stuck / SLA detection + escalation** (Step 6 engine, reuses cron + human-notify).
  A node "running" or "awaiting" too long — an unactioned human node, an idle or
  crashed assigned agent — escalates: ping the assignee, or notify the human via
  the existing `human.notify` channel. Turns the monitor into an active assistant.
  Especially valuable for the "follow the infra team's checklist" / business-process
  use cases where steps stall on people. Cost: M.

- **Handoffs as inbox messages** (Step 6 engine). When node A completes and hands to
  node B, deliver A's captured output to B's assigned agent as an inbox message — so
  the workflow's data-flow *is* agent-to-agent messaging, fully visible in the
  existing inbox, and "node ready" = the agent gets nudged. Makes the workflow a
  real multi-agent orchestration over tclaude's comms rather than a side system.
  Cost: M.

- **Node approval gates / permission-slugged nodes** (Step 4 + security; gates Step 7).
  A node can require explicit human approval before it runs — gated on the existing
  permission/sudo machinery. Important for `tool`/`program` nodes that execute
  commands, *especially* ones sourced from an external `git:` template (Step 7),
  which is third-party code execution. Cost: M.

- **Audit timeline scrubber + per-instance cost** (Step 5+). The `workflow_events`
  table → a timeline you can scrub to replay the instance (which node when, outcomes,
  captured I/O at each point) for debugging/post-mortem. Plus aggregate token/cost
  per instance from tclaude's existing usage tracking ("this workflow cost X"),
  which informs whether a process is worth automating. Cost: M.

## Future / new steps

- **Sub-workflow nodes.** A node whose executor instantiates *another* workflow and
  waits — composition/reuse (e.g. "apply-for-infra" as a node inside
  "launch-microservice"). agent-runner has linear sub-workflows; our graph version is
  more powerful. Own future step. Cost: L.

- **Parameterized fan-out (foreach).** A node that fans out over a captured list
  (one branch / sub-workflow per item), with a join. Dynamic cardinality in a graph
  is non-trivial; defer. Cost: L.

- **AI workflow authoring.** Describe a process in prose → an agent drafts the
  mermaid + node YAMLs, live-validated against the Step-1 loader and previewed in
  the dashboard. Templates are user data, so lowering the authoring barrier matters.
  Could be a skill + a dashboard "new workflow" flow. Cost: M–L.

- **Replay / clone an instance.** Re-run a finished or failed instance from scratch
  or from a chosen node (reusing the snapshot) — analogous to tclaude's agent clone.
  Cost: S–M.

- **Git-state checkpoints per node.** An instance pins a worktree; nodes checkpoint
  (commit) so a failed node retries from a clean state. Ties workflows to tclaude's
  worktree machinery. Cost: M–L.
