# Dashboard search filter: also match role + descr + cwd

Shipped 2026-05.

The search box at the top of each tab now matches across more
human-meaningful fields. Bundled with the
[`dashboard-groups-tab-cwd-column`](dashboard-groups-tab-cwd-column.md)
slice so the new CWD field is searchable the moment it becomes visible.

## What shipped

### Groups tab

`filterGroups` predicate extended. New matches:

- group `descr` (in addition to the existing `name`)
- member `role`
- member `descr`
- member `state.cwd`

Existing matches (member alias / title / conv-id) preserved. Group
name / descr hits keep all members so the user sees full context;
member hits narrow to the matching subset.

### Agents tab

`filterAgents` predicate extended. New matches:

- agent `state.cwd`
- alias / role / descr pulled from any group membership the agent
  belongs to (via new `memberMetaForAgent` helper — same shape as
  the add-member overlay's `memberMetaForConv`)

Existing matches (title / conv-id / group tags) preserved.

### Cron tab

`filterCron` predicate extended:

- cron `subject` (existing match on `body`, but `subject` is what
  the recipient sees in their inbox surface, so it was a natural
  gap)

Existing matches (name / owner_label / target_label / group_name /
body) preserved.

### Placeholder text

Filter input placeholders updated to mention the new fields, so the
human discoverably knows what they can search by.

## Files

- `pkg/claude/agentd/dashboard.html`:
  - `filterGroups`, `filterAgents`, `filterCron` predicates.
  - New `memberMetaForAgent(convID)` helper at module scope.
  - Filter input placeholder text on Groups / Agents / Cron tabs.

## Cross-references

- [`dashboard-groups-tab-cwd-column.md`](dashboard-groups-tab-cwd-column.md)
  — companion slice shipping the CWD column the new search hits land on.
