# Cross-agent lifecycle / manager pattern (2026-05)

An elevated agent (or group owner) can act on *other* agents — the
manager pattern. Typical use: a manager watches workers and
reincarnates ones whose context has rotted with a follow-up
pointing them at the next batch of work.

## Permission model

- `self.<verb>` — operate on yourself only.
- `agent.<verb>` — operate on another agent (target by conv-id /
  alias / selector). Default: human-only. Granted to manager
  agents explicitly.
- **Group ownership grants implicit power.** A group owner can
  call any `agent.<verb>` against any member of a group they own
  without holding the slug. Powered by
  `ownerOfGroupContaining(caller, target)` in
  `pkg/claude/agentd/agent_dispatch.go`.

## Endpoints

`/v1/agent/{selector}/{verb}`. The dispatcher resolves the
selector via `agent.ResolveSelector`, then routes to the per-verb
handler which calls `requireCrossAgentPermission` and runs the
shared orchestration helper with the target conv as subject.

## Slugs shipped

| Slug | CLI | Default |
|------|-----|---------|
| `agent.reincarnate` | `agent reincarnate --target` | human-only |
| `agent.compact` | `agent compact --target` | human-only |
| `agent.rename` | `agent rename --target` | human-only |
| `agent.clone` | `agent clone --target` | human-only |
| `agent.stop` | `agent stop <selector>` | human-only |
| `agent.resume` | `agent resume <selector>` | human-only |
| `agent.schedule` | `agent cron add --target` | human-only |

Self path uses `self.<verb>`; cross-agent uses `agent.<verb>` OR
group ownership. Both paths call the same shared orchestration
(`runReincarnationOrchestration`, `runSlashOrchestration`,
`runRenameOrchestration`, `runCloneOrchestration`).

## Audit

Cross-agent migrations record `granted_by` as
`system:reincarnate:by=<caller-conv>` (vs plain
`system:reincarnate` for self), so "who killed my agent"
forensics work from `agent_permissions` /
`agent_group_owners` audit columns alone.

The handoff message for reincarnate sets `from_conv = caller`, so
the new agent sees who asked it to pick up the work and can
reply directly.

## Popup escape hatch (X-Tclaude-Ask-Human)

`requireCrossAgentPermission` now honors the popup header as a
last-chance branch when slug + ownership both fail. Mirrors the
self-targeted path; payload surfaces `caller + target + perm slug`
so the popup can render "<caller> wants to <verb> <target>".

Three flow scenarios pinned via `RequestHumanApprovalImpl`
indirection (no real browser needed):

- no-header-still-refuses
- header-and-approval-allows-call (200 + succession row recorded)
- header-and-denial-still-refuses (403, no orchestration runs)

## Skill update

`agent-lifecycle` skill updated with the manager-pattern section.

## Open / orthogonal

- `agent.<verb>` and `self.<verb>` are orthogonal (granting one
  doesn't grant the other). Keeping them split for now; revisit
  if managers always also want self-management.
