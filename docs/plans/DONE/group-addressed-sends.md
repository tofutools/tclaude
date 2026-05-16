# Group-addressed sends / role-filtered multicast — SHIPPED

Status: **shipped.** Extends the `group:` multicast target grammar on
`tclaude agent message` so three send shapes collapse to one verb + one
flag — no new verbs. Built on top of `universal-inbox` (PR #102).

## What shipped

The human's three send shapes, all on the existing `message` verb:

```bash
tclaude agent message group:reviewers          "body"   # named group
tclaude agent message group:7                  "body"   # group by id
tclaude agent message group:                   "body"   # own group
tclaude agent message group:team-A --role PO   "body"   # role-filtered
```

### Extended `group:` target grammar

The multicast target stays `group:<token>`; the token after the prefix
(trimmed) resolves as:

| `<token>`                       | Resolves to                                    |
|---------------------------------|-------------------------------------------------|
| empty (`group:`)                | the sender's own group                          |
| matches a group **name**        | that group (pre-existing behaviour)              |
| all-digits, no name match       | the group with that **id** (`agent_groups.id`)   |
| anything else                   | `404 not_found`                                  |

`GetAgentGroupByName` is tried first; the numeric-id fallback
(`GetAgentGroupByID`) only fires when the token matches no name — so a
group a human chose to *name* `"42"` stays reachable, and the only
precedence rule is "an explicit name wins over a numeric id."

Empty `group:` resolves against the sender's *membership*
(`ListGroupsForConv`, archived groups excluded): exactly one active
group → use it; 0 or >1 → `400` (the >1 error names every candidate).
Membership, not ownership, is what counts.

### `--role` filter

`--role <role>` restricts a `group:` multicast to members whose
`agent_group_members.role` matches the value, case-insensitively
(`EqualFold` after `TrimSpace`). It is a `400` on a 1:1 (non-`group:`)
target — checked both in the CLI (fast feedback) and the daemon
(authoritative). The filter narrows the recipient set *after* the
member-or-owner auth gate, so it can never widen reach. A `--role` that
matches nobody is a non-error `200` with an empty recipient set — the
CLI prints a visible `0 recipients` line so a typo is not a silent
no-op. No schema change: the `role` column already existed (the
alias-removal work, #106, dropped the alias, not the role).

### Authorization

Unchanged. `handleMulticast` keeps its member-or-owner gate; the role
filter is applied to the recipient set afterwards. Multicast rows carry
the real `group_id`, so the feature is orthogonal to `universal-inbox`'s
`group_id = 0` direct-message sentinel and never touches the
`message.direct` slug.

## Touch points

- `agentd/handlers.go` — `sendReq.Role` field; a `--role`-requires-
  `group:` early validation in `handleMessages`; new
  `resolveMulticastGroup` (name → numeric-id grammar) and
  `resolveOwnGroup` (empty-`group:` = own group) helpers; `handleMulticast`
  restructured to resolve via those helpers and apply the role filter in
  the fan-out loop.
- `agent/message.go` — `messageParams.Role` flag; `--role`-on-1:1 CLI
  pre-check in `runMessage`; `role` added to the `/v1/messages` payload;
  role-aware `0 recipients` rendering; updated `Target`/`Short`/`Long`
  help text.
- `agent/completion.go` — `completeRoles` alternatives func (distinct
  non-empty roles from `agent_group_members`), wired onto `--role`.
- `agent/skills/agent-coord/SKILL.md` — "Broadcasting to a group"
  section documents `group:<name|id>`, bare `group:` = own group, and
  `--role`.
- `testharness/flow.go` — `HaveMemberWithRole` DSL helper (`HaveMember`
  delegates to it with an empty role).

## Flow tests

`pkg/claude/agentd/group_addressed_sends_flow_test.go` — 12 scenarios:

1. `group:<name>` regression — fan-out to all non-sender members, rows +
   nudge.
2. `group:<id>` — numeric id resolves to the group; row carries its id.
3. Bare `group:`, sender in exactly one group → resolves to it.
4. Bare `group:`, sender in no group → `400`.
5. Bare `group:`, sender in >1 group → `400` naming both candidates.
6. `--role` narrows the fan-out; non-matching members get no row.
7. `--role` matches nobody → `200`, empty recipients, no rows.
8. `--role` on a 1:1 target → `400`, no row.
9. `--role` match is case-insensitive (`--role po` ↔ stored `PO`).
10. Numeric token equal to a real group *name* → name precedence.
11. Owner-not-member multicast with `--role` → allowed, filter applies.
12. Unknown name and unknown numeric id → `404`.

No DB-level / migration test — there is no schema change.

## History

- **Phase 1 — design** (PR #108, doc only) — design of record; PO
  approved, with case-insensitive role matching and the `0 recipients`
  CLI line confirmed (PO msg #102/#107).
- **Phase 2 — park** — held until `universal-inbox` (PR #102) merged.
- **Phase 3 — implement** — this change, combined into PR #108 (the doc
  move + implementation + flow tests, one atomic PR), rebased on
  post-`universal-inbox` `main`.
