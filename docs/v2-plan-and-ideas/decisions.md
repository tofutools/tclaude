# Decision register

Keep this short. Architectural decisions live here; work allocation and delivery
status live in AWB. Dates identify discussion, not implementation completion.

| ID | Status | Decision / question | Consequence |
|---|---|---|---|
| D1 | Accepted direction, 2026-09-09 | Refactor working main incrementally; the replacement rewrite remains stopped | Short feature branches and independently useful integration; no wholesale branch merge |
| D2 | Accepted direction, 2026-09-09 | Living design in this folder; work items/epics in AWB | No duplicated task tracker in Markdown; update design with changed assumptions |
| D3 | Accepted direction, 2026-09-09 | Preserve operator functionality and usability; improvements may be explicit | Current main behavior is the baseline, not the reduced replacement model |
| D4 | Accepted direction, 2026-09-09 | Reusable UI dialogs/inputs and consistent small styling matter | Reuse existing framework/components and migrate actual views |
| D5 | Accepted direction; details unresolved | Agent-spawn sandbox inheritance should be identical configuration plus optional denies | Separate policy change from mechanical extraction; establish update/retry examples first |
| D6 | Accepted requirement | Operators may update sandbox profiles while agents run | Stable profile IDs/names; no operator hash workflow or save-time freeze |
| D7 | Operator context | Plugins were experimental, diagnostics relatively simple, remote mTLS a prototype; log viewing production-level | Prioritize production workflows; equal tab counts are not value/effort weights |
| D8 | Proposed | Start with shared UI, resolution, or one direct application action | Select a small first wave after reviewing these candidates |
| D9 | Proposed | Prefer current DB/API structures and temporary narrow adapters | Schema/public changes need a concrete benefit and separate migration proof |
| D10 | Proposed | Track caller removal, change fan-out and slice acceptance | No claim of whole-project percentage based on counts of uneven features |

## Questions to settle when their slice is selected

1. Which repeated maintenance task should be the first measured example?
2. For each option family, what are the exact direct/team/restart precedence rules?
3. When an operator edits a running parent's selected sandbox profile, which
   current authored baseline governs the next delegated child, and how are
   inherited deny overlays retained? Profile edits themselves must remain allowed.
4. Which existing shared dialog/control should be the canonical starting point?
5. Which compatibility facade can be removed within the first slice, and which
   genuinely needs a short follow-up?

These questions should not become a new up-front architecture phase. Resolve
only those that affect the selected increment, record the answer and proceed.
