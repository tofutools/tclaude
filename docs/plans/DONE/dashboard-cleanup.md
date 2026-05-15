# Dashboard cleanup — bulk-prune offline agents from groups / delete them

Shipped: a 🧹 **cleanup** affordance on the agentd dashboard for
decluttering groups and the agents tab of confirmed-offline agents.
Long-running coordination sessions accumulate dead workers and
abandoned experiments; cleanup bulk-prunes them, with the option to
also remove the agents' git worktrees.

## Surface

Dashboard-only — no `tclaude agent` CLI surface, no SQLite migration,
no permission slugs. Cleanup is human-only by construction: the
endpoints live on the loopback dashboard server behind the same
cookie + Origin pinning as every other `/api` mutation.

### Endpoints (loopback dashboard mux)

- `POST /api/cleanup/group` — body `{group, members[], include_owners}`.
  Removes the listed confirmed-offline members from one group. An
  offline member that is also an owner is skipped unless
  `include_owners` is set, which also drops the owner row; a
  now-ownerless group comes back in `warnings`.
- `POST /api/cleanup/agents` — body
  `{agents[], delete, include_owners, delete_worktrees}`.
  - `delete=false` — unjoins the agents from every group they belong
    to (history kept on disk).
  - `delete=true` — full purge via `conv.DeleteConvByID`.
  - `delete_worktrees=true` (delete only) — also removes each purged
    agent's git worktree.
- `GET /api/agents/{conv}/worktree` — `{kind, path, branch, shared,
  removable}`; the delete-agent modal reads this to render its
  worktree checkbox.
- `DELETE /api/agents/{conv}?delete_worktree=1` — the per-row delete
  button's worktree opt-in. Without the param the original `204`
  contract holds; with it, `200` + `{conv_id, worktree}`.

### Safety

Every conv-id is re-checked against live tmux (`isConvOnline`) at
execute time — the daemon never trusts the snapshot's "offline"
label. An agent that came back online between snapshot and submit is
reported `skipped`, never touched. Endpoints are idempotent.

Worktree removal spares the repo's **main** worktree and any worktree
a surviving agent still works in ("shared"). The worktree directory
is removed with `--force`; its branch and commits are kept. An
already-gone worktree is a silent no-op. "Survivor" excludes exactly
the set actually being deleted (a pre-pass), so an online agent that
gets skipped still counts as keeping its worktree alive.

## Files

- `pkg/claude/agentd/dashboard_cleanup.go` — the two cleanup
  endpoints + `resolveCleanupConv`, group-removal helpers.
- `pkg/claude/agentd/worktree_cleanup.go` — `inspectAgentWorktree`,
  `otherAgentWorktreeRoots` (shared detection), `applyWorktreeCleanup`,
  the `GET .../worktree` handler. Git seam: `inspectWorktreeFn` /
  `removeWorktreeFn`.
- `pkg/claude/worktree/cleanup.go` — `InspectWorktree` (classifies a
  dir's worktree none/main/linked) and `RemoveLinkedWorktree`
  (idempotent, force, refuses main).
- `pkg/claude/agentd/dashboard_edit.go` — registers `/api/cleanup/`;
  `handleDashboardAgentsAPI` gained the `worktree` GET subverb and
  `?delete_worktree=`.
- `pkg/claude/agentd/dashboard.html` — per-group / Groups-tab /
  Agents-tab 🧹 buttons; `openCleanupModal` (editable include/exclude
  checklist, inactivity-age quick-filter, per-item outcome log);
  `deleteAgentModal` (per-row delete with the worktree checkbox).
- `docs/dashboard.md` — the **Cleanup** section.

## Tests

- `pkg/claude/worktree/cleanup_test.go` — real-git unit tests:
  inspect main vs linked vs none, remove + idempotency, refuses main,
  force clears a dirty tree.
- `pkg/claude/agentd/cleanup_flow_test.go` — flow tests via the
  dashboard mux: offline removal vs the tmux re-check protecting an
  online agent; owner opt-in + ownerless warning; delete vs unjoin;
  unknown-group 404; worktree removal for a linked worktree; shared
  and main worktrees kept; `delete_worktrees` opt-in respected;
  single-delete `GET .../worktree` + `DELETE ?delete_worktree=1`.
- Test seam: `SetWorktreeFnsForTest` (testhooks_test.go) swaps the
  git-worktree seam so flow tests need no real repos.

## Possible follow-ups

- Surface a cleanup entry point in the `tclaude agent` CLI (currently
  dashboard-only by design — the human is the only actor).
- A scheduled / cron-driven auto-cleanup of agents idle past a
  threshold (deliberately not built — cleanup stays human-triggered).
