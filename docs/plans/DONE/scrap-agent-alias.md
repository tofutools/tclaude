# Scrap the per-group agent alias

## What shipped

The per-group agent **alias** concept is gone. An agent now has exactly
**one name** — its conversation title (`conv_index.custom_title`,
surfaced via `agent.FreshTitle` / `displayTitle`).

The old `agent_group_members.alias` column was pure duplication: every
spawn path set it equal to the title (the daemon injected
`/rename <alias>`), so the two were always identical. Per-group
semantics that genuinely differ between groups are still carried by the
member `role` and `descr` fields.

This is distinct from the **head-alias** layer (`tclaude agent alias`,
`agent_head_aliases`) — stable handles that resolve to the live head of
a reincarnation chain. That feature was left untouched.

## Schema

- Migration **v34 → v35** (`migrateV34toV35`, `pkg/claude/common/db/migrate.go`):
  `ALTER TABLE agent_group_members DROP COLUMN alias`. The column was a
  bare `TEXT` field — not in the `(group_id, conv_id)` primary key, not
  indexed, no FK/CHECK — so a plain `DROP COLUMN` suffices (SQLite ≥
  3.35; the bundled modernc.org/sqlite is well past that). `currentVersion`
  bumped to 35. (Originally authored as v32→v33; renumbered to v34→v35
  after the spawn-guardrails branch landed v33 + v34 ahead of it.)

## CLI surface

- `tclaude agent spawn <group>`: `--alias` → **`--name`** (`-n`). The
  value becomes the new agent's conversation title via the post-spawn
  `/rename` injection.
- `tclaude session new --join-group` / `tclaude --join-group`:
  `--alias` → **`--name`**.
- `tclaude agent groups create --member`: the spec key `alias=` →
  **`name=`** (e.g. `--member name=lead,role=tech-lead,descr=...`).
  `name` is the required key.
- `tclaude agent groups add`: `--alias` flag **removed** (an existing
  conv keeps its own title; role/descr remain).
- `tclaude agent groups update-member`: `--alias` flag **removed**
  (edits role/descr only; rename an agent with `tclaude agent rename`).
- `tclaude agent ls` / `groups members`: the `ALIAS` column is gone;
  the `NAME` column (the conv title) is the agent's single name.
- Selector help text everywhere now says **title** where it said alias.

## Daemon / wire

- `POST /v1/groups/{name}/spawn` request body: `alias` → `name`.
- Member/peer JSON (`/v1/peers`, `/v1/groups/{name}/members`,
  `/api/snapshot`): the `alias` field is gone — only `title` remains.
- Message-send response `recipients[]`: `alias` → `title`.
- `/v1/messages/{id}`: `from_alias` → `from_title`;
  `to_recipients`/`cc_recipients` entries: `alias` → `title`.
- `groups stop`/`resume` per-member result: `alias` → `title`.
- `PATCH .../members/{conv}` body: accepts only `role`/`descr`.
- `db.AgentGroupMember` lost its `Alias` field;
  `UpdateAgentGroupMember` lost its `alias` param;
  `FindAgentMembersBySelector` resolves by conv-id / prefix only (a
  freshly-spawned conv is still findable by conv-id before its
  `/rename` is scanned into `conv_index`; by title once it is).
- `agent.AliasFor` → `agent.TitleFor` (conv title, cached lookup).
- Clone naming: `uniqueCloneAlias`/`scanCloneSuffixesGlobal` →
  `uniqueCloneTitle`/`scanCloneSuffixes` — now title-based, scanning
  `conv_index.custom_title` exactly like reincarnate's `-r-N` scheme.
  The clone (per-conv and per-group-member) is `/rename`d to
  `<original-title>-c-<N>`.
- Dead `alias` slot removed from `config.MatchSudoOverride` /
  `sudoOverrideKeyMatches` / `resolveSudoConfig` (callers always
  passed `""`).

## Dashboard

`pkg/claude/agentd/dashboard.html`: the edit-member modal's Alias field
removed; the spawn form's "Alias" input → "Name" (`#agent-spawn-name`);
the agents-table `Alias / Title` sort column → `Name`; every
`m.alias || m.title` fallback collapsed to `m.title`; the `.alias` CSS
class renamed to `.rowname`.

## Tests

- `pkg/claude/common/db/migrate_alias_test.go` — **new**:
  `TestMigrateV32toV33_DropsAliasColumn` seeds a v32 table with the
  legacy `alias` column + rows, runs the migration, asserts the column
  is gone, all rows survive, and the PK still holds.
- `pkg/claude/agentd/spawn_flow_test.go` — **new**:
  `TestSpawn_NameBecomesTitleResolvableBySelector` — `spawn --name X`
  → the agent's title is X on the members surface AND `X` resolves as
  a selector via `/v1/lookup`.
- `pkg/claude/agentd/clone_test.go` — rewritten to test
  `uniqueCloneTitle` against `conv_index` titles (mirrors
  `reincarnate_test.go`).
- testharness (`pkg/testharness/flow.go`): `HaveMember` dropped its
  alias arg; `Spawn` takes a name; `CloneFresh` dropped its dead alias
  arg; `AssertGroupMember` dropped `wantAlias`;
  `AssertCloneAliasInGroup` → `AssertCloneTitle`; new `Lookup` /
  `AssertResolvesByTitle` helpers; `MemberView`/`PeerView` lost `Alias`.
- Every `*_flow_test.go` / DB unit test that constructed an alias was
  updated.

## Note on the schema version

This shipped as schema version **35**. It was originally written as
v32→v33; the spawn-guardrails branch merged first with two migrations
(v33 `agent_groups.max_members`, v34 `agent_spawn_history`), so this
was renumbered to v34→v35 on rebase — `migrateV34toV35`,
`currentVersion = 35`, dispatch entry, and the migration test's
version assertions all moved together.
