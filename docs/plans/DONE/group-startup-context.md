# Group startup context

Shipped 2026-05.

A group can carry an optional **startup context** — a block of shared
guidance the human attaches to the group. When an agent is spawned
into that group, the daemon injects the context into the new agent's
pane on startup, right after the spawn welcome. Each spawn can opt
out individually.

## Why

A team of agents spawned into one group nearly always needs the same
orientation — what project they're on, where the repo is, which
conventions to follow, who to coordinate with. Before this, the human
typed that into every new agent by hand (or baked it into a CLAUDE.md
that only covers one repo). A per-group context makes "spawn into
group X" carry the right briefing for free, while staying optional —
groups that don't want one simply don't set it.

It is a deliberately flexible primitive: free-form text, injected as
one extra turn, no enforced structure. The group decides what goes in
it.

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

`PATCH /v1/groups/{name}` → `handleGroupUpdate` (`handlers.go`) now
patches **two** fields — `default_cwd` and `default_context`. Both are
`*string`, so `""` clears and omitting leaves untouched; an empty body
(neither field) is a 400. `default_context` is run through
`normalizeGroupContext`: CRLF / lone-CR line endings are folded to LF
(so the pasted block renders consistently regardless of where it was
authored) and the result is rejected with a 400 when it exceeds
`maxGroupContextBytes` (16 KiB) — the context is bracketed-pasted into
a pane, so an unbounded blob has no business there. Gated on the
**`groups.rename`** slug, same as `default_cwd` — human-curated group
config, blast radius is a spawn-time injection. Default human-only.

### Create

`POST /v1/groups` accepts an optional `default_context` in the body.
It's validated up front (`normalizeGroupContext`) and applied as a
post-create `SetAgentGroupDefaultContext` — keeps `CreateAgentGroup`'s
signature untouched (it's shared with the clone path and flow-test
helpers).

`PATCH /api/groups/{name}` and `POST /api/groups` (the dashboard
cookie-auth twins) delegate straight to the same handlers.

## Spawn-time injection

`handleGroupSpawn` (`lifecycle.go`) takes a new request field
`include_group_context` — a `*bool` so an omitted field means **opt-in**:
every spawn path (`tclaude agent spawn`, `/v1` API, `groups create
--member`, dashboard) inherits the group context by default, the same
way it inherits `default_cwd`. The dashboard sends `false` explicitly
when the human unticks the checkbox.

`runSpawnPostInit` was restructured: after the `/rename` + welcome
injection it injects the group context (when present) as its own turn,
via the new `injectMultilineAndSubmit`. Helpers extracted:
`aliveTmuxTarget` (resolve the `<session>:0.0` pane address) and
`buildGroupContextMessage` (wrap the raw context in a `[system: …]`
header so the agent reads it as group guidance, not a human prompt).

### `injectMultilineAndSubmit`

`handlers.go`. The existing `injectTextAndSubmit` sends text with a raw
`send-keys`, where every embedded newline is delivered as an Enter
keypress — fine for the single-line welcome, but a multi-line context
would submit on its first line and scatter the rest. The new helper
instead stages the text into a tmux paste buffer (`set-buffer`) and
pastes it with bracketed paste (`paste-buffer -p`) and no LF→CR
translation (`-r`). CC, seeing bracketed paste, inserts the newlines
literally into its input box; a separate Enter submits the whole block
as one turn. `-d` drops the buffer afterwards.

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

`pkg/claude/agentd/group_default_context_flow_test.go` — 8 flow tests:

- `TestGroupDefaultContext_InjectedOnSpawn` — PATCH stores it; a spawn
  with the flag omitted (opt-in default) injects it after the welcome.
- `TestGroupDefaultContext_OptOutSkipsInjection` — `include_group_context:
  false` → welcome lands, context does not.
- `TestGroupDefaultContext_NoContextNoInjection` — a context-less group
  injects only the welcome.
- `TestGroupDefaultContext_PatchClears` — `default_context:""` clears the
  row; a later spawn no longer inherits it.
- `TestGroupDefaultContext_CreateWithContext` — `POST /v1/groups` with
  `default_context` stores it.
- `TestGroupDefaultContext_MultilinePreserved` — a 3-line context's
  last line reaches the pane verbatim (bracketed paste held the
  newlines).
- `TestGroupDefaultContext_PatchTooLongRejected` — >16 KiB → 400,
  nothing persisted.
- `TestGroupDefaultContext_PatchNormalizesCRLF` — CRLF / lone-CR folded
  to LF on store.

The simulator gained paste-buffer support: `pkg/testharness/tmux_sim.go`
now models `set-buffer` / `paste-buffer`, routing the buffered text to
the target pane's `CCSim` (and logging it like a send-keys) so
`AssertSentContains` sees the bracketed-pasted context.

## Files

- `pkg/claude/common/db/migrate.go` — `migrateV28toV29`, `currentVersion = 29`
- `pkg/claude/common/db/agent.go` — `DefaultContext`, `SetAgentGroupDefaultContext`,
  `scanAgentGroup` + group SELECTs
- `pkg/claude/agentd/handlers.go` — `handleGroupUpdate` two-field patch,
  `handleGroups` create-time context, `normalizeGroupContext`,
  `maxGroupContextBytes`, `injectMultilineAndSubmit`
- `pkg/claude/agentd/lifecycle.go` — `handleGroupSpawn` `include_group_context`,
  `runSpawnPostInit`, `aliveTmuxTarget`, `buildGroupContextMessage`
- `pkg/claude/agentd/dashboard.go` — `dashboardGroup.DefaultContext`
- `pkg/claude/agentd/dashboard.html` — create-modal textarea, header chip,
  group-context modal, spawn checkbox
- `pkg/claude/agent/groups.go` — `groupsSetContextCmd`, `groups create
  --context` / `--context-file`
- `pkg/testharness/tmux_sim.go` — `set-buffer` / `paste-buffer` modelling
- `pkg/claude/agentd/group_default_context_flow_test.go` — 8 flow tests

## Out of scope (deferred)

- **Per-group clone/reincarnate inheriting the context** — clone and
  reincarnate carry the predecessor's own conversation; the group
  context only governs fresh spawns. Consistent with `default_cwd`.
- **`groups clone` copying default_context** — group clone doesn't copy
  `default_cwd` either; left alone for symmetry.
- **Structured / templated context** (per-role sections, variable
  substitution) — the primitive is deliberately free-form text. Add
  structure only if it shows up in practice.
