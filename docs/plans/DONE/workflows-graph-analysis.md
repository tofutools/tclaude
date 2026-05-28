# workflows: static graph analysis in the validator — SHIPPED (PR #228)

Part of the **Workflows** feature — see `docs/plans/workflows.md`. A focused
enhancement to Step 1's loader/validator (`pkg/claude/workflow`): static
topology analysis over the parsed mermaid graph, run as the last phase of
`(*Template).validate()`.

## What shipped

New file `pkg/claude/workflow/analyze.go` with `(*Template).analyzeGraph(add)`,
wired into `validate()` after `Entry` is settled. One new field on `Template`
in `template.go`: `Warnings []string` (non-fatal smells, sorted/deterministic).
Hard problems still flow through `ValidationError`.

Checks (all plain BFS over `Edges` / `MermaidNodes` / `Entry`, sorted ids for
stable messages):

1. **Reachability** (problem) — forward BFS from the valid entry set; any chart
   node not reached is `node "x" is unreachable: ... (entry: ...)`. The message
   lists the entries actually walked from (valid ones), not the raw declared
   list. Skipped entirely when no declared entry exists in the chart — the
   missing/empty-entry problems in `validate()` already cover that, and seeding
   from nothing would spuriously flag every node.
2. **Can-reach-terminal** (problem) — reverse BFS from terminals (chart nodes
   with no outgoing edge); any node not reached is a no-exit pocket:
   `node "x" cannot reach any terminal node: ...`.
3. **Terminal sanity** (problem) — if there is **no** terminal, a single
   root-cause `flow.mmd: no terminal node — ...` fires and the per-node
   can-reach-terminal walk is suppressed (it would otherwise flag every node).
4. **Enum coverage** (warning) — for an `enum`-verified node, a declared value
   with no matching outgoing edge label is appended to `Template.Warnings`
   (`node "x": enum value "v" has no outgoing edge ...`). Non-fatal: the
   template still loads (terminal-on-that-outcome is sometimes intended).

Helper `reachable(seeds, adj)` is a shared dedupe-seeded BFS used for both the
forward and reverse walks. Every edge endpoint is guaranteed to be a declared
`MermaidNode` (the mermaid parser records a node for anything on an edge), so
the chart node set covers every id the walks touch.

## Tests (`pkg/claude/workflow/analyze_test.go`)

Scenario tests: unreachable node (problem, and stays separate from the
co-reach/no-terminal checks), no-exit pocket (problem), no-terminal graph
(problem, no per-node flood), enum-value-without-edge (warning + still loads),
enum fully covered (no warning), single-node workflow (passes — node is both
entry and terminal), sorted-determinism of multiple unreachable nodes, and the
shipped `example/implement-microservice` loads clean with **no** warnings.

## Quality gates (all green at merge)

`go build ./...` · `go test ./...` · `golangci-lint run ./...` — 0 issues.

## Cold review

A fresh sub-agent reviewed the diff blind (reachability/co-reach walk bugs +
edge cases). No high/medium correctness bugs. One nit applied: the unreachable
message now prints the entries actually walked from rather than the raw declared
list. Documented skips: an all-invalid-entry chart surfaces only the bad-entry
problem on that pass (load fails regardless); a node that is both unreachable
and trapped is double-reported (both facts are true; the checks are independent).

## Source files

- `pkg/claude/workflow/analyze.go` — `analyzeGraph`, `reachable`
- `pkg/claude/workflow/load.go` — `analyzeGraph` call site in `validate()`
- `pkg/claude/workflow/template.go` — `Template.Warnings` field
- `pkg/claude/workflow/analyze_test.go` — scenario tests

## Downstream note

`Template.Warnings` is now available for downstream steps (CLI/dashboard) to
surface non-fatal topology smells alongside a successful load.
