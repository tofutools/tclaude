# `tclaude agent sudo` — proactive dashboard grants (v2 follow-up)

Shipped 2026-05.

V1 sudo is strictly agent-initiated: the agent hits `POST /v1/sudo`,
the popup asks the human, on approve a grant lands. v2 slice 3
shipped the dashboard's revoke twins but no equivalent for the
grant path — the human had to wait for an agent to ask before
elevating it.

This adds the missing direction: a cookie-auth `POST /api/sudo`
that lets the human seed a time-bounded grant from the dashboard
without involving the popup. Same DB rows as the agent path, same
policy gates (blocklist + duration cap), distinct audit label so
forensics can tell proactive grants apart.

## Endpoint

`pkg/claude/agentd/dashboard_edit.go` — `handleDashboardSudoAPI`
extended to dispatch on method:

```
POST   /api/sudo                 → proactive grant (no popup)
DELETE /api/sudo/{id}            → revoke one  (existing)
DELETE /api/sudo?conv=<selector> → revoke per conv (existing)
DELETE /api/sudo?all=1           → revoke all (existing)
```

POST body:

```json
{
  "conv":     "<selector>",          // alias / conv-id / prefix
  "slugs":    ["groups.spawn", "..."],
  "duration": "5m",                  // optional, defaults to policy
  "reason":   "team-bootstrap"       // optional
}
```

Response shape mirrors `POST /v1/sudo` so the dashboard's existing
JSON handler doesn't need a special case:

```json
{
  "conv_id":    "<conv>",
  "expires_at": "<rfc3339nano>",
  "grants": [{ "id": 42, "slug": "groups.spawn", ... }]
}
```

## Why no popup

The dashboard cookie + Origin/Referer pinning is the human-consent
layer. An agent forging the cookie is already the threat-model
boundary that protects every other dashboard mutate (group create,
permission grant, etc.); proactive sudo doesn't widen that
surface. Mounting a popup *on top of* the cookie would be
double-prompting the same human.

## Same policy as the agent path

The same `resolveSudoConfig(cfg, convID, "", title)` resolution
runs, so `agent.sudo.{max_duration, blocklist}` and per-conv
overrides apply identically. Bypassing the policy is **not** a
feature:

- The blocklist (`permissions.grant`, `permissions.revoke`) prevents
  permanent escalation. A "just 5 minutes" exception would defeat
  the whole point — blocklisted slugs grant their bearer the
  ability to grant themselves anything, and that grant outlives the
  elevation window.
- The duration cap stops accidental "30 days" grants. The human can
  edit `agent.sudo.max_duration` first if they really want a
  longer window — that's an explicit human-policy edit, not a
  one-off click-through.

For "I want to grant a slug to one agent permanently" the existing
`tclaude agent permissions grant <conv> <slug>` already covers it
(no expiry, also human-only).

## Audit label

`granted_by = "<human-dashboard>:proactive"` (the constant
`dashboardSudoGranter`).

Distinct from `<human-dashboard>` (used by every other dashboard
mutate that records audit) and from the agent-initiated path's
`human:popup-id=<n>`. Three forensic queries, three labels:

| Path                        | granted_by                        |
|-----------------------------|-----------------------------------|
| Agent → /v1/sudo → popup    | `human:popup-id=<n>`              |
| Dashboard `POST /api/sudo`  | `<human-dashboard>:proactive`     |
| Dashboard mutates elsewhere | `<human-dashboard>`               |

## Tests (4 new)

`pkg/claude/agentd/dashboard_sudo_test.go`:

- `TestDashboardSudo_GrantProactive` — happy path: dashboard POSTs
  grant, row appears with the proactive audit label, IsActive
  returns true.
- `TestDashboardSudo_GrantBlocklist` — `permissions.grant` in the
  bundle → 403 + named blocked slug, no rows inserted (even the
  non-blocked sibling).
- `TestDashboardSudo_GrantDurationCap` — `duration: "24h"` → 400
  before any DB writes.
- `TestDashboardSudo_GrantAuthRequired` — POST without cookie /
  Origin → non-200, no rows.

## Files

- `pkg/claude/agentd/dashboard_edit.go` — method dispatch in
  `handleDashboardSudoAPI`; new `handleDashboardSudoGrant`; new
  `dashboardSudoGranter` audit constant.
- `pkg/claude/agentd/dashboard_sudo_test.go` — 4 new tests.

## Open follow-up: dashboard JS form

The dashboard.html still has no UI for this — a JS form (conv +
multi-slug + duration + reason → POST /api/sudo) would surface
the new endpoint. Same trade-off as the rest of slice 3's
JS-rendering work: out of scope for the Go-testable surface.

## Cross-references

- [`DONE/agent-sudo-elevation-v1.md`](agent-sudo-elevation-v1.md) —
  the agent-initiated path this complements.
- [`DONE/agent-sudo-elevation-dashboard-api.md`](agent-sudo-elevation-dashboard-api.md)
  — sibling slice that shipped the revoke twins + snapshot
  extension.
