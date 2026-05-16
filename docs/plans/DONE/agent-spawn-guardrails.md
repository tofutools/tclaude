# Agent-spawn guardrails — shipped 2026-05

`tclaude agent spawn` (`POST /v1/groups/{name}/spawn`) is now safely
delegatable to an **agent**. `groups.spawn` is still human-only by
default, but the human can now grant it to a coordinator agent without
risking a runaway recursive spawn — a guardrail layer caps what a
spawn-capable agent can do.

Deliberately **no new permission slug**: the feature is the guardrail
layer on the existing `PermGroupsSpawn` (`groups.spawn`), not a new
permission. Humans (no claude ancestor) bypass the agent-only
guardrails, as everywhere else in the daemon.

## The three guardrails

Enforced by `checkSpawnGuardrails` in `pkg/claude/agentd/spawn_guardrails.go`,
called from `handleGroupSpawn` (`lifecycle.go`) right after the body
decode and before any subprocess is launched.

1. **Group restriction** (agent-only, default ON). An agent may only
   spawn into a group it is a **member or owner** of. Owner counts
   alongside member — a group owner already wields unilateral power
   over members elsewhere in the daemon, and a coordinator agent that
   grows a team is typically an owner. Refusal: `403 group_restricted`.
   - Widened by an allowlist of group names (`spawn_allowed_groups`).
   - Disablable entirely (`spawn_group_restriction = false`).
2. **Rate limit** (agent-only). At most `spawn_max_per_hour` spawns per
   caller-agent per rolling hour (default 10; 0 = unlimited). Refusal:
   `429 rate_limited`. Modeled on the clone rate limit: an atomic
   count-and-insert (`db.ClaimSpawnSlot`) against an append-only
   `agent_spawn_history` table. The attempt is recorded up-front, so a
   failed spawn still consumes a slot — a retry loop can't drain the
   limit by burning rejected attempts.
3. **Max group size** (binds EVERY caller, the human included). A
   spawn that would push a group past `agent_groups.max_members` is
   refused with `409 group_full`. 0 = unlimited (the default / upgrade
   behaviour). A hard property of the group, not a limit on the caller
   — a human raises the cap to add more.

## Config (global knobs)

`~/.tclaude/config.json` → `agent` section (`config.AgentConfig`).
Resolved once into the agentd package vars at daemon startup
(`resolveSpawnGuardrailConfig`, called from `runServe`):

| Field | Type | Default | Meaning |
|-------|------|---------|---------|
| `spawn_group_restriction` | `*bool` | nil → on | Toggle guardrail 1 |
| `spawn_allowed_groups` | `[]string` | empty | Group names an agent may always spawn into |
| `spawn_max_per_hour` | `*int` | nil → 10 | Guardrail 2 cap; 0 disables |

The per-group `max_members` cap is NOT config — it is a hard property
of the group (DB column), set per-group.

agentd tunables (package vars, flow tests shrink them like
`CloneCooldown`): `SpawnGroupRestriction`, `SpawnAllowedGroups`,
`SpawnMaxPerWindow`, `SpawnRateWindow`.

## Schema migrations

- **v32→v33** — `agent_groups.max_members INTEGER NOT NULL DEFAULT 0`.
- **v33→v34** — `agent_spawn_history(spawner_conv_id, spawned_at)` +
  index `idx_spawn_history_spawner`. Append-only audit; sibling of
  `agent_clone_history`.

## CLI / API / dashboard surface

- `tclaude agent groups set-max-members <group> [max]` — set/clear the
  cap (omit `max` or pass 0 to clear). Gated on `groups.rename`, like
  `set-default-dir` / `set-context`.
- `tclaude agent groups create --max-members N`.
- `groups ls` shows the member count against the cap (`3/10`).
- `POST /v1/groups` and `PATCH /v1/groups/{name}` accept `max_members`;
  `GET /v1/groups` returns it. Dashboard create-group form has a "Max
  members" field, and the Groups tab carries a clickable, inline-
  editable `👥 N/cap` chip per group (turns orange when the group is
  full).

## Files

- `pkg/claude/agentd/spawn_guardrails.go` — guardrail logic + tunables.
- `pkg/claude/agentd/lifecycle.go` — `handleGroupSpawn` calls `checkSpawnGuardrails`.
- `pkg/claude/agentd/serve.go` — `resolveSpawnGuardrailConfig` at startup.
- `pkg/claude/agentd/handlers.go` — `max_members` on group create / update / summary.
- `pkg/claude/common/db/agent_spawn_history.go` — `ClaimSpawnSlot` / `CountSpawnsSince`.
- `pkg/claude/common/db/migrate.go` — `migrateV32toV33`, `migrateV33toV34`.
- `pkg/claude/common/db/agent.go` — `AgentGroup.MaxMembers`, `SetAgentGroupMaxMembers`.
- `pkg/claude/common/config/config.go` — `AgentConfig` spawn knobs.
- `pkg/claude/agent/groups.go` — `set-max-members` CLI + `create --max-members`.
- `pkg/claude/agentd/dashboard.html` — create form field + `👥` chip.

## Tests

- `pkg/claude/agentd/spawn_guardrails_flow_test.go` — agent spawns into
  own group OK; owner-not-member OK; foreign group → 403; allowlist
  widens; restriction toggle off allows any; rate limit → 429 after N
  (and the refused attempt records no row); rate limit is per-caller;
  `max_members` full → 409 (even for the human); raising the cap
  releases it; human bypasses restriction + rate limit.
- `pkg/claude/common/db/agent_spawn_history_test.go` — `ClaimSpawnSlot`
  unit tests: empty spawner rejected; max≤0 = unlimited / no rows;
  cap enforced within window; per-spawner isolation; window expiry;
  `CountSpawnsSince`.

## Notes

- `member.add` is intentionally NOT gated by `max_members` — the
  runaway risk is *spawn* (it creates brand-new agents); `member.add`
  only attaches an existing conv. Scope kept to spawn per the design.
- The non-group `agent.spawn` slug (routing `tclaude session new`
  through the daemon) remains a separate open item — see
  `TODO/med-prio/agent-self-service-permissions.md`.
