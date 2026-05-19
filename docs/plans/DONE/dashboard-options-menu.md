# Dashboard: per-agent / per-group ⚙ options menus

Shipped 2026-05.

The dashboard's Groups tab had grown too many per-agent and per-group
buttons — each member row carried up to ~9 actions, each group header
~11. This slice collapses the less-used ones behind a ⚙ "options" cog
on every agent row and every real group header. Clicking the cog opens
a small dropdown of the collected actions; the most-used buttons stay
at the top level.

## What shipped

### Button distribution

**Group header** — kept at the top level: `spawn-agent` (relabelled
"spawn"), `power-on-group`, `shutdown-group`. Moved into the ⚙ menu:
`add-member`, `cron-new` (⏰ multicast), `message-new` (✉),
`rename-group`, `export-group`, `cleanup-group`, `window-modal-group`
(🪟), `delete-group`.

**Agent row** — kept at the top level: `focus` (jump) + `hide`, the
online-only window pair. Moved into the ⚙ menu: `term`, `clone`,
`reincarnate`, `edit-member` (grouped rows only), the owner toggle
`grant-owner`/`revoke-owner` (grouped only), `sudo-grant`, `perm-edit`,
`cron-new` (⏰), and the destructive `remove-member` (grouped) /
`delete-agent` (ungrouped). The cog is present on every agent row,
online and offline — an offline row shows just the cog.

The virtual Conversations / Retired groups keep their single-button
rows (`promote` / `reinstate`) as-is; virtual groups have no
`.group-actions`, so the group cog applies only to real groups. The
status dot and the click-to-rename name cell are untouched.

### Mechanism

`actionCog(act, items)` (helpers.js) emits a `<button class="cog-btn">`
plus a hidden `<div class="action-menu">` of the moved buttons, both as
siblings inside the existing `.row-actions` / `.group-actions`
container — NOT floated to `document.body`, so a menu item's handler
that walks up to its `<summary>` (`rename-group`) still resolves. Moved
buttons keep their `data-act` and every `data-*` attribute exactly; the
delegated dispatcher in `row-actions.js` is unchanged — only the
buttons' DOM position moves.

- The cog's `data-act` is `row-menu` (agent row) / `group-menu` (group
  header). The `row-menu` / `group-menu` case in `bindRowActions`
  toggles the sibling `.action-menu`'s `.open` class and closes any
  other open menu (at most one open at a time).
- The group cog lives inside `<summary>`; `bindRowActions` already
  calls `e.preventDefault()` for every `[data-act]` button, so opening
  the menu does not collapse/expand the `<details>`.
- `closeAllActionMenus()` handles dismissal: a click on a menu ITEM
  closes the menu then dispatches the item; a click anywhere outside
  every menu closes them (click-away); a click on a menu's own padding
  leaves it open.
- The auto-refresh is suspended while a menu is open —
  `refreshSuspended()` checks `document.querySelector('.action-menu.open')`,
  a DOM-derived check that cannot leak (closing the menu drops the
  class and lifts the suspension), so a 5s poll can't re-render the
  menu away mid-use.
- The menu is `position: absolute` against the `position: relative`
  `.row-actions` / `.group-actions` cluster; no ancestor sets
  `overflow`, so it escapes the table cell. A `:has(.action-menu.open)`
  rule keeps the cluster fully opaque while its menu is open (the menu
  drops past the row, so the pointer leaving would otherwise fade it).
- The cog glyph is sized up (15px) and tinted amber (`#e3a008`) so it
  reads as a distinct affordance rather than another grey row button.
  A U+FE0E text variation selector pins ⚙ to its monochrome glyph so
  the CSS colour applies (some platforms would otherwise render U+2699
  as a colour emoji that ignores `color`).

## Files

- `pkg/claude/agentd/dashboard/js/helpers.js` — `lifecycleAndFocusButtons`
  split into `focusHideButtons` (top-level focus+hide, online-only) and
  the menu's `termButton`; new `actionCog`; `memberActions` /
  `ungroupedMemberActions` rebuilt around the cog. `actionCog` exported.
- `pkg/claude/agentd/dashboard/js/render.js` — new `groupActionsHTML(g,
  members)` builds the group header cluster (4 top-level buttons + cog
  menu); `renderGroups` calls it.
- `pkg/claude/agentd/dashboard/js/row-actions.js` — `closeAllActionMenus`;
  menu-dismissal logic at the top of `bindRowActions`; `row-menu` /
  `group-menu` toggle cases.
- `pkg/claude/agentd/dashboard/js/refresh.js` — `refreshSuspended()`
  pauses the poll while a `.action-menu.open` exists.
- `pkg/claude/agentd/dashboard/dashboard.css` — `.cog-btn`,
  `.action-menu`, `.action-menu.open`, `.action-menu button`, the
  `:has()` opacity rule; `position: relative` on `.row-actions` /
  `.group-actions`.

## Tests

`pkg/claude/agentd/dashboard_options_menu_html_test.go` —
`TestDashboardHTML_OptionsMenu` string-searches the embedded
`dashboardAssets` (the established pattern for client-side dashboard
render guards — there is no JS test runner and no daemon behaviour
change to flow-test). Asserts the cog/menu wiring is present, the kept
top-level buttons still render, and every moved button is still
rendered (relocated, not removed).

## Out of scope

- The group-header chips (📝 descr, 📁 dir, 📋 context, 👥 cap) are
  separate `<span>`s, not `.group-actions` buttons — left top-level.
- A full Groups→Agents tab rename, the group-header chip migration to
  the shared `inlineEdit` primitive — pre-existing deferred follow-ups,
  untouched.
