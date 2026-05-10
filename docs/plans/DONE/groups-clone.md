# `tclaude agent groups clone <source> [<new-name>]`

Shipped 2026-05.

## CLI

```
tclaude agent groups clone <source> [<new-name>] [--ask-human <duration>]
```

Default new group name is `<source>-c-<N>` (smallest free N globally).
Clone-of-a-clone strips the existing `-c-<N>` suffix before computing
the next, so names don't nest (`team-c-1` clones to `team-c-2`, not
`team-c-1-c-1`).

If `<new-name>` is supplied and already exists → 409, no mutations.
Source group untouched on success.

## Daemon orchestration

`POST /v1/groups/{source}/clone` with optional body
`{"new_name": "...", "no_copy_conv": <bool>}`.

Steps (best-effort per member):

1. Validate body (charset rules via `validateGroupName` if name
   given). Reject 400 / 409 collisions before any mutation.
2. Snapshot source members + owners (read-only).
3. `db.CreateAgentGroup(newName, src.Descr)` — fresh group, never
   archived (cloning an archived group is refused upstream by
   `requireGroupActive`).
4. For each source member:
   - Pick a live tmux session for the source's `cwd`. If the source
     has no live session, skip with `error: "skipped: source has no
     live tmux session (cwd unknown)"` — partial result, the rest of
     the clones proceed.
   - `cloneSpawnOnce(srcConv, cwd, noCopyConv)` mints the new
     conv-id (and optionally its jsonl). Same race-handled spawn
     loop the single-conv `agent clone` uses.
   - Add a single membership row to the **new** group (NOT to the
     source's other groups). Alias is `<srcAlias>-c-<N>` via
     `uniqueCloneAlias`.
   - Best-effort copy of the source conv's per-conv permissions to
     the new conv via `db.GrantAgentPermission`.
5. For each source owner, insert an `agent_group_owners` row on the
   new group with the **same conv-id** (no owner cloning — owners
   are separate from members per the schema; the human/agent who
   owns the source typically owns the clone too).

Failure handling: best-effort per member. If member 2 of 3 fails to
spawn, the new group keeps members 1 + 3, member 2 surfaces with an
`error` field, the response is still 200. The human can retry the
failed member with `agent clone <src-conv> --target <new-group>`.
**No auto-rollback** — partial success is recoverable, full rollback
would destroy work that already landed.

Auth: slug `groups.rename`'s sibling `groups.clone`, default
human-only.

## CloneSpawnOnce extraction

`runCloneOrchestration` and the new `handleGroupClone` share the
spawn-and-poll logic via `cloneSpawnOnce(sourceConv, cwd, noCopyConv)`
in `pkg/claude/agentd/clone.go`. Pure refactor (commit 1 of this
ship), behaviour-preserving — both branches (copy + no-copy) were
moved into the helper without changing semantics. The wrapper struct
`cloneSpawnError{Status, Code, Msg}` mirrors `writeError` so callers
can either surface a single HTTP error (single-conv clone) or
accumulate per-member errors (group clone).

## Test coverage

`pkg/claude/agentd/groups_clone_flow_test.go`:
- `TestGroupsClone_DefaultsSuffix` — 2-member group cloned with
  default name → `team-c-1`, source untouched, both members spawned
- `TestGroupsClone_ExplicitName` — explicit name accepted
- `TestGroupsClone_NameCollisionIsConflict` — 409 on collision, no
  mutations
- `TestGroupsClone_OwnersCopied` — source owners copied (same
  conv-ids) to new group
- `TestGroupsClone_OfClone_StripsSuffix` — `team-c-1` cloned →
  `team-c-2`, not `team-c-1-c-1`
- `TestGroupsClone_ArchivedSourceRejected` — archived sources refused
  via `requireGroupActive` (409)
- `TestGroupsClone_OfflineMemberSkipped` — partial failure: offline
  member skipped, live members cloned, response surfaces both

Tests use `no_copy_conv: true` to skip the
`convops.CopyConversationToPath` path the simulator doesn't model
(same convention as `clone_flow_test.go` / `CloneFresh`).

## Files

- `pkg/claude/agentd/clone.go` — `cloneSpawnOnce` extracted from
  `runCloneOrchestration`
- `pkg/claude/agentd/groups_clone.go` — `handleGroupClone`
  orchestration, `nextGroupCloneName` + `scanGroupCloneSuffixes`
  default-name picker
- `pkg/claude/agentd/identity.go` — `PermGroupsClone` slug
- `pkg/claude/agentd/handlers.go` — dispatcher branch for `/clone`
- `pkg/claude/agent/groups.go` — `groupsCloneCmd` + `runGroupsClone`
- `pkg/claude/agentd/groups_clone_flow_test.go` — 7 flow tests

## Out of scope (deferred)

- **`--blank` / `--no-copy-conv` CLI flag** — the wire format
  accepts `no_copy_conv` (used by tests), but the CLI doesn't expose
  it yet. Add the flag if a "fresh-context whole team" workflow
  emerges.
- **`--include-archived`** — archived sources are refused outright
  in v1. The TODO contemplated a flag to override; defer until
  someone hits the limitation.
- **`--dry-run`** — preview-only mode that returns the snapshot
  without spawning. Cheap to add; defer until volume of group clones
  makes "rate-limit headroom check" useful.
- **Cron jobs targeting the source group** — TODO open question
  said "lean no" for v1: the human can re-add cron jobs against the
  new group explicitly. Auto-cloning cron jobs is surprising and
  easy to mis-target.
- **Per-group permission scoping** — today permissions are
  per-conv-globally; the source's clones inherit slugs by virtue of
  being clones (per-conv copy step). If per-group perm scoping ever
  ships, this file needs to copy those scoped perms onto the new
  group too.
- **Dashboard "Clone group" button** — calls the same daemon
  endpoint; ship with the framework migration sketched in
  `dashboard-group-membership-ux.md`. (Could ship a vanilla-JS
  variant inline like the rename button — defer for now.)

## Cross-references

- [`DONE/agent-self-lifecycle.md`](agent-self-lifecycle.md) — the
  `runCloneOrchestration` machinery this leans on per member
- [`DONE/clone-and-suffix-scheme.md`](clone-and-suffix-scheme.md) —
  `-c-<N>` semantics reused for group names
- [`DONE/groups-rename.md`](groups-rename.md) — sibling team-level
  verb shipped alongside
