# Agent clone — open follow-ups

`tclaude agent clone [follow-up] [--no-copy-conv] [--target <peer>]`
ships. Identity is copied (not migrated) so the original keeps every
group membership / permission grant / ownership. Clone gets a copy
with a `<base>-c-<N>` per-group alias. Conv jsonl copy is the default;
`--no-copy-conv` flips to blank context. No `/exit` on the original.

Slugs: `self.clone` (default-granted alongside `self.compact` /
`self.reincarnate`) and `agent.clone` (default human-only;
manager pattern). Both routed through `runCloneOrchestration`.

Rate limiting shipped 2026-05 (commit fc2f9cc): 1-clone-per-cooldown
per source conv. `agentd.CloneCooldown` exported (default 1m); flow
tests shrink it. Atomic INSERT-WHERE-NOT-EXISTS via
`db.ClaimCloneSlot`.

## Open

- **--no-copy-conv polish.** Today the no-copy path uses the same
  poll-for-new-conv-id loop as reincarnate; CC has to mint the
  conv-id before identity can be copied. Hopefully fast enough. If
  it ever grows slow, consider pre-seeding a placeholder row in
  `sessions` so identity copy can happen synchronously.

## Files
- `pkg/claude/agentd/clone.go` — orchestration + rate limit
- `pkg/claude/common/db/agent_clone_history.go` — rate-limit storage
