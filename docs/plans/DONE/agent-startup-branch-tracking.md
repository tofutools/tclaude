# Immutable startup branch for the location view (2026-05)

## The bug

The dashboard's Groups/Agents tabs render an agent's git branch as an
`init → now` pair (`branchCell` in `dashboard.html`). For an agent
that ran a plain `git checkout` mid-session it showed the pair
**inverted** — e.g. `init: feature-x, now: main` for a session that
genuinely started on `main` and is now on `feature-x`.

Root cause — the two values came from stores that didn't mean what
the labels said (`agent.ResolveLocation`):

| Label | Old source | What it actually was |
|---|---|---|
| `init` (`StartupBranch`) | `conv_index.git_branch` | the **current** branch — the scanner keeps the *last* `gitBranch` stamp (`convops.go` is explicitly last-wins), and `FreshConvRowResolved` live-rescans, so it tracks the latest turn |
| `now` (`CurrentBranch`) | `agent_workdir.branch` | the branch at the **last file edit** — stale after a `git checkout` with no edit since |

There was no genuine "startup branch" stored anywhere: the only record
of it is the `.jsonl`'s first turn, which the scanner discarded.

## What shipped

A real, immutable startup branch — first-wins, captured once.

### Schema (migration v31→v32)

`migrateV31toV32` adds `conv_index.git_branch_startup TEXT NOT NULL
DEFAULT ''`. Existing rows backfill to `''` and self-heal on the next
`.jsonl` rescan (the scanner fills the column; `UpsertConvIndex`
carries it through `ON CONFLICT`).

### Conv scanner

`parseJSONLSession` (`convops.go`) now keeps **two** branch values:
`GitBranch` stays last-wins ("current"); `GitBranchStartup` is
first-wins — the branch the first turn was stamped with, the branch
Claude Code launched on. `SessionEntry` / `db.ConvIndexRow` gain the
field; `UpsertConvIndex` + the 5 conv_index SELECTs + `scanOneConvIndex`
carry it; `conv cp` / `conv mv` copy it from the source.

### ResolveLocation

- `StartupBranch` = `conv_index.git_branch_startup` (immutable),
  falling back to `git_branch` for convs indexed before v32 until the
  rescan heals them.
- `CurrentBranch` = `conv_index.git_branch` (last-wins, the freshest
  signal — Claude Code re-stamps it every turn), unless the agent has
  moved into a worktree distinct from its launch dir, in which case
  that worktree's own branch (`agent_workdir.branch`) is the current
  one.

The frontend needed no change — `branchCell` already renders
`startup_branch` / `branch` as the `init` / `now` pair; it just gets
correct values now.

## Files

- `pkg/claude/common/db/migrate.go` — `currentVersion` 31→32,
  `migrateV31toV32`.
- `pkg/claude/common/db/convindex.go` — `ConvIndexRow.GitBranchStartup`,
  `UpsertConvIndex` + the 5 SELECTs + `scanOneConvIndex`.
- `pkg/claude/common/convops/convops.go` — `SessionEntry.GitBranchStartup`,
  first-wins capture in `parseJSONLSession`, `dbRowToEntry` /
  `entryToDBRow`.
- `pkg/claude/conv/cp.go`, `mv.go` — carry the field on copy/move.
- `pkg/claude/agent/location.go` — `ResolveLocation` rewrite.

## Tests

- `convops_test.go` — `TestParseJSONLSession_GitBranchFirstAndLastWins`
  (extended): pins both the first-wins `GitBranchStartup` and the
  last-wins `GitBranch` from one crafted `.jsonl`.
- `db/convindex_branch_test.go` (new) —
  `TestConvIndex_GitBranchStartupRoundtrip`: the column round-trips
  through Upsert/Get/List and `git_branch_startup` survives a rescan
  that moves `git_branch` forward.
- `agentd/agent_branch_flow_test.go` —
  `TestAgentBranch_LastWinsAfterMidSessionSwitch` (extended): an agent
  starts on `main`, switches to `feature-x` mid-session; the dashboard
  snapshot's member row must show `branch=feature-x` and
  `startup_branch=main`.

## Cross-references

- `dashboard-clickable-links.md` — the `init`/`now` branch pair this
  feeds, and the branch-compare / PR links derived from each branch.
