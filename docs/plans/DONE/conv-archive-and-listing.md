# Conv archive + `conv ls` listing features (2026-05)

Archived conversations get first-class soft-delete behaviour, both
for reincarnate's old-conv handling and for manual cleanup.

## Schema v17 — `conv_index.archived_at`

Canonical archived-state column. Reincarnate stamps the column
directly on the old conv (alongside the cosmetic `/rename
<prev>-x` injection — the column is canonical, the title suffix
is the visible UX cue). `(*SessionEntry).IsArchived()` checks
the column FIRST (preferred) and falls back to the title suffix
for legacy convs that pre-date the column.

`UpsertConvIndex` deliberately omits `archived_at` from its
ON CONFLICT update so a routine .jsonl rescan never clobbers
the flag. Helper `db.SetConvIndexArchived(convID, archived)` is
the canonical write path.

`convops.IsArchivedTitle(customTitle)` /
`(*SessionEntry).IsArchived()` is the canonical check; consumers
should reuse it rather than open-coding the suffix test.

## CLI

- `tclaude conv archive <selector>`
- `tclaude conv unarchive <selector>`

Both call `db.SetConvIndexArchived` directly, for manual cleanup
of orphan / abandoned convs without a rename.

## Terminology rename: "expired" → "archived" (commit 7cf609b)

Originally shipped under "expired" terminology; renamed to
"archived" in the same release to unify with `groups archive`
(same conceptual soft-delete state, same default-hidden listing
behaviour).

## `conv ls` — default-hide `-x` rows (commit 8b01e05)

Default behaviour now hides archived rows. `--show-archived`
opt-in flag reveals them.

## `conv ls -w` archived toggle

Defaults to hiding `-x` rows; press `x` to toggle (mnemonic:
press `x` to see convs marked with `-x`). Originally `e` for
"expired"; remapped to `x` after the rename. Delete actions
freed up: `del` / `backspace` / `ctrl+d` still trigger delete.

Composes with both text-search and semantic-search filters via
the same `applySearchFilter` / `rebuildSemanticFiltered` pass.
Status-line message confirms the toggle on every press; help
screen lists the binding under Actions.

## `conv ls -w` GROUPS column polish

- **Sort + search on GROUPS column** (commit 5887920) — column
  works like the others.
- **`f` key opens group-name filter** in normal mode; Enter
  applies, Esc clears. Composes with `/` search and `x` archived
  toggle as three independent filter passes (entry must pass
  all). Status line + header indicate active filter ("Group:
  [alpha]" or "Search: [...] + Group: [alpha]"). Help screen
  updated. Unit tests added: `TestApplyGroupFilter_*` (filters,
  composes-with-search, empty-shows-all, case-insensitive),
  `TestMatchesGroupFilter` (table-driven predicate verification).

## Reincarnate's `-x` suffix injection

When reincarnating, the daemon injects `/rename <prevTitle>-x`
into the OLD pane right before the `/exit`, writing a custom-
title record to the old conv's .jsonl. The watch model /
FreshConvRow refresh picks it up the next time someone looks at
the conv, so dashboards / `conv ls` can render `worker-x` (dead)
distinctly from `worker-r-1` (live successor). Idempotent — no
double-suffix on retries. Mnemonic: `-x` = archived.
