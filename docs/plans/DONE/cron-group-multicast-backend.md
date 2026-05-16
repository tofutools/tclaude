# Cron group-multicast backend

Shipped 2026-05.

The dashboard cron form has had a **Group (multicast)** target radio
since [`dashboard-cron-create-form.md`](dashboard-cron-create-form.md),
and the Groups tab has had a per-group `⏰ multicast` button. Both were
**dead UI** — the backend never implemented a group target:

1. `handleCronCreate` resolved the target through `agent.ResolveSelector`,
   which has no `group:` branch — so `--target group:NAME` 404'd with
   "no conversation matches group:NAME".
2. `agent_cron_jobs` stored a single `target_conv` and `fireCronJob`
   inserted exactly one `agent_messages` row — there was no fan-out, so
   even a resolved group target could never multicast.

This slice makes a group target real: a cron job can target a whole
group, and on each fire it fans the body out to **every current member**
— membership resolved at fire time, so a recurring job tracks the live
roster as members join and leave.

## What shipped

### Schema — migration v41

`agent_cron_jobs` gained a `target_kind` column (`migrateV40toV41`):

```sql
ALTER TABLE agent_cron_jobs
    ADD COLUMN target_kind TEXT NOT NULL DEFAULT 'conv'
    CHECK (target_kind IN ('conv', 'group'));
```

- `target_kind = 'conv'` — `target_conv` is the recipient; `group_id`
  (when >0) is the routing group. The long-standing job shape.
- `target_kind = 'group'` — `group_id` IS the target group; the
  scheduler fans out to its membership at fire time. `target_conv` is
  unused.

Existing rows backfill to `'conv'`. The discriminator is `target_kind`,
**not** `group_id > 0` — a conv-target job routed through a shared group
also carries a non-zero `group_id`. `db.AgentCronJob.IsGroupTarget()` is
the one true discriminator; callers must use it.

### Target resolution — `group:` grammar

`handleCronCreate` (and `decodeCronPatchBody`, for symmetry) now resolve
the `target` field through `resolveCronTarget`:

- `group:<name>` / `group:<id>` → a group (`resolveGroupToken`, the
  non-HTTP twin of `resolveMulticastGroup`; an empty `group:` token is
  rejected — a recurring job has no "own group").
- anything else → a single conv via `agent.ResolveSelector`, exactly as
  before.

Solo conv targets behave identically to before this change.

### Fire-time fan-out — shared with `group:` multicast

`handleMulticast`'s fan-out loop was extracted into `fanOutToGroup`
(`handlers.go`) — the shared core that delivers a body to every member
of a group except a given conv, with per-recipient rows + nudges and an
optional role filter. `handleMulticast` now calls it; its external HTTP
behaviour is unchanged.

`fireCronJob` gained a group branch (`fireCronGroupJob`): for a
group-kind job it resolves the target group, then calls `fanOutToGroup`
— the **same path** as a one-shot `group:` multicast, so the two can
never drift. The job owner is the message sender and is skipped if it is
itself a member (a PO scheduling a team ping does not ping itself).
Status: `no_target` if the group was deleted, `send_failed` if any
recipient row failed, `ok` otherwise (including a fan-out to zero other
members — an empty group is a successful no-op, not an error).

### Auth

A group-target job is gated by `authCronWriteGroup` — the caller must be
a member or owner of the target group, mirroring `handleMulticast`'s
broadcast gate (no extra permission slug). `authCronJob` dispatches the
by-id mutations (enable / disable / run-now / patch / delete) to the
conv- or group-target gate, so a group job — whose `target_conv` is
empty — is authorised against its group rather than against `""`.
`jobVisibleTo` likewise makes a group job visible to every member and
owner of its target group. Humans bypass all checks, as elsewhere.

### Dashboard + CLI rendering

The cron form already POSTed `target: "group:<name>"` — no markup
change was needed. Two JS discriminators in `dashboard.html`
(`cronTargetCell`, `jobToPrefill`) were switched from `group_id > 0` to
`target_kind === 'group'`, fixing a latent mislabel: a conv-target job
routed through a shared group also has a non-zero `group_id` and was
being rendered/edited as a multicast. The daemon now only populates
`group_name` for group-kind jobs.

The `tclaude agent cron` CLI (`ls`, `add`) renders a group target as
`group:<name>` and the `add` output explains the multicast.

### The per-group `⏰ multicast` button (investigation result)

Item 4 of the brief: the Groups-tab group-header `⏰ multicast` button
is **not** a separate one-shot surface — its `data-act="cron-new"`
opens the cron-create modal pre-filled with `targetMode: 'group'`. It is
a cron-job creation entry point, so this slice makes it work. (The
one-shot dashboard group multicast is a separate surface owned by the
`group-message` worker; it reuses the same `fanOutToGroup` core via
`handleMulticast`.)

## Files

- `pkg/claude/common/db/migrate.go` — `currentVersion` 40→41,
  `migrateV40toV41`.
- `pkg/claude/common/db/agent_cron.go` — `CronTargetConv` /
  `CronTargetGroup` consts, `AgentCronJob.TargetKind` +
  `IsGroupTarget()`, `target_kind` in insert / select / scan /
  `UpdateCronPatch`.
- `pkg/claude/agentd/handlers.go` — `fanOutToGroup` extracted from
  `handleMulticast`.
- `pkg/claude/agentd/cron.go` — `fireCronGroupJob`; `fireCronJob` group
  branch.
- `pkg/claude/agentd/cron_handlers.go` — `resolveCronTarget`,
  `resolveGroupToken`, `resolveCronOwner`, `authCronWriteGroup`,
  `authCronJob`; `handleCronCreate` / `decodeCronPatchBody` group
  support; `jobVisibleTo` group case; `target_kind` / `group_name` in
  `jobJSON`.
- `pkg/claude/agentd/dashboard.go` — `dashboardCronJob.TargetKind`;
  `collectCronSnapshot` gates `group_name` on `target_kind`.
- `pkg/claude/agentd/dashboard.html` — two JS discriminators switched to
  `target_kind`.
- `pkg/claude/agent/cron.go` — CLI `target_kind` / `group_name`,
  `cronTargetLabel`, `add` output + help text.

## Tests

- `pkg/claude/common/db/migrate_v41_test.go`
  - `TestMigrateV40toV41_AddsTargetKind` — existing conv-targeted rows
    survive and backfill to `conv`; a group-kind row inserts; the CHECK
    constraint rejects an unknown kind.
  - `TestMigrateV40toV41_FreshSchemaHasTargetKind` — full chain reaches
    v41; `target_kind` defaults to `conv`.
- `pkg/claude/agentd/cron_group_multicast_flow_test.go`
  - `TestCronGroupMulticast_FiresToEveryMember` — fan-out to every
    member except the owner; online member nudged.
  - `TestCronGroupMulticast_MembershipResolvedAtFireTime` — a member
    added / removed between fires changes the fan-out.
  - `TestCronGroupMulticast_ConvTargetDeliversToExactlyOne` — a
    conv-target job delivers to one inbox, not the whole group.
  - `TestCronGroupMulticast_AuthGate_DeniesNonMember` — a non-member
    cannot schedule a group multicast.
  - `TestCronGroupMulticast_DashboardCreate` — the dashboard
    `POST /api/cron` group path (the `⏰ multicast` button's wire path).
  - `TestCronGroupMulticast_DeletedGroup_NoTarget` — a job whose group
    was deleted fires cleanly with `no_target`.

## Cross-references

- [`dashboard-cron-create-form.md`](dashboard-cron-create-form.md) — the
  form whose Group radio + `⏰ multicast` button this slice makes real.
- [`group-addressed-sends.md`](group-addressed-sends.md) — the `group:`
  multicast send whose fan-out (`fanOutToGroup`) is now shared with cron.
