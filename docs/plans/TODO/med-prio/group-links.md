# Inter-group links + multi-POV org modelling

**Status:** design / partial implementation in flight. PR scaffolds the
backend + auth + minimal CLI; UI and parent-group display hint deferred
to follow-ups.

## Motivation

Today `agent_groups` is a flat label set. The auth rule for
`tclaude agent message <target>` is:

1. Sender and target share a group (`shared-group`).
2. OR sender owns a group containing the target (`owner-of-group`).

That's enough for "team chat" and "manager talks to subordinates", but
it forces every cross-team conversation through one of two awkward
shapes:

- **Co-membership** — drop everyone who needs to talk to anyone into
  the same group. Permission set grows quadratically with team count;
  noise from broadcasts goes everywhere.
- **Owner bridging** — give the talker `groups.grant-owner` and let it
  insert itself as an owner of every adjacent team. Works, but the
  "owner" relation is meant to model managerial authority, not "two
  peer teams agreed to exchange status updates".

What's missing is a **flat, declarative relation between two groups**
that says "members of A may message members of B" without either group
having to absorb the other. Add it and the same `agent_groups` table
becomes a label/POV system:

- Conv X is in `engineering` (functional org), `proj-falcon` (project),
  `oncall-w19` (rotation) — three labels, three peer sets.
- A link `proj-falcon -> security-review` enables a single team-to-team
  channel without merging memberships.

We explicitly **reject nested groups** as the implementation: every
send becomes a recursive auth check, parent/child ambiguity ("which
parent's policy wins?") is unresolved, and overlapping memberships
already express everything a tree would.

## Design

### 1. Backend (schema + queries)

New migration `v24 → v25` in `pkg/claude/common/db/migrate.go`:

```sql
CREATE TABLE agent_group_links (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    from_group_id   INTEGER NOT NULL REFERENCES agent_groups(id) ON DELETE CASCADE,
    to_group_id     INTEGER NOT NULL REFERENCES agent_groups(id) ON DELETE CASCADE,
    mode            TEXT    NOT NULL,        -- see modes below
    created_at      TEXT    NOT NULL,
    by_conv         TEXT    NOT NULL DEFAULT '',  -- '' for human, conv-id for agent
    UNIQUE (from_group_id, to_group_id, mode)
);
CREATE INDEX idx_agent_group_links_from ON agent_group_links(from_group_id);
CREATE INDEX idx_agent_group_links_to   ON agent_group_links(to_group_id);
```

**Modes** (v1 ships the first two; the third is reserved):

| Mode               | Semantics                                                                |
|--------------------|--------------------------------------------------------------------------|
| `members->members` | Any member of `from` may message any member of `to`. Most common.        |
| `owners->members`  | Only owners of `from` may message members of `to`. Manager-style bridge. |
| `members->owners`  | Reserved; not parsed in v1. For "subordinates can reach upstream lead".  |

Links are **directional** by design — `A -> B` does NOT imply `B -> A`.
The human creates the reverse link explicitly if they want symmetric
comm. Symmetric is the common case so the CLI's `--bidir` flag (see
below) inserts both rows in one call.

**DB CRUD** in `pkg/claude/common/db/agent.go`:

- `InsertAgentGroupLink(fromID, toID int64, mode, byConv string) (id int64, err error)`
- `DeleteAgentGroupLink(id int64) error`
- `ListAgentGroupLinks(groupID int64, direction string) ([]*AgentGroupLink, error)` — direction ∈ {`out`, `in`, `both`}
- `LinkReachableGroupsFor(senderID string) (out []groupReach, err error)` — for a given sender, returns `(via_group, target_group, mode)` triples enumerating "which groups can this conv message into via a link?". Used by both the auth check and `why-can-i-message`.

### 2. Auth wire-up

`db.CanSenderReachTarget(senderID, targetID)` gains a third clause,
checked **after** shared-group and owner-of-group (so existing reasons
keep priority when both paths exist — keeps audit logs stable):

```go
// 3. via-link: sender is in (or owns) group A; A has a link to B;
//    target is a member of B.
for each link A->B reachable from senderID:
    if targetID in members(B):
        return groupA, "via-link:"+linkID, nil
```

The reason label `via-link:<id>` lets the recipient's inbox header
(and `why-can-i-message`) cite the exact edge that authorised the
send.

Edge cases:

- **Archived groups.** Existing `requireGroupActive` check applies to
  the routing group (`via.ID`). For via-link, the routing group is the
  *sender's* group — the link is just the bridge — so an archived
  destination group is fine, but an archived source group blocks the
  send. We keep this behaviour for v1; revisit if confusing.
- **Self-links.** Reject `from == to` at insert (use the existing
  shared-group rule for intra-group; a self-link adds no info).
- **Multiple matching links.** Pick the lowest link ID for
  determinism. Same rationale as "pick first shared group by name".

### 3. API (HTTP over Unix socket)

New routes under the existing `/v1/groups/{name}` tree in
`pkg/claude/agentd/handlers.go`:

```
GET    /v1/groups/{name}/links?dir=out|in|both   → [{id, from, to, mode, created_at}]
POST   /v1/groups/{name}/links                   → body: {to: "<group>", mode, bidir?: bool}
DELETE /v1/groups/{name}/links/{id}              → 204
```

Plus a debug endpoint surfaced as the `why-can-i-message` CLI verb:

```
GET /v1/agent/can-message?to=<conv>   → {allowed: bool, reason: "shared-group"|"owner-of-group"|"via-link", via_group: "...", link_id?: N}
```

All gated by the daemon's existing peer-cred + permission logic; see
permissions below.

### 4. Permissions

Three new slugs registered in `pkg/claude/agent/permissions.go`
(default = human-only, matching the rest of `groups.*` admin):

| Slug              | Default | Granted to                                       |
|-------------------|---------|--------------------------------------------------|
| `groups.link.add` | human   | adding `--grant <conv>` lets an agent create links |
| `groups.link.rm`  | human   | symmetric removal grant                          |
| `groups.link.ls`  | open    | reading links is open (same model as `groups.members`) |

The `agent.can-message` debug endpoint is always open — it only
discloses information the caller could derive by trial-and-error
anyway (and helps agents debug their own routing).

**Owner-of-group shortcut.** As elsewhere (`requireCrossAgentPermission`),
an agent that is an owner of the `from` group is allowed to add/remove
links *originating from* that group without needing the slug. The
rationale: an owner already has unilateral comm with the group's
members, so letting them open additional outbound channels is a
natural extension. Adding `to`-side authorisation would require dual
consent and is overkill for v1.

### 5. CLI surface

New verbs under `tclaude agent groups`:

```bash
# read
tclaude agent groups link ls <group> [--dir out|in|both]   # default: both
tclaude agent groups links                                  # all links across all groups (human view)

# write
tclaude agent groups link add <from> <to> [--mode members->members|owners->members] [--bidir]
tclaude agent groups link rm  <link-id>

# debug
tclaude agent groups why-can-i-message <target>
```

`why-can-i-message <target>` prints:

```
allowed: yes
reason:  via-link
path:    proj-falcon (member) → security-review (member of target)
link:    #14  proj-falcon → security-review  mode=members->members
```

…or `allowed: no` with the same probe results to make troubleshooting
self-serve.

### 6. UI (dashboard)

Deferred to a follow-up — the dashboard already renders the groups +
members tree; links want a different shape (edges between nodes). Two
options to consider when we get there:

- **List per group**: each group's panel grows an "Outbound links" /
  "Inbound links" section under members.
- **Graph view**: a top-level "Org map" tab rendering groups as nodes
  and links as arrows. Nicer, more work.

When we ship the dashboard surface, also add the `parent_group_id`
**display-only hint** (separate migration) so groups can be rendered
as a tree without the auth path consulting it.

## Out of scope (for the first PR)

- `parent_group_id` column on `agent_groups` — display-only org hint;
  doesn't affect auth. Wait until the dashboard is ready to render it.
- Transitive link traversal (`A -> B -> C`). v1 is one hop only.
- Per-link audit log (who created which link when). The `by_conv`
  column captures author; an audit table can be added if needed.
- Cross-machine links. Same constraint as the rest of agent-coord.

## Files

- `pkg/claude/common/db/migrate.go` — `migrateV24toV25`, `currentVersion = 25`
- `pkg/claude/common/db/agent.go` — link CRUD, `LinkReachableGroupsFor`, extend `CanSenderReachTarget`
- `pkg/claude/common/db/agent_test.go` — migration + CRUD + auth tests
- `pkg/claude/agentd/handlers.go` — routes for `/v1/groups/{name}/links` + `/v1/agent/can-message`
- `pkg/claude/agent/groups.go` — `link {ls,add,rm}` + `why-can-i-message` verbs
- `pkg/claude/agent/permissions.go` — register the new slugs
- `pkg/claude/agentd/handlers_messages_test.go` — flow test: A in `proj-x`, B in `proj-y`, link added, A messages B successfully; remove link, fails.

## Open questions

- **Bidirectional default?** Currently planned to require `--bidir`
  for symmetric. Alternative: default to bidir and add `--one-way`.
  Symmetric is the common case; revisit if `--bidir` feels noisy.
- **`agent_messages.group_id` for via-link sends.** What should we
  record? The sender's `via` group makes the most sense (audit "this
  message originated from group X") and matches existing semantics —
  but it means an audit reader doesn't immediately see that the
  message *crossed* a link. Add a second column `via_link_id INTEGER
  NOT NULL DEFAULT 0` if/when this matters.
- **Owner-cross-link.** Should being an owner of group A grant you
  the "members->members" permission of every outbound link from A?
  v1: yes (owners are super-members for messaging purposes). Revisit
  if it creates confusion.
