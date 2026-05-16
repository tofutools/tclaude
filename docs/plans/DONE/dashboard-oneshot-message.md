# Dashboard one-shot message — solo send + group multicast

Shipped 2026-05.

The dashboard could schedule a recurring nudge (the Cron tab's create
form, with a solo-vs-group target picker), but had no way to send a
single immediate message — neither to one agent nor as a group
multicast. This slice ships that surface: a "Send a message" modal
plus a cookie-authed `POST /api/message` endpoint.

The group-multicast send path itself already existed end-to-end —
`POST /v1/messages` with `to: "group:NAME"` fans out via
`handleMulticast` (see [`group-addressed-sends`](group-addressed-sends.md)).
This slice reuses that machinery rather than duplicating it; the only
new backend logic is a cookie-auth front door.

## What shipped

### Shared solo/group target picker

The cron form's target picker (radio: Solo agent / Group multicast;
solo = text input + 🔍 agent picker; group = `<select>` of group
names) was factored into a reusable JS module so the cron form and the
new message form cannot drift:

- `targetPickerMarkup(prefix)` — returns the picker markup with all
  element ids derived from `prefix` (e.g. `cron-create` →
  `#cron-create-target`, `#cron-create-group`, radio name
  `cron-create-target-mode`).
- `bindTargetPicker(prefix)` — mounts the markup into the host's
  `<div id="${prefix}-target-mount">` (idempotent) and wires the mode
  radios + the 🔍 button.
- `setTargetPickerMode` / `populateTargetPickerGroups` /
  `populateTargetPicker(prefix, {targetMode,target,groupName})` /
  `readTargetPicker(prefix) → {mode, target}`.

The cron form keeps every element id it had (`cron-create-target`,
`cron-create-group`, …) — only the radio group name changed
(`cron-target-mode` → `cron-create-target-mode`) and the static markup
became a JS-mounted block. Cron form behaviour is unchanged.

### "Send a message" modal

`#message-create-modal` (reusing the `.cron-create-modal` shell).
Fields:

- **From** — text + 🔍 picker. Required. The sender the message is
  attributed to and replied to; mirrors the cron form's Owner field.
- **Target** — the shared solo/group picker. Solo = one agent; Group
  = `to: "group:<name>"` multicast.
- **Subject** — optional, capped at 100 chars.
- **Body** — textarea, required.

Result feedback: a multicast toast reports how many group members the
send reached (and how many were nudged live); a solo toast reports
whether the recipient was nudged live or the row just queued.

### Context-aware entry points

| Entry point | Pre-fill |
|-------------|----------|
| Agents tab — per-row `✉` | Solo target = that conv-id |
| Groups tab — group header `✉ message` | Group mode = `group:<name>` |

Both serialise their pre-fill into a `data-prefill` JSON blob; the
`message-new` action handler decodes it and calls
`openMessageCreateModal(prefill)`. From is left blank for the human
to pick, mirroring how the cron entry points leave Owner blank.

### Daemon-side additions

- `handleMessages` (`POST /v1/messages`) was split: the post-decode
  validation + routing moved into a new shared core, `dispatchSend(w,
  fromID, *sendReq)`. `handleMessages` is now just method check +
  `requireAgent` + decode + `dispatchSend`.
- `dashboard_message.go` registers `POST /api/message`
  (`handleDashboardMessageCreate`): cookie-auth, decodes
  `{from,to,subject,body,role}`, resolves the From selector, and calls
  the same `dispatchSend`. No routing logic is duplicated — the group
  member/owner gate in `handleMulticast` and the shared-group /
  `message.direct` gate in `resolveMessageRouting` both live below
  `dispatchSend`, so the dashboard path cannot route around them. The
  cookie + Origin pin in `checkDashboardAuth` is the human-consent
  layer; the From conv's own group standing is still enforced.

`handleMulticast` itself is untouched — `dispatchSend` calls it
unchanged.

## Files

- `pkg/claude/agentd/handlers.go` — `handleMessages` split; new
  `dispatchSend` shared core.
- `pkg/claude/agentd/dashboard_message.go` (new) —
  `registerDashboardMessageRoutes` + `handleDashboardMessageCreate`.
- `pkg/claude/agentd/dashboard_edit.go` — wires
  `registerDashboardMessageRoutes` into the dashboard mux.
- `pkg/claude/agentd/dashboard.html`:
  - cron form Target row → JS-mounted `#cron-create-target-mount`.
  - new shared `targetPicker*` JS module.
  - new `#message-create-modal` HTML + `openMessageCreateModal` /
    `closeMessageCreateModal` / `submitMessageForm` / `bindMessageModal`.
  - per-group `✉ message` button, per-agent `✉` button,
    `message-new` action handler, `bindMessageModal()` in init.
- `pkg/claude/agentd/dashboard_message_flow_test.go` (new) — flow
  tests: group target fans out to every member; solo target reaches
  exactly one; missing From / unknown From / empty body rejected.

## Cross-references

- [`group-addressed-sends`](group-addressed-sends.md) — the
  `group:NAME` multicast grammar + `handleMulticast` fan-out this
  slice reuses.
- [`dashboard-cron-create-form`](dashboard-cron-create-form.md) — the
  cron form whose target picker was factored into the shared module.
