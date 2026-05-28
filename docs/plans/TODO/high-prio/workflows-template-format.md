# workflows: template format + parser + discovery + example

Part of the **Workflows** feature — see `docs/plans/workflows.md` for the whole
design. This step is the foundation everything else builds on.

## Open / to build

A new package (proposed `pkg/claude/workflow/`) that defines, loads, and
validates workflow templates.

1. **Types** for a parsed template: `Template{Name, Description, Params, Entry,
   Mermaid string, Nodes map[string]Node, Edges []Edge}`; `Node{ID, Label,
   Executor, Verify, Capture, Retries, MaxVisits, OnFail, Join}`; `Edge{From, To,
   Label}`.
2. **Mermaid flowchart parser** — parse the supported subset into nodes + labeled
   edges:
   - headers: `flowchart`/`graph` + `TD|TB|BT|LR|RL`
   - node decls: `A`, `A[text]`, `A(text)`, `A{text}`, `A((text))` (shape is
     cosmetic; we only need id + display text)
   - edges: `A --> B`, `A --> C & D`, `A -->|label| B`, `A -- text --> B`
   - ignore: `subgraph`/`end`, `classDef`, `class`, `style`, `%% comments`,
     `click` (or reject with a clear message — decide; leaning "ignore comments
     & style, reject unknown structural syntax").
   - Keep it a small focused parser; document the subset. This is the main
     implementation risk — keep the grammar tight and well-tested.
3. **Loader** — read a template directory: `workflow.yaml` + `flow.mmd` +
   `nodes/*.yaml`. Cross-validate: every mermaid node id has a `nodes/<id>.yaml`
   and vice-versa; every edge endpoint exists; `entry` (if set) exists; enum
   `verify` nodes' edge labels ⊆ declared `values`; no unknown executor/verify
   kinds. Return rich, line-referenced errors.
4. **Discovery** — resolve a template by name across project
   (`<repo>/.tclaude/workflows/`), user (`~/.tclaude/workflows/`), and the
   embedded `example:` namespace. Project shadows user shadows built-in. List all
   discoverable templates (for the dashboard Templates panel).
5. **Embedded example** — one realistic template via `go:embed` (e.g.
   `implement-microservice`) demonstrating ai/human/tool nodes, an enum decision
   branch, and a fix loop. Doubles as a parser test fixture.
6. **Tests** — table-driven parser tests (valid + malformed mermaid), loader
   cross-validation tests, discovery/shadowing tests, and "the shipped example
   parses + validates" test.

## Shipped context

Nothing yet — this is the first step. Config dir is `~/.tclaude/` (see
`pkg/claude/common/config`). `go:embed` is already used for dashboard assets
(`pkg/claude/agentd/dashboard/`) — mirror that for the example template.

## Relevant source files

- NEW: `pkg/claude/workflow/{template.go,parse_mermaid.go,load.go,discover.go,embed.go}` + tests
- NEW: `pkg/claude/workflow/example/implement-microservice/{workflow.yaml,flow.mmd,nodes/*.yaml}`
- Ref for go:embed + asset layout: `pkg/claude/agentd/dashboard/` embed
- Config/dirs: `pkg/claude/common/config`, `pkg/common` (dirs)

## Open questions

- Strict vs lenient on unknown mermaid syntax (lean: ignore comments/style,
  reject unknown structural edges).
- Directory format (chosen) vs single-file. Keep directory; revisit if painful.
