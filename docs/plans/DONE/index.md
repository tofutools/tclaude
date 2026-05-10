# Shipped — agent coordination

Condensed log of features that have shipped. New entries go on top
within their section. Each line should be specific enough that a
future agent can grep for behaviour and find the commit / PR.

For the active TODOs see `docs/plans/TODO/`. For the original v1
design see `docs/plans/agent-coord.md`. For the daemon design see
`docs/plans/agentd.md`. For the simulator harness see
`docs/plans/testharness-v2.md`.

---

## 2026-05 (recent commits)

- **`groups create --member` spawn-on-create**. Repeatable
  `--member alias=...,role=...,descr=...,cwd=...` flag bootstraps a
  team in one call. CLI parses + validates up-front (typo aborts
  before any DB work), creates the group via existing endpoint, then
  iterates `groups.spawn` per member. Partial failure leaves the
  group up; human retries via `agent spawn`. Caller's cwd defaulted
  per-member when `cwd=` is omitted; explicit `cwd=` overrides.
  Three flow scenarios pinned:
  `TestGroupsCreateTeam_BootstrapsMembers`,
  `TestGroupsCreateTeam_PerMemberCwdOverride`,
  `TestGroupsCreateTeam_BadSpecAbortsBeforeCreate`. Persistent-
  template Phase B left for later (see TODO/high-prio/group-
  lifecycle.md).
- **Clone rate limiting** (commit fc2f9cc). Schema v19
  `agent_clone_history` table; `db.ClaimCloneSlot` does atomic
  INSERT-WHERE-NOT-EXISTS; `runCloneOrchestration` returns 429
  rate_limited if the same source was cloned within
  `agentd.CloneCooldown` (default 1m). Per-source-conv. Failed
  attempts don't extend the timer.
- **Real CLI sim tests for spawn/--join-group cwd** (commit ec04b74).
  Replaces stub-fake with testharness-v2 sim test under
  `pkg/claude/agentd/spawn_cli_flow_test.go`. Bridges
  `agent.DaemonRequestImpl` into the real daemon mux via httptest;
  CCSim + TmuxSim are the only fakes. Hoisted `SpawnResponse` to a
  named type so real types flow through. Exported `RunSpawn`,
  `RunJoinGroup`, `SpawnParams`.
- **Spawn cwd defaulting** (commit d7b13e6). `tclaude agent spawn`
  and `tclaude --join-group` (and `tclaude session new --join-group`)
  capture `os.Getwd()` CLI-side when `-C` / `--cwd` is omitted.
  Explicit `-C` overrides. Help text updated; daemon endpoint
  unchanged.
- **CLAUDE.md testing section + e2e harness shipped** (commit ac14c8b).
- **rewire-based flow harness + v2 behavior-accurate simulators**
  (PR #49, commit b3f131d). `pkg/testharness/` with `CCSim` +
  `TmuxSim` + Given/When/Then DSL. Boundary mocking via plain Go
  interfaces (`clcommon.Tmux`, `agentd.Spawner`); no toolchain
  dependency. Four pinned scenarios:
  `TestSpawn_RenamesAndResumes`,
  `TestReincarnate_OfRN_ProducesRNplus1`,
  `TestClone_EmptyAlias_DerivesFromOriginalTitle`,
  `TestDelete_PurgesAllReferencingRows`.
- **Adjust review prefix and make it configurable** (commit 17bfd31).
- **conv ls -w supports sort + search on GROUPS column**
  (commit 5887920).
- **`tclaude agent delete <selector>` for orphan cleanup**
  (commit e8e4215).
- **Spawn injects `/rename` + welcome to materialise .jsonl**
  (commit bc7ec81).
- **Clone fixes — alias fallback + post-spawn `/rename`**
  (commit d0cb0e1).
- **`-r-N` / `-c-N` short suffix scheme** (commit a5dafb3) — uniform
  monotonic N for reincarnate (`<base>-r-<N>`) and clone
  (`<base>-c-<N>`). Strip regex recognises BOTH new short form AND
  legacy `-reincarnate-<N>` / `-clone-<N>` for clean transition.
- **Multi-recipient send `--cc`** (commit eb9f977). Schema v18
  `agent_messages.{to_recipients,cc_recipients}` (commit 9c61e60).
  Pre-flight resolve rejects whole send if any CC selector is bad.
- **`conv archive` / `conv unarchive`** (commit 8c492d7). Schema v17
  `conv_index.archived_at` column (commit 17e8022).
- **Rename "expired" → "archived" terminology** (commit 7cf609b).
  Unifies with `groups archive`.
- **Groups archive / unarchive (schema v16)** (commit 4561453) —
  soft-delete; mutating ops refused with 409; default-hidden in
  listings.
- **`agent.stop` / `agent.resume` slugs + CLI** (commit 63bacad) —
  single-conv lifecycle, `agent.stop` / `agent.resume`. Routes
  through same `stopOneConv` / `resumeOneConv` helpers as bulk.
- **`conv ls -w`: `e` (later remapped to `x`) toggles archived rows**
  (commit 89fe475).
- **`conv ls`: default-hide `-x` rows** (commit 8b01e05) +
  `--show-archived` opt-in.
- **Numeric suffix in base names doesn't collide with `-r-N` /
  `-c-N`** (commit 0d19f2b).

## Agent self-lifecycle

- `tclaude agent compact [follow-up]` — daemon injects `/compact`
  into caller's pane; optional follow-up queues as next prompt.
  Slug `self.compact`.
- `tclaude agent reincarnate [follow-up]` — full identity
  migration; old pane gets `/exit` + `-x` archived suffix; new
  pane gets follow-up via `agent_messages` + flush. Slug
  `self.reincarnate`. Auto-detach-and-reattach via
  `tmux switch-client`.
- `tclaude agent clone [follow-up] [--no-copy-conv] [--target]` —
  identity copied (not migrated); conv jsonl copy default; original
  keeps running. Slug `self.clone`. Cross-agent: `agent.clone`.
- `tclaude agent context-info` — reads `sessions.context_pct` +
  `compact_pending`. No slug (read-only).
- New `agent-lifecycle` skill with thresholds (~50% on 1M context,
  ~75% on 200k) and the "keep a navigable index, don't reload
  massive context after compact" pattern.

## PR #47 — v1 agent coordination + agentd

- `tclaude agent` CLI: `whoami`, `lookup`, `ls`, `message`,
  `groups create|rm|ls|members|add|remove`, `inbox ls|read`,
  `reply`.
- DB schema v8: `agent_groups`, `agent_group_members`,
  `agent_messages`.
- Tmux send-keys nudge when target online; queued otherwise.
- Group-shared enforcement — peers must share a group to message
  (later relaxed: replying to a received message bypasses the
  shared-group requirement).
- Mutating-groups gate — refuses if a `claude` / `node` ancestor is
  found.
- `tclaude agentd serve` — Unix-socket HTTP, peer-cred identity.
- CLI requires daemon (no DB fallback).
- Skills bundled under `pkg/claude/agent/skills/`; installable via
  `tclaude setup --install-agent-skills`.

## Polish (post-#47)

- `pkg/claude/common/table` rendering across agent list views.
- `groups ls` MEMBERS + ONLINE columns.
- Groups column on `conv ls` / `conv ls -w`.
- ONLINE indicator on `agent ls` and `groups members`.
- `groups update-member` (alias/role/descr in place).
- Self-rename: `tclaude agent rename "<title>"`, slug `self.rename`,
  `requirePermission()` framework with config defaults + overrides.
- Group lifecycle Phase A: `groups stop` (soft `/exit`, `--force`
  kill-session) / `groups resume` (spawn detached
  `tclaude session new -r <conv> -d --global`). Slugs
  `groups.stop` / `groups.resume`.
- Browser dashboard v1 (read-only): Groups / Agents / Permissions /
  Slugs tabs, polls `/api/snapshot` every 5s, opens via
  `tclaude agent dashboard`.
- Multicast: `tclaude agent message group:<name> "..."` fan-out.
- User-facing docs: `docs/agent.md` + navbar entry.
- Permissions CLI + storage split: `tclaude agent permissions
  ls|grant|revoke|slugs`. Defaults in config.json; per-agent grants
  in SQLite (`agent_permissions`, schema v9). Effective set =
  `union(defaults, grants)`.
- Agent state on dashboard (idle/working/awaiting/exited)
  mirroring `session/list.go` colours; `<details>` open state
  persisted in localStorage.
- Shell autocompletions across `tclaude agent(d)`.
- System tray icon v1 (`fyne.io/systray`). Menu: Open dashboard,
  Reinstall agent skills, Open config.json, copy-paste rows for
  socket + popup URL, Quit. `--no-tray` opt-out.
- `tclaude agent inbox sent` (outbox view).
- `--state=online|offline` filter on `agent ls` and `agent groups
  ls`.
- `tclaude agent inbox prune --older-than <dur> [--read-only]`.
- Message threading (schema v10). `agent_messages.parent_id`
  auto-set by `reply`.
- Flush-on-online: identity middleware kicks a debounced background
  flush of any `delivered_at = ''` rows. Race-free via
  `ClaimAgentMessageDelivery`.
- `tclaude agent spawn <group>`: fresh CC session + auto-join.
  Slug `groups.spawn`.
- Lookup fallback to `agent_group_members` for fresh-spawned
  convs.
- Group owners (schema v11). `agent_group_owners` table; owners can
  message a group's members and multicast without being members.
  CLI: `groups owners`, `grant-owner`, `revoke-owner`. Slug
  `groups.own`. Auto-own-on-create.

## 2026-05 (later)

- `tclaude --join-group <group>` — top-level + `session new` flag
  via `groups.spawn` endpoint. Optional `--alias` / `--role` /
  `--descr`. Tab-completion.
- Dashboard "(unknown)" fix for fresh-spawned convs:
  `agent.FreshConvRowResolved(convID)` falls back to a session-row
  cwd lookup when the conv has never been indexed.
- Dashboard Cron tab. New "Cron" entry on the nav, table view of
  every `agent_cron_jobs` row.
- Conv-succession chain (schema v15). `agent_conv_succession`
  records `old_conv_id → new_conv_id` every time a conv is
  replaced. Reincarnate eagerly rewrites
  `agent_cron_jobs.{owner_conv,target_conv}` from old → new via
  `db.MigrateCronJobConvRef`. `db.ResolveLatestConv(id)` walks the
  chain forward.
