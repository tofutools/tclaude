# Dashboard Cron tab: `+ new cron job` form

Shipped 2026-05.

The Cron tab had a read-only render only — humans had to drop to the
CLI (`tclaude agent cron add ...`) to schedule a recurring nudge.
This slice ships the create+edit form so every cron operation has a
dashboard surface, plus context-aware entry points on the Agents /
Groups tabs that pre-fill the form for the common cases.

Companion slice: [`dashboard-cron-patch-endpoint.md`](dashboard-cron-patch-endpoint.md)
shipped the PATCH endpoint this form's "save" path uses.

## What shipped

### Single modal serves Create + Edit

`#cron-create-modal` (anchored by the existing `.modal-overlay`
pattern) reused for both. Same fields, just different submit verbs
and prefill source:

- **Create** — blank fields, POST `/api/cron`. Shows the
  "Save & create another" button so power users can batch.
- **Edit** — fields prefilled from the selected row, PATCH
  `/api/cron/{id}`. Hides "Save & create another" (doesn't fit
  the edit semantics).

Fields:

- **Name** — text, alnum + `-` / `_` charset (server-side
  `validateCronName`).
- **Owner** — text + 🔍 picker button. Optional; defaults to the
  target on the server side. Allows attributing a job to a
  specific agent (e.g. a PO agent that monitors workers).
- **Target** — radio: Solo agent (text + 🔍 picker) / Group
  (`<select>` populated from snapshot's `groups[]`). Group mode
  sends `target: "group:<name>"`.
- **Schedule** — preset chips (`5m`, `15m`, `1h`, `4h`, `daily`)
  + free-text Go-duration input. Clicking a chip fills the text
  input + highlights; typing custom clears the highlight.
- **Subject** — optional, capped at 100 chars.
- **Body** — textarea, required.
- **Enabled** — checkbox, default ON. Lets the human stage a
  disabled job and flip it on later.

Validation surfaces inline via `#cron-create-error` — same shape
as the sudo modal's error well. Daemon also validates and returns
400s the form surfaces as the inline message.

### Context-aware entry points

The spec § "Where the `+ new cron job` button lives" listed four
trigger locations. All shipped:

| Entry point | Pre-fill |
|-------------|----------|
| Cron tab — `+ new cron job` filter-bar button | (empty — human picks) |
| Agents tab — per-row `⏰` | Solo target = that conv-id; Owner = that conv-id (self-nudge) |
| Groups tab — per-member row `⏰` | Solo target = that conv-id; Owner = that conv-id |
| Groups tab — group header `⏰ multicast` | Group mode = `group:<name>` |

All four open the **same modal**; the entry-point button serialises
its pre-fill state into a `data-prefill` JSON blob, and the
`cron-new` action handler in `bindRowActions` decodes + passes to
`openCronCreateModal(prefill)`.

### Daemon-side additions

- `handleCronCreate` (POST `/v1/cron`) gained an `owner` field —
  human callers can attribute a job to a specific conv via the
  selector resolver. Existing CLI users unaffected (field is
  optional and defaults to the previous "owner = target" rule).
- `handleCronCreate` also accepts an optional `enabled` field so
  the form can stage a disabled job.
- `decodeCronPatchBody` (PATCH `/v1/cron/{id}`) gained the same
  `owner` field for symmetry.
- `dashboard_cron.go` registers both `POST /api/cron` and
  `PATCH /api/cron/{id}` as cookie-auth twins that delegate to
  the daemon handlers via `asDashboardHumanPeer`.

### Search affordance

The owner / target picker overlay (`pickCronTargetModal`) reuses
the sudo agent-picker pattern: `.add-member-modal` CSS shape,
↑↓ navigation, Enter to pick, Esc to close. Sources candidates
from `lastSnapshot.agents[]` (already populated by the dashboard's
single-poll model — no extra round-trip). "Include offline /
archived" checkbox widens the candidate pool.

Group mode uses a plain `<select>` populated from
`lastSnapshot.groups[]` — simpler than a picker overlay, and group
names are typed/searched easily inside the select dropdown.

### Optimistic UI

On successful POST/PATCH, the form code splices the returned row
into `lastSnapshot.cron` and calls `renderCronTab()` so the table
updates before the next 5s snapshot poll. The full re-render at the
next poll fills in the snapshot-only fields (owner_label,
target_label, group_name) that the bare POST response doesn't
carry.

## Files

- `pkg/claude/agentd/dashboard.html`:
  - New CSS: `.cron-create-modal` family (mirrors `.sudo-grant-modal`).
  - New HTML: `#cron-create-modal`, `#cron-pick-target-modal`.
  - New JS: `openCronCreateModal`, `openCronEditModal`,
    `populateCronForm`, `submitCronForm`, `pickCronTargetModal`,
    `bindCronModal`.
  - `renderCron` now emits an `edit` button per row.
  - `memberActions` now emits a `⏰` button per row (Groups tab).
  - `renderAgents` row-actions cell now emits a `⏰` button.
  - Group header now has a `⏰ multicast` button.
  - `bindRowActions` handles `cron-edit` + `cron-new`.

- `pkg/claude/agentd/cron_handlers.go`:
  - POST `/v1/cron` body grew `owner` (optional, resolver-driven)
    and `enabled` (optional, defaults to true).
  - PATCH body grew `owner` (resolver-driven).

- `pkg/claude/agentd/cron_patch_flow_test.go`:
  - `TestDashboardCron_Create_OwnerOverride` — `owner` field flows
    into the DB row.
  - `TestDashboardCron_CreateAuthRequired` — uncookied POST refused.

## Cross-references

- [`dashboard-cron-patch-endpoint.md`](dashboard-cron-patch-endpoint.md)
  — companion slice. The form's edit-save path uses this PATCH.
- The sudo elevation dashboard UI series (sibling create-from-form
  pattern; the picker overlay + modal CSS were cloned from there).
