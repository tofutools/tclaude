# Succession-aware message routing + global head aliases

Shipped 2026-05.

Closes the gap [`DONE/conv-succession-chain.md`](conv-succession-chain.md)
flagged: messages addressed to a superseded conv-id used to land in
the archived inbox where nobody read them. Now the daemon walks the
chain forward to the live successor on every send / reply, and the
new global head-alias layer gives humans a stable handle that survives
arbitrary reincarnation depth without ever needing to be re-pointed.

Two complementary slices, separate commits:

- **Phase 1** (commit 706ca5c) — chain-walk on send + reply,
  Original-To: header, sender redirect notice.
- **Phase 2** (this doc's commit) — global head-alias layer
  (`tclaude agent alias …`), resolver integration, daemon endpoints.

## Phase 1 — chain-walk on send + reply

### Wire shape

- `agent_messages.original_to_conv` — schema v21, `TEXT NOT NULL DEFAULT ''`.
  Non-empty when the daemon redirected a send away from the addressed
  conv-id; carries the original target for forensics + recipient
  surface.
- `sendResp.redirected_from` — populated on the direct-send
  response when the redirect happened. Mirrored as
  `recipients[].redirected_from` for `--cc` / multicast.
- `GET /v1/messages/{id}` — gains `original_to_conv` in the JSON
  body so `inbox read` can render an `Original-To:` header.
- `tclaude agent message` — when the response indicates a redirect,
  prints `→ redirected from <id>, superseded by current target`
  inline so the sender notices their stale selector.
- `tclaude agent inbox read` — renders
  `Original-To: <id> (superseded by current <id>)` when the column
  is non-empty, both via the daemon and the direct-DB path.

### Redirect attribution

`agent.ResolveSelector` already auto-redirected indexed convs to the
chain head via `redirectResolvedToLatest`. The handler can't tell
*whether* a redirect happened just from the resolved id, so we
attribute by walking the *raw input string* through
`db.ResolveLatestConv`. When the input differs from the resolved
target AND walks to it via the chain, the input was a literal
superseded conv-id and we record `original_to_conv = input`. Alias /
prefix inputs naturally skip the branch (no chain row keyed on alias
text), so only literal-id inputs trigger the `Original-To:` surface
— exactly the use case the column is for.

### Resolver fallback for pruned convs

`tryResolve` gained a final fallback (`5)` in the cascade): when the
selector matches nothing in conv_index, group members, or by title,
try `db.ResolveLatestConv(selector)`. A succession row alone is
enough to declare the input a known historical id. Covers the
"old conv pruned but the chain is still there" case for cross-machine
sync, manual DB edits, or pruned conv_index entries.

### Authority check

The send handler runs `db.CanSenderReachTarget` against the LIVE
successor, not the input. The outdated id may have lost membership
by the time the successor took over; the successor is who actually
receives the message. Defensive: if the successor is the sender
itself (rare manager-pattern edge case), the existing self-message
rejection still fires.

### Tests

`pkg/claude/agentd/succession_routing_flow_test.go`:

- `TestMessageRouting_SupersededConv_RoutesToSuccessor` — Alice
  messages bob-old (superseded); message lands in bob-new with
  `original_to_conv = bob-old`.
- `TestMessageRouting_MultiHopSuccession_FollowsToHead` — chain
  bob-v0 → bob-v1 → bob-v2; original target attribution names v0.
- `TestReply_FromSupersededSender_RoutesToSuccessor` — reply path
  rewrites the original sender's id to the live head.
- `TestMessageRouting_RedirectsOntoSelf_Rejected` — chain pointing
  at the sender's own conv yields 400 (`cannot message self`)
  rather than a self-loop.
- `TestMessageRouting_NoSuccession_NoRedirect` — fast path:
  `redirected_from` is omitted, `original_to_conv` stays empty
  when there's nothing to walk.

## Phase 2 — global head-alias layer

### Why both?

Per-group `agent_group_members.alias` ALREADY survives reincarnate:
the orchestration eagerly migrates the row onto the new conv-id with
the same alias text, so messaging "bob" within a group already lands
on the live head. The chain-walk closes the gap for *direct conv-id
references* (raw UUIDs in scripts, transcripts, replies). The new
global head-alias layer is the third leg: a stable handle that's
NOT scoped to a group — useful when a conv isn't in any group, or
when a global "ceo" / "po" handle reads cleaner than per-group
membership.

### Schema + helpers

Schema v22 adds `agent_head_aliases (handle, anchor_conv_id, created_at, by_conv)`.
`handle` is the PRIMARY KEY; lower-cased on insert so case folding
doesn't surprise lookups. `anchor_conv_id` is the conv we initially
pointed at; the **current head** is computed lazily by walking the
succession chain forward from anchor via
`db.ResolveLatestConv` at lookup time. Multi-hop chains collapse to
the head; the row never has to be re-pointed on reincarnate.

Helpers (`pkg/claude/common/db/agent_head_aliases.go`):

- `SetHeadAlias(handle, anchor, byConv)` — INSERT OR REPLACE, idempotent.
- `RemoveHeadAlias(handle)` → rows removed.
- `GetHeadAlias(handle)` — raw row (no chain walk).
- `ResolveHeadAlias(handle)` — anchor walked through `ResolveLatestConv`.
- `ListHeadAliases()` — sorted by handle, used by `ls` and dashboard.
- `ValidateHeadAliasHandle(h)` — rejects empty, ".", "-", `group:` prefix,
  whitespace / path separators, UUID-shaped strings (would shadow
  conv-id selectors).

### Resolver integration

`tryResolve` checks the head-alias table BEFORE indexed conv-id
lookup (step 0 in the cascade). Handles take precedence because
they're validated to never shadow UUIDs / `group:` / `.` / `-` —
no ambiguity possible. A successful head-alias hit returns the
chain head, optionally decorated with the conv_index row when one
exists (for display titles).

### Daemon endpoints

Mounted in `serve.go`'s mux:

```
GET    /v1/agent/aliases             # list every handle (open)
POST   /v1/agent/aliases             # set handle → conv (human-only)
GET    /v1/agent/aliases/{handle}    # read one (open)
DELETE /v1/agent/aliases/{handle}    # drop handle (human-only)
```

`POST` and `DELETE` go through `requireHuman` (no claude ancestor in
the caller's process tree). v1 is human-only by design; agent paths
can ladder up via a future slug if a real use case appears. The
list / get verbs are open — same threat model as `/v1/peers`.

The `set` endpoint runs the conv selector through `agent.ResolveSelector`
just like the rest of the daemon, so any selector form (UUID / prefix
/ title / per-group alias) works.

### CLI

`tclaude agent alias …`:

- `set <handle> <conv-selector>` — anchor a handle to a conv
- `rm <handle>` — drop the handle
- `ls` — list every handle and its current head + title
- `get <handle>` — resolve one handle, optionally as JSON

`ls` and `get` render the chain-walk transparently: when the head
differs from the anchor (the agent has been reincarnated since the
handle was set), the output shows `anchor:abc12345 → head:def67890`
so the human sees the chain has moved.

### Tests

`pkg/claude/agentd/head_alias_flow_test.go`:

- `TestHeadAlias_SurvivesReincarnationChain` — set handle on bob-v0,
  reincarnate twice; messaging the handle lands on bob-v2 (the
  multi-hop case the user flagged in design review).
- `TestHeadAlias_DaemonEndpointsHappyPath` — POST / GET (single +
  list) / DELETE round-trip; pins lower-casing on insert and
  observable 404 on re-delete.
- `TestHeadAlias_AgentMutationsRejected` — agent peers get 403 on
  POST and DELETE; pre-existing rows survive a rejected DELETE.
- `TestHeadAlias_ValidationRejectsUnsafeHandles` — empty, "." / "-",
  `group:` prefix, whitespace, path separators, UUID-shaped strings
  all surface 400 before hitting the DB.

## Per-group alias semantics — reconfirmed during design

The user asked during review whether group aliases get the `-r-N`
suffix on reincarnate. They DON'T:

- `agent_group_members.alias` (per-group handle) — preserved verbatim
  by the reincarnate orchestration (`reincarnate.go:333-349`).
  Sender-friendly: messaging "bob" within a group always lands on
  the current incarnation.
- Conv title (`/rename` injected on the new pane) — DOES get
  `-r-N` so the operator-visible name reflects the generation.
- Old pane title — gets `-x` injected before the pane dies
  (`reincarnate.go:464-465`).

So on top of group-member aliases (already shipped, in-group stable
handles) the layers added here are:

1. **Chain-walk on send** — fallback for direct conv-id references
   that bypass aliases entirely.
2. **Global head alias** — stable handle for convs that aren't in
   a group, or where a global handle reads cleaner.

Per the user's "as long as archived convs get -x" — already met for
the reincarnation case; nothing new to add.

## Files

- `pkg/claude/common/db/migrate.go` — schema v21 + v22 migrations,
  `currentVersion = 22`.
- `pkg/claude/common/db/agent.go` — `AgentMessage.OriginalToConv`
  field; INSERT / GET / scan threading.
- `pkg/claude/common/db/agent_head_aliases.go` — new file. Set / Get
  / Remove / Resolve / List / Validate helpers.
- `pkg/claude/agent/lookup.go` — head-alias step in `tryResolve`,
  succession-chain fallback.
- `pkg/claude/agent/inbox.go` — `Original-To:` rendering (daemon +
  direct paths).
- `pkg/claude/agent/message.go` — sender redirect notice on
  direct + per-recipient.
- `pkg/claude/agent/alias.go` — new file. CLI commands (set, rm,
  ls, get).
- `pkg/claude/agent/agent.go` — register `aliasCmd()`.
- `pkg/claude/agentd/handlers.go` — chain-walk + attribution in
  `handleMessages`, `handleMultiRecipient`, `handleMulticast`,
  `handleMessageReply`. New `walkSuccession` helper.
  `original_to_conv` in the GET /v1/messages/{id} response.
- `pkg/claude/agentd/head_aliases.go` — new file. HTTP handlers
  for `/v1/agent/aliases[/{handle}]` + `requireHuman` helper.
- `pkg/claude/agentd/serve.go` — register the routes ahead of the
  catch-all `/v1/agent/`.
- `pkg/claude/agentd/succession_routing_flow_test.go` — 5 flow tests
  (Phase 1).
- `pkg/claude/agentd/head_alias_flow_test.go` — 4 flow tests
  (Phase 2).

## Out of scope (deferred)

- **`alias.set` / `alias.rm` slugs.** v1 is human-only at the daemon.
  An agent that needs to wire its own handles can ladder up later.
- **Eager rewrite of in-flight `agent_messages.to_conv = old`.** The
  reincarnate orchestration already migrates membership / perms /
  cron-job refs eagerly; pending undelivered messages (rare) take
  the lazy chain-walk on the next send. Promote to eager only if
  measurement shows the walk is hot.
- **Cross-machine succession chains.** Out of scope while tclaude
  is single-host.
- **Visualising the chain in the dashboard.** A small "older
  versions" toggle on each agent row showing reincarnation history
  is plausible UX. Separate piece of work.
- **Sender opt-out** (`--no-follow-succession`). Probably
  unnecessary; ship if it shows up as a real ask.

## Cross-references

- [`DONE/conv-succession-chain.md`](conv-succession-chain.md) —
  schema + `RecordConvSuccession` + `ResolveLatestConv`. This file
  finally wires those into the message path.
- [`DONE/agent-self-lifecycle.md`](agent-self-lifecycle.md) —
  reincarnate is the producer of succession rows.
