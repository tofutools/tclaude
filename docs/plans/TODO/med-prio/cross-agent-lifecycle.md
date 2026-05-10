# Cross-agent lifecycle (manager pattern) — open follow-ups

The manager pattern: an elevated agent (or group owner) acts on
*other* agents. Typical use: a manager watching workers, reincarnating
the ones whose context has rotted with a follow-up pointing them at
the next batch of work.

Permission model:
- `self.<verb>` — operate on yourself only.
- `agent.<verb>` — operate on another agent (target by selector).
  Default human-only; granted to manager agents explicitly.
- **Group ownership grants implicit power.** A group owner can call
  any `agent.<verb>` against any member of a group they own without
  needing the slug. Powered by `ownerOfGroupContaining(caller, target)`
  in `pkg/claude/agentd/agent_dispatch.go`.

Endpoints follow `/v1/agent/{selector}/{verb}`. Audit:
`granted_by = system:<verb>:by=<caller-conv>` for cross-agent calls.

Shipped (2026-05):
- `agent.reincarnate` / `agent.compact` / `agent.rename` /
  `agent.clone` / `agent.stop` / `agent.resume` / `agent.schedule`
  slugs + `--target <peer>` CLI flags. All routed through shared
  `run<Verb>Orchestration` helpers.
- Group-owner implicit power.
- Handoff message FromConv = caller.

## Open

- **X-Tclaude-Ask-Human on cross-agent endpoints.** Today
  `requireCrossAgentPermission` doesn't honor the popup header
  (manager pattern is opt-in via explicit grants).
  `pkg/claude/agentd/agent_dispatch.go:77-80` documents the gap.
  Re-evaluate when a use case appears — e.g. a manager that wants to
  act on a peer it doesn't normally manage with one-off escalation.
  Scope: ~10 lines: call `parseAskHumanHeader` in
  `requireCrossAgentPermission` and route to the existing popup logic.
  Design question: should approval show it's a cross-agent request
  ("Agent X wants to reincarnate Agent Y")?
- **Orthogonal vs. implication.** Today `agent.<verb>` and
  `self.<verb>` are orthogonal — granting one doesn't grant the other.
  Revisit if managers always also want self-management.

## Files
- `pkg/claude/agentd/agent_dispatch.go` — `requireCrossAgentPermission`
- `pkg/claude/agentd/identity.go` — `parseAskHumanHeader`
