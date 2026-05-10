# Agent self-service permissions — open

Mostly shipped (2026-05). The graduated permission model is in:
`requirePermission()` consults defaults + per-conv overrides per
permission slug. Humans (no claude ancestor) bypass entirely. See
`pkg/claude/agentd/identity.go` for the slug list and
`pkg/claude/agentd/permissions.go` for per-slug metadata.

Shipped slugs (default human-only unless noted):
- `member.add` / `member.remove` / `member.redesignate`
- `groups.create` / `groups.rm` / `groups.own` / `groups.spawn` /
  `groups.stop` / `groups.resume` / `groups.archive`
- `permissions.grant` / `permissions.revoke`
- `self.rename` / `self.compact` / `self.reincarnate` / `self.clone`
  / `self.schedule` (default-granted)
- `agent.rename` / `agent.compact` / `agent.reincarnate` /
  `agent.clone` / `agent.stop` / `agent.resume` /
  `agent.schedule` (cross-agent / manager pattern, default
  human-only; group-owner implicitly bypasses)

Storage: `agent_permissions` table (schema v9).

## Open

### `agent.spawn` slug

Generic "spawn a fresh CC session by some identifier (not tied to a
group)" slug. Today an agent can already call `tclaude session new`
directly (it doesn't route through the daemon), so there's nothing
for the daemon to gate yet. Routing `session new` through the daemon
would make this enforceable — bigger refactor, deferred.

## Files
- `pkg/claude/agentd/identity.go`
- `pkg/claude/agentd/permissions.go`
- `pkg/claude/session/new.go` (would change to route through daemon)
