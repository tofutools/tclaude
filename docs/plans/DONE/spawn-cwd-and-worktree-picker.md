# Spawn cwd validation + worktree picker

Shipped on branch `fix-spawn-cwd`.

## What shipped

Three related improvements to spawning/cloning agents:

1. **Spawn cwd is validated up front.** A bad working directory (typo,
   non-existent path) used to sail past the daemon into a detached
   `tclaude session new` subprocess that failed silently — the caller
   then waited out the full 30s conv-id poll and got a confusing
   `504 gateway-timeout`. The daemon now stat-checks the cwd before
   forking and returns an immediate `400 invalid_cwd` with a clear
   message. The dashboard's spawn modal already renders the error body
   verbatim, so the failure is now visible instead of mysterious.

2. **`~` expansion.** A cwd of `~` or `~/git/myproject` is expanded to
   the human's home directory daemon-side. Inputs the shell never
   expanded (quoted args, dashboard text fields) now work.

3. **Optional worktree picker** in the dashboard spawn + clone modals.
   The picker lists the git worktrees of the repo the spawn/clone
   targets and lets the human either select an existing one or create a
   new worktree (new branch off a chosen base, or checkout of an
   existing branch). The selected worktree's path becomes the spawn /
   clone `cwd`. Entirely opt-in — the default leaves behaviour
   unchanged.

## Surface

### HTTP (daemon, cookie-auth / dashboard-only)

- `GET  /api/worktrees?repo=<path>` — worktrees + branches + default
  branch of the repo containing `<path>`. Non-repo `repo` returns
  `{"is_repo": false}` (not an error).
- `POST /api/worktrees` — body `{repo, branch, from_branch?, path?}`,
  creates a worktree and returns `{path, branch}`.

### Request body additions

- `POST /v1/groups/{name}/spawn` — `cwd` is validated + `~`-expanded
  (was: passed through raw).
- `POST /v1/agent/{conv}/clone` (+ `/v1/whoami/clone`, dashboard twin) —
  new optional `cwd` field overrides where the clone's CC session
  spawns. Empty = inherit the source's cwd (historical behaviour).

## Files

- `pkg/claude/agentd/cwd.go` — `expandTilde`, `resolveSpawnCwd` (new).
- `pkg/claude/agentd/worktrees.go` — `/api/worktrees` GET/POST handlers
  (new).
- `pkg/claude/agentd/lifecycle.go` — `handleGroupSpawn` validates cwd.
- `pkg/claude/agentd/clone.go` — `decodeCloneBody` returns `cwd`;
  `runCloneOrchestration` takes a `cwdOverride`.
- `pkg/claude/agentd/dashboard_edit.go` — route registration + clone
  twin threads `cwd`.
- `pkg/claude/worktree/repo.go` — repo-path-anchored twins of the
  CWD-implicit helpers: `RepoRootForPath`, `ListWorktreesIn`,
  `BranchesIn`, `DefaultBranchIn`, `AddWorktreeIn` (new). The porcelain
  parser is now shared via `parseWorktreePorcelain`.
- `pkg/claude/agentd/dashboard.html` — shared worktree-picker JS
  (`wtLoad` / `wtResolveCwd` / `bindWtPicker`) + picker rows in the
  spawn and clone modals.

## Tests

- `pkg/claude/agentd/cwd_test.go` — `expandTilde` / `resolveSpawnCwd`
  units.
- `pkg/claude/worktree/repo_test.go` — repo-anchored helpers against a
  real temp git repo.
- `pkg/claude/agentd/spawn_cwd_flow_test.go` — spawn with a bad cwd →
  `400`; spawn with `~` → expands to home.
- `pkg/claude/agentd/clone_cwd_flow_test.go` — clone `cwd` override
  lands the sibling in the override dir; bad override → `400`.
- `pkg/testharness/flow.go` — `SpawnWith` / `CloneWith` DSL helpers
  (raw, non-fatal — for failure-path assertions).

## Notes / possible follow-ups

- The worktree picker is dashboard-only. `tclaude agent spawn --cwd`
  gets the cwd validation + `~` expansion for free (same daemon
  endpoint) but has no worktree flag yet — `tclaude worktree add` then
  `--cwd <path>` covers the CLI case.
- When the targeted cwd is itself a linked worktree, a newly created
  worktree defaults to a sibling of *that* worktree rather than of the
  main repo. Functional, slightly ugly path; an explicit `path` in the
  POST body sidesteps it.
