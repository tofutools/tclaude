# Dashboard `/api/snapshot` ungrouped extension (2026-05)

The dashboard's snapshot endpoint now surfaces a dedicated
`ungrouped[]` array of online conv-sessions that aren't members of
any group. This is the data foundation for the eventual
"Ungrouped virtual group" drag source and the `+ add member` overlay
candidate list — both shippable on top of this without further
backend work once the dashboard JS framework migration lands.

Carve-out from `dashboard-group-membership-ux.md`. Backend-only —
no JS / framework migration involved.

## Wire shape change

`snapshotPayload` (the JSON `/api/snapshot` returns) gains an
`ungrouped` field of `[]dashboardAgent`, parallel to the existing
`agents` array. Every entry mirrors the `dashboardAgent` shape so
clients can render either tab with the same row component.

```jsonc
{
  "agents":    [ {"conv_id":"…","title":"alpha-worker","groups":["alpha"], …} ],
  "ungrouped": [ {"conv_id":"…","title":"loose-worker","groups":[],         …} ]
}
```

## Population rule

After the existing `agentRows` are built from group members and
explicit permission grants, the snapshot now also iterates
`db.ListSessions()` and adds any conv-id that:

- Has a non-empty `conv_id` on the session row,
- Isn't already in `agentRows`,
- Is `isConvOnline()` (live tmux session).

Then the partition step that produces the broader `agents[]` list
ALSO emits any agent with `len(Groups) == 0` into `ungrouped[]`.

> **Update (`dashboard-ungrouped-virtual-group.md`):** the partition
> step now additionally requires `a.Online`. The per-conv permission
> loop adds offline grant-holders to `agentRows`, which would
> otherwise leak into `ungrouped[]`; the online gate keeps the array
> to live loose convs as documented.
>
> **Superseded (`dashboard-enrollment-surface-fixes.md`):** the
> `a.Online` gate has since been removed. `ungrouped[]` now carries
> every active agent in no group, online or offline — a promoted
> offline conversation must be visible in the virtual "Ungrouped"
> group to be dragged into a real one. The `+ add member` overlay
> applies its own online filter, so it does not regress.

This means an entry can appear in both arrays (the broader `agents`
list is a superset; `ungrouped` is the subset with zero memberships).
Effective permissions still come from the broader row — the dashboard
should treat `ungrouped` purely as a candidate-set hint, not as the
authoritative agent record.

## Stale-row filter

Offline session rows from past runs do NOT pollute `ungrouped`. The
`isConvOnline()` gate filters the loose-conv enumeration so only
currently-running tmux sessions surface. Without this gate, every
previously-spawned conv would shore up indefinitely as the daemon's
history grows.

## Tests

Two flow scenarios pinned via the existing testharness:

- `TestDashboardSnapshot_UngroupedSurfacesLooseConvs` — alive
  conv with no group membership appears in `ungrouped`; alive
  conv that IS a member of a group appears in `agents` but NOT
  `ungrouped`.
- `TestDashboardSnapshot_UngroupedFiltersOfflineSessions` —
  `MarkOffline()` flips a session off; the now-offline conv must
  drop out of `ungrouped` on the next snapshot.

The tests use a new `BuildDashboardHandlerForTest()` test hook
(test-only via `_test.go` suffix) that exposes the dashboard mux
parallel to `BuildHandlerForTest()` for the /v1 mux. The hook also
injects the dashboard cookie + Origin so the cookie/Origin auth
checks pass for synthetic httptest peers.

## Files

- `pkg/claude/agentd/dashboard.go` — `snapshotPayload.Ungrouped`,
  loose-conv enumeration, partition step.
- `pkg/claude/agentd/testhooks_test.go` —
  `BuildDashboardHandlerForTest` + `dashTestHandler` cookie/Origin
  injection.
- `pkg/claude/agentd/dashboard_ungrouped_flow_test.go` — flow tests.

## Cross-references

- `TODO/high-prio/dashboard-group-membership-ux.md` — parent UX
  feature (still open: DnD, add-member overlay; both blocked on
  framework migration).
- `TODO/med-prio/web-dashboard.md` — framework migration prereq.
