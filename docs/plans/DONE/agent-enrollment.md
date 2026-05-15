# Agent enrollment — explicit promote / retire — SHIPPED

## What shipped

Agent-ness used to be a read-time heuristic: a conv counted as an agent
if it was a group member, held a permission grant, **or** had a live
tmux session. Two consequences — an ungrouped + offline conv was
invisible on every agent surface, and cleanup had no middle ground
between "unjoin from groups" and "permanently delete the .jsonl".

Agent-ness is now an explicit, durable, reversible bit:
`agent_enrollment`, keyed by conv-id, with a nullable `retired_at`.

| State                   | Meaning                            | Surfaces as        |
|-------------------------|------------------------------------|--------------------|
| no row                  | plain conversation, never an agent | Conversations list |
| row, `retired_at` empty | active agent                       | Agents list        |
| row, `retired_at` set   | retired — demoted, data intact     | Retired section    |

## Schema (migration v29→v30)

`agent_enrollment(conv_id PK, enrolled_at, enrolled_via, retired_at,
retired_by, retire_reason)` — TEXT columns, `retired_at = ''` means
active. `backfillAgentEnrollment` enrolls every conv-id found in
`agent_group_members/owners`, `agent_permissions`, `agent_sudo_grants`,
`agent_head_aliases`, `agent_conv_succession`, `agent_clone_history`,
`agent_cron_jobs`, `agent_workdir` and `agent_messages`, so no agent
disappears on upgrade. `DeleteAgentByConvID` cascades the enrollment row.

## Enrollment triggers (db.EnrollAgent — INSERT OR IGNORE)

- `AddAgentGroupMember` / `AddAgentGroupOwner` / `GrantAgentPermission`
  / `InsertSudoGrant` — joining a group or holding a grant enrolls.
- `RecordConvSuccession` enrolls the successor; `clone.go` enrolls the
  clone; `reincarnate.go` calls `DeleteEnrollment` on the superseded
  predecessor so it doesn't linger as an offline ghost.
- `withIdentity` middleware enrolls any conv that calls a `/v1`
  endpoint (`enrollCallerOnce`, one DB write per conv per daemon run).
- Daemon startup runs `reconcileOnlineEnrollment` — enrolls every
  currently-online session the SQL backfill couldn't tmux-probe.

## Read path

`dashboard.go` and `/v1/peers` switched from the online heuristic to
`db.ListActiveAgents()`. Offline enrolled agents now show; retired
agents are excluded everywhere; `/v1/peers` pass 2 surfaces only
ungrouped agents (preserving group scoping). The snapshot gained
`conversations[]` (recent non-enrolled convs, recency-capped via
`ListRecentConvIndex`) and `retired[]`.

## Lifecycle verbs

DB: `EnrollAgent`, `PromoteAgent`, `RetireAgent`, `ReinstateAgent`,
`EnrollmentState`, `ListActiveAgents`, `ListRetiredAgents`,
`DeleteEnrollment`. `retireAgentConv` (agentd) unjoins all groups +
revokes perm/sudo grants + flips the bit.

- `POST /v1/agent/{sel}/promote|retire|reinstate` — gated by
  `requireCrossAgentPermission` with new slugs `agent.promote` /
  `agent.retire`.
- `POST /api/agents/{conv}/{promote,retire,reinstate}` — dashboard
  cookie-auth twins via `dashboardEnrollmentVerb`.
- `/api/cleanup/agents` gained a `mode` field: `unjoin | retire |
  delete` (legacy `delete` bool still honoured).
- CLI: `tclaude agent promote | retire [--reason] | reinstate`.

## Dashboard

Agents tab gained a **Conversations** sub-list (promote buttons) and a
collapsible **Retired** section (reinstate buttons); a per-row retire
button; a 3-tier cleanup modal (Unjoin / Retire / Delete). The
add-member modal offers non-agent conversations, tagged "promotes to
agent". The Groups tab gained a virtual **Conversations** group
(mirroring the virtual Ungrouped group, opt-in via a "show
conversations" checkbox) — drag a row onto a real group to promote +
add it.

## Tests

- `db/agent_enrollment_test.go` — `TestBackfillAgentEnrollment` (the
  upgrade-safety test), `TestEnrollmentLifecycle`.
- `agentd/agent_enrollment_flow_test.go` — non-enrolled→conversations,
  offline-enrolled stays on roster, promote, retire (revokes
  groups+grants), reinstate, cleanup retire tier, cleanup retire skips
  online, reincarnate/clone preserve agent status, add-to-group
  promotes.
- Updated `dashboard_ungrouped_flow_test.go` /
  `dashboard_addmember_flow_test.go` for the enrollment model.

## Open follow-ups

- Reincarnation un-enrolls the predecessor outright; if a lineage view
  is ever wanted, retire-instead-of-delete could surface it in the
  Retired section.
