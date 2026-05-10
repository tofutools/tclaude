# `tclaude agent sudo` — audit annotations on downstream ops (v2 slice 2)

Shipped 2026-05.

V1 left an audit-trail blind spot: an op that succeeded via a sudo
grant recorded the same `granted_by` value it would record without
sudo. So a forensic query "what did agent X do during the elevation
window 18:30–18:34?" couldn't distinguish elevated ops from regular
ones — the elevation was invisible in the downstream rows.

This slice plumbs the sudo grant-id through the `granted_by`
composition at every site where requirePermission's caller conv-id
becomes part of an audit string. Forensics can now answer the
elevation-window query with a single `LIKE '%via-sudo%'` over the
relevant tables.

## DB helper

`pkg/claude/common/db/agent_sudo_grants.go`:

```go
func LookupActiveSudoGrantID(convID, slug string) (int64, error)
```

Returns the soonest-to-expire active grant id for `(convID, slug)`,
or `0` if none. Same ordering `sudo ls` uses, so the audit string
ties to the row the human is most likely to act on first if multiple
active grants for the same pair exist (re-request before the first
expired).

## Audit composer

`pkg/claude/agentd/permissions.go`:

```go
func auditedCaller(callerConvID, perm string) string
```

Returns the `granted_by` form for a permission-gated mutate:

- `""` — human callers (callerConvID empty); sites that label humans
  differently keep doing so via `granterLabel`.
- `"<conv>:via-sudo:grant-id=<n>"` — the agent passed *only* because
  of an active sudo grant for `perm`. The annotation is queryable
  (LIKE) and ties back to a specific grant row.
- `"<conv>"` — the agent had a non-sudo source for the permission
  (default-permissions list or `agent_permissions` row).

Only annotates when sudo was load-bearing. An agent that already had
the slug via a normal grant would lie if its ops were marked
"via-sudo" — the elevation window was incidental, not the reason
the call passed.

Re-checks config + DB at the audit-write layer (not the hot read
path).

## Wired sites

| Handler                 | Perm                  | Audit write                                              |
|-------------------------|-----------------------|----------------------------------------------------------|
| `handleGroupCreate`     | `groups.create`       | `db.AddAgentGroupOwner(id, creator, auditedCaller(...))` |
| `handleGroupOwnersAdd`  | `groups.own`          | `db.AddAgentGroupOwner(g.ID, res.ConvID, ...)`           |
| `handleGroupRename`     | `groups.rename`       | `db.RenameAgentGroup(g.Name, newName, ...)`              |
| `handleGroupClone`      | `groups.clone`        | `granter = "system:groups.clone:by=" + auditedCaller(...)` |
| `runCloneOrchestration` | `self.clone` / `agent.clone` | `granter = "system:clone:by=" + auditedCaller(...)` (cross) or `system:clone:via-sudo:grant-id=N` (self) |
| `runReincarnationOrchestration` | `self.reincarnate` / `agent.reincarnate` | same shape as clone |

`runCloneOrchestration` and `runReincarnationOrchestration` gained a
`perm` parameter so they can call `auditedCaller`/`LookupActiveSudoGrantID`
with the slug requirePermission gated on. Each entry point passes
the slug it's protecting; the dashboard cookie-auth twins pass `""`
(no agent path → no sudo to annotate).

`handlePermissionsGrant` is **not** annotated: `permissions.grant` is
in `sudoDefaultBlocklist`, so the via-sudo path is impossible there.

## Self-clone / self-reincarnate annotation

The cross-agent path is naturally `system:<op>:by=<caller>` so
appending `:via-sudo:grant-id=<n>` to `<caller>` covers it. The self
path emits a bare `system:<op>` (no `:by=` because the target *is*
the caller), so via-sudo would otherwise fall on the floor. Self
paths now do an explicit `LookupActiveSudoGrantID` and emit
`system:<op>:via-sudo:grant-id=<n>` when there's an active grant —
preserving the format change minimally:

| Mode                | granted_by                                                |
|---------------------|-----------------------------------------------------------|
| Self, no sudo       | `system:clone`                                            |
| Self, via sudo      | `system:clone:via-sudo:grant-id=42`                       |
| Cross, no sudo      | `system:clone:by=<conv>`                                  |
| Cross, via sudo     | `system:clone:by=<conv>:via-sudo:grant-id=42`             |

## Tests (2 new flow tests)

`pkg/claude/agentd/sudo_flow_test.go`:

- `TestSudo_DownstreamAuditAnnotation_GroupsCreateOwner` — agent
  sudoes `groups.create`, creates group, the auto-owner row's
  `granted_by` = `<conv>:via-sudo:grant-id=<grant-id>`. Pins the
  full plumbing (request → popup → grant row → downstream write
  → audit string).
- `TestSudo_DownstreamAuditAnnotation_NoSudoNoAnnotation` — agent
  has `groups.create` via `agent_permissions` row (not sudo);
  creates group; `granted_by` = plain conv-id (no annotation). Pins
  the "only annotate when sudo was load-bearing" rule — without
  this, an op that didn't need elevation would be misleadingly
  marked.

## Files

- `pkg/claude/common/db/agent_sudo_grants.go` — new
  `LookupActiveSudoGrantID`.
- `pkg/claude/agentd/permissions.go` — new `auditedCaller`.
- `pkg/claude/agentd/handlers.go` — wired into `handleGroupCreate`
  and `handleGroupOwnersAdd`.
- `pkg/claude/agentd/groups_rename.go` — wired into the
  `RenameAgentGroup` audit field.
- `pkg/claude/agentd/groups_clone.go` — wired into the
  `system:groups.clone:by=...` granter.
- `pkg/claude/agentd/clone.go` — `runCloneOrchestration(perm)`
  parameter; self-path emits the via-sudo form when applicable.
- `pkg/claude/agentd/reincarnate.go` — same change for
  `runReincarnationOrchestration`.
- `pkg/claude/agentd/dashboard_edit.go` — passes `""` for `perm` on
  the cookie-auth twin (no sudo).
- `pkg/claude/agentd/sudo_flow_test.go` — two new flow tests.

## Cross-references

- [`DONE/agent-sudo-elevation-v1.md`](agent-sudo-elevation-v1.md) — v1
  shipped the grant model these annotations point back into.
- [`DONE/agent-sudo-elevation-config-defaults.md`](agent-sudo-elevation-config-defaults.md)
  — slice 1; same-shape per-feature DONE.
- [`TODO/high-prio/agent-sudo-elevation.md`](../TODO/high-prio/agent-sudo-elevation.md)
  — remaining v2 slices: dashboard panel, tray-icon orange state.
