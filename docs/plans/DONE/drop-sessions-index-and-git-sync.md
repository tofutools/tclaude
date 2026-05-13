# Drop sessions-index.json reads + remove git-sync entirely

## What shipped

Two intertwined cleanups: removed the `tclaude git` subcommand and all
of `pkg/claude/git/` + `pkg/claude/syncutil/`, and dropped tclaude's
own reliance on the legacy `sessions-index.json` file. The file is
still surgically maintained on conv mutations for any external tooling
that may read it (Claude Code's own resume UI is the suspected
consumer), but tclaude never reads from it for its own listings,
lookups, or prune logic — SQLite (`conv_index`) is the system-of-truth
everywhere.

## Surface changes

### CLI

- `tclaude git` (and every subcommand: `init`, `sync`, `status`,
  `repair`, `fetch`) is **gone**. The cobra registration in
  `pkg/claude/claude.go` no longer wires `claudegit.Cmd()`.

### Docs

- `docs/git-sync.md` deleted.
- `mkdocs.yml` nav no longer lists `Git Sync`.
- `docs/index.md` "Documentation" section no longer links to
  `git-sync.md`.
- Root `CLAUDE.md` package table no longer mentions the `git` package
  or `~/.claude/projects_sync`; the `conv` row was updated to call out
  SQLite as the source-of-truth and explain that sessions-index.json
  is "written-but-never-read for external-tooling compatibility."

## Code deletions

- `pkg/claude/git/` — entire package gone (10 files including
  `sync.go`, `repair.go`, `status.go`, `init.go`, `fetch.go`,
  `config.go`, `git.go`, plus tests).
- `pkg/claude/syncutil/` — entire package gone (`syncutil.go` +
  `syncutil_test.go`).
- `pkg/claude/conv/delete.go::AddTombstoneForProject` helper removed.
- `convops.LoadSessionsIndexFile` removed (only used by the deleted
  git-sync repair/status tooling).
- `pkg/claude/conv/conv.go` re-exports: dropped `LoadSessionsIndexFile`;
  renamed `SaveSessionsIndex` re-export to the new surgical pair below.

## Code changes — sessions-index.json maintenance

`convops.SaveSessionsIndex(projectPath, *index)` (full overwrite) was
**replaced** by two surgical helpers in `pkg/claude/common/convops/convops.go`:

- `RemoveSessionsIndexEntry(projectPath, sessionID)` — drops one entry
  by ID; preserves all other entries verbatim and any unknown
  top-level fields.
- `UpsertSessionsIndexEntry(projectPath, entry SessionEntry)` —
  inserts/replaces one entry; preserves other entries' unknown
  per-entry fields and unknown top-level fields.

Both helpers **no-op when the file doesn't exist** — we never create
it; we only maintain it for external tooling that already writes it.
Both go through `json.RawMessage` round-trips so unknown fields
survive forward-compat with future tclaude versions or other writers.

Call sites updated:

- `pkg/claude/conv/cp.go` — `UpsertSessionsIndexEntry` after copy.
- `pkg/claude/conv/mv.go` — `RemoveSessionsIndexEntry` on src,
  `UpsertSessionsIndexEntry` on dst.
- `pkg/claude/conv/delete.go::DeleteConvByID` — `RemoveSessionsIndexEntry`.
- `pkg/claude/conv/prune.go` — `RemoveSessionsIndexEntry` per pruned
  conv (and per dangling dir, just in case).
- `pkg/claude/conv/watch.go` — interactive delete path uses
  `RemoveSessionsIndexEntry`.
- `pkg/claude/common/convops/convops.go::CopyConversationToPath` —
  internal worktree-add path uses `UpsertSessionsIndexEntry`.

Also dropped all `syncutil.IsInitialized()` / `AddTombstoneForProject`
plumbing from those files.

## Read-side migration: legacy file → SQLite

`pkg/claude/conv/prune.go` was reading the legacy sessions-index.json
to decide "which convs are tracked" (for the `(indexed)` display tag)
and to find "index entries with no .jsonl on disk."

- `loadSessionsIndexOnly` helper removed.
- `findEmptyConversations` now pulls indexed-IDs from
  `db.ListConvIndex(projectPath)`.
- `findMissingFileEntries` now walks `db.ListConvIndex(projectPath)`
  rows and checks each file on disk.

## Tests

- `pkg/claude/common/convops/convops_test.go`:
  - Old `TestSaveSessionsIndex_WritesParseableFile` deleted.
  - New `TestSessionsIndex_SurgicalUpdatesPreserveUnknownFields`
    seeds a file with extra top-level + per-entry fields, runs an
    upsert-replace + upsert-insert + remove, and asserts the unknown
    fields survive.
  - New `TestSessionsIndex_NoFileNoCreate` asserts the helpers never
    create the file when it doesn't already exist.

- `pkg/claude/conv/prune_test.go`:
  - `setupTestDB` + `indexInDB` helpers added (mirrors what
    `convops_test.go::setupTestDB` does — per-test isolated HOME,
    `db.ResetForTest`).
  - `TestFindEmptyConversations_IndexedFlag`,
    `TestFindMissingFileEntries`,
    `TestRunPruneEmpty_DanglingDirExclusion` now seed `conv_index`
    via `db.UpsertConvIndex` instead of writing a fake
    sessions-index.json.

## Verification

- `go build ./...` — clean.
- `go test ./...` — all packages pass.
- `golangci-lint run ./...` — 0 issues.

## Why this matters

`pkg/claude/git/` was experimental cross-device sync built on top of
the legacy sessions-index.json file (with `.tombstones` markers in a
separate `~/.claude/projects_sync` git working tree). It was always
flagged as "may eat your data" in `docs/index.md`, and the rise of
agentd / multi-agent coordination on a single host made the
cross-device story irrelevant for now (see
`docs/plans/TODO/future/cross-machine.md` for the deferred sketch).

The sessions-index.json file itself has been a dead-letter cache for
several releases — `LoadSessionsIndex` switched to SQLite cache +
disk-walk on conv_index migration. The only thing tying us to the
file was a worry that Claude Code's own resume UI might consult it.
By keeping the file under surgical maintenance instead of dropping it
entirely, we get the cleanup benefits (no git-sync footgun, no
full-file rewrite, no read-from-file in our hot paths) without
risking external-tooling regressions.

## Follow-ups deferred

- `docs/plans/TODO/med-prio/fk-cascade-conv-id.md` captures the
  next-up cleanup: FK + ON DELETE CASCADE from every `conv_id` column
  to `conv_index.conv_id`. That collapses
  `db.DeleteAgentByConvID`'s hand-maintained child-table list to a
  single `DELETE FROM conv_index`. Schema-level, separate concern.
- `docs/plans/TODO/future/cross-machine.md` continues to describe the
  deferred cross-host coordination story; the git-sync sketch in
  there now refers to a package that no longer exists, which is
  acknowledged as legacy context for any future implementer.
