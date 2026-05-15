# Group default start dir

Shipped 2026-05.

A group can carry a **default start directory**. When an agent is
spawned directly into that group, the spawn inherits the group's
default cwd unless the caller pins one explicitly. The dashboard
also pre-fills its spawn form's CWD field from it.

## Why

Spawning a team of agents into a group nearly always means landing
them in the same project directory. Before this, every spawn (CLI,
API, or dashboard) had to repeat the path. A per-group default makes
"spawn into group X" carry the right cwd for free.

## Schema bump (v27)

```sql
ALTER TABLE agent_groups ADD COLUMN default_cwd TEXT NOT NULL DEFAULT '';
```

`migrateV26toV27` in `pkg/claude/common/db/migrate.go`. Empty string =
no default (spawn falls back to the daemon's own cwd, the pre-feature
behaviour).

## DB layer

`pkg/claude/common/db/agent.go`:

- `AgentGroup.DefaultCwd` field; `scanAgentGroup` + all five
  `SELECT … FROM agent_groups` queries carry the column.
- `SetAgentGroupDefaultCwd(name, cwd)` — `UPDATE … SET default_cwd`.
  Returns rows-affected so callers can answer 404. `cwd == ""` clears.

## Daemon API

`PATCH /v1/groups/{name}` → `handleGroupUpdate` (`handlers.go`).
Partial-update contract matching `handleGroupMembersUpdate`:
`default_cwd` is a `*string`, so `""` clears and omitting it is a
400 ("nothing to update"). A non-empty value is run through
`resolveGroupDefaultCwd` (`cwd.go`): `~` is expanded and the path
**must be absolute** — a relative default would resolve against the
daemon's own cwd at spawn time, which is meaningless, so it's
rejected with a 400 rather than silently made absolute. The handler
also surfaces a 404 when the update touches zero rows (group renamed
or deleted between the dispatcher's lookup and the write). Gated on
the **`groups.rename`** slug — setting a group's default cwd is the
same class of human-curated group config as renaming it (blast
radius is a UI prefill / spawn fallback, strictly lower), so it
rides the existing slug rather than minting a new one. Default
human-only.

`PATCH /api/groups/{name}` is the dashboard-cookie-auth twin — it
delegates straight to `handleGroupUpdate` with `asDashboardHumanPeer`,
same as `dashboardSpawnInGroup`.

### Create-time default

`POST /v1/groups` also accepts an optional `default_cwd` alongside
`default_context`. It is validated up front through the same
`resolveGroupDefaultCwd` (expand `~`, require absolute → 400 on a
relative path) **before** the row is inserted, so a bad value never
leaves a half-created group behind, then applied as a post-create
`SetAgentGroupDefaultCwd` — mirroring how `default_context` is handled.
This is the path the dashboard's "create group" modal rides.

### Spawn fallback

`handleGroupSpawn` (`lifecycle.go`): when the spawn request leaves
`cwd` blank, it substitutes `g.DefaultCwd` before launching. This is
the key design choice — the default is applied **server-side**, so it
reaches every spawn path (`tclaude agent spawn`, the `/v1` API,
`groups create --member`, and the dashboard) rather than only the
dashboard's client-side prefill. An empty default leaves cwd blank,
preserving the prior daemon-cwd-inheritance behaviour.

## CLI

```bash
tclaude agent groups set-default-dir <group> [<dir>] [--ask-human <d>]
```

`groups.go` — `groupsSetDefaultDirCmd` / `runGroupsSetDefaultDir`.
A non-empty `<dir>` is resolved to an absolute path (`filepath.Abs`)
so the stored value is unambiguous. Omitting `<dir>` clears the
default. Tab-completion suggests existing group names.

## Dashboard

- **Snapshot**: `dashboardGroup.DefaultCwd` (`json:"default_cwd"`).
- **Group header**: a `.group-default-cwd` chip (`📁 <shortCwd>` or a
  faint `📁 no default dir`). The chip itself is click-to-edit
  (`data-act="set-group-dir"` lives on the span) — clicking it
  replaces the chip with an `<input>`, Enter saves (`PATCH
  /api/groups/{name}`), Esc / blur cancels. Auto-refresh suspends via
  the existing `renameEditing` flag so the 5s poll can't drop the
  input mid-keystroke. (There is no separate "start dir" button — an
  earlier cut had one next to "rename"; the chip absorbed it so the
  affordance sits on the thing it edits.)
- **Create modal**: the "create group" form carries a **Default cwd**
  field (`#group-create-cwd`) alongside name / descr / startup context.
  `submitGroupCreate()` sends it as `default_cwd` in the `POST
  /api/groups` body; an empty field omits the default. Enter in the
  field submits the form, same as the name / descr inputs.
- **Spawn modal**: `prefillSpawnCwd()` fills the CWD field from the
  selected group's `default_cwd` when the modal opens, and re-fills it
  when the group `<select>` changes (Agents-tab spawn). It never
  clobbers a path the user typed — `lastSpawnCwdPrefill` tracks the
  last auto-fill so a manual edit is left alone.

## Test coverage

`pkg/claude/agentd/group_default_cwd_flow_test.go`:

- `TestGroupDefaultCwd_PrefillsSpawn` — PATCH stores it; a blank-cwd
  spawn lands the new session in the group default dir.
- `TestGroupDefaultCwd_ExplicitCwdOverrides` — an explicit cwd in the
  spawn request wins over the group default.
- `TestGroupDefaultCwd_PatchClears` — `default_cwd:""` clears the row,
  and a later blank-cwd spawn no longer inherits the old value.
- `TestGroupDefaultCwd_PatchRejectsRelative` — a relative `default_cwd`
  400s and nothing is persisted.
- `TestGroupDefaultCwd_PatchEmptyBodyRejected` — empty PATCH body 400s.
- `TestGroupDefaultCwd_CreateWithCwd` — `POST /v1/groups` with
  `default_cwd` stores it at create time; a later blank-cwd spawn
  inherits it.
- `TestGroupDefaultCwd_CreateRejectsRelative` — a relative `default_cwd`
  in the create payload 400s and no group is inserted.

## Files

- `pkg/claude/common/db/migrate.go` — `migrateV26toV27`, `currentVersion = 27`
- `pkg/claude/common/db/agent.go` — `DefaultCwd`, `SetAgentGroupDefaultCwd`,
  `scanAgentGroup` + group SELECTs
- `pkg/claude/agentd/handlers.go` — `PATCH` dispatch + `handleGroupUpdate`;
  `handleGroups` POST accepts `default_cwd`
- `pkg/claude/agentd/cwd.go` — `resolveGroupDefaultCwd` (expand `~`, require absolute)
- `pkg/claude/agentd/lifecycle.go` — `handleGroupSpawn` blank-cwd fallback
- `pkg/claude/agentd/dashboard.go` — `dashboardGroup.DefaultCwd`
- `pkg/claude/agentd/dashboard_edit.go` — `PATCH /api/groups/{name}` dispatch
- `pkg/claude/agentd/dashboard.html` — header chip, inline edit, spawn prefill
- `pkg/claude/agent/groups.go` — `groupsSetDefaultDirCmd`
- `pkg/claude/agentd/group_default_cwd_flow_test.go` — 5 flow tests

## Out of scope (deferred)

- **`groups create --default-dir`** (CLI flag) — the create-time
  default now ships for the dashboard + the `POST /v1/groups` API, but
  the `tclaude agent groups create` CLI still has no `--default-dir`
  flag. Cheap to add later (a `GroupsCreateParams` field + the existing
  `default_cwd` body key); for now CLI users `create` then
  `set-default-dir`.
- **Per-group reincarnate/clone inheriting the dir** — clone/reincarnate
  already carry the predecessor's own cwd; the group default only
  governs fresh spawns.
