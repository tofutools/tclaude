# Agent self-lifecycle (2026-05)

Self-driven context management for long-running agents — `compact`,
`reincarnate`, `clone`, plus a read-only `context-info`.

## Verbs

- `tclaude agent compact [follow-up]` — daemon injects `/compact`
  into caller's pane; optional follow-up queues as next prompt.
  Slug: `self.compact`. Default-granted.
- `tclaude agent reincarnate [follow-up]` — full identity
  migration. Daemon snapshots groups / permissions / ownership,
  spawns a fresh tclaude session, migrates the snapshot to the new
  conv-id in a single transaction, soft-stops the old conv with
  `/exit`. Old pane gets a `/rename <prev>-x` archive marker
  (column `archived_at` is canonical; suffix is the visible UX
  cue). New pane gets the follow-up via an `agent_messages` row +
  flush — more reliable than racing tmux send-keys against pane
  startup.
  - **Auto-detach-and-reattach**: daemon runs `tmux list-clients
    -t <old>` and `tmux switch-client -c <tty> -t <new>` for each
    attached client right before injecting `/exit`. Carry-over
    count surfaced as `switched_clients` in the response.
  - Slug: `self.reincarnate`. Default-granted.
- `tclaude agent clone [follow-up] [--no-copy-conv] [--target]` —
  identity copied (NOT migrated). Original keeps every group
  membership / permission grant / ownership. Clone gets a copy with
  a `-clone-<N>` alias suffix per group. Conv jsonl copy is
  default (`convops.CopyConversationToPath` + `tclaude session
  new -r <new-conv>`); `--no-copy-conv` flips to blank context.
  Both running after the call (no `/exit` on the original).
  Slug: `self.clone`. Default-granted. Cross-agent: `agent.clone`.
- `tclaude agent context-info` — reads `sessions.context_pct` +
  `compact_pending`. No slug (read-only).

## Skill

`agent-lifecycle` skill bundled with thresholds (~50% on 1M
context, ~75% on 200k) and the "keep a navigable index, don't
reload massive context after compact" pattern.

## Continuity contract

Identity migrates; *task state* doesn't. The agent must persist
work-in-progress (decisions, plan, partial results, file paths,
next step) to disk *before* calling reincarnate. The project's
CLAUDE.md should document where progress is written and how a
freshly-reincarnated agent reloads enough to continue.

## Files

- `pkg/claude/agentd/runReincarnationOrchestration` (shared by
  self + cross-agent paths)
- `pkg/claude/agentd/runCloneOrchestration` (same)
- `pkg/claude/common/convops/CopyConversationToPath`

See also: `clone-and-suffix-scheme.md` for clone alias /
rate-limit details, `cross-agent-manager-pattern.md` for the
`--target` cross-agent variant.
