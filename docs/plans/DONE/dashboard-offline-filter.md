# Dashboard "show offline" filter

Shipped: the agentd browser dashboard can now hide offline agents/members.

## What shipped

A `show offline` toggle on the dashboard's **Groups** and **Agents** tabs.
"Offline" = the conv's tmux pane isn't alive (`online === false` in the
`/api/snapshot` payload).

- **Agents tab** — one tab-wide checkbox in the filter bar. Unchecking it
  drops offline agents from the table. The filter-count (`N / total`)
  reflects the offline filter as well as the text filter.
- **Groups tab** — a tab-wide checkbox in the filter bar acts as the
  default, *plus* a per-group override control in each group's
  `<summary>` header. The override cycles on click:
  `inherit → always show → always hide → inherit`. In `inherit` mode the
  control spells out the resolved value, e.g. `offline: auto (hidden)`.
  Hidden offline members still count toward the header total — the
  header gains a `· N offline hidden` note — so `5 members, 2 online`
  stays truthful. A group whose every visible member is filtered out
  shows a muted `(N offline members hidden …)` line instead of the
  table.

## Persistence (localStorage, client-side only — no daemon/schema change)

- `tclaude.dash.offline.groups` / `tclaude.dash.offline.agents` — `'1'`/`'0'`
  tab-wide checkbox state. Absent ⇒ defaults to checked (show all).
- `tclaude.dash.group.offline.<group-name>` — `'show'` / `'hide'`; absent
  ⇒ `inherit` (follow the tab-wide default).

## Files

- `pkg/claude/agentd/dashboard.html` — only file touched. New CSS
  (`.filter-toggle`, `.group-offline-toggle`), two filter-bar checkboxes,
  JS helpers (`offlineDefault`, `groupOfflineOverride`, `groupShowOffline`,
  `groupOfflineToggleHTML`), member filtering in `renderGroups`, agent
  filtering in `renderAgentsTab`, a `cycle-group-offline` client-side
  `data-act` case, and checkbox wiring in `bindFilter`.

## Notes

Pure front-end change — the dashboard HTML is `go:embed`-ed, there is no
Go logic and no `/api` surface, so there is no flow test. Verified via
`node --check` on the embedded script + `go build`/`go test ./pkg/claude/agentd/...`.
