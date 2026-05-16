# Group-addressed sends / role-filtered multicast

Status: **design of record** (Phase 1 — doc only). Implementation is
**parked** pending PO green light, which is itself gated on the
`universal-inbox` feature (PR #102) merging first. See "Dependency".

## Problem

The human wants three send shapes without exploding the `tclaude agent`
verb tree:

1. *"send to group reviewers"* — broadcast to a named group.
2. *"send to group X with role=PO"* — broadcast, but only to the members
   that hold a given role.
3. *"send to my own group"* — broadcast to whatever single group the
   sender belongs to, without having to name it.

Today only (1) exists: `tclaude agent message group:<name> "body"` fans
out via `handleMulticast` (`multicastPrefix = "group:"`,
`agentd/handlers.go`). (2) and (3) have no surface at all.

The naive fix — new verbs like `message-role` / `message-own-group` —
multiplies the command tree and the skill docs for what is really *one*
operation (multicast) with *two* extra knobs (which group, which subset
of members).

## Goal

> Approved design (PO + the `universal-inbox` worker already settled
> this): **NO new verbs.** Extend the existing `group:` multicast target
> grammar plus one new flag. All three human cases then collapse to one
> verb:
>
> ```
> tclaude agent message group:reviewers          "body"   # case 1
> tclaude agent message group:team-A --role PO   "body"   # case 2
> tclaude agent message group:        "body"              # case 3
> ```

## Design

### (a) Extended `group:` target grammar

The multicast target stays `group:<token>`. What changes is how `<token>`
(everything after the `group:` prefix, trimmed) is resolved:

| `<token>`                       | Resolves to                                    |
|---------------------------------|-------------------------------------------------|
| empty (`group:`)                | the sender's own group — see (c)                |
| matches a group **name**        | that group (current behaviour, unchanged)       |
| all-digits, no name match       | the group with that **id** (`agent_groups.id`)  |
| anything else                   | `404 not_found` (current behaviour)             |

**Numeric-id support.** `GetAgentGroupByName(token)` is tried *first*.
Only if it returns no group **and** `token` is all-digits do we fall
back to `GetAgentGroupByID(int64(token))`. This ordering is deliberate:

- It is fully backwards-compatible — every group reachable by name today
  stays reachable, including a group a human chose to *name* `"42"`.
- The id path is a strict fallback for tokens that match no name, so the
  only observable precedence rule is "an explicit name always wins over
  a numeric id." Documented as an edge case below.

Group ids are not a primary human-facing surface (humans use names); the
id form exists for scripting and for the dashboard, which already knows
group ids. The PO brief explicitly asked for it.

### (b) `--role <role>` filter flag

A new optional flag on `tclaude agent message`:

```
--role <role>   Restrict a group: multicast to members whose
                agent_group_members.role matches <role>.
                Error if used with a non-group: (1:1) target.
```

- The `role` column on `agent_group_members` already exists — the recent
  alias-removal work (#106) dropped the per-group *alias*, not the role.
  No schema change.
- The flag value is matched **case-insensitively** (`strings.EqualFold`,
  after `TrimSpace`) against each member's `Role`. Roles are free-form
  human-curated strings (`role: dev`, `role: PO`); case-insensitive match
  is the forgiving choice and avoids a `PO` vs `po` footgun. (Decision —
  see Open questions.)
- `--role ""` (empty after trim) is treated as *no filter* — identical to
  omitting the flag.
- `--role` on a non-`group:` target is a hard **error**, not a silent
  no-op: a 1:1 send has no member set to filter, so the flag is
  meaningless and almost certainly a mistake.

### (c) Empty `group:` = the sender's own group

`group:` with an empty token resolves against the sender's *membership*:

1. `ListGroupsForConv(fromID)` → every group the sender is a member of.
2. Drop archived groups (`g.IsArchived()`) — an archived group is never a
   valid multicast target (`requireGroupActive` would reject it anyway).
3. Then:
   - **exactly 1** active group → use it.
   - **0** active groups → `400 invalid_arg`: *"you are not a member of
     any group; 'group:' needs an explicit name or id"*.
   - **>1** active groups → `400 invalid_arg`, listing the candidate
     names: *"you are a member of N groups (a, b, c); name one
     explicitly, e.g. group:a"*.

Membership — not ownership — is what counts here. A coordinator agent
that *owns* groups but is a *member* of none gets the 0-groups error
from `group:`; that is correct, because "my own group" is ambiguous for a
manager and they should name the team. (Owner-of-group reach is still
fully available via the explicit `group:<name>` form — the auth gate in
(d) already permits it.)

### (d) Authorization — unchanged

`handleMulticast` keeps its **existing member-or-owner gate**: the sender
must be a member of, or an owner of, the resolved group to broadcast into
it. This is multicast *into* a group and is deliberately distinct from
the `message.direct` slug that `universal-inbox` adds for 1:1 off-group
sends. **This design does not touch that gate's policy.**

The `--role` filter is applied *after* and *on top of* the gate: the gate
authorizes the send against the whole group; the role filter then narrows
the recipient set. A sender who is allowed to broadcast to `group:team-A`
is allowed to broadcast to `group:team-A --role PO` — the filter can only
ever *shrink* the audience, never widen it or cross a trust boundary.

### (e) Handler changes (`agentd/handlers.go`)

**`sendReq`** gains one field:

```go
type sendReq struct {
    To      string   `json:"to"`
    Cc      []string `json:"cc,omitempty"`
    Subject string   `json:"subject,omitempty"`
    Body    string   `json:"body"`
    Role    string   `json:"role,omitempty"` // multicast role filter
}
```

**`handleMessages`** — one early validation, before the multicast/direct
split: if `strings.TrimSpace(req.Role) != ""` **and** `req.To` does not
have the `group:` prefix → `400 invalid_arg` (*"--role is only valid with
a group: target"*). The daemon is the authoritative boundary (any client
can POST); the CLI adds a friendlier pre-check too — see (f).

**`handleMulticast`** — restructured into three steps:

1. **Resolve the group.** Replace the current "trim prefix → reject empty
   → `GetAgentGroupByName`" block with a `resolveMulticastGroup(fromID,
   token string) (*db.AgentGroup, *muxErr)` helper implementing grammar
   (a) + own-group rule (c). It returns the group, or a typed error
   carrying the right HTTP status (400 for own-group ambiguity / empty,
   404 for unknown name/id, 500 for DB errors).
2. **Auth + active checks** — `requireGroupActive` then the member-or-owner
   gate, both **exactly as today**.
3. **List + filter + fan out.** `ListAgentGroupMembers(g.ID)`, then:
   - skip `m.ConvID == fromID` (self — unconditional, as today),
   - if `req.Role` is non-empty, skip members whose `Role` does not
     `EqualFold` the requested role,
   - insert an `agent_messages` row + `nudgeIfAlive` for each survivor
     (the existing succession-walk / per-recipient error handling is
     unchanged).

Multicast rows continue to carry the real `group_id` (`g.ID`) — this
feature does not produce direct (`group_id = 0`) messages, so it is
orthogonal to `universal-inbox`'s schema change.

`sendResp` is unchanged: `ViaGroup` + the per-recipient `Recipients`
slice already express everything the CLI needs. (No need to echo the
role — the CLI knows what it sent.)

### (f) CLI changes (`agent/message.go`, `agent/completion.go`)

`messageParams` gains:

```go
Role string `long:"role" optional:"true" help:"Restrict a group: multicast to members with this role (group: targets only)"`
```

- `messageCmd` `InitFuncCtx`: wire a `completeRoles` alternatives func on
  `--role` (new helper in `completion.go` — distinct non-empty values
  from `agent_group_members.role`).
- `runMessage` (before the daemon round-trip): if `p.Role != ""` and
  `p.Target` lacks the `group:` prefix → friendly error + `rcInvalidArg`.
  This is a UX nicety; the daemon check in (e) is the authoritative one.
- `runMessageDaemon`: add `"role": p.Role` to the JSON payload when
  non-empty.
- Multicast result rendering: **always print the resolved recipient
  count** so a typo'd `--role` is visible rather than silently doing
  nothing (PO ask, msg #102). The non-empty path already prints
  `N recipients (...)`; the empty path must too:
  - role filter active, `resp.Recipients` empty → role-aware line that
    states the count, e.g. *"0 recipients: no members with role %q in
    group %q; nothing sent."* — replaces the current *"you're the only
    member"* text, which would be a wrong explanation.
  - no role filter, `resp.Recipients` empty → keep today's
    *"you're the only member"* line (it already conveys 0 sent).
  Both stay `rcOK` — see Open questions.
- Help text: update the `Target` positional help and the command `Long`
  to document `group:<name|id>`, empty `group:` = own group, and
  `--role`.

### Edge cases

| Case | Behaviour |
|------|-----------|
| `--role` matches **nobody** in the group | `200`, `recipients: []`, no rows written. CLI prints a clear "0 members with role X" line. Consistent with today's empty-group multicast (already a non-error 200). |
| Sender in **0** active groups, `group:` empty | `400` — "not a member of any group". |
| Sender in **>1** active groups, `group:` empty | `400`, error lists the candidate group names. |
| `--role` on a **1:1** (non-`group:`) target | `400` (daemon) + friendly CLI pre-check. |
| `group:42` where a group is **named** `"42"` | Name wins — resolves to that group. The id path is fallback-only. |
| `group:99999` — no name, no such id | `404 not_found`. |
| `--role ""` (empty value) | Treated as no filter. |
| Sender is **owner-not-member**, `group:X --role dev` | Allowed by the existing owner gate; role filter applies to members normally. |
| Archived group, by name or as own-group candidate | Rejected by `requireGroupActive` / excluded from own-group counting. Unchanged. |
| `--cc` on a `group:` target | Pre-existing behaviour: `handleMulticast` ignores `req.Cc`. **Out of scope** — not changed here, just noted. |

### Flow-test plan

New `pkg/claude/agentd/group_addressed_sends_flow_test.go` (testharness
v2 `Flow` DSL — `HaveGroup` / `HaveMember` / `HaveAliveSession`,
`AssertSentContains`, raw `f.do(...)` for `POST /v1/messages`). Roles are
set via the `role` argument to `HaveMember` / `AddAgentGroupMember`.

1. **`group:<name>` regression** — baseline fan-out to all non-sender
   members still works (guard against the resolver rewrite).
2. **`group:<id>`** — numeric id resolves to the same group; fan-out
   identical to test 1.
3. **`group:` empty, sender in exactly 1 group** — resolves to that
   group, fans out.
4. **`group:` empty, sender in 0 groups** — `400`.
5. **`group:` empty, sender in 2 groups** — `400`, error body names both
   groups.
6. **`--role` filter** — group with mixed roles (`PO`, `dev`, `dev`);
   `--role dev` → only the two `dev` members get rows; the `PO` member
   gets none; `recipients` lists exactly the `dev` members.
7. **`--role` matches nobody** — `--role qa` on the test-6 group →
   `200`, `recipients: []`, zero rows written.
8. **`--role` on a 1:1 target** — `POST` with `role` set and a
   conv-id `to` → `400`.
9. **`--role` case-insensitivity** — `--role po` matches a member whose
   stored role is `PO`.
10. **Numeric token equal to a real group *name*** — group named `"7"`;
    `group:7` resolves to it (name precedence), not to id 7.
11. **Owner-not-member multicast with `--role`** — owner (not a member)
    sends `group:X --role dev` → allowed, only `dev` members get rows.
12. **Unknown name/id** — `group:does-not-exist` → `404` (regression).

No DB-level / migration test is needed — there is **no schema change**
(the `role` column already exists).

## Dependency — `universal-inbox` (PR #102) must merge first

The human's explicit steer: *"merge the universal inbox first."* Phase 3
implementation must branch off a `main` that already contains
`universal-inbox`.

The two features are **mostly orthogonal**:

- `universal-inbox` makes `agent_messages.group_id` optional (`0` =
  direct, no routing group), drops the FK, adds the `message.direct`
  slug, and reworks the *1:1 direct* path (`resolveMessageRouting`,
  `handleMessages` / `handleMultiRecipient` / `handleMessageReply`).
- This feature touches only the **multicast** path (`handleMulticast`,
  the `group:` grammar) and the CLI flag. Multicast rows always carry a
  real `group_id`, so they never produce the new `0`-sentinel rows and
  never exercise `message.direct`. The `universal-inbox` doc itself
  confirms: *"handleMulticast keeps its member-or-owner gate."*

The only real overlap is **textual** — both features edit `sendReq` in
`handlers.go`, the `handleMessages` dispatch region, and `agent/message.go`
/ its tests. Expect small merge conflicts, no semantic ones. Building
Phase 3 on a post-`universal-inbox` `main` makes those conflicts
disappear entirely.

## Phasing

- **Phase 1 — design (this doc).** Small doc-only PR. PO notified with
  the link.
- **Phase 2 — park.** No implementation until PO gives the explicit green
  light (gated on `universal-inbox` merging to `main`).
- **Phase 3 — implement.** Fresh worktree, separate PR rebased on current
  `main` (post-`universal-inbox`), with the flow tests above. This doc
  moves to `docs/plans/DONE/`.

## Resolved questions

Both were raised as low-stakes "defaults chosen, PO can veto" — **PO
approved both as defaulted** (msg #102):

- **Role match case-sensitivity → case-insensitive** (`EqualFold`).
  Roles are free-form human-set strings; nobody sanely creates `PO` and
  `po` as distinct roles. Confirmed.
- **`--role` matches nobody → non-error `200`** with an empty recipient
  list, consistent with today's "empty group" multicast. Confirmed, with
  one PO ask now folded into (f): the CLI must **always print the
  resolved recipient count** (incl. `0 recipients`) so a typo'd `--role`
  is visibly a no-op rather than a silent one.
