# Per-group reincarnate thresholds (UI-configurable)

Per-group context-window thresholds that nudge — or eventually
auto-trigger — reincarnation when an agent gets close. Different
groups want different limits: a long-running PO/coordinator might
sit happy at 80%, a tight worker that needs sharp output should
reincarnate at 40%.

Builds on top of [`context-nudges.md`](context-nudges.md) — the
opt-in fixed-step (30/40/.../90%) nudge feature. This file is the
**per-group, UI-configurable, optionally-auto-acting** evolution.

## Problem

`tclaude agent context-info` already exposes `context_pct`. Today
the agent has to self-check + decide; nothing nudges them at the
right moment. Even with `context-nudges.md` shipped, the threshold
is global — but in practice different roles want different limits.

A "tech-lead" agent reviewing PRs in 200-line chunks can keep
working at 70% context. A "worker" agent doing fresh code edits
should hit fresh context at 40-50% to keep output sharp. One global
threshold can't serve both.

## Configuration model

**Per-group, configurable from the dashboard.** Schema sketch:

- New table or column: `agent_groups.reincarnate_policy` (TEXT-as-
  JSON or dedicated columns), holding:
  - `nudge_pct` (int, default off / null) — first threshold to fire
    a nudge. Single value, not a step interval — keep simple for v1.
  - `auto_reincarnate_pct` (int, default off / null) — if set AND
    > nudge_pct, daemon initiates reincarnate when the agent crosses
    this. Off by default (loud, blast-radius high).
  - `auto_reincarnate_grace_sec` (int, default 300) — how long the
    agent has to stash work before the daemon actually pulls the
    trigger. Nudge fires at threshold, auto-trigger fires N seconds
    later.

Per-conv override stays available via the existing
`agent.permission_overrides[conv]` pattern (or a parallel
`reincarnate_overrides` map) so a single agent in a group can opt
out / customise.

## Nudge copy

When the threshold crosses, the agent receives a `[system: ...]`
line (transport per `context-nudges.md` — back-channel preferred,
agent_messages with a marker as fallback):

> Heads up: you're at ~52% context (group `<name>` nudges at 50%).
> To avoid context rot, consider reincarnating when you reach a
> natural breakpoint. Before reincarnating: persist your in-flight
> work to disk (notes file / TODO doc / partial results) so the
> successor can resume cleanly. Use `tclaude agent reincarnate
> [follow-up]` when ready, or ignore if your task is short.

When auto-reincarnate is set and grace expires:

> Auto-reincarnate triggered at ~62% context (group `<name>`
> auto-trigger at 60%). The daemon will reincarnate this agent in
> 60s. Persist any in-flight work to disk NOW. To cancel, set
> `auto_reincarnate_pct = null` on this group via the dashboard
> or `tclaude agent groups update <name> --auto-reincarnate-pct
> off`.

## Dashboard UI

In the Groups tab, each group's settings/expanded view gets a
**"Reincarnate policy"** section:

- **Nudge at:** slider 0-100% (off / 10%-90% in 5% steps).
  Default off.
- **Auto-reincarnate at:** slider 0-100%, must be >= nudge or off.
  Default off. Strong warning copy ("destructive — agent loses
  current process state, only the conv jsonl survives").
- **Grace period before auto-reincarnate:** preset chips (30s /
  2m / 5m / 15m / off). Default 5m.
- Shows a preview row per current group member: "alias — context
  N% (nudge / auto / nothing pending)".

Also surface in the Agents tab per-agent: which group's policy
governs their nudges (if multiple groups overlap), what the
effective threshold is, time since last nudge.

## CLI

`tclaude agent groups update <name> --nudge-pct 50
--auto-reincarnate-pct 60 --grace 5m`. Mutates same column.

`tclaude agent groups show <name>` (today's `groups members` plus
a `policy:` block) renders the config.

## Daemon behaviour

A periodic checker (already needed for `context-nudges.md`) walks
every online agent's `sessions.context_pct` against the effective
policy:

- Effective policy = per-conv override (if any) ELSE primary
  group's policy. Primary group could be: only group, or first
  group sorted by name, or one explicitly tagged
  `agent_group_members.policy_governs = TRUE`. Open question; lean
  toward "first group sorted alphabetically" for v1 simplicity.
- For each crossing: fire nudge (once per (conv, threshold) — record
  in a small dedupe table so a fluctuating context doesn't spam).
- For each crossing of auto-trigger threshold: schedule a
  reincarnate via the existing cron path with the configured grace.
  Auto-cancel if context drops back below threshold within grace
  (e.g. /compact happened).

## Manager-pattern tie-in

A group owner can already call `tclaude agent reincarnate --target
<peer>`. The auto-reincarnate path is the daemon doing the same
thing on the human's behalf via the policy. Audit
`granted_by = system:reincarnate:auto-policy:group=<name>` so
"who killed my agent" forensics distinguish auto-policy from
human / manager-agent calls.

## Test coverage

Per project convention — flow tests under
`pkg/claude/agentd/*_flow_test.go`:

- Crossing the nudge threshold fires exactly one nudge per
  (conv, threshold).
- Crossing the auto-reincarnate threshold schedules a reincarnate
  after the grace period.
- A `/compact` (or context-drop) during the grace cancels the
  auto-trigger.
- Per-conv override beats group policy.
- Multi-group membership picks deterministic policy.

## Out of scope (for later)

- **Per-role policies** (e.g. "all `role=worker` get nudge at
  40%"). Useful but design-heavy; do once role becomes a first-
  class type rather than a free-form string.
- **Token-budget-based thresholds** instead of percent (e.g.
  "nudge when remaining < 80k tokens"). Same idea, different
  axis.
- **Auto-compact instead of auto-reincarnate** as a softer
  alternative. Could be a third tier of the policy: `auto_compact_pct`
  fires `/compact` (which preserves conv-id) at a lower threshold,
  `auto_reincarnate_pct` fires reincarnate at a higher one. Useful
  but adds policy surface area; ship the binary nudge/auto first.

## Files (when implementing)

- `pkg/claude/common/db/agent_groups.go` — schema migration for
  policy columns
- `pkg/claude/agentd/policy.go` (new) — periodic checker + dedupe
  table
- `pkg/claude/agentd/dashboard.html` — Groups tab policy UI
- `pkg/claude/agent/groups.go` — CLI flags on `groups update`

## Cross-references

- [`context-nudges.md`](context-nudges.md) — the simpler
  global-threshold version this builds on. Probably ship that
  first; this file extends it with per-group config + UI + auto-
  trigger.
- [`agent-self-lifecycle.md` (DONE)](../../DONE/agent-self-lifecycle.md)
  — reincarnate orchestration this leans on.
- [`cross-agent-manager-pattern.md` (DONE)](../../DONE/cross-agent-manager-pattern.md)
  — how the daemon already calls reincarnate against another conv.
