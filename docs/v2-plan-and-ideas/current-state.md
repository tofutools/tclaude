# Current-state evidence and lessons

Observed on main `a976aef8e53788a0a9e78575ffad2346b5df4e63`,
9 September 2026. These are targeted observations, not a complete architecture audit.
Paths and symbols are more durable than line numbers.

| Current evidence | What it suggests | What it does not prove |
|---|---|---|
| [lifecycle.go](../../pkg/claude/agentd/lifecycle.go): 10,128 lines; multiple `resolve*LaunchField` helpers | Extract coherent policy/resolution and orchestration owners; scalar/boolean helpers are already shared by direct/team callers | Every helper is duplicated, or file size alone justifies a rewrite |
| [templates.go](../../pkg/claude/agentd/templates.go): 4,891 lines; `resolveTemplateAgentLaunch`, `resolveTemplateAgentAccess` | Compare direct/team precedence and share machinery carefully | Team and ordinary spawn have identical merge semantics |
| [triggers.go](../../pkg/claude/agentd/triggers.go): synthetic `http.NewRequest`, `httptest.NewRecorder` for guardrails | A shared application action can serve HTTP and internal callers | Existing checks are redundant or may be removed |
| [spawner.go](../../pkg/claude/agentd/spawner.go): global `Spawn`; comment prohibits parallel flow tests | Inject this effect per action/runtime instance | A universal dependency-injection framework is needed |
| [db.go](../../pkg/claude/common/db/db.go): global DB lifecycle | Introduce focused instance-owned operations along migrated paths | Replace SQLite or rewrite all repositories |
| [dashboard_rowcache.go](../../pkg/claude/agentd/dashboard_rowcache.go): `flushCodexContextWrites` | Separate observation work from projection ownership | Remove writes without replacing their freshness behavior |
| [harness](../../pkg/claude/harness): existing descriptors, lifecycle and native implementations | Strengthen an already useful boundary | Start a second parallel provider framework |
| [config-form-adapter.js](../../pkg/claude/agentd/dashboard/js/config-form-adapter.js): clone-preserving form state and explicit save/conflict handling | Preserve round-trip behavior while consolidating controls | Rebuild forms with a smaller schema |
| [dialog-focus](../../pkg/claude/agentd/dashboard/js/dialog-focus.js), [dialog-resize](../../pkg/claude/agentd/dashboard/js/dialog-resize.js), [approval-controls](../../pkg/claude/agentd/dashboard/js/approval-controls.js) | Existing shared components are starting points | All frontend code needs replacement |

## Lessons from the paused replacement effort

- Architectural completeness and operator feature completeness are different.
  Main's working workflows remain the acceptance baseline.
- Default resolution is domain behavior. Omitted versus explicit values, task
  versus context, native options and scope-local metadata cannot be flattened.
- A provider compatibility abstraction can hide meaningful native differences.
  Keep native mechanisms owned by existing adapters and make unsupported explicit.
- Much complexity comes from policy repeated across flows, not lack of packages.
- Preserving durable identity does not require making every editable profile
  immutable or exposing hashes to operators.
- Large serial chains of tiny feature PRs and repeated broad CI can spend a great
  deal without establishing a finite deliverable. Prefer bounded coherent slices.
- A useful extraction should remove an old path and leave a working product
  even if the larger effort stops tomorrow.

These lessons are not claims that every alternative implementation in the paused
branch should be reused. Reassess each candidate against current main.
