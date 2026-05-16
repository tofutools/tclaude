# Group-scoped cron-create modal — target picker locked to the group

Shipped 2026-05.

This is the cron-form sibling of
[`message-modal-group-scope`](message-modal-group-scope.md). The
cron-create modal reuses the *same* shared solo/group target picker as
the one-shot message modal (`bindTargetPicker('cron-create')`). Opened
from a group header's "⏰ multicast" button it only pre-selected that
group in the dropdown — the user could still flip to Solo mode and
pick *any* agent, or switch the dropdown to a *different* group. Same
class of leak #132 fixed for messages: the selection list was not
scoped to the group the button belonged to.

## What shipped

When the cron modal is opened from a group X's "⏰ multicast" button,
the target picker is **scoped to group X** — the selection cannot
leave the group. Both target modes stay available:

- **Group (multicast)** — unchanged: fires to the whole of group X.
  The dropdown is locked to X (single option, disabled) so a scoped
  multicast cannot be retargeted to another group.
- **Solo agent** — the all-agents free-text input + 🔍 picker is
  replaced by a `<select>` of *only group X's members*. Picking a
  member is structural — there is no way to reach a non-member.

The cron job itself is unchanged: it still targets either `group:X`
or a specific conv-id (which, via the scoped `<select>`, is always a
member of X). The existing target model handles both — **no schema
change, no backend change.** This is purely a dashboard picker fix.

### The picker scope mechanism

The shared target-picker module gained an opt-in scope:

- `targetPickerScopes[prefix]` — a per-picker registry; when set to a
  group name the picker is scoped to it. `setTargetPickerScope(prefix,
  groupName)` arms / clears it.
- `targetPickerMarkup` gained a third row, `#${prefix}-target-scoped`,
  holding `#${prefix}-scoped-member` — the scoped-mode member
  `<select>`. Inert unless the picker is scoped.
- `setTargetPickerMode` shows the scoped member `<select>` (not the
  free-text row) in Solo mode while scoped.
- `populateTargetPickerGroups` locks the group dropdown to `[scope]`
  and disables it when scoped.
- `populateTargetPickerMembers` (new) fills the member `<select>` from
  the scoped group's members.
- `populateTargetPicker` / `readTargetPicker` read the member
  `<select>` in scoped Solo mode.

The scope is armed by `populateCronForm` from the prefill's
`scopeGroup` field — set **only** by the group header's "⏰ multicast"
button. Every other cron entry point passes no `scopeGroup`, so
`populateCronForm` clears the scope and the picker is unrestricted, as
before:

- the global "+ new cron job" button (`#cron-create-open`),
- a per-member / per-agent ⏰ (`cronMemberButton`, Agents-row ⏰) —
  already a solo self-nudge for one specific conv,
- editing an existing cron job (`openCronEditModal`).

The modal title reads `Schedule a cron job for group "X"` in scoped
mode.

The message modal (the picker's other host) never arms a scope, so
the new `#message-create-target-scoped` row is inert there — #132's
behaviour is unchanged.

## Files

- `pkg/claude/agentd/dashboard.html` — the `targetPickerScopes`
  registry + `setTargetPickerScope`, the scoped row in
  `targetPickerMarkup`, `populateTargetPickerMembers`, the scope
  branches in `setTargetPickerMode` / `populateTargetPickerGroups` /
  `populateTargetPicker` / `readTargetPicker`, the `scopeGroup` wiring
  in `populateCronForm`, the scoped modal title in
  `openCronCreateModal`, and the `scopeGroup` on the group header's
  "⏰ multicast" button.

## Tests

This is a frontend-only change: the scoping lives entirely in
dashboard.html's embedded JS, there is no daemon surface (the cron
backend already accepts both a `group:` target and a member conv-id
target — nothing server-side changed), and the repo has no JS test
runner.

- `pkg/claude/agentd/dashboard_cron_group_scope_html_test.go` — a
  guard test (same pattern as `dashboard_context_meter_html_test.go`)
  pinning the scoping contract in the embedded HTML: the scope
  registry, `setTargetPickerScope`, the scoped row + member `<select>`
  markup, the locked group dropdown, the scoped `readTargetPicker`
  path, and the `scopeGroup` wiring on the group button and through
  `populateCronForm`.

**Manual verification:** from the dashboard's Groups tab, the group
header "⏰ multicast" button → modal titled for the group; Group mode
dropdown shows only that group and is disabled; Solo mode shows a
`<select>` of only that group's members. The global "+ new cron job",
a per-member ⏰, and editing an existing cron job all still show the
unrestricted picker.
