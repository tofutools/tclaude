# Polish post-PR #47 (2026-05)

Small polish items that don't justify their own dedicated file
but matter for usability. Grouped chronologically.

## Listings + tables

- **`pkg/claude/common/table` rendering across agent list views**
  — unified bubbletea-based interactive table for `agent ls`,
  `groups ls`, `groups members`, etc.
- **`groups ls` MEMBERS + ONLINE columns** — at-a-glance team
  health.
- **Groups column on `conv ls` / `conv ls -w`** — see which
  groups a conv belongs to from the conv list.
- **ONLINE indicator on `agent ls` and `groups members`** —
  ● online / ○ offline marker.

## Mutations

- **`groups update-member`** — update alias / role / descr in
  place without remove + re-add.

## Self-rename

- **`tclaude agent rename "<title>"`** — slug `self.rename`.
  Builds the `requirePermission()` framework with config
  defaults + overrides; `agent-rename` skill bundled. Used the
  same `injectSlashCommand` path the dashboard would later
  reuse.

## CLI ergonomics

- **Shell autocompletions** across `tclaude agent(d)` — group
  names, conv selectors (with title descriptions), permission
  slugs, message targets (`group:` prefix), inbox message IDs,
  `--ask-human` durations. Wired via boa
  `InitFuncCtx` + `SetAlternativesFunc`.

## Docs

- **User-facing docs** — `docs/agent.md` + navbar entry.

## Misc

- **Adjust review prefix and make it configurable** (commit
  17bfd31).
