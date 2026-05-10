# `tclaude agent sudo` — time-bounded permission elevations (v1)

Shipped 2026-05.

Modeled after Unix `sudo` and GCP PAM. An agent requests a bundle of
permission slugs for a bounded duration; the request always pops a
human-approval popup; on approve the slugs join the agent's effective
permission set until the window closes.

V1 ships the core lifecycle (request / list / revoke) on the daemon
+ CLI surface. Dashboard panel, tray-icon orange state, audit
annotations on downstream operations, and config-driven defaults
remain open in
[`TODO/high-prio/agent-sudo-elevation.md`](../TODO/high-prio/agent-sudo-elevation.md).

## What ships

### Schema

Migration v22→v23 adds:

```sql
CREATE TABLE agent_sudo_grants (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  conv_id     TEXT NOT NULL,
  slug        TEXT NOT NULL,
  granted_at  TEXT NOT NULL,
  expires_at  TEXT NOT NULL,
  granted_by  TEXT NOT NULL,                  -- "human:popup-id=<id>"
  reason      TEXT NOT NULL DEFAULT '',
  revoked_at  TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_sudo_active
  ON agent_sudo_grants(conv_id, expires_at)
  WHERE revoked_at = '';
```

Active = `revoked_at = ''` AND `expires_at > now()`. The partial
index keeps the active-grants probe O(matching rows) for the hot
path (`requirePermission` calls it on every check).

`revoked_at` is distinct from "expired by time" so audit can tell
the two apart — `RevokeSudoGrant` stamps `revoked_at = now()`,
expiration is implicit when `expires_at` slips into the past.

### `requirePermission` integration

Third source for the union:

```
allowed = HasDefaultPermission(perm)
       OR HasAgentPermissionRow(conv, perm)
       OR HasActiveSudoGrant(conv, perm)         ← NEW
```

`HasActiveSudoGrant` is one indexed lookup, safe to call on every
permission check. The popup escape hatch and audit columns the
existing flows touch are unchanged.

### Daemon endpoints

```
POST   /v1/sudo                    request (popup-gated)
GET    /v1/sudo                    list active for caller (agent)
GET    /v1/sudo?all=1              list active across all (human-only)
DELETE /v1/sudo/{id}               revoke one (human-only)
DELETE /v1/sudo?conv=<selector>    revoke all for one conv (human-only)
DELETE /v1/sudo?all=1              nuke every active grant (human-only)
```

The request flow:

1. Refuse if caller is human (humans hold every permission already
   — no need for sudo) or has no resolvable conv-id.
2. Validate body: slugs[] non-empty, duration parseable + ≤ cap,
   blocklisted slugs refused without popping the popup.
3. Build the popup payload (slug list + duration + expires_at +
   reason) and `requestHumanApproval(req, popupBaseURL)` blocks
   until decided.
4. On approve: insert one row per slug with the same
   `granted_at` / `expires_at` so the bundle reads as a coherent
   approval in audit views. `granted_by = "human:popup-id=<n>"`
   ties each row to the approval that produced it.
5. On deny / timeout: 403, no rows.

### Hardcoded defaults (config block deferred to v2)

```go
const sudoMaxDuration     = 1 * time.Hour      // upper bound on a single grant
const sudoDefaultDuration = 5 * time.Minute    // when --duration is omitted
const sudoPopupTimeout    = 60 * time.Second   // how long the popup blocks
var sudoBlocklist = []string{
    PermPermissionsGrant,    // "permissions.grant"
    PermPermissionsRevoke,   // "permissions.revoke"
}
```

Blocklisted slugs would enable PERMANENT escalation if elevated
even briefly: `permissions.grant` could grant itself anything
during the window and the grant outlives the elevation. Block at
the request-validation layer (no popup) so a misclick or runaway
loop can't even surface them.

`groups.own` is intentionally **not** blocklisted — it spreads
power but the time-bound + popup audit make it recoverable.
Forbid only the truly recursive escalation.

### CLI

```
tclaude agent sudo request <slug>... --duration 5m --reason "..."
tclaude agent sudo ls                # active grants for self
tclaude agent sudo ls --all          # all active across all (human-only)
tclaude agent sudo revoke <id>       # revoke one
tclaude agent sudo revoke --conv X   # revoke all for one conv
tclaude agent sudo revoke --all      # nuke every active grant (with confirm)
```

`request` blocks on the popup (no silent grant path, by design).
The CLI prints the approved slug list with grant IDs so the human
can revoke individual ones later. `ls` groups output by conv-id
with the soonest-to-expire grants first. `revoke --all` confirms
before firing unless `--force` is set.

## Tests

`pkg/claude/agentd/sudo_flow_test.go`:

- `TestSudo_Approved_GrantsForDuration` — happy path: popup
  approves, row lands, `HasActiveSudoGrant` returns true.
  `granted_by` carries the popup-id prefix.
- `TestSudo_Denied_NoGrant` — popup denies → 403, no rows
  inserted.
- `TestSudo_Blocklist_RefusesWithoutPopup` — request includes
  `permissions.grant` → 403, no rows for ANY slug in the bundle
  (a valid slug accompanying a blocked one is also rejected so a
  partial-bundle can't slip through).
- `TestSudo_DurationCap_RejectedWithoutPopup` — `duration: 24h`
  → 400 before popup, no rows.
- `TestSudo_RevokedEarly_TakesEffectImmediately` — DELETE the
  grant by id mid-window; `HasActiveSudoGrant` flips to false.
- `TestSudo_Ls_AgentSeesOnlyOwnGrants` — agent GET sees self
  only; human GET `?all=1` sees everyone; agent GET `?all=1` is
  refused.

Tests stub the popup via the existing `StubApprovalForTest(decision)`
indirection — no real browser involvement.

## Files

- `pkg/claude/common/db/migrate.go` — `migrateV22toV23`,
  `currentVersion = 23`
- `pkg/claude/common/db/agent_sudo_grants.go` — new file. CRUD +
  `HasActiveSudoGrant`, `RevokeAllActiveSudoGrants`, lazy
  `PurgeExpiredSudoGrants` housekeeping helper.
- `pkg/claude/agentd/identity.go` — third source in
  `requirePermission`'s union.
- `pkg/claude/agentd/sudo.go` — new file. Request / list / revoke
  handlers + popup integration + blocklist + duration cap.
- `pkg/claude/agentd/serve.go` — register `/v1/sudo` and
  `/v1/sudo/{id}` routes.
- `pkg/claude/agent/sudo.go` — new file. CLI commands.
- `pkg/claude/agent/agent.go` — register `sudoCmd()`.
- `pkg/claude/agentd/sudo_flow_test.go` — 6 flow tests.

## Out of scope (deferred to v2 — see TODO file)

- **Dashboard "Sudo" tab** with per-row indicator on Groups +
  Agents tabs (🔓 / orange highlight when an agent holds at least
  one active grant). Cookie-auth twin endpoints
  (`DELETE /api/sudo/{id}` + `?conv=`).
- **Tray icon orange state** when at least one active grant
  exists (slots into the existing colour matrix alongside
  pending-approval yellow).
- **Audit annotations on downstream ops** — when `groups.spawn`
  passes via sudo, the resulting `granted_by` column should
  carry `"system:groups.spawn:via-sudo:grant-id=<id>:by=<conv>"`
  so forensic queries can answer "what did agent X do during
  the window 18:30–18:34?".
- **Config-driven defaults** — `agent.sudo.{max_duration,
  default_duration, blocklist, popup_timeout}` in
  `~/.tclaude/config.json` plus per-conv overrides via the
  existing `permission_overrides[conv|prefix|title]` pattern.
- **Manager-pattern approval** — letting a group owner approve
  sudo for a group member without involving the human. Trust
  laundering risk; defer until a use case appears.
- **Auto-extend on activity** — explicitly NOT shipping. The
  re-request friction is the feature.

## Cross-references

- [`DONE/permissions-framework.md`](permissions-framework.md) —
  `requirePermission()` and the union-of-grants model this
  extends.
- [`DONE/cross-agent-manager-pattern.md`](cross-agent-manager-pattern.md)
  — popup escape hatch (`X-Tclaude-Ask-Human`) this reuses.
- [`TODO/high-prio/agent-sudo-elevation.md`](../TODO/high-prio/agent-sudo-elevation.md)
  — full design doc; will get trimmed to just the v2 follow-ups
  when the parent file is moved over.
