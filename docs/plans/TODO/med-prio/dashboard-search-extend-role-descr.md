# Dashboard search filter: also match role + descr

The search box at the top of the Groups view today filters by:

- group name
- member alias
- permission slug

It should ALSO match:

- **member role** (`agent_group_members.role`)
- **member descr** (`agent_group_members.descr`)

So a user typing `developer-advocate` finds the right member even
if they don't remember the alias, and typing keywords from the
descr ("announcements", "blog", "Q&A") surfaces the right agent.

## Implementation

Pure JS / framework filter logic. No daemon changes — both fields
are already in the `/api/snapshot` payload.

In `pkg/claude/agentd/dashboard.html` (or post-migration
component), find the search-filter predicate and extend it:

```
match = q in group.name
     || any(member.alias contains q for member in group)
     || any(member.role  contains q for member in group)   // NEW
     || any(member.descr contains q for member in group)   // NEW
     || any(slug contains q for slug in member.permissions)
```

Case-insensitive substring match. Same shape as the existing
clauses.

## Same treatment for the Agents view

The Agents tab has its own search box that filters the agent
tree. Same change applies: extend the predicate to match role +
descr alongside the alias / slug.

## Test coverage

JS — no Go-side test needed. Whatever framework lands brings its
own integration test story.

## Files

- `pkg/claude/agentd/dashboard.html` (or post-framework-migration
  equivalent) — search predicate function

## Cross-references

- [`web-dashboard.md`](web-dashboard.md) — parent v2 plan; the
  "Search at the top filters by group name / member alias /
  permission slug" line should be updated to include role + descr
  once this ships.
