# workflows: external template sources (Step 7) — SHIPPED (PR #236, JOH-12)

Part of the **Workflows** feature — see `docs/plans/workflows.md`. Lets a
template be referenced from **any directory or git repo**, not just the
project/user/example dirs. Extended Step 1's discovery (`pkg/claude/workflow`)
only — no DB/agentd/dashboard changes.

## What shipped

Two new ref schemes (`discover.go`) and a new `fetch.go`:

- **`dir:<path>`** → `LoadDir` with `Source = SourceDir`. Absolute or
  relative-to-CWD.
- **`git:<url>[@<ref>][#<path>]`** → fetch + cache + load, `Source = SourceGit`.
  `<ref>` is a branch / tag / commit (default branch if omitted); `<path>` is
  the template dir within the repo (repo root if omitted).

`Resolve` now delegates to
`ResolveOpts(ref, ResolveOptions{Refresh, TTL, Timeout, CacheDir}, projectDirs...)`.
`dir:`/`git:` specs skip the single-segment `validRefName` (they carry
`/ : @ #`).

### Source constants + seam (`template.go`)

- New `SourceDir = "dir"`, `SourceGit = "git"`.
- `func (Source) IsExternal() bool` → true for `dir`/`git`. **The seam**: the
  execution engine + node approval gates (JOH-17) call this to require
  confirmation before running an externally-sourced `tool`/`program` node.
  Callers use it, rather than re-implementing the `dir||git` check, so a future
  external kind stays single-source-of-truth.

### Git fetch + cache (`fetch.go`)

- `parseGitSpec` — authority-aware so scp (`git@host:org/repo`) and https
  userinfo (`https://user@host/...`) aren't mistaken for the `@ref` delimiter;
  branch refs may contain `/`.
- Cache under `~/.tclaude/workflows-cache/<repo>-<hash(normalised-url+ref)>`
  (ref in the key → refs cached independently). Shallow `--depth 1 --branch`
  for branch/tag; full clone + detached checkout fallback for commit SHAs.
- Refresh policy: a full **40-hex commit SHA is immutable** (never refetch);
  branch/tag/default are re-fetched on TTL (default 1h) or explicit `Refresh`.
  Re-fetch = clone-to-temp + atomic rename.
- Explicit `slog` of cloned / refreshed / cached.

### Security / trust model (documented in `fetch.go`)

Resolution only fetches + parses **static YAML**; it never executes anything.
Hardening shipped (all from the blind cold review):

- url/ref may not begin with `-` (flag-injection guard, e.g. `--upload-pack`);
  positional args fenced with `--`; ext transport disabled
  (`-c protocol.ext.allow=never`).
- every git invocation runs under a deadline (`DefaultGitTimeout`, configurable)
  so a hostile/slow remote can't hang the resolver.
- `git:` subpath traversal rejected lexically (cross-platform: `\` + drive
  letters) AND a symlinked template dir escaping the clone is rejected
  (`ensureWithin`).
- `GIT_TERMINAL_PROMPT=0` → missing private-repo creds fail fast (auth via the
  user's own git credential helpers / SSH).

Residual (documented): a hostile repo could ship an individual config file as a
symlink, which `LoadDir` would *read* (parsed as YAML, no execution, no network
egress at resolve time). Bounded by the explicit opt-in; a symlink-safe loader
(`os.Root`) is a noted follow-up if external sourcing sees wide use.

## Source contract (recorded on JOH-12, consumed by engine + CLI/dashboard)

Read `Template.Source` + call `Source.IsExternal()`. `Template.Ref` carries the
full external spec verbatim; `Template.Dir` is the resolved local path (the dir,
or the git clone checkout). The execution engine recovers external-ness from the
**instance snapshot** (`workflow.Source(snap.Source).IsExternal()`); per PO, an
external `tool`/`program` node is left `awaiting` (human-gated) until JOH-17.

## Deferred (seam shipped, consumer owns it)

- **Dashboard (#5)** — the instantiate modal / Templates panel surfacing the
  source + ref + git refresh control. Same pattern as `Template.Warnings`: the
  resolution seam is shipped; the dashboard worker consumes it.

## Open questions — resolved

- **Cache eviction / size cap** — none for now (templates are tiny, cache is
  keyed + reused). Revisit if it grows.
- **`dir:`/`git:` for the example slot** — kept distinct; examples stay embedded.
- **Private-repo auth** — yes, the user's git credential helpers / SSH.

## Tests

- `dir:` — absolute, relative-to-CWD, not-a-template, empty.
- `git:` against a **local bare-repo fixture (no network)** — branch / tag /
  commit / default branch, repo-root + subpath, cache-hit (source removed →
  still loads), commit immutability vs TTL, forced refresh re-fetch,
  subpath-traversal rejection, missing-template-dir.
- `parseGitSpec` table (https / scp / userinfo / ssh / local / ref-with-slash) +
  malformed + flag-injection errors; `isCommitSHA` (full-SHA only); `checkSubpath`
  cross-platform; `cacheKey` stability.

## Source files

- `pkg/claude/workflow/discover.go` — `Resolve` → `ResolveOpts`, `splitRef` for
  `dir:`/`git:`
- `pkg/claude/workflow/fetch.go` — git fetch/cache, `parseGitSpec`, trust model
- `pkg/claude/workflow/template.go` — `SourceDir`/`SourceGit` + `IsExternal()`
- `pkg/claude/workflow/{fetch,discover}_test.go` — tests

## Prior art note (for the execution engine, Step 6)

The old `pkg/claude/task` runner is not a sourcing mechanism, but its
`tasks.json` verify/review loops (`max_verify_iterations`, `verify_timeout`,
`review_skill`, …) + rate-limit handling (`pkg/claude/common/ratelimit`) are
reusable prior art for Step 6's tool/ai verification loops.
