# Statusbar-driven dashboard cwd/branch freshness — SHIPPED

The dashboard's "cwd:NOW / branch:NOW" cells now update on Claude Code's
statusline render cadence instead of waiting for a `.jsonl` turn append.
A plain `git checkout` in an idle launch dir reaches the dashboard
within seconds, matching what the terminal statusbar already shows.

## The problem

Before this change, three writers populated the dashboard's location
data:

- `sessions.cwd` — set once, at session launch.
- `conv_index.git_branch` — last-wins gitBranch from each `.jsonl` turn
  entry. Only refreshes when CC appends a turn (now driven by the
  fsnotify monitor).
- `agent_workdir` — written by the `PostToolUse` hook. Only fires on
  tool calls.

None of those fires when the user runs `git checkout` in the launch
dir of an idle agent. The dashboard stayed on the previous branch for
many minutes, while the terminal statusline updated almost immediately
— the statusbar runs on CC's render cadence regardless of agent
activity, and it shells out to `git branch --show-current` live.

## What shipped

### `agent_workspace` table — `pkg/claude/common/db/agent_workspace.go` (NEW)

Schema v46:

```sql
CREATE TABLE agent_workspace (
    conv_id        TEXT PRIMARY KEY,
    cwd            TEXT NOT NULL DEFAULT '',
    branch         TEXT NOT NULL DEFAULT '',
    repo_url       TEXT NOT NULL DEFAULT '',
    default_branch TEXT NOT NULL DEFAULT '',
    pr_number      INTEGER NOT NULL DEFAULT 0,
    pr_url         TEXT NOT NULL DEFAULT '',
    pr_state       TEXT NOT NULL DEFAULT '',
    updated_at     TEXT NOT NULL DEFAULT ''
);
```

Accessors: `UpsertAgentWorkspace`, `GetAgentWorkspace`,
`DeleteAgentWorkspace`. One row per conv-id; upsert overwrites in
place; `UpdatedAt` defaults to `time.Now()` when the caller leaves it
zero.

### Statusbar writer — `pkg/claude/statusbar/statusbar.go`

- `StatusLineInput` learned `SessionID string \`json:"session_id"\`` so
  the writer can key on whichever conv-id Claude Code is using *right
  now* (survives `/clear` rotation). Falls back to `TCLAUDE_SESSION_ID`
  on older CC versions.
- `getGitInfo` refactored into a `getGitData() *cachedGitData` helper
  so `run()` can use both the rendering AND publish the structured
  fields without re-paying for `git`/`gh`.
- `cachedGitData` gained `PRNumber`/`PRState` (previously only the URL
  was cached).
- `getPRURL` → `getPRInfo`: returns number + url + state via
  `gh pr view --json number,url,state`.
- After resolving git data, `run()` calls `db.UpsertAgentWorkspace`
  with cwd + branch + repo_url + default_branch + PR snapshot. Best-
  effort; failure logs a warning and never blocks the statusline.

### `ResolveLocation` precedence — `pkg/claude/agent/location.go`

Three writers can now contribute to `CurrentBranch`/`CurrentDir`.
"Most-recent wins" rule:

- **Launch-dir case** (agent hasn't moved, or last edit was in-tree of
  the launch dir): `agent_workspace.branch` supersedes
  `conv_index.git_branch` when `agent_workspace.UpdatedAt` is newer.
  The statusbar's render cadence usually dominates the per-turn cadence
  of conv_index, so this is the common path now.
- **Moved case** (agent has Bash'ed into a worktree distinct from the
  launch dir): `agent_workdir` stays in charge — the statusbar can only
  see CC's launch dir, never the worktree, so its row would be wrong.

### Branch links — `pkg/claude/agentd/branchlinks.go`

`branchLinksFor(convID, loc)` now takes the conv-id and, when
`agent_workspace` has a matching repo+branch, prefers its
`repo_url` + PR snapshot over the bl_ git_cache. Bridges the 5–90s
gap between a branch flip and the next async `bl_` refresh:
agent_workspace already paid for `git` + `gh` on the statusbar's
render, so the dashboard's Branch column links light up the moment
the statusbar publishes.

Three dashboard call sites updated to pass `convID`:
`dashboard.go:643/695/723`.

### Migration — `pkg/claude/common/db/migrate.go`

`migrateV45toV46`: `CREATE TABLE agent_workspace`. `currentVersion`
bumped to 46. The literal version pin (`require.Equal(t, 46,
currentVersion, "currentVersion is 46")`) moved into the new
`migrate_v46_test.go`; the v45 test was simplified.

## Tests

- `pkg/claude/common/db/agent_workspace_test.go` — round trip, PR
  fields clearing on re-upsert, empty-convID no-op.
- `pkg/claude/common/db/migrate_v46_test.go` — raw v45→v46 + the
  through-the-full-chain pin.
- `pkg/claude/agent/location_test.go` — added
  `TestResolveLocation_WorkspaceFreshensLaunchBranch` (fresher
  workspace beats conv_index for the launch-dir case; older workspace
  loses to a fresher conv_index turn append) and
  `TestResolveLocation_WorkspaceSkippedWhenMoved` (workspace never
  clobbers the moved-worktree path).
- `pkg/claude/agentd/dashboard_workspace_freshness_flow_test.go` —
  end-to-end via `/api/snapshot`: agent on `feature-old`, write a
  workspace row for `feature-new`, the next snapshot reports the new
  branch + its PR # + branch web URL without any `.jsonl` turn append.

## Known limitations / follow-ups

- The agent_identity_migration path (`/clear` / reincarnate conv-id
  rotation) doesn't rekey `agent_workspace` rows. Same shape as
  `context_snapshot` and `agent_workdir` — those don't migrate either.
  Worth a unified pass next time the migration touches these tables.
- A killed agent leaves a stale `agent_workspace` row; nothing GCs it.
  Same as `agent_workdir` — both could share a sweep over conv_index
  retire-time cleanup.
- The statusbar uses CC's `workspace.current_dir` (CC's launch dir),
  not the agent's live edit dir; the dashboard's `CurrentDir` for the
  launch-dir case therefore still equals `StartupDir`. For the moved
  case `agent_workdir` carries the worktree path — unchanged behavior.
