# Permissions framework (2026-05)

Graduated trust model for what agents can do — defaults in
config, per-conv overrides in SQLite.

## CLI

`tclaude agent permissions ls | grant | revoke | slugs`.

## Storage split

- **Defaults** in `~/.tclaude/config.json` under `agent.default_permissions`.
- **Per-conv grants** in SQLite (`agent_permissions` table, schema v9).
- **Effective set** = `union(defaults, grants)`.

Per-conv overrides: `agent.permission_overrides[conv | prefix |
title]`.

## `requirePermission()`

Daemon's gate. Consults defaults + per-conv overrides per slug.
Humans (no `claude` ancestor) bypass entirely. See
`pkg/claude/agentd/identity.go` for the slug list and
`pkg/claude/agentd/permissions.go` for per-slug metadata.

## Slugs (default human-only unless noted)

| Slug | Default |
|------|---------|
| `member.add` / `member.remove` / `member.redesignate` | human-only |
| `groups.create` / `groups.rm` / `groups.own` / `groups.spawn` / `groups.stop` / `groups.resume` / `groups.archive` | human-only |
| `permissions.grant` / `permissions.revoke` | human-only (recursive) |
| `self.rename` / `self.compact` / `self.reincarnate` / `self.clone` / `self.schedule` | **default-granted** |
| `agent.rename` / `agent.compact` / `agent.reincarnate` / `agent.clone` / `agent.schedule` / `agent.stop` / `agent.resume` | human-only (group-owner implicitly bypasses against group members) |

`tclaude setup --install-default-agent-permissions` installs the
default-granted self-lifecycle slugs.

## Open follow-ups

See `med-prio/agent-self-service-permissions.md` for the residual
items (e.g. `agent.spawn` for non-group-tied spawning, wildcard /
pattern overrides like `"role:reviewer": [...]`).
