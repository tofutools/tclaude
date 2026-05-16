# Inter-group links + via-link auth path (2026-05)

Shipped in PR #51 (commit `59f18ab`). `agent_groups` was a flat
label set; cross-team messaging forced everyone into one shared
group or abused the owner relation. Inter-group **links** add a
flat, declarative "members of A may message members of B" edge
without either group absorbing the other — so the same
`agent_groups` table now doubles as a label / POV system (a conv
can sit in `engineering` + `proj-falcon` + `oncall-w19` and a link
bridges two of them). Nested groups were explicitly rejected.

## Schema

`migrateV24toV25` creates `agent_group_links`:

```sql
CREATE TABLE agent_group_links (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    from_group_id INTEGER NOT NULL REFERENCES agent_groups(id) ON DELETE CASCADE,
    to_group_id   INTEGER NOT NULL REFERENCES agent_groups(id) ON DELETE CASCADE,
    mode          TEXT    NOT NULL,
    created_at    TEXT    NOT NULL,
    by_conv       TEXT    NOT NULL DEFAULT '',   -- '' for human, conv-id for agent
    UNIQUE (from_group_id, to_group_id, mode)
);
-- + idx_agent_group_links_from / _to
```

Links are **directional** — `A -> B` does not imply `B -> A`; the
CLI's `--bidir` flag inserts both rows. Modes: `members->members`
and `owners->members` ship; `members->owners` reserved.

## Auth wire-up

`db.CanSenderReachTarget` gained a third clause, checked **after**
shared-group and owner-of-group so existing reasons keep priority.
When a link authorises the send the reason label is
`via-link:<id>` (see `pkg/claude/common/db/agent.go:1066`), so the
recipient's inbox header and `why-can-i-message` can cite the exact
edge.

## Permissions

- `groups.link.add` (`PermGroupsLinkAdd`) — default human-only.
- `groups.link.rm` (`PermGroupsLinkRm`) — default human-only.
- Reading links (`link ls` / `links`) is open, matching the
  `groups.members` model — no slug.
- **Owner shortcut:** an owner of the `from` group may add/remove
  outbound links originating from that group without the slug.

## CLI surface (`tclaude agent groups`)

```
groups link ls <group> [--dir out|in|both]   # links touching a group
groups links                                 # every link across all groups
groups link add <from> <to> [--mode ...] [--bidir]
groups link set-mode <link-id> <mode>
groups link rm <link-id>
groups why-can-i-message <target>            # explain / debug routing
```

## HTTP

Routes under the existing group tree in `agentd`:
`GET/POST /v1/groups/{name}/links`, `DELETE
/v1/groups/{name}/links/{id}`, plus `GET /v1/agent/can-message`
(always open — only discloses what trial-and-error would).

## Dashboard

Shipped — not deferred as the original plan expected. The snapshot
carries a `links` array (`dashboardLink`, `collectLinksSnapshot` in
`dashboard.go` via `db.ListAllAgentGroupLinks`) and each group
panel renders an inbound/outbound `.group-links-section` table in
`dashboard.html`.

## Files

- `pkg/claude/common/db/migrate.go` — `migrateV24toV25`
- `pkg/claude/common/db/agent_links.go` (+ `agent_links_test.go`)
- `pkg/claude/common/db/agent.go` — `CanSenderReachTarget` via-link clause
- `pkg/claude/agentd/groups_links.go` — HTTP routes
- `pkg/claude/agent/groups_links.go` — CLI verbs
- `pkg/claude/agentd/permissions.go` — `PermGroupsLinkAdd` / `PermGroupsLinkRm`
- `pkg/claude/agentd/dashboard.go` + `dashboard.html` — links surface

## Deferred (not part of this work)

- **`parent_group_id`** display-only org-tree hint on `agent_groups`
  — a separate migration; wait until the dashboard renders groups
  as a tree. Doesn't affect auth.
- **Graph / "Org map" view** in the dashboard — current surface is
  the list-per-group table; a node/edge graph is a nice-to-have.
- **Transitive link traversal** (`A -> B -> C`) — v1 is one hop.
- Cross-machine links — same constraint as the rest of agent-coord.
