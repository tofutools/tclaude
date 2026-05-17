# `tclaude agent spawn` CLI ⇄ dashboard spawn-UI parity

Shipped: the `tclaude agent spawn` CLI can now do everything the
agentd dashboard's spawn modal can. The dashboard modal exposed a
worktree picker, an auto-focus checkbox, and an "include group
context" checkbox that the CLI had no equivalent for; the CLI now has
all three.

## Audit — before / after

`POST /v1/groups/{name}/spawn` body fields, and which surface set them:

| Body field              | Dashboard (before) | CLI (before)        | CLI (after)              |
|-------------------------|--------------------|---------------------|--------------------------|
| `name`                  | ✅ field           | ✅ `--name/-n`      | ✅ `--name/-n`           |
| `role`                  | ✅ field           | ✅ `--role/-r`      | ✅ `--role/-r`           |
| `descr`                 | ✅ field           | ✅ `--descr/-d`     | ✅ `--descr/-d`          |
| `initial_message`       | ✅ textarea        | ✅ `--initial-message/-m`, `--file/-f` | ✅ (unchanged) |
| `cwd`                   | ✅ field           | ✅ `--cwd/-C`       | ✅ `--cwd/-C`            |
| `timeout_seconds`       | ✗ (fixed)          | ✅ `--timeout/-t`   | ✅ `--timeout/-t`        |
| `reply_to`              | ✗                  | ✅ `--reply-to`     | ✅ `--reply-to`          |
| `worktree_path`/`_branch` | ✅ worktree picker | **✗ — gap**        | ✅ `--worktree` (+ `--worktree-base`, `--worktree-repo`) |
| `auto_focus`            | ✅ checkbox (on)   | **✗ — gap**         | ✅ `--auto-focus`        |
| `include_group_context` | ✅ checkbox (on)   | **✗ — gap**         | ✅ `--no-group-context`  |

The three CLI gaps (`worktree_*`, `auto_focus`, `include_group_context`)
are closed. `reply_to` / `timeout_seconds` / `--file` remain CLI-only —
they are sensible CLI affordances the dashboard doesn't need; bringing
the dashboard up to the CLI was out of scope (the task was CLI→dashboard
parity).

## What shipped

### New CLI flags on `tclaude agent spawn`

- `--worktree BRANCH` / `-w` — create (or reuse) a git worktree on
  `BRANCH` and spawn the agent into it. The CLI equivalent of the
  dashboard modal's worktree picker.
- `--worktree-base BRANCH` — base branch a freshly-created `--worktree`
  is cut from (default: the repo's default branch). Ignored when the
  branch already exists.
- `--worktree-repo DIR` — create the worktree in a repo other than the
  one containing `--cwd` (the monorepo sub-repo case): the agent then
  launches in `--cwd` and the worktree path/branch ride into its
  welcome message, rather than the agent launching inside the worktree.
- `--auto-focus` — open a terminal window attached to the new agent
  once it spawns. Off by default for the CLI (spawns are usually
  programmatic); the dashboard modal still defaults its checkbox on.
- `--no-group-context` — opt the new agent out of the group's shared
  startup context (delivered by default, as on every other spawn path).

### Worktree resolution

The CLI resolves the worktree itself, in-process, via the `worktree`
package — the same `worktree.AddWorktreeIn` git operation the
dashboard's worktree picker performs server-side through
`/api/worktrees`. It then sends the resolved `cwd` / `worktree_path` /
`worktree_branch`, the **identical wire shape** the dashboard already
sends, so the daemon's spawn handler needed no behaviour change.

- Common case (`--worktree` only, or `--worktree-repo` == `--cwd`): the
  worktree becomes the spawn `cwd` — the agent launches inside it.
- Monorepo case (`--worktree-repo` points at a different repo): the
  agent launches in `--cwd`; `worktree_path`/`worktree_branch` ride
  along so the daemon's welcome tells it where to edit code.
- An existing worktree already checked out on the branch is reused;
  otherwise a fresh one is created.
- If the spawn request then fails, a freshly-created worktree is torn
  back down (`worktree.RemoveLinkedWorktree`) so a retry starts clean —
  the branch is kept, so the retry reuses it. The one exception is a
  `504` conv-id-poll timeout: the spawn subprocess did launch and the
  new CC may still be coming up inside the worktree, so the dir is left
  in place rather than yanked out from under a recovering session.

## Single source of truth

`agent.SpawnRequest` is a new shared Go struct — the one JSON body type
for `POST /v1/groups/{name}/spawn`. `tclaude agent spawn`,
`tclaude --join-group`, and agentd's `handleGroupSpawn` all use it
(previously the CLI marshalled an ad-hoc `map[string]any` and the
daemon decoded a private anonymous struct, which could silently drift).
The dashboard's JS spawn body already uses the same JSON keys, so all
three spawn surfaces share one contract.

## Files

- `pkg/claude/agent/spawn.go` — new `SpawnRequest` type; `SpawnParams`
  gains `--worktree`, `--worktree-base`, `--worktree-repo`,
  `--auto-focus`, `--no-group-context`; `RunSpawn` resolves the
  worktree, builds a `SpawnRequest`, and cleans up an orphaned worktree
  on spawn failure.
- `pkg/claude/agent/spawn_worktree.go` — new: `resolveSpawnWorktree`
  (reuse-or-create) and `removeSpawnWorktree` (failure cleanup).
- `pkg/claude/agent/join_group.go` — `RunJoinGroup` now builds a
  `SpawnRequest` instead of a `map[string]any`.
- `pkg/claude/agentd/lifecycle.go` — `handleGroupSpawn` decodes
  `agent.SpawnRequest` instead of a private anonymous struct.
- `pkg/claude/agentd/spawn_cli_worktree_flow_test.go` — new flow tests:
  `TestSpawnCLI_WorktreeCreatesAndLaunchesInIt` (worktree created + the
  agent's cwd is the worktree),
  `TestSpawnCLI_WorktreeRepoMonorepoRidesAlong` (monorepo: agent
  launches in `--cwd`, welcome names the worktree),
  `TestSpawnCLI_NoGroupContextOptsOut` (flag toggles group-context
  delivery), `TestSpawnCLI_WorktreeModifiersRequireWorktree` (usage
  errors for the modifier flags without `--worktree`).
- `docs/agent.md` — spawn section: synopsis + new-flag prose.

## No daemon behaviour change

The daemon's spawn handler was unchanged except for the struct
refactor — it already accepted and validated `worktree_path` /
`auto_focus` / `include_group_context`. The CLI simply now produces the
same wire shape the dashboard always did. Spawn stays gated on
`groups.spawn`; the new flags ride that existing gate.
