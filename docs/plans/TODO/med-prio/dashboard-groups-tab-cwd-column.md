# Dashboard Groups tab: cwd column on member rows

Each member row in the dashboard's Groups tab should show the
agent's working directory (cwd) alongside the existing alias /
role / descr / online / permissions cells.

## Why

The cwd is load-bearing context for a human glancing at the
Groups tab. Today: same alias, two members in different groups
working different repos — indistinguishable in the UI without
hopping into their tmux panes. With cwd visible, "is the
reviewer agent working on the right repo?" becomes a one-glance
check.

Same applies to the Agents tab.

## Source of truth

`sessions.cwd` already exists per session row. The snapshot
already pulls session data. Either the cwd is already in the
payload or the snapshot needs a small extension to include it on
each member row. Verify at impl time.

If the conv has multiple historical sessions (reincarnated /
resumed), use the live one's cwd. After succession, that's the
new conv's cwd; if the conv is offline, fall back to the most
recent session's cwd (still meaningful).

## Display

- Truncate from the LEFT, not the right — `…/git/tclaude` is far
  more useful than `/home/gigur/git/tcla…`. Most paths share a
  common prefix; the tail is what distinguishes them.
- Tooltip / hover surfaces the full absolute path.
- Empty / unknown cwd renders as a light dash (`—`), not blank.
- Optional polish: home-directory prefix replacement (`~/git/tclaude`)
  for compactness, since `/home/<user>` is noise.

Sortable like the other columns. Optional but nice: clicking
the cwd cell could `cd` the dashboard's "open terminal here"
affordance (if/when that feature exists) — out of scope for v1.

## Search

The cwd should also feed into the search filter (see
[`dashboard-search-extend-role-descr.md`](dashboard-search-extend-role-descr.md))
so typing `tclaude` finds agents working in any repo with that
substring. Add cwd to the same predicate when this lands.

## Files

- `pkg/claude/agentd/dashboard.go` — verify `cwd` is in the
  member-row payload; if not, extend the snapshot.
- `pkg/claude/agentd/dashboard.html` (or post-migration
  components) — new column.

## Cross-references

- [`web-dashboard.md`](web-dashboard.md) — parent v2 plan.
- [`dashboard-search-extend-role-descr.md`](dashboard-search-extend-role-descr.md)
  — extend the search predicate to include cwd when both ship.
