# Dashboard cleanup tool — all conversation categories

**Status: shipped.**

## What shipped

The dashboard's 🧹 cleanup tool used to see only **active enrolled
agents**. The snapshot exposes three disjoint conversation categories —
`agents[]` (active), `retired[]` (demoted/reinstatable), `conversations[]`
(plain, never enrolled) — but the cleanup modal built its candidate list
from `agents[]` alone. Retired agents had no delete affordance anywhere on
the dashboard (only a per-row "reinstate"); plain conversations only had
"promote". Both were dead-ends that accumulated.

The Agents-tab cleanup modal now spans all three categories, with category
/ online / search filters and a fourth tier.

### Tiers

| Tier        | Acts on                          |
|-------------|----------------------------------|
| `unjoin`    | active agents — drop group memberships |
| `retire`    | active agents — demote to a plain conversation |
| `delete`    | **any** conversation — agent, retired or plain |
| `reinstate` | retired agents — return to the active roster (new) |

A target whose tier doesn't apply is reported `skipped`, never `failed`, so
a mixed-category selection degrades gracefully.

### Backend — `pkg/claude/agentd/dashboard_cleanup.go`

`POST /api/cleanup/agents` body gained:
- `mode: "reinstate"` — new tier; calls `db.ReinstateAgent`.
- `include_online: bool` — opt-in that lifts the skip-online guard. `delete`
  force-stops the running session first (`stopOneConv`); `retire`/`unjoin`
  apply directly; `reinstate` ignores liveness regardless.

`cleanupResponse` gained `reinstated int`; `cleanupOutcome.Result` gained the
`reinstated` value. The legacy `delete: bool` body field still works.

No DB migration — `db.ReinstateAgent` / `db.EnrollmentState` already existed.
The `delete` tier already ran on any conv-id via `conv.DeleteConvByID`; the
gap was purely that the frontend never offered non-agent conv-ids.

### Frontend — `pkg/claude/agentd/dashboard.html`

`openCleanupModal()` for `mode:'agents'` rewritten:
- Candidates built from all three snapshot lists, each tagged `category`.
- Filter pipeline: category checkboxes · "include online sessions" toggle ·
  text search (title / conv-id) · inactivity-age bulk-select. The tier
  doubles as a category gate (retire/unjoin → active only, reinstate →
  retired only, delete → all).
- Default tier `delete` (every category visible on open), nothing
  pre-checked, submit disabled until a selection is made.
- List rows grouped under category sub-headers; result phase shows the
  `reinstated` count.
- New entry point: a 🧹 cleanup button in the "Retired agents" section
  header opens the modal pre-scoped to `categories:['retired']`.

`group` and `all-groups` modes are unchanged.

## Files

- `pkg/claude/agentd/dashboard_cleanup.go` — reinstate tier, `include_online`.
- `pkg/claude/agentd/dashboard.html` — modal rework, CSS, entry point.
- `pkg/claude/agentd/cleanup_flow_test.go` — new flow tests.

## Test scenarios (`cleanup_flow_test.go`)

- `TestCleanup_Agents_DeleteRetiredAgent` — delete reaches a retired agent.
- `TestCleanup_Agents_DeletePlainConversation` — delete reaches a plain conv.
- `TestCleanup_Agents_ReinstateRetiredAgent` — reinstate returns it to active.
- `TestCleanup_Agents_ReinstateSkipsActiveAgent` — graceful skip.
- `TestCleanup_Agents_RetireSkipsRetiredAgent` — graceful skip.
- `TestCleanup_Agents_IncludeOnlineDeletesRunning` — opt-in force-stop.
- `TestCleanup_Agents_MixedCategoriesDelete` — one pass, all three categories.

## Known limitation

The snapshot caps `conversations[]` at 75 rows (recency-scanned 200) — the
cleanup modal inherits that cap. Raising it is a separate snapshot concern.
