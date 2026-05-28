# workflows: sub-workflow nodes + dynamic sub-graph expansion

Part of the **Workflows** feature — see `docs/plans/workflows.md`. The most
advanced later step: nodes that instantiate **other graphs**, and graphs that
**grow at runtime**. Builds on the engine (Step 6), DB (Step 2), and external
sources (Step 7, for referencing sub-workflow templates). Two capabilities,
landed in this order:

## Capability A — sub-workflow node (static composition)

A node whose executor instantiates **another workflow template** and waits for it.

```yaml
# nodes/provision.yaml
label: Provision infra
executor:
  kind: subworkflow
  workflow: apply-for-infra          # a template ref (user:/example:/dir:/git:)
  params:                            # map parent vars/params into the child
    service: "{{service_name}}"
    env: "{{env}}"
verify: { kind: none }               # the child's completion is the verdict
capture: infra                       # child's bubbled-up captures land here
```

- Instantiating creates a **child instance**; the node is `running` until the
  child reaches a terminal status, then settles pass/fail from the child.
- DB: child `workflow_instances` row gets `parent_instance_id` + `parent_node_id`
  (add columns). Captures bubble up into the parent's `vars` under `capture`.
- Dashboard: the sub-workflow node links to / drills into the child instance's
  own graph view.
- Lets big processes compose reusable building blocks ("apply-for-infra" as a node
  inside "launch-microservice").

## Capability B — dynamic fan-out / on-demand expansion

The operator's scenario: an early node **investigates how many objects** to act
on, then the graph **materializes one branch per object** to run in parallel,
then a **join** continues. The cardinality isn't known at authoring time, so the
static mermaid can't pre-declare it — the engine expands the instance's graph at
runtime.

Proposed authoring shape (a fan-out node referencing a per-item body):

```yaml
# nodes/per-object.yaml
label: Process each object
fan_out:
  over: "{{object_ids}}"     # a captured list (string/json) from an earlier node
  as: object_id              # each expansion gets this var bound
  body: process-one-object   # a sub-workflow ref (or an inline sub-graph) run per item
  join: all                  # all | any — when the join node downstream unblocks
```

- At runtime the engine reads the list, and for each item **inserts dynamic nodes
  /child instances** into the *instance* (not the template) — running `body` per
  item, in parallel. The downstream join node waits per `join`.
- The instance's node set, edges, **and snapshotted mermaid grow at runtime** (the
  instance already snapshots its mermaid; here the snapshot is appended to and
  re-rendered). The dashboard renders the dynamically-added nodes live — which,
  combined with live agent vitals, is a striking view: a graph that visibly grows
  as work is discovered.
- Per-item failures settle per the `join` rule and the fan-out node's `on_fail`.

## Open / to build (when scheduled)

1. **Model**: `kind: subworkflow` executor (A); a `fan_out` node spec (B). Validate
   the referenced template resolves; `over` names a capture; `body` resolves.
2. **DB**: parent/child instance links; ability to insert dynamic nodes into a
   running instance (the schema already keys nodes by `(instance_id, node_id)`, so
   dynamic ids like `process-one-object#3` fit — just inserted at runtime).
3. **Engine** (Step 6 extension): instantiate children; expand fan-out; manage the
   dynamic join; bubble captures; aggregate child status.
4. **Dashboard**: render dynamically-added nodes + drill into child instances;
   show fan-out progress (N items, k done).

## Open questions

- Inline sub-graph (mermaid fragment in the node) vs always a referenced template
  for `body`? A referenced template is simpler and reuses everything; lean that
  way first.
- Re-rendering a growing mermaid without flicker — diff + append (ties into the
  dashboard's render-vs-restyle strategy).
- Bounding runaway expansion (max items / depth) — a hard cap + a warning, like the
  node `max_visits` loop guard.
- Recursion (a sub-workflow that fans out into itself) — allow with a depth cap.
