# Group templates — define a team blueprint, instantiate working groups

Shipped 2026-05.

Spinning up a working group for a new feature/project meant creating
the group, then spawning each agent one at a time, hand-typing every
role / descr / task brief / permission grant, then granting ownership.
Repetitive, and easy to get inconsistent between teams.

This slice ships **group templates**: a reusable *blueprint* for a
group — a name, a shared default context, and an ordered list of agent
specs (name, role, descr, per-role task brief, owner flag, permission
slugs). Instantiating one creates a fresh group and spawns its whole
agent team in a single action.

A template is deliberately **distinct from a group export**
([`group-export`](group-export.md)): an export is a conv-bound
*snapshot* of a live group (DB rows + every member's `.jsonl`), whereas
a template has no conv-ids — it is a recipe for a group that does not
exist yet.

## What shipped

### DB — `group_templates` + `group_template_agents` (migration v42)

`pkg/claude/common/db/group_templates.go` + `migrate.go`
(`migrateV41toV42`):

- `group_templates(id, name UNIQUE, descr, default_context,
  created_at, updated_at)`.
- `group_template_agents(id, template_id → group_templates ON DELETE
  CASCADE, ordinal, name, role, descr, initial_message, is_owner,
  permissions)`. `permissions` is a JSON array of permission slugs.
- `GroupTemplate` / `GroupTemplateAgent` structs + CRUD:
  `CreateGroupTemplate`, `GetGroupTemplate`, `ListGroupTemplates`
  (agents bucketed in one pass — no N+1), `UpdateGroupTemplate`
  (full-replace: delete-then-reinsert agents in a transaction),
  `DeleteGroupTemplate`. A name collision surfaces as
  `ErrGroupTemplateNameTaken`.

### Daemon — `/v1/templates` surface

`pkg/claude/agentd/templates.go`, routes registered in `serve.go`:

| Method + path | Purpose |
|---|---|
| `GET /v1/templates` | list templates (open, read-only) |
| `POST /v1/templates` | create a template |
| `GET /v1/templates/{name}` | fetch one (open) |
| `PATCH /v1/templates/{name}` | replace a template (full state) |
| `DELETE /v1/templates/{name}` | delete a template |
| `POST /v1/templates/{name}/instantiate` | create a group + spawn its team |
| `POST /v1/templates/from-group` | snapshot a live group into a template |

Reads are open (introspection, like `/v1/permissions`); mutations are
gated on `templates.manage`; instantiate on `templates.instantiate`.
Both new slugs are effectively human-only (not default-granted) —
instantiate in particular spawns a whole team at once.

**Instantiation** (`POST /v1/templates/{name}/instantiate`, body
`{group_name, task, cwd?, descr?}`):

1. `group_name` doubles as the agent-name prefix — template agent `PO`
   becomes `<group_name>-PO`.
2. The multi-line `task` is folded under a `## Task` header into the
   template's `default_context`; the result becomes the new group's
   `default_context`, so every spawned agent's startup briefing carries
   the boilerplate **and** the assignment.
3. Each template agent is spawned **sequentially** via the shared
   `executeSpawn` core; the owner agent gets `agent_group_owners`
   ownership and each agent gets its `permissions` slugs as per-conv
   grant overrides.
4. A per-agent spawn failure is recorded and reported but does **not**
   abort the rest — tearing half-spawned agents back down is
   destructive, so a partial team is surfaced for the human to finish
   or retry by hand.

**from-group** snapshots a live group: it carries the group's
descr + default_context and one template agent per member (role,
descr, owner flag, the member's per-conv permission grants). Per-agent
`initial_message` comes through blank — a live group has no stored task
brief per member — for the human to fill in the editor afterwards.

### Shared spawn core — `executeSpawn`

`handleGroupSpawn`'s post-validation body — label → detached
`tclaude session new` → conv-id poll → group-membership → pending-name
→ auto-focus → inbox briefing → post-init `/rename`+welcome — was
extracted into `executeSpawn(g, spawnParams) → (*spawnOutcome,
*spawnFailure)`. The single-spawn HTTP handler and the template
instantiator now drive the same code path; `handleGroupSpawn` keeps
only the HTTP shape (decode + validate, error/JSON mapping).

### Dashboard — Templates tab + modals

`pkg/claude/agentd/dashboard_templates.go` (cookie-auth `/api/templates*`
twins, delegating to the shared handlers via `asDashboardHumanPeer`),
`dashboard.go` (`Templates []templateJSON` on the snapshot,
`collectTemplatesSnapshot`), `dashboard.html`:

- A new **Templates tab** — one card per template (name, descr, agent
  chips with owner ★ + permission count), with `instantiate / edit /
  delete` actions.
- A **template editor modal** — name, descr, default context, and a
  dynamic list of agent rows (name, role, descr, task brief, owner
  checkbox enforcing at-most-one, a collapsible permission-slug
  checklist). Used for both create and edit.
- An **instantiate modal** — template select, group name, multi-line
  task/project field, cwd, and a **live preview** of the final agent
  names (`<group>-<name>`) updating as the human types. Reachable from
  each template card *and* from the Groups tab's `⎘ from template`
  button.
- A **save-as-template modal** (`⤓ from a group` on the Templates tab)
  — pick a group + name; on success the editor opens on the fresh
  template so the human can fill in the blank per-agent task briefs.

### Permission slugs

`templates.manage` (create / edit / delete / snapshot-from-group) and
`templates.instantiate` (instantiate a group). Registered in
`permissionRegistry`; neither is default-granted.

## Files

- `pkg/claude/common/db/group_templates.go` — structs + CRUD.
- `pkg/claude/common/db/migrate.go` — `migrateV41toV42`, `currentVersion = 42`.
- `pkg/claude/common/db/migrate_v42_test.go` — migration + cascade test.
- `pkg/claude/agentd/templates.go` — `/v1/templates` handlers,
  `buildTemplateFromJSON`, `composeInstantiationContext`,
  `collectTemplatesSnapshot`, `deriveTemplateAgentName`.
- `pkg/claude/agentd/lifecycle.go` — `executeSpawn` + `spawnParams` /
  `spawnOutcome` / `spawnFailure` extracted from `handleGroupSpawn`.
- `pkg/claude/agentd/identity.go` — `PermTemplatesManage` /
  `PermTemplatesUse` slug consts.
- `pkg/claude/agentd/permissions.go` — registry entries.
- `pkg/claude/agentd/serve.go` — `/v1/templates*` route registration.
- `pkg/claude/agentd/dashboard_templates.go` — cookie-auth `/api`
  twins.
- `pkg/claude/agentd/dashboard.go` — `snapshotPayload.Templates`.
- `pkg/claude/agentd/dashboard.html` — Templates tab + 3 modals + CSS/JS.

## Tests

`pkg/claude/agentd/templates_flow_test.go`:

- `TestGroupTemplate_InstantiateSpawnsTeam` — a 3-agent template (PO
  marked owner + granted `groups.spawn`, two devs) instantiates a
  group: 3 members joined, `<group>-<name>` final names, ownership on
  the PO's conv, the per-conv `groups.spawn` grant, and the task folded
  into every member's "Startup context" inbox briefing.
- `TestGroupTemplate_FromGroupSnapshot` — a live group with an owner
  snapshots into a template carrying the group context + one agent per
  member, owner flag preserved.
- `TestGroupTemplate_CRUDRoundTrip` — create → PATCH (rename + replace
  agents) → delete round-trip; duplicate-name create is a 409.

`pkg/claude/common/db/migrate_v42_test.go`:

- `TestMigrateV41toV42_AddsGroupTemplates` — the v42 migration lands
  both tables; the `name` UNIQUE constraint rejects a duplicate.
- `TestMigrateV41toV42_FreshSchema` — fresh DB reaches `currentVersion`;
  deleting a template cascade-drops its agent rows.

## Notes / deferred

- Instantiation is **synchronous** — each `executeSpawn` resolves a
  conv-id in seconds, so a 3–4 agent template lands in ~10–20s; the
  dashboard shows a spinner. An async/progress-streamed variant was
  considered and deferred.
- No CLI surface (`tclaude agent templates …`) yet — the dashboard is
  the v1 client; the `/v1/templates` endpoints are the primitive a CLI
  would wrap later.
- Worktree-per-agent is not template-managed — instantiated agents
  share the group's cwd; the PO/devs create their own worktrees.
- Placeholder substitution in template task briefs (`{{task}}` etc.)
  was considered unnecessary: the task lands in the group context
  every agent already sees.
