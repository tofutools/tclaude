# Spawn into a sub-repo worktree of a monorepo launch dir

The dashboard's spawn modal can now auto-worktree a git repo that is a
*nested sub-directory* of the agent's launch dir, instead of requiring
the launch dir itself to be the repo.

## The problem

A "virtual monorepo" is a plain directory holding shared docs
(`CLAUDE.md`, architecture notes) alongside several independent git
repos:

```
~/git/virtual-monorepo/        ← CLAUDE.md, docs/  (NOT a git repo)
  some-category/
    actual-repo/               ← a real git repo
```

Before this change the spawn modal's worktree picker was hard-wired to
the repo *containing the CWD field*. With CWD set to the monorepo, the
picker showed "not a git repo — worktrees unavailable", and there was
no way to worktree the nested `some-category/actual-repo`.

## What shipped (Option 2: launch in the monorepo)

The agent's cwd stays the monorepo (so top-level `CLAUDE.md` / docs are
its working context); the worktree is created from the sub-repo and the
agent is *told* about it in its welcome message.

**Dashboard spawn modal** (`agentd/dashboard.html`)
- New **"Worktree repo"** field (`#agent-spawn-wt-repo`), decoupled
  from CWD. It mirrors CWD until the human edits it (`spawnWtRepoEdited`
  flag), then stays pinned.
- The worktree picker now targets the Worktree-repo field, not CWD.
- A sub-repo `<datalist>` (`#agent-spawn-subrepo-list`) offers nested
  repos discovered under a non-repo CWD — drill into one by picking it.
- Submit rule: **Worktree repo == CWD** → the worktree becomes the
  spawn cwd (long-standing single-dir behaviour). **Worktree repo ≠
  CWD** → cwd stays the launch dir; `worktree_path` + `worktree_branch`
  ride along separately.
- `wtResolveCwd` refactored into `wtResolve` → `{path, branch}`;
  `wtResolveCwd` kept as a thin wrapper for the (unchanged) clone modal.

**Daemon** (`agentd/lifecycle.go`)
- `POST /v1/groups/{name}/spawn` accepts optional `worktree_path` /
  `worktree_branch`. `worktree_path` is validated for existence
  (`resolveSpawnCwd`) → an immediate 400 on a stale path.
- Threaded through `runSpawnPostInit` → `buildSpawnWelcome`, which
  appends a sentence pointing the agent at its worktree + branch.

**Worktree discovery** (`worktree/repo.go`)
- `FindSubRepos(dir, maxDepth)` walks a non-repo dir for nested git
  repos (a `.git` entry = repo; doesn't descend into one; skips hidden
  dirs + `node_modules`/`vendor`). Returns sorted `[]SubRepo{Path,Rel}`.
- `GET /api/worktrees?repo=<path>` (`agentd/worktrees.go`) returns a
  `sub_repos` array when `<path>` isn't itself a repo
  (`subRepoScanDepth = 4`).

## Tests
- `worktree/repo_test.go::TestFindSubRepos` — depth cap, skip dirs,
  repo-is-a-leaf, sorted output, degenerate inputs.
- `agentd/spawn_subdir_worktree_flow_test.go` — spawn launches in the
  monorepo (not the worktree); welcome names the worktree path/branch;
  bad `worktree_path` → 400; an omitted worktree leaves the welcome
  clean.
- `agentd/dashboard_subdir_worktree_test.go` — structural guard on the
  modal's markup/JS wiring.
- `agentd/lifecycle_test.go::TestBuildSpawnWelcome_*` — extended with
  worktree-field cases.

## Known limitation / follow-up

With cwd = the monorepo (not a git repo), the dashboard **BRANCH
column stays blank** for these agents — conv-index captures the branch
from cwd. A proper fix means recording the worktree branch on the
`agent_group_members` row (a schema migration). Deferred.

Also deferred: a group-level `default_worktree_repo`, and
`tclaude agent spawn --worktree` CLI parity (the CLI can pass `-C`).
