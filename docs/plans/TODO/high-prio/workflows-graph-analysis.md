# workflows: static graph analysis in the validator

Part of the **Workflows** feature — see `docs/plans/workflows.md`. A focused
enhancement to Step 1's loader/validator (`pkg/claude/workflow`). Independent of
the DB/API/dashboard steps — touches only the `workflow` package, so it can land
in parallel with them.

## Open / to build

Extend `(*Template).validate()` in `pkg/claude/workflow/load.go` (and/or a new
`analyze.go`) with topology checks over the parsed graph. Distinguish hard
**problems** (fail load) from **warnings** (surfaced but non-fatal) — this likely
means giving `Template` a `Warnings []string` field (populated during load) while
keeping `ValidationError` for problems.

Checks:
1. **Reachability** — every node must be reachable from some entry node. An
   unreachable node is a **problem** (it can never run; almost always an authoring
   mistake or a typo'd edge endpoint).
2. **Can-reach-terminal** — from every node there must be a path to some terminal
   node (a node with no outgoing edges). A node trapped in a pocket with no exit
   is a **problem** (the instance could never complete from there). Note loops are
   fine as long as the cycle has an exit edge somewhere.
3. **Terminal sanity** — at least one terminal node must exist (else the workflow
   can never complete) → **problem**.
4. **Enum coverage** — for an `enum`-verified node, a declared value with no
   matching outgoing edge is a **warning** (it's a terminal-on-that-outcome, which
   is sometimes intended but often a forgotten edge). Already allowed today; just
   warn.
5. (Optional, nice) **dead `|fail|` edge** is already a problem; keep it.

Implement with a plain BFS/DFS from the entry set (forward reachability) and a
reverse walk from terminals (co-reachability). Keep it allocation-light and
deterministic (sort ids for stable messages).

## Shipped context (Step 1, merged PR #226)

`pkg/claude/workflow` already parses the mermaid chart into `Edges` + `MermaidNodes`,
computes `Entry` (declared, or nodes with no incoming edge), and aggregates
validation problems via `ValidationError`. `OutEdges(id)` exists. The example
template (`example/implement-microservice`) must still load clean after this —
it has a fix loop (`test -->|fail| implement`, `review -->|changes| implement`)
and a terminal `done`, so reachability + can-reach-terminal both hold.

## Quality gates

`go build ./...` · `go test ./pkg/claude/workflow/...` · `golangci-lint run ./...`
all clean. Add table-driven tests: unreachable node (problem), no-exit pocket
(problem), no-terminal graph (problem), enum value without edge (warning, still
loads), and that the shipped example stays clean.

## Relevant source files

- `pkg/claude/workflow/load.go` — `validate()`, `computeEntry`, `OutEdges`
- `pkg/claude/workflow/template.go` — add `Warnings` to `Template` if needed
- `pkg/claude/workflow/*_test.go` — add analysis tests

## Open questions

- Reachability of a declared-but-unreferenced single-node workflow (one node, no
  edges): the node is both entry and terminal → reachable + terminal. Ensure this
  edge case passes.
