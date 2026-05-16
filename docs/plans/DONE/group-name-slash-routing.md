# Group names with slashes broke routing — SHIPPED

## Problem

A human created a group, then could not rename it from the dashboard.
The rename failed with:

```text
rename failed: expected /api/groups/{name}[/{members|owners}[/{conv}]]
```

The same break hit every other operation on that group — spawn, members,
owners, delete — not just rename.

## Root cause

Two bugs combined, both around group names that contain a `/`.

1. **Group create never validated the name.** `validateGroupName`
   (`groups_rename.go`) rejects embedded slashes precisely because the
   URL dispatch splits on `/`. It was called by group **rename** and
   **clone** but **not** by **create** — `handleGroups` POST only
   checked `name != ""`. So a "poison" group with a slash in its name
   could be created from the dashboard create modal or `tclaude agent
   groups create`.

2. **The route dispatchers decoded `%2F` before splitting.** Both
   `handleDashboardGroupsAPI` (`/api/groups/...`) and `handleGroupByName`
   (`/v1/groups/...`) hand-rolled their parse: `strings.TrimPrefix(
   r.URL.Path, …)` + `strings.SplitN(…, "/", …)`. `r.URL.Path` is the
   **already percent-decoded** path. The browser correctly sent
   `team%2Fsub` (via `encodeURIComponent`), but Go decoded it back to
   `team/sub` in `.Path`, so the slash re-split the name into bogus path
   segments and the route was lost.

The dashboard frontend was **not** at fault — every `/api/groups/...`
fetch already used `encodeURIComponent`.

## What shipped

Three changes, in one PR (`fix-group-name-slash-routing`):

**(a) Validate the name at group create.** `handleGroups` POST now calls
`validateGroupName(body.Name)` — the same guard rename and clone already
apply. This covers both the CLI (`POST /v1/groups`) and the dashboard
(`POST /api/groups`, which delegates to `handleGroups`). No new
slash-named group can be created.

**(b) Migration v38 to repair existing poison data.**
`migrateV37toV38` (`pkg/claude/common/db/migrate.go`) scans
`agent_groups` for names containing `/` or `\`, folds the slashes to
`-`, and resolves UNIQUE-name collisions with a numeric `-2`, `-3`, …
suffix (deterministic — ordered by id, lower id keeps the bare name).
Group references are integer foreign keys (`group_id`), so each repair
is a single-row UPDATE with no cascade. `currentVersion` 37 → 38.

**(c) Modernized both group dispatchers to Go 1.22 method+wildcard
routing.** The hand-rolled `TrimPrefix`/`SplitN` parsing in
`handleDashboardGroupsAPI` and `handleGroupByName` was deleted. Routes
are now registered as `POST /api/groups/{name}/rename`,
`DELETE /v1/groups/{name}/members/{conv}`, etc., and the `{name}` /
`{conv}` / `{id}` wildcards are read via `r.PathValue`. The mux matches
each wildcard against one segment of the **escaped** path, so an
embedded `%2F` stays inside the segment and `PathValue` returns it
decoded — slash-safe by construction.

- `registerDashboardGroupRoutes` + `groupRoute` adapter (dashboard
  cookie auth + group resolve) — `dashboard_edit.go`.
- `registerV1GroupRoutes` + `v1GroupRoute` adapter + new
  `handleGroupDelete` leaf — `handlers.go`. Wired in `serve.go`.

## Files

- `pkg/claude/agentd/handlers.go` — `validateGroupName` at create;
  `registerV1GroupRoutes`, `v1GroupRoute`, `handleGroupDelete` replace
  `handleGroupByName`.
- `pkg/claude/agentd/dashboard_edit.go` — `registerDashboardGroupRoutes`,
  `groupRoute` replace `handleDashboardGroupsAPI`.
- `pkg/claude/agentd/serve.go` — registers `registerV1GroupRoutes`.
- `pkg/claude/common/db/migrate.go` — `migrateV37toV38`, `currentVersion`
  38.

## Tests

- `pkg/claude/common/db/migrate_v38_test.go` — migration sanitizes
  slash/backslash names, resolves collisions, leaves clean names and ids
  intact; no-op on a healthy DB.
- `pkg/claude/agentd/group_name_slash_flow_test.go` — slash-named group
  is renameable via the dashboard and via `/v1`, reflected at the
  snapshot read surface; group create with a slashed name is rejected
  (400).
- `pkg/claude/agentd/dashboard_edit_test.go` /
  `groups_archive_test.go` — existing handler tests rerouted through the
  new mux (`serveDashboardGroups` / `serveV1` helpers).
