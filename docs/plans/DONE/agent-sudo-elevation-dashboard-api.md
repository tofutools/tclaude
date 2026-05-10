# `tclaude agent sudo` — dashboard API + snapshot extension (v2 slice 3, Go side)

Shipped 2026-05.

The Go side of v2 slice 3 lands here: cookie-auth twin endpoints
mirroring the daemon's `/v1/sudo` revoke surface, plus snapshot
extensions that surface every active grant per-agent (for the
"this agent currently holds" 🔓 indicator on the Groups + Agents
tabs) and a top-level list (for the dedicated Sudo tab).

The JavaScript rendering — the actual tab markup, the row badges,
the click-to-revoke handlers — is **not** in this slice. Per the v2
doc: "JS-side rendering not covered by Go flow tests." A future
slice consumes the snapshot fields and binds the cookie-auth
endpoints. Calling the JS done from a sandboxed agent without a
browser would be claiming victory I can't verify.

## Cookie-auth endpoints

`pkg/claude/agentd/dashboard_edit.go`:

```
DELETE /api/sudo/{id}            → revoke one
DELETE /api/sudo?conv=<selector> → revoke every active grant for one conv
DELETE /api/sudo?all=1           → revoke every active grant globally
```

Same DB writes as `/v1/sudo` (`db.RevokeSudoGrant`,
`db.RevokeSudoGrantsByConv`, `db.RevokeAllActiveSudoGrants`). Same
human-only contract — the dashboard is human-only by definition,
so no peer-cred check is needed; the cookie + Origin + Referer
pinning that protects every other dashboard route guards these
too.

Read paths (list active grants) are **not** surfaced separately.
The snapshot already carries everything the dashboard needs to
render, so a poll loop on `/api/snapshot` is enough.

The handler accepts both `/api/sudo` (no slash) for the bulk
selectors and `/api/sudo/<id>` for the per-id revoke. Routed
through a single `handleDashboardSudoAPI`.

## Snapshot extension

`pkg/claude/agentd/dashboard.go`:

```go
type snapshotPayload struct {
    // ...
    Sudo []dashboardSudoEntry `json:"sudo"` // global list, all active grants
    // ...
}

type dashboardAgent struct {
    // ...
    ActiveSudo []dashboardSudoEntry `json:"active_sudo,omitempty"` // per-agent
}

type dashboardSudoEntry struct {
    ID               int64
    ConvID           string  // omitted on dashboardAgent.ActiveSudo[]
    ConvTitle        string
    Slug             string
    GrantedAt        string
    ExpiresAt        string
    GrantedBy        string
    Reason           string
    RemainingSeconds int64
}
```

One DB scan (`db.ListAllActiveSudoGrants`) feeds both the global
`Sudo[]` and the per-agent `Agents[*].ActiveSudo[]`. The per-agent
slice omits `ConvID` (the agent row's own ConvID already identifies
who holds the grant) — saves bytes on agents with many grants and
keeps the JSON readable in browser devtools.

`Sudo` is initialised to `[]` (not nil) so JSON serializes as
`"sudo":[]` even when no grants are active — the dashboard's JS can
call `.length` without a guard.

## Tests (7 new)

`pkg/claude/agentd/dashboard_sudo_test.go`:

Endpoint coverage:

- `TestDashboardSudo_RevokeByID` — single revoke, sibling grants
  for the same conv stay intact.
- `TestDashboardSudo_RevokeByConv` — bulk per conv. Targeted
  incident response: kill ALL grants for one runaway agent. Uses
  conv-id-as-selector via `agent.ResolveSelector`; bob's grants
  stay (no collateral).
- `TestDashboardSudo_RevokeAll` — emergency-stop kill-switch. Three
  agents' grants all gone.
- `TestDashboardSudo_BadRequest_NoSelector` — bare `DELETE /api/sudo`
  with no id, no `?conv=`, no `?all=1` is rejected with 400 BEFORE
  any DB writes. Pins the validation: a JS typo can't become an
  accidental nuke.
- `TestDashboardSudo_AuthRequired` — request without the dashboard
  cookie is rejected; existing grants untouched.

Snapshot coverage:

- `TestSnapshot_ActiveSudoSurfaces` — one agent with one active
  grant: top-level `Sudo[]` carries the row with full fields;
  `Agents[*].ActiveSudo[]` mirrors it on alice's row, omitting
  ConvID.
- `TestSnapshot_ActiveSudoEmptyByDefault` — pins `"sudo":[]`
  serialisation when nothing is active.

## Files

- `pkg/claude/agentd/dashboard_edit.go` — route registration +
  `handleDashboardSudoAPI`.
- `pkg/claude/agentd/dashboard.go` — `dashboardSudoEntry`;
  `snapshotPayload.Sudo`; `dashboardAgent.ActiveSudo`; one DB scan
  + bucket fan-out in the snapshot builder.
- `pkg/claude/agentd/dashboard_sudo_test.go` — 7 new tests.

## Open follow-up: JS rendering

`dashboard.html` still needs:

- A new "Sudo" tab consuming `snapshot.sudo[]`. Columns: conv |
  slug | granted_at | expires_in | reason | revoke. Group rows by
  conv-id with the soonest expiry first inside each block.
- A 🔓 badge on the Groups + Agents tabs for any agent whose
  `active_sudo[]` is non-empty. Click could open a popover listing
  the agent's slugs + remaining time + per-row revoke.
- Click handlers on `DELETE /api/sudo/{id}` (single) and
  `DELETE /api/sudo?conv=…` (bulk).

Out of scope here because Go flow tests can't exercise browser JS
and the v2 doc explicitly carves it out. Everything the JS needs is
already on the wire — render against snapshot, mutate against the
endpoints, no new server work.

## Cross-references

- [`DONE/agent-sudo-elevation-v1.md`](agent-sudo-elevation-v1.md) —
  the daemon /v1/sudo surface this twins.
- [`DONE/dashboard-v1.md`](dashboard-v1.md) — cookie-auth pattern
  these handlers follow.
- [`DONE/agent-sudo-elevation-tray-orange.md`](agent-sudo-elevation-tray-orange.md)
  — sibling slice that surfaces the same active-grants state in the
  tray icon.
