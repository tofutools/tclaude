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

The cooldown is configurable as of 2026-05: `tclaude agentd serve
--agent-clone-cooldown <duration>` (tier 1) or the
`agent.clone_cooldown` config.json field (tier 2) overwrite
`CloneCooldown` at daemon startup; the 1m `defaultCloneCooldown` const
is tier 3. `0` disables the cooldown; an unparseable/negative value at
a tier is warned and skipped. `agentd.resolveCloneCooldown` does the
resolution — unit-tested in `clone_cooldown_test.go`.

The cooldown applies only to **agent-initiated** clones (`caller != ""`
in `runCloneOrchestration`) — the threat model is a runaway agent loop.
Human-initiated clones (CLI / dashboard, `caller == ""`) skip
`db.ClaimCloneSlot` entirely and never touch `agent_clone_history`.
Manager *agents* cloning peers via `agent.clone` keep a non-empty
caller and stay limited. Flow coverage in `clone_rate_limit_flow_test.go`
(agent paths use the manager-pattern caller; `TestClone_RateLimitExemptsHuman`
pins the human bypass).

## Open

- **--no-copy-conv polish.** Today the no-copy path uses the same
  poll-for-new-conv-id loop as reincarnate; CC has to mint the
  conv-id before identity can be copied. Hopefully fast enough. If
  it ever grows slow, consider pre-seeding a placeholder row in
  `sessions` so identity copy can happen synchronously.

## Files
- `pkg/claude/agentd/clone.go` — orchestration + rate limit; `CloneCooldown` / `defaultCloneCooldown`
- `pkg/claude/agentd/serve.go` — `--agent-clone-cooldown` flag + `resolveCloneCooldown`
- `pkg/claude/common/config/config.go` — `AgentConfig.CloneCooldown` field
- `pkg/claude/common/db/agent_clone_history.go` — rate-limit storage
