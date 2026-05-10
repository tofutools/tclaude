# Dashboard Cron: PATCH endpoint

Shipped 2026-05.

The Cron tab's edit-form (separate slice) needed a partial-update
endpoint to save form edits. Before this, the only mutating cron
endpoints were create / delete / enable / disable / run-now — no way
to change `name`, `interval`, `target`, `subject`, `body` after the
fact without a delete + re-create. This slice ships the PATCH side so
the create-form slice can wire `Edit` (pencil) buttons onto the table.

## Surface

```
PATCH /v1/cron/{id}        (peer-cred, Unix-socket)
PATCH /api/cron/{id}       (dashboard cookie + Origin pin)
```

Body shape (all fields optional — only present fields are written):

```jsonc
{
  "name":     "po-pings",        // alnum + - _; rejected otherwise (400)
  "target":   "<selector>",      // resolved through agent.ResolveSelector
  "interval": "10m",             // Go duration, >= 30s
  "subject":  "...",
  "body":     "...",             // non-empty if present (400 on "")
  "enabled":  false,
  "group_id": 7                  // optional override; routing inference is POST-only
}
```

Returns the full updated row as JSON (same shape as POST /v1/cron),
or 404 if `{id}` doesn't exist.

## Key rule: last_run_at is never touched

`db.UpdateAgentCronJobFields` only writes the columns named in the
patch. Re-enabling a paused job after a long pause must not trigger
50 catch-up fires — the scheduler's "now - last_run_at >= interval"
predicate decides, and we don't move `last_run_at` from under it.

The PATCH endpoint never accepts `last_run_at` or `last_run_status`
fields either (omitted from the decoded body type so they can't sneak
in).

## Auth

`/v1/cron/{id}` PATCH goes through the same `authCronWrite` helper as
the existing POST/DELETE/enable/disable paths:

- human (no Claude ancestor) → pass
- caller == target → `self.schedule`
- caller != target → `agent.schedule` OR owns a group containing the
  target

`/api/cron/{id}` PATCH is gated only by the dashboard cookie + Origin
pin (`checkDashboardAuth`). The handler then delegates to
`handleCronPatch` after stamping a synthetic human peer onto the
request via `asDashboardHumanPeer` — the cookie IS the consent layer,
and the inner `authCronWrite` short-circuits past slug checks on the
`!HasClaudeAncestor` branch.

## Validation

Mirrors POST /v1/cron, extracted to two small helpers:

- `validateCronName` — alphanumeric + `-` / `_`. Applies to both POST
  and PATCH so the surface is consistent. Stricter than
  `validateGroupName` on purpose: cron-job names appear in subject
  prefixes (`[cron:<name>] ...`), table rows, and `cron logs`, so the
  conservative shape avoids quoting + rendering surprises.
- `decodeCronPatchBody` — pointer-shaped fields so `nil` (unset)
  reliably distinguishes "leave alone" from "set to zero". Returns a
  `db.UpdateCronPatch`; the DB helper builds dynamic SQL from the
  non-nil fields.

## Files

- `pkg/claude/common/db/agent_cron.go` —
  - `UpdateCronPatch` struct (pointer-shaped optional fields).
  - `UpdateAgentCronJobFields(id, patch) (rowsAffected, error)`.
  - Dynamic SQL build; empty patch is a 0-row no-op (not an error).
- `pkg/claude/common/db/agent_cron_test.go` — 4 new unit tests:
  - `TestAgentCronJob_UpdateFields_Partial` (only-named-fields-change)
  - `TestAgentCronJob_UpdateFields_IntervalLeavesLastRunAlone`
  - `TestAgentCronJob_UpdateFields_EmptyPatchNoop`
  - `TestAgentCronJob_UpdateFields_NotFound`
- `pkg/claude/agentd/cron_handlers.go` —
  - `handleCronPatch`, `decodeCronPatchBody`, `validateCronName`.
  - `handleCronByID` routes PATCH to `handleCronPatch`.
  - `handleCronCreate` now also calls `validateCronName` for symmetry.
- `pkg/claude/agentd/dashboard_cron.go` —
  - `/api/cron` (POST) and `/api/cron/{id}` (PATCH) cookie-auth twins.
  - Both delegate to the daemon handlers via `asDashboardHumanPeer`.
- `pkg/claude/agentd/identity.go` — `asDashboardHumanPeer(r)` helper
  for the delegation pattern.
- `pkg/claude/agentd/testhooks_test.go` —
  `RegisterDashboardRoutesForTest` exposes the un-wrapped mux so a
  test can prove that a missing cookie actually fails.
- `pkg/claude/agentd/cron_patch_flow_test.go` — 7 flow tests:
  - `TestCronPatch_PartialFields` — only enabled changes
  - `TestCronPatch_IntervalDoesNotBumpLastRunAt` — the load-bearing rule
  - `TestCronPatch_RejectsBadCharset` — name validator gate
  - `TestCronPatch_RejectsTooShortInterval` — `<30s` rejected
  - `TestCronPatch_NotFound` — 404 on unknown id
  - `TestCronPatch_AuthGate_DeniesUnrelatedAgent` — unrelated peer 403
  - `TestDashboardCron_Patch_RoundTrips` — cookie-auth twin works
  - `TestDashboardCron_PatchAuthRequired` — uncookied is refused

## Cross-references

- [`dashboard-cron-create-form.md`](dashboard-cron-create-form.md) —
  parent feature; the UI slice that the edit pencil + form lives in.
  Uses this PATCH endpoint as its save path.
