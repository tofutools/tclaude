# Authority / safety — open

The mutating-groups gate (refuse if a `claude` / `node` ancestor is
found) is shipped. `~/.tclaude/config.json` has an `agent` section
with `default_permissions` and `permission_overrides[conv|prefix|title]`;
the daemon's `requirePermission()` consults overrides → defaults →
refuses. Humans (no CC ancestor) bypass entirely. Per-conv grants
live in `agent_permissions` (schema v9). See
`agent-self-service-permissions.md` for the slug list.

## Open

### Parent-tree detector refinements

Possible refinement: more granular config, e.g. allow `add` but not
`rm`/`create`. Useful if we want agents to self-onboard into known
groups.

Possible refinement: extend the same gate to other sensitive commands
(spawning new sessions, killing groups via `groups stop`). Map
command → required policy in config.

### More granular gates on the existing `groups …` mutating endpoints

Currently absolute via `requireHuman`; want them to also accept a
permission like `member.redesignate` so a delegated agent can run
specific verbs without holding the entire human bypass.

### Wildcard / pattern overrides for permission_overrides

`"role:reviewer": [...]` instead of pinning to a single conv-id.
Useful for granting permissions by role across a fleet of agents.
Design-heavy.

## Files
- `pkg/claude/agentd/identity.go` — `requirePermission`
- `pkg/claude/common/config/config.go` — `permission_overrides`
- `pkg/claude/agent/permissions.go` — CLI
