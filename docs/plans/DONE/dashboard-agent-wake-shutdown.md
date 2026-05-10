# Dashboard: per-agent wake / shut-down buttons

Shipped 2026-05.

## What ships

Per-row **wake** / **shut down** / **focus** trio in BOTH the Groups
tab member rows AND the Agents tab. Mutually exclusive based on
online state so the row stays visually stable as the agent toggles:

- **Online** → `focus` + `shut down` buttons
- **Offline** → `wake` button

Wake is non-destructive (resume is idempotent: already-online conv
surfaces as `skipped:already_online`), so the click fires immediately
without a confirm modal — toast announces the daemon's per-conv
result (`resumed` / `skipped:...`).

Shut down opens a 3-button confirm modal with two distinct paths:

- **Soft exit** — injects `/exit` into the tmux pane. Default action.
  Conv jsonl is preserved; in-flight tool calls interrupted.
- **Force kill** (destructive) — `tmux kill-session`. Surfaced as
  the danger button so a stray click doesn't accidentally kill.
- **Cancel** — Esc / outside-click also cancels.

Refresh fires after both wake and shutdown so the snapshot poll
confirms the state transition.

## Daemon side

The daemon-side stop/resume verbs were already shipped (see
`DONE/cli-shortcuts.md`, `DONE/cross-agent-manager-pattern.md`). The
per-conv helpers `stopOneConv` / `resumeOneConv` are the same ones
the bulk `groups.stop` / `groups.resume` paths use, so semantics
match exactly.

New dashboard routes (cookie auth, thin pass-through):

- `POST /api/agents/{conv}/stop` body `{"force": <bool>}` →
  `stopOneConv`. Returns the per-conv result JSON.
- `POST /api/agents/{conv}/resume` (no body) → `resumeOneConv`.

Both resolve the conv-id selector via `agent.ResolveSelector` so the
dashboard can pass aliases / prefixes / canonical conv-ids (same
shape as the existing dashboard delete-agent route).

## Tests

`pkg/claude/agentd/dashboard_edit_test.go`:
- `TestDashboardEdit_StopAgent_OfflineSkipped` — pass-through wired,
  returns `skipped:already_offline` without exercising side-effecting
  tmux send-keys
- `TestDashboardEdit_StopAgent_NotFound` — 404 on unresolvable selector
- `TestDashboardEdit_ResumeAgent_NotFound` — 404 on unresolvable
  selector. (Happy-path resume isn't unit-tested here since it would
  call SpawnDetachedTclaudeResume which spawns a real subprocess; the
  /v1/agent/{conv}/resume flow tests already cover the orchestration
  through the simSpawner.)
- `TestDashboardEdit_StopResume_WrongMethod` — GET on either subpath
  → 405

## Files

- `pkg/claude/agentd/dashboard_edit.go` — `dashboardStopAgent`,
  `dashboardResumeAgent`, dispatcher branches in
  `handleDashboardAgentsAPI`
- `pkg/claude/agentd/dashboard_edit_test.go` — 4 unit tests
- `pkg/claude/agentd/dashboard.html`:
  - `lifecycleAndFocusButtons` (shared between member + agent rows)
  - `editMemberButton`, `ownerToggleButton`, `removeMemberButton`
    (extracted from inline `memberActions` for symmetry)
  - `wake-agent` + `shutdown-agent` cases in `bindRowActions`
  - `shutdownConfirm` 3-button modal helper
  - `shutdown-modal` overlay markup

## Out of scope (deferred)

- **Bulk wake / shut-down from a group header.** Already exists as
  `groups resume` / `groups stop` CLI; surfacing those as
  group-header buttons is a natural follow-up but separate (different
  confirm-modal copy, blast-radius warning matters more). Add a
  follow-up TODO once these single-conv buttons land.
- **Reincarnate / compact buttons** alongside wake/shutdown.
  Mentioned in `web-dashboard.md`. Same shape but more destructive —
  wait for the human to ask.
- **Optimistic UI.** The button doesn't pre-flip the row to the
  post-action state — we wait for the next snapshot poll (5s tick or
  manual refresh) to confirm. Vanilla-JS optimistic UI would have to
  hand-roll snap-back-on-error handling; defer to the framework
  migration that simplifies that pattern.

## Cross-references

- [`DONE/cli-shortcuts.md`](cli-shortcuts.md) — `agent.stop` /
  `agent.resume` CLI shipped here
- [`DONE/cross-agent-manager-pattern.md`](cross-agent-manager-pattern.md)
  — endpoint dispatcher pattern these reuse
- [`DONE/groups-rename.md`](groups-rename.md),
  [`DONE/dashboard-member-metadata-editing.md`](dashboard-member-metadata-editing.md)
  — sibling dashboard work shipped this session
