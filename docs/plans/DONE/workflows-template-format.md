# workflows: template format + parser + discovery + example (SHIPPED)

Step 1 of the Workflows feature — see `docs/plans/workflows.md`. Shipped in
PR #226 (squash-merged into the `agent-workflows` feature branch).

## What shipped

New package `pkg/claude/workflow` — the static template definition layer (parse +
validate only; running state lives in SQLite, added in later steps).

A template is a directory: `workflow.yaml` (name/description/params/entry) + a
pure `flow.mmd` mermaid chart + `nodes/<id>.yaml` (one per node, keyed by the
mermaid node id). Each node has one **executor** (`human`/`ai`/`tool`/`program`)
and one **verification** (`none`/`human`/`ai`/`tool`/`program`/`enum`/`format`).

**Branching model:** every node settles to an outcome string; an outgoing edge
whose label equals the outcome is followed, unlabeled edges are the success path
(`pass`), and an `enum` verification's value selects the branch — branching maps
directly onto mermaid edge labels (`review -->|approved| deploy`).

Files:
- `template.go` — types (`Template`, `Node`, `Executor`, `Verify`, `Edge`,
  `MermaidNode`, `Param`), executor/verify kind constants, outcome/mode/onfail/
  join constants, `DisplayLabel`, `OutEdges`.
- `mermaid.go` — focused parser for a documented flowchart subset (header; node
  shapes; `-->`/`---`/`-.->`/`==>`/`--x`/`--o` incl. dash/equals lengthening;
  `|pipe|` labels; `&` multi-target; chains; `;` separation; comments/subgraph
  (depth-tracked)/classDef/style ignored by exact-token match; reversed and
  inline-text links rejected with clear errors).
- `load.go` — `io/fs` loader (on-disk + embedded) with aggregated cross-
  validation (chart↔node 1:1, entry resolution, params, per-node executor/verify
  rules, edge-label/outcome consistency, duplicate-node-file detection).
- `discover.go` — `Resolve(ref, projectDirs...)` + `List(projectDirs...)` with
  `project > user > example` shadowing; `source:name` ref scheme (extensible to
  Step 7 external `dir:`/`git:` sources); path-traversal guard.
- `embed.go` + `example/implement-microservice/` — one shipped example
  exercising ai/human/tool nodes, an `enum` decision branch, and a
  `test -> implement` fix loop.

Tests: `mermaid_test.go`, `load_test.go`, `discover_test.go`, `example_test.go`,
`regressions_test.go` (the last from a cold-review pass — keyword-prefix node ids,
multi-dash links, subgraph/end handling, duplicate node files, path traversal).

## Notes / follow-ups

- `gopkg.in/yaml.v3` promoted to a direct dependency.
- Cold-review fixes folded in: exact-token directive matching (the prefix match
  silently dropped `endNode`/`subgraphX`/`classDefault` edges), link lengthening,
  duplicate `.yaml`/`.yml` detection, qualified-ref path-traversal guard.
- The advance/branch logic (follow the matching labeled edge, skip non-taken
  branches) is specified here but implemented later — shared between the agentd
  API (Step 3, manual driving) and the execution engine (Step 6).
