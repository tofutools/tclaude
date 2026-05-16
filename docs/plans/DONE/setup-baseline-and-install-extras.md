# `tclaude setup`: always-run baseline + additive `--install-*` extras (2026-05)

Reworked `tclaude setup` so the baseline integration is never skipped,
and the `--install-*` flags are purely additive extras on top.

## The bug

`runSetup` had a "focused branch": when `--install-agent-skills` or
`--install-default-agent-permissions` was set, it skipped the entire
setup flow and ran only that one action. So a machine first set up with
`tclaude setup --install-agent-skills` ended up with skills but **no
hooks** — tclaude session status tracking silently never worked.

## The model that shipped

1. **The baseline always runs**, regardless of flags. The baseline is
   the no-flag content of `tclaude setup`: tmux prerequisite check,
   hooks (`~/.claude/settings.json`), status bar, the `tclaude://`
   protocol handler / clickable-notification setup, and the
   notifications config.
2. **Each `--install-*` flag adds an optional extra** on top of that
   baseline — it never replaces or gates it.
3. **`--install-all`** is shorthand for passing every `--install-*`
   flag.

`tclaude setup` → baseline only.
`tclaude setup --install-agent-skills` → baseline + skills.
`tclaude setup --install-all` → baseline + skills + default perms.

An intermediate cut of this work added an `--install-hooks` flag; it
was dropped. Hooks are baseline content, so a flag for them would add
nothing — it would contradict "each `--install-*` flag adds *additional*
content".

## CLI surface

- `--install-all` — new. Enables every optional extra.
- `--install-agent-skills` — unchanged flag; help text now says "Also
  install …" to signal it layers on the baseline.
- `--install-default-agent-permissions` — likewise "Also grant …".
- The focused branch is deleted; `runSetup` runs the baseline linearly,
  then calls `installExtras(params)` for the flag-gated extras.

## Files

- `pkg/claude/setup/setup.go` — `Params`: dropped `InstallHooks`, added
  `InstallAll`. Deleted the focused branch. New `installExtras(params)`
  helper (gated by `InstallAgentSkills`/`InstallDefaultAgentPerms`/
  `InstallAll`), called at the end of `runSetup`. Updated the cobra
  `Long` description.
- `pkg/claude/setup/setup_test.go` — new. Isolated temp `HOME`/
  `USERPROFILE`:
  - `TestInstallExtras_NoFlags_NoOp` — baseline-only setup installs no
    extras.
  - `TestInstallExtras_SkillsOnly` / `_PermsOnly` — each flag installs
    only its own extra.
  - `TestInstallExtras_All` — `--install-all` installs every extra.
  - `TestInstallExtras_AllEqualsBothFlags` — `--install-all` ≡ passing
    both individual flags.
  - `TestInstallExtras_Idempotent` — no duplicate permission slugs on
    re-run.
  - `TestRunSetup_BaselineRunsAlongsideExtras` — end-to-end: the
    baseline still installs hooks when an `--install-*` flag is passed
    (the regression guard for the deleted focused branch). Guarded to
    run only on native Linux (macOS may `brew install`; WSL writes a
    Windows registry key); skips if tmux is absent.
