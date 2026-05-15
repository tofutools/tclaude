# dashboard-clickable-links — clickable CWD + branch cells

Shipped 2026-05 (prototype / first cut).

The dashboard's CWD and Branch columns were inert text. This makes
them actionable:

- **CWD cell** → click a path to open a terminal window there.
- **Branch cell** → the branch name links to its GitHub compare view,
  and an open pull request is appended as a `#<num>` link.

## What shipped

### CWD → click-to-open-a-terminal

`cwdCell()` now renders each path as a `data-act="term-dir"` span
carrying `data-conv` + `data-which`. The stacked init/now pair maps to
the two `/api/term` selectors that resolve to those exact directories:
the launch dir → `which="start"`, the live worktree → `which="worktree"`.
A new `term-dir` branch in `bindRowActions` POSTs straight to the
existing `POST /api/term/{conv}` — no new endpoint, no new capability.
It skips the **term** button's 3-way dir-picker modal; that button
stays for the case where you want the "current" edit dir (not shown in
the CWD column).

### Branch → GitHub compare + PR links

`branchCell()` renders the branch name as an `<a>` to the branch's
GitHub web view — a compare view (`/compare/<default>...<branch>`) for
a feature branch, a tree view for the default branch — and, when a PR
exists, appends a `#<num>` `<a>` to it. Plain `<a target="_blank">`:
native navigation, no daemon round-trip. Falls back to plain text when
the repo isn't on GitHub or links haven't resolved yet.

### Branch-link resolution — `branchlinks.go`

The link data (repo's GitHub URL, default branch, branch's PR) comes
from `git` + `gh`. `ResolveLocation` deliberately stays a pure DB read
(the v28 no-git-per-refresh goal), so the snapshot path **never**
shells out:

- `lookupBranchLink(repoDir, branch)` reads a DB-backed cache and, on
  a missing/stale entry, schedules an async background refresh —
  serving whatever the cache holds meanwhile (empty on a cold miss).
- The cache is the existing `git_cache` table (no migration). Keyed by
  `branchLinkCacheKey` = `bl_` + `sha256("branchlink\0"+repoDir+"\0"+branch)`,
  prefixed so it never collides with the statusbar's bare repo-hash keys.
- Background refresh is single-flighted via `branchLinkInflight` so the
  5s snapshot poll can't stack refreshes. Runs through `goBackground`.
- `branchLinkTTL = 90s`. A non-GitHub repo / git failure still writes a
  *negative* cache entry (empty `RepoURL`) so it isn't re-resolved on
  every poll.
- `gitInfoResolver` is the subprocess seam (mirrors `clcommon.Default`
  / `agentd.Spawn` / `openTerminal`): production is `liveGitInfoResolver`
  (`git remote get-url` + `git symbolic-ref` + `gh pr view`); flow
  tests swap a fake via `SetGitInfoResolverForTest`. All git/gh calls
  are best-effort — a missing/unauthenticated `gh` just yields no PR link.

### Wire shape

`repoLinksView` — `branch_url` / `branch_pr_number` / `branch_pr_url`
plus the `startup_*` trio — is embedded in `dashboardAgent` and
`dashboardMember` (dashboard-only; it never rides the agent-facing
`/v1/peers` surface, which must not pay a git/gh cost). Populated by
`branchLinksFor(loc)` at each of the three snapshot row-build sites.

## Files

- `pkg/claude/agentd/branchlinks.go` — new: resolver, cache, enricher,
  `repoLinksView`, `gitInfoResolver` seam.
- `pkg/claude/agentd/dashboard.go` — embed `repoLinksView`, call
  `branchLinksFor` in `handleDashboardSnapshot`.
- `pkg/claude/agentd/dashboard.html` — `cwdCell` / `branchCell`
  rewrite, `term-dir` dispatch, `.cwd-link` / `.branch-link` /
  `.pr-link` CSS.
- `pkg/claude/agentd/testhooks_test.go` — `SetGitInfoResolverForTest`.
- `pkg/claude/agentd/dashboard_branchlinks_flow_test.go` — flow test.
- `pkg/claude/agentd/dashboard_ungrouped_flow_test.go` — link fields on
  the shared `dashAgent` / `dashMember` test structs.
- `docs/dashboard.md` — Groups section note.

## Tests

`dashboard_branchlinks_flow_test.go`: two agents on feature branches
(one with PR #42, one without), the git/gh resolver swapped for a
fake. Drives the two-phase resolution — first snapshot is a cold cache
miss that kicks the async resolve, second snapshot (after draining)
reads the populated cache — and asserts `branch_url` / `branch_pr_*`
on both the Agents and Groups tabs of the real `/api/snapshot`.

## Open / deferred

- GitHub only. A non-GitHub host (GitLab, Bitbucket, GHE) resolves to
  no link — the URL-scheme templates are GitHub-specific.
- `gh pr view <branch>` returns whatever PR is most relevant, including
  a closed/merged one — not filtered to open PRs. Acceptable: a link
  to a merged PR is still informative.
- `branchLinkTTL` is a fixed 90s const, not configurable.
- The CWD cell opens a terminal on any click — selecting the path text
  to copy it also fires the open. The full path is in the tooltip.
