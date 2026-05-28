# workflows: composite nodes (multiple tasks + success rules)

Part of the **Workflows** feature — see `docs/plans/workflows.md`. A later step:
a single graph node that bundles **several tasks** and settles by a **success
rule** over their results. Builds on Step 1 (model/parse/validate), Step 2 (DB),
and Step 6 (engine to run the tasks). Operator-flagged as **important**.

## The idea

Today a node has exactly one `executor`. A **composite node** instead has a list
of `tasks` (each its own mini-executor + optional verify) and a `success` rule
deciding the node's outcome from the per-task results. Examples:

- "Run lint **and** test **and** typecheck — **all** must pass."
- "Try 3 approaches — **any** one succeeding is enough."
- "3 reviewers — need **2 of 3** approvals."

## Proposed schema

A node declares EITHER `executor:` (simple, today) XOR `tasks:` (composite):

```yaml
# nodes/checks.yaml
label: Pre-merge checks
tasks:
  - id: lint
    executor: { kind: tool, run: golangci-lint run ./... }
  - id: test
    executor: { kind: tool, run: go test ./... }
  - id: typecheck
    executor: { kind: tool, run: go vet ./... }
success:
  rule: all            # all | any | n_of | weighted
  # n: 2               # for n_of: at least N tasks must pass
  # threshold: 0.6     # for weighted: sum(weight of passing) / sum(weight)
parallel: true         # run tasks concurrently (default); false = sequential
```

- Each task settles pass/fail by its own executor exit / verify (reuse the
  single-node outcome logic per task).
- The composite node's outcome is `pass`/`fail` per the rule (composite nodes are
  pass/fail; enum branching stays a simple-node feature). On fail it follows the
  `|fail|` edge / `on_fail` like any node.
- Per-task `capture` flows into the instance vars (namespaced, e.g.
  `checks.test`).
- Optional per-task `weight` for the `weighted` rule.

## Open / to build

1. **Model + validation** (Step 1 pkg): add `Tasks []Task` + `Success` to `Node`;
   enforce executor-XOR-tasks; `n_of` needs `0 < n <= len(tasks)`; weighted needs
   weights; each task validated like a node executor/verify; unique task ids.
2. **DB** (Step 2 pkg): per-task status. Either a `workflow_node_tasks` table
   (instance_id, node_id, task_id, status, outcome, output, assignee, timestamps)
   or a tasks JSON blob on the node row. A table is cleaner for the dashboard and
   for AI-task assignees; lean table.
3. **Engine** (Step 6): run the tasks (parallel or sequential), each through the
   normal executor/verify path; apply the success rule; settle the node. Honor
   per-task retries.
4. **Dashboard** (Step 5): a composite node renders as a node that expands to show
   its task checklist with per-task status + the rule and its progress ("2/3, need
   2 ✓"). Mermaid stays one node; the detail panel shows the tasks.

## Relevant source files (when built)

- `pkg/claude/workflow/template.go` + `load.go` — `Task`, `Success`, validation
- `pkg/claude/common/db/workflows.go` — per-task state
- engine + dashboard per their steps

## Open questions

- Can a composite node be an `enum` decision (each task → a value)? Probably no —
  keep composite = pass/fail; use a simple enum node for decisions.
- Mixed executors within one composite (a human task + tool tasks)? Allow it; the
  node stays "running/awaiting" until human tasks settle.
- Does a composite node get its own agents for AI tasks (one per AI task) in the
  instance group? Likely yes — ties into group integration (Step 4).
