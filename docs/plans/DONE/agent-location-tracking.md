# Agent location tracking — startup dir vs. current dir/branch

tclaude tracks two locations per agent and keeps the second one live
as the agent hops between directories during its work:

- **startup** — where Claude Code was launched (`sessions.cwd`) and
  that dir's git branch (CC's per-turn `gitBranch` stamp, via the
  conv_index row). Empty branch when the launch dir isn't a git repo.
- **current** — the git worktree root the agent last edited a file in,
  and its branch. Recorded by the PostToolUse hook.

The two diverge whenever an agent works somewhere other than its
launch dir — most importantly a worktree of a sub-repo inside a
"virtual monorepo" launch dir (see `spawn-subdir-worktree.md`), or any
session that `git checkout`s / hops between repos mid-run. Before this,
the launch dir's branch was the only branch any surface showed, so a
monorepo agent's branch column was blank and a branch-switching agent
showed a stale value once it left its launch dir.

## What shipped

**Storage** — `agent_workdir` widened (migration **v27→v28**) from
`{conv_id, dir, updated_at}` to also carry `worktree_root` + `branch`.
The PostToolUse hook computes both with `session.GitLocationOf(dir)`
(two `git rev-parse` calls, best-effort) at edit time and upserts all
three. Reads never shell out to git.

**Resolver** — `agent.ResolveLocation(convID) Location`:

| field | source |
|-------|--------|
| `StartupDir` | `sessions.cwd`, fallback conv_index `ProjectPath` |
| `StartupBranch` | conv_index `GitBranch` (CC's stamp) |
| `EditDir` | `agent_workdir.dir` — the granular last-edit dir |
| `CurrentDir` | `agent_workdir.worktree_root`, fallback `EditDir`, fallback `StartupDir` |
| `CurrentBranch` | `agent_workdir.branch`, fallback `StartupBranch` |
| `Tracked` | true once the hook has recorded an edit |

`Location.Moved()` reports `CurrentDir != StartupDir`. A pre-v28
`agent_workdir` row carries no `worktree_root`/`branch` — `CurrentDir`
degrades to the edit dir and the branch to startup, self-healing on
the agent's next edit. `agent.FreshBranch` and `agentd`'s `resolveDirs`
both delegate to `ResolveLocation`.

**Wire surfaces** — a shared `agentLocationView` (fields `branch` =
current, `startup_dir`, `startup_branch`, `current_dir`) is embedded in
`peerEntry` (`/v1/peers`), `memberJSON` (`/v1/groups/{name}/members`),
and the dashboard snapshot's `dashboardMember` / `dashboardAgent`. The
`/v1/.../dir` endpoints' `dirResp` gained `start_branch` /
`current_branch`. `branch` stays the primary single value every
existing CLI/dashboard reader already consumes — it now means *current*
branch.

**Dashboard UI** — the CWD and Branch columns render one line when
startup and current agree, and stack an `init` / `now` pair (the
`stackedLoc` / `cwdCell` / `branchCell` helpers, `.loc-pair` CSS) when
they diverge — fitting both values without adding columns.

## Files
- `pkg/claude/common/db/migrate.go` — `migrateV27toV28`
- `pkg/claude/common/db/agent_workdir.go` — struct + `UpsertAgentWorkdir` / `GetAgentWorkdir`
- `pkg/claude/session/workdir.go` — `GitLocationOf`
- `pkg/claude/session/hook_callback.go` — PostToolUse stores location
- `pkg/claude/agent/location.go` — `Location` + `ResolveLocation`
- `pkg/claude/agentd/agent_location_view.go` — `agentLocationView` + `locationView`
- `pkg/claude/agentd/dir.go` — `resolveDirs` / `dirResp` via `ResolveLocation`
- `pkg/claude/agentd/dashboard.html` — stacked CWD/Branch cells

## Tests
- `session/workdir_test.go::TestGitLocationOf` — real repo, subdir, non-repo.
- `agent/location_test.go::TestResolveLocation` — combination + pre-v28 fallback.
- `agentd/agent_location_flow_test.go` — startup/current surfaced across
  members + snapshot; branch follows the latest edit across sub-repo hops.
- `agentd/dir_flow_test.go` — `current_branch` on the `dir` endpoint.
- `agentd/dashboard_subdir_worktree_test.go::TestDashboardHTML_LocationCellsWired`.

## Deferred
The current branch is "as of last file edit" — a `git checkout` with no
following edit isn't picked up until the next edit. Acceptable; a live
re-resolve on read was rejected as N git calls per dashboard refresh.
