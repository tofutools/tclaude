# Group startup context

Shipped 2026-05.

A group can carry an optional **startup context** — a block of shared
guidance the human attaches to the group. When an agent is spawned
into that group, the daemon delivers the context to the new agent's
**inbox** as part of its spawn startup briefing. Each spawn can opt
out individually.

> Delivery note: this feature originally pasted the context into the
> new agent's pane via a bracketed tmux paste (`injectMultilineAndSubmit`).
> That was changed to inbox delivery — see "Spawn-time delivery" below
> — because a large briefing bracketed-pasted into CC's input box can
> overflow its input-size limit. The config surface (this whole doc bar
> that section) is unchanged.

## Why

A team of agents spawned into one group nearly always needs the same
orientation — what project they're on, where the repo is, which
conventions to follow, who to coordinate with. Before this, the human
typed that into every new agent by hand (or baked it into a CLAUDE.md
that only covers one repo). A per-group context makes "spawn into
group X" carry the right briefing for free, while staying optional —
groups that don't want one simply don't set it.

It is a deliberately flexible primitive: free-form text, no enforced
structure. The group decides what goes in it.

## Schema bump (v29)

```sql
ALTER TABLE agent_groups ADD COLUMN default_context TEXT NOT NULL DEFAULT '';
```

`migrateV28toV29` in `pkg/claude/common/db/migrate.go`. Empty string =
no group context (the pre-feature behaviour).

## DB layer

`pkg/claude/common/db/agent.go`:

- `AgentGroup.DefaultContext` field; `scanAgentGroup` + all six
  `SELECT … FROM agent_groups` queries carry the column.
- `SetAgentGroupDefaultContext(name, context)` — `UPDATE … SET
  default_context`. Returns rows-affected so callers can answer 404.
  `context == ""` clears.

## Daemon API

### Update

`PATCH /v1/groups/{name}` → `handleGroupUpdate` (`handlers.go`)
patches **two** fields — `default_cwd` and `default_context`. Both are
`*string`, so `""` clears and omitting leaves untouched; an empty body
(neither field) is a 400. `default_context` is run through
`normalizeGroupContext`: CRLF / lone-CR line endings are folded to LF
(so the briefing renders consistently regardless of where it was
authored) and the result is rejected with a 400 when it exceeds
`maxGroupContextBytes` (16 KiB). Gated on the **`groups.rename`** slug,
same as `default_cwd` — human-curated group config. Default human-only.

### Create

`POST /v1/groups` accepts an optional `default_context` in the body.
It's validated up front (`normalizeGroupContext`) and applied as a
post-create `SetAgentGroupDefaultContext` — keeps `CreateAgentGroup`'s
signature untouched (it's shared with the clone path and flow-test
helpers).

`PATCH /api/groups/{name}` and `POST /api/groups` (the dashboard
cookie-auth twins) delegate straight to the same handlers.

## Spawn-time delivery

`handleGroupSpawn` (`lifecycle.go`) takes a request field
`include_group_context` — a `*bool` so an omitted field means **opt-in**:
every spawn path (`tclaude agent spawn`, `/v1` API, `groups create
--member`, dashboard) inherits the group context by default, the same
way it inherits `default_cwd`. The dashboard sends `false` explicitly
when the human unticks the checkbox.

The group context does not get its own delivery turn. Instead it is
folded — together with the per-spawn `initial_message` — into a single
**startup briefing** that `handleGroupSpawn` inserts into the new
agent's inbox as one `agent_messages` row (`Subject: "Startup
context"`). `buildSpawnContextBody` assembles the body: a group-context
section (when present and opted-in) and a task-brief section (when an
initial message was supplied), `---`-separated, each under a plain-text
header. When both inputs are empty no message is inserted at all.

The spawn welcome line points the agent at that inbox message
(`tclaude agent inbox read <id>`); `runSpawnPostInit` marks it
delivered once the welcome lands. See
`DONE/dashboard-spawn-initial-message.md` for the briefing assembly,
the welcome's three-way trailing instruction, and the test coverage of
the merged delivery.

## CLI

```bash
tclaude agent groups set-context <group> [<context>] [--file <path>]
tclaude agent groups create <name> [--context <text>] [--context-file <path>]
```

`groups.go` — `groupsSetContextCmd` / `runGroupsSetContext` mirrors
`set-default-dir`. The context is the positional arg, or `--file` /
`--context-file` loads it from disk (better for multi-line). Omitting
both clears (set-context) or leaves empty (create). Tab-completion
suggests existing group names.

## Dashboard

- **Snapshot**: `dashboardGroup.DefaultContext` (`json:"default_context"`).
- **Create modal**: a "Startup context" `<textarea>`; the value rides
  the create POST as `default_context`.
- **Group header**: a `📋 startup context` / faint `📋 no startup
  context` chip beside the `📁` cwd chip. Clicking it opens a dedicated
  **group-context modal** (a `<textarea>` — context is multi-line, so
  unlike the cwd chip's inline `<input>` it gets a real editor). Save
  PATCHes `/api/groups/{name}` with `default_context`.
- **Spawn modal**: an "Include group default context" checkbox
  (default checked). `updateSpawnGroupContextRow` shows the row only
  when the selected group actually has a context — there's nothing to
  opt into otherwise — and re-shows/re-checks it when the group
  `<select>` changes. Submit sends `include_group_context`.

## Test coverage

`pkg/claude/agentd/group_default_context_flow_test.go`:

- `TestGroupDefaultContext_InjectedOnSpawn` — PATCH stores it; a spawn
  with the flag omitted (opt-in default) delivers it to the agent's
  inbox; the welcome points there.
- `TestGroupDefaultContext_OptOutSkipsInjection` — `include_group_context:
  false` with no task brief → no inbox message; welcome says "wait".
- `TestGroupDefaultContext_NoContextNoInjection` — a context-less group
  spawned with no brief → no inbox message.
- `TestGroupDefaultContext_PatchClears` — `default_context:""` clears the
  row; a later spawn no longer inherits it.
- `TestGroupDefaultContext_CreateWithContext` — `POST /v1/groups` with
  `default_context` stores it.
- `TestGroupDefaultContext_MultilinePreserved` — a 3-line context
  reaches the inbox message verbatim.
- `TestGroupDefaultContext_MergedWithInitialMessage` — group context +
  initial message land in ONE inbox briefing, group context first.
- `TestGroupDefaultContext_PatchTooLongRejected` — >16 KiB → 400,
  nothing persisted.
- `TestGroupDefaultContext_PatchNormalizesCRLF` — CRLF / lone-CR folded
  to LF on store.

## Files

- `pkg/claude/common/db/migrate.go` — `migrateV28toV29`, `currentVersion = 29`
- `pkg/claude/common/db/agent.go` — `DefaultContext`, `SetAgentGroupDefaultContext`,
  `scanAgentGroup` + group SELECTs
- `pkg/claude/agentd/handlers.go` — `handleGroupUpdate` two-field patch,
  `handleGroups` create-time context, `normalizeGroupContext`,
  `maxGroupContextBytes`
- `pkg/claude/agentd/lifecycle.go` — `handleGroupSpawn` `include_group_context`
  + briefing assembly, `buildSpawnContextBody`
- `pkg/claude/agentd/dashboard.go` — `dashboardGroup.DefaultContext`
- `pkg/claude/agentd/dashboard.html` — create-modal textarea, header chip,
  group-context modal, spawn checkbox
- `pkg/claude/agent/groups.go` — `groupsSetContextCmd`, `groups create
  --context` / `--context-file`
- `pkg/claude/agentd/group_default_context_flow_test.go` — flow tests

## Out of scope (deferred)

- **Per-group clone/reincarnate inheriting the context** — clone and
  reincarnate carry the predecessor's own conversation; the group
  context only governs fresh spawns. Consistent with `default_cwd`.
- **`groups clone` copying default_context** — group clone doesn't copy
  `default_cwd` either; left alone for symmetry.
- **Structured / templated context** (per-role sections, variable
  substitution) — the primitive is deliberately free-form text. Add
  structure only if it shows up in practice.
