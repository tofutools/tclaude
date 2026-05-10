# Dashboard Groups tab: cwd column on member rows

Shipped 2026-05.

The Groups tab's member subtable now shows the agent's working
directory alongside the existing alias / role / descr / online /
permissions cells. Lets a human glance at a group and tell apart
two same-aliased members working different repos without hopping
into their tmux panes.

Bundled with
[`dashboard-search-extend-role-descr.md`](dashboard-search-extend-role-descr.md)
so the new column is searchable the moment it appears.

## What shipped

### Snapshot

No daemon change required. `dashboardMember.State.Cwd` was already
populated by `stateForConv` — it's the cwd from the live tmux
session row (or, if the conv is offline, the most-recent row).

### Column

Inserted between **Last** and **Role** in the Groups tab subtable,
matching the position the Agents tab uses (Last → CWD → Groups).
Cell renders `shortCwd(state.cwd)` with the full absolute path on
hover via `title=...`.

### shortCwd rewrite

Old behaviour: only home-prefix replacement (`/home/gigur/foo` →
`~/foo`).

New behaviour:

- Home prefix replacement preserved.
- If the result still exceeds 40 chars, truncate from the LEFT
  (`…/git/tclaude/pkg/...`) — far more useful than right-truncation
  because most paths share a long common prefix and the tail is the
  distinguishing detail.
- Empty / unknown input renders as `—` (em dash) so the column
  stays visually consistent across rows.

This also improves the existing Agents-tab CWD column, since both
share the same helper.

### Search

The cwd is now part of the Groups + Agents tab search predicates
(see the companion search slice). Typing `tclaude` finds every
member working in any repo with that substring.

## Files

- `pkg/claude/agentd/dashboard.html`:
  - New `<th>CWD</th>` + matching `<td>` in the Groups tab
    member subtable.
  - `shortCwd` rewritten with left-truncation + em-dash fallback.

## Cross-references

- [`dashboard-search-extend-role-descr.md`](dashboard-search-extend-role-descr.md)
  — companion slice making the new column searchable.
