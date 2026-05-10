# Dashboard: group membership editing UX

Direct-manipulation editing on the Groups tab. Today every membership
change requires a CLI command — this file is the biggest UX win on
the dashboard's wishlist.

Carve-out from `med-prio/web-dashboard.md`. Shippable independently.

Two complementary affordances, scoped together because they share
the same framework migration prerequisite, the same daemon
endpoints, and the same flow-test surface:

1. **Drag-and-drop** between groups (move + Ctrl-clone + ungrouped
   virtual group as a drag source).
2. **Per-group `+ add member` button** with a live text-search
   filter over candidate agents.

## Part 1 — Drag-and-drop

Three behaviours, in priority order:

1. **Move (no modifier).** Drag a member row from group A onto group
   B's header → the conv leaves A and joins B. Order the calls:
   `POST groups/B/members` then `DELETE groups/A/members/{conv}`,
   so the conv is never groupless mid-drag. On a failed delete the
   conv ends up in both groups (visible, recoverable) rather than
   nowhere (silently lost).
2. **Clone (Ctrl+drag).** Drops an `agent clone` of the source row
   into the target group; original stays in A. Uses the already-
   shipped `POST /v1/agent/{conv}/clone` endpoint (the same one
   `tclaude agent clone` calls). Inherits the `-clone-<N>` alias
   suffix scheme already wired into `runCloneOrchestration`.
3. **Ungrouped virtual group.** Pinned row at the top or bottom of
   the Groups tab listing every conv-id that is online OR has a
   recent session AND holds zero `agent_group_members` rows. Acts
   as a drag SOURCE: drag an ungrouped agent into a real group to
   add it. Hidden / shows an empty-state line when the set is empty.

Drop targets pulse on hover. A small modifier-hint pill ("→ move",
"→ clone") follows the cursor while dragging so the user can see
which behaviour they're about to commit before releasing.

## Part 2 — Per-group `+ add member` button

Each group's header gets a `+ add member` button alongside the
existing per-group affordances (delete, future rename). Clicking
opens an overlay/dropdown anchored to the button:

- **Text input** at the top, autofocused. Live-filters the candidate
  list as the user types — match on alias, role, descr, and
  conv-id prefix. Case-insensitive substring match is fine for v1.
- **Scrollable candidate list**. Each row shows alias / role /
  descr / online indicator / "groups: A, B, C" so the user has
  context before adding. Members already in this group are
  filtered out (cleaner than disabling them with an "(in this
  group)" tag).
- **Keyboard nav**: ↑/↓ moves the highlight, Enter adds, Esc
  closes.

Selecting a candidate calls `POST /v1/groups/{name}/members` with
that conv-id. After a successful add, **keep the overlay open**
with the just-added row removed from the list — the close-on-add
pattern forces a re-click for every member, which is the exact
pain we're fixing. The user dismisses with Esc / click-outside
when done.

### Candidate set

"Every conv we know about that isn't already a member of this
group". Concretely, the union of:

- Members of any group (for promoting across groups).
- Currently-online conv-sessions (covers fresh agents not yet in
  any group — same pool the ungrouped virtual group draws from).
- An optional **"include all conversations"** checkbox at the
  bottom of the overlay extends the set to every conv-id we know
  about, for the rare case of pulling in a long-archived conv.
  Off by default; when on, the search filter becomes load-bearing
  to keep the list manageable.

This is the same data the snapshot extension for the ungrouped
virtual group surfaces — reuse it. No new endpoint needed beyond
`POST groups/{name}/members` (which already ships).

## Explicitly out of scope (deferred)

Belong to later slices, NOT this file:

- **Shift+drag (multi-membership).** Add to B without removing from
  A. Useful eventually; ship after move + clone bake.
- **Drop ON the ungrouped row** to remove from every group ("kick
  from all groups I'm in"). Destructive and easy to misclick.
- **Inline rename.** Separate item.
- **Cross-group bulk move/clone via multi-select.** Single-row DnD
  first.

## Framework migration prerequisite

`web-dashboard.md` already flagged this: vanilla JS for
`dashboard.html` crossed the ~700-line threshold when the Cron tab
landed (status: trigger fired, 2026-05). Both halves of this file
genuinely want component state — DnD wants ghost images / drop-
zone hover / modifier-hint pill / optimistic refresh; the
add-member overlay wants live-filtered list, keyboard nav, and
the keep-open-after-add pattern.

**Do the framework migration FIRST**, then build these on the
new foundation. Candidates: React / Preact / Svelte / Solid.
Build chain: vite + esbuild to keep the embedded `//go:embed`
asset small. Don't ship DnD or the overlay on top of vanilla JS
just to redo it after the migration.

## Daemon endpoints

All behaviours use endpoints that ALREADY ship — no new daemon
mutations needed:

| Behaviour | Endpoint(s) |
|-----------|-------------|
| Move | `POST /v1/groups/{B}/members` then `DELETE /v1/groups/{A}/members/{conv}` |
| Clone | `POST /v1/agent/{conv}/clone` (target-group in body) |
| Add-from-overlay | `POST /v1/groups/{name}/members` |
| Ungrouped enum + candidate list source | shipped — `/api/snapshot.ungrouped[]`, see `DONE/dashboard-snapshot-ungrouped.md` |

For the snapshot extension: add an `ungrouped[]` array of
`{conv_id, alias?, online, last_seen}` plus a broader `agents[]`
array (members of any group ∪ online conv-sessions ∪ optionally
all known convs when a query flag is set). Cheaper than separate
endpoints and keeps the dashboard's single-poll model intact.

Auth: dashboard cookie (already wired). Humans bypass slug
checks; the underlying mutations go through the same daemon code
paths the CLI uses, so audit columns (`granted_by`) get filled
the usual way.

## Optimistic UI + rollback

Both halves feel broken without optimistic updates — the dropped
row / added member should appear immediately, not after the next
5-second poll. On success: nothing more to do (the next poll
confirms). On failure: snap back to the prior state and surface
the error in a toast. The framework choice should make this
trivial; don't hand-roll it on vanilla JS.

## Test coverage

Per project convention (and the auto-memory feedback note): add a
flow test under `pkg/claude/agentd/*_flow_test.go` exercising:

- **Move**: set up agent in group A, call the add+remove sequence,
  assert membership in B and not A via the production read path
  (`GET /v1/groups/{name}/members`).
- **Clone-into-group**: assert the clone lands in target group with
  a `-clone-<N>` alias and the original is untouched in source.
- **Ungrouped enumeration**: spawn an agent, immediately remove it
  from its only group, assert it appears in the snapshot's
  `ungrouped[]` array.
- **Add-member overlay path**: snapshot exposes a candidate list;
  POST to `groups/{name}/members` with one of those conv-ids; the
  next snapshot shows the conv as a member and removes it from the
  ungrouped list (if it had been there).

The DnD interaction and the overlay UX are JS — out of scope for
Go flow tests. Whatever framework lands brings its own integration
test story.

## Files (when implementing)

- `pkg/claude/agentd/dashboard.html` (or the new component tree
  under `pkg/claude/agentd/dashboard/` post-migration)
- `pkg/claude/agentd/dashboard.go` — snapshot extension for the
  ungrouped + candidate-list arrays
- `pkg/claude/agentd/dashboard_edit.go` — sanity-check the clone
  path and the `groups/{name}/members` POST are reachable from
  the dashboard cookie auth (the move endpoints already are)

## Cross-references

- `med-prio/web-dashboard.md` — parent v2 dashboard plan. The
  DnD + add-member bullets there now point HERE; don't edit in
  two places.
- `high-prio/agent-clone.md` (if present) — clone semantics this
  file leans on.

## When done

Delete this file or rewrite it inline with what shipped, then
append a one-line entry to `DONE/index.md`.
