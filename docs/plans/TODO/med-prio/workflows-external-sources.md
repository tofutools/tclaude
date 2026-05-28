# workflows: external template sources (Step 7)

Part of the **Workflows** feature — see `docs/plans/workflows.md`. Lets a
template be referenced from **any directory or git repo**, not just the
project/user/example dirs. Extends Step 1's discovery (`pkg/claude/workflow`).
Med-prio: the monitoring MVP works with local + example templates; this widens
sourcing afterwards.

## Idea (from the operator)

Reference a template externally so any repo can host workflows:

- a **plain directory** (absolute or relative), or
- a **git repo + ref + path**: `git:<url>[@<ref>][#<path>]`, where `<ref>` is a
  branch / tag / commit (a pinned commit/tag → immutable, reproducible), and
  `<path>` is the template dir within the repo.

The referenced dir contains exactly what a local template does: `flow.mmd` +
`workflow.yaml` + `nodes/<id>.yaml` (one spec file per node). So the *format*
already works — this step only adds new ways to *locate* it.

## Open / to build

1. **Ref scheme** — extend `splitRef`/`Resolve` in `discover.go` with two new
   sources (the scheme was designed to extend):
   - `dir:<path>` → `LoadDir(path, ...)` with `Source` = a new `SourceDir`.
   - `git:<url>[@<ref>][#<path>]` → fetch + load (below). `Source` = `SourceGit`.
2. **Git fetch + cache** — a new module (e.g. `pkg/claude/workflow/fetch.go`):
   - clone/fetch into `~/.tclaude/workflows-cache/<hash-of-url>/`, checkout the
     requested ref, load the template from `<path>`.
   - pinned commit/tag → cache is immutable; branch → refresh policy (TTL or an
     explicit `--refresh`). Be explicit in logs about what was fetched/cached.
   - shallow clone where possible; reuse an existing checkout.
3. **Instantiation already snapshots** — `workflow_instances` snapshots the
   resolved mermaid + node defs (see `workflows-db-schema.md`), so once an
   instance is created it is independent of the external source even if the
   upstream repo moves. This step only affects *resolution*, not running state.
4. **Security** — cloning arbitrary repos and (Phase-2) running their
   `tool`/`program` nodes is code execution from a third party. Gate it:
   explicit operator opt-in per source, surface the source + ref prominently in
   the dashboard, and never auto-run external `tool`/`program` nodes without
   confirmation. Document the trust model.
5. **Dashboard** — the instantiate modal accepts an external ref; the Templates
   panel shows the source (dir/git url + ref) and a refresh control for git
   sources.
6. **Tests** — `dir:` resolution against a temp dir; `git:` against a local
   bare repo fixture (no network) exercising branch/tag/commit + subpath;
   cache-hit/refresh behavior; malformed-ref errors.

## Prior art: the old `pkg/claude/task` runner

The operator asked to check the old tclaude task runner for inspiration.
Findings (it is **not** a sourcing mechanism, so little applies to Step 7 — but
it is good prior art for the **execution engine**, Step 6):

- It is a **TODO.md-driven, single-agent, linear** runner: `task add` appends to
  TODO.md, `task list` reads it, `task run` drives a Claude Code session through
  each task. Strictly less general than our graph + multi-executor + multi-agent
  model — the operator's "too limiting" read is correct.
- Worth reusing in the **execution engine**: its `tasks.json` config models a
  **verify command with `max_verify_iterations` + `verify_timeout`** and a
  **review skill pass** (`review_skill`, `max_review_iterations`, …) — i.e. the
  validate-and-fix and review loops we want, with iteration caps and timeouts.
  It also handles **rate limits** (`pkg/claude/common/ratelimit`) and
  **fsnotify** while driving sessions. Cross-reference from
  `workflows-execution-engine.md` when building tool/ai verification + loops.

## Relevant source files

- `pkg/claude/workflow/discover.go` — extend `splitRef` + `Resolve`
- NEW: `pkg/claude/workflow/fetch.go` (+ tests) — git fetch/cache
- `pkg/claude/task/{run.go,task.go}` — prior art for verify/review loops (engine)
- `pkg/claude/common/ratelimit` — rate-limit handling to reuse in the engine

## Open questions

- Cache location + eviction (`~/.tclaude/workflows-cache/`); size cap?
- Auth for private repos — rely on the user's git credentials/SSH (likely yes).
- Should `dir:`/`git:` refs be allowed for the *example* slot, or kept distinct?
