# Group lifecycle — phase A (2026-05)

Persistent teams that can be spawned, suspended, resumed, archived,
and bootstrapped in one call.

## Verbs

- **`groups stop <group>`** — soft default (inject `/exit` via tmux
  send-keys); `--force` does `tmux kill-session`. Per-member result
  table. Membership preserved. Slug: `groups.stop`.
- **`groups resume <group>`** — has-conv-but-dead-tmux case:
  spawns `tclaude session new -r <conv> -d --global` for each
  offline member; idempotent. Slug: `groups.resume`. Phase B
  (no-conv-yet placeholder + bootstrap prompts) deferred —
  see `high-prio/group-lifecycle.md`.
- **`groups archive <group>` / `groups unarchive <group>`** —
  soft-delete. Schema v16 adds `agent_groups.archived_at`.
  Refuses subsequent mutating ops (`member.add` / `remove` /
  `update`, `owners.*`, messages, group multicast, spawn) with
  409. Hides the group from default `groups ls` output. Members
  + ownership + message history preserved. Idempotent. Slug:
  `groups.archive`.
  - `--archived` flag on `groups ls` reveals them with an
    "(archived)" tag; `unarchive` tab-completes only on
    archived groups.
  - Lifecycle ops (`groups stop` / `groups resume`) are
    intentionally still allowed on archived groups so a human
    can shut down running members of a sealed group.
  - Archive does NOT auto-stop running members — the destructive
    `groups stop --force` step is left explicit (two-step keeps
    the blast radius visible).
- **`groups spawn <group>`** — bulk version of `agent.spawn` over a
  group's members. Slug: `groups.spawn`.

## Group ownership (schema v11)

`agent_group_owners` table. Owners can message a group's members
and multicast without being members themselves.

CLI:

- `groups owners` — list owners.
- `groups grant-owner <conv>`.
- `groups revoke-owner <conv>`.

Slug: `groups.own`. Default human-only.

`groups members` shows `(owner)` tag for member-owners; pure-
owners surface as their own rows with `role=owner`.

Reply path no longer requires shared-group — if you received a
message you can reply, even out-of-group.

**Auto-own-on-create**: an agent that creates a group becomes its
owner automatically (skipped for human creator since humans
bypass the permission system).

## `tclaude agent spawn <group>` — fresh CC + auto-join

Fork `tclaude session new -d --global --label <random>`, poll
SQLite for the new conv-id, then register it in the group with
optional `--alias` / `--role` / `--descr`. Slug: `groups.spawn`
(human-only by default).

Lookup fallback to `agent_group_members` for fresh-spawned convs
and per-group aliases. Existing refresh-on-miss still fires when
both `conv_index` and members miss.

## `groups create --member` spawn-on-create (commit fc2f9cc-ish)

Repeatable `--member alias=...,role=...,descr=...,cwd=...` flag
bootstraps a team in one call. CLI parses + validates up-front
(typo aborts before any DB work), creates the group via existing
endpoint, then iterates `groups.spawn` per member. Partial failure
leaves the group up; human retries via `agent spawn`. Caller's
cwd defaulted per-member when `cwd=` is omitted; explicit `cwd=`
overrides.

Three flow scenarios pinned:

- `TestGroupsCreateTeam_BootstrapsMembers`
- `TestGroupsCreateTeam_PerMemberCwdOverride`
- `TestGroupsCreateTeam_BadSpecAbortsBeforeCreate`

Persistent-template Phase B left for later (see
`high-prio/group-lifecycle.md`).
