---
name: process-runs
description: >-
  Start, watch and steer daemon-owned process runs via `tclaude process`:
  run (start a saved template, scoped to a group or agent), runs ls, show,
  events, pause/resume, decide, assign, resolve-blocked, reconcile, reissue,
  record-outcome; report a node you were dispatched; and read event triggers
  via `tclaude agent triggers ls|show|explain`. Covers the
  process.runs.read/start/manage and self.process permissions (group ownership
  confers read and group- or member-scoped start, never manage). Use when asked to run, kick off,
  monitor, unblock, approve or cancel a process/workflow, when a process node
  arrives in your inbox, or to see which trigger rule would fire on a PR/CI or
  agent-state event. Not for authoring or editing templates (that is
  process-templates), and not for `tclaude agent process`, a group's advisory
  phase checklist.
---

# Process runs

A run is a daemon-owned execution of a saved process template. Starting one
pins the exact template head and parameters; later template edits never
change a run in flight. Authoring templates is the process-templates skill.

## Permissions

| Slug | Covers | How you get it |
|---|---|---|
| `process.runs.read` | `runs ls`, `show`, `events`, `reconcile` | group owners hold it; otherwise a grant |
| `groups.resources.read` | `runs ls --group <g>`, `show`, `events` of runs scoped to your group | membership, live; no mutation |
| `process.runs.manage` | every mutation: unscoped `run`, `pause`, `resume`, `decide`, `assign`, `resolve-blocked`, `reissue`, `record-outcome`; and any `run` with `--authorize-program-profile` | explicit grant only (can be scoped to a template), never via ownership |
| `process.runs.start` | `run --group <g>` | group owners hold it for their groups |
| `self.process` | `run --agent <self>` | default-granted |
| `agent.process` / `groups.members.process` | `run --agent <other>` | grant / owners hold it for their members |

Membership lets you read your group's runs, never steer them: every mutation
above stays on `process.runs.manage`, for members too.

`report` needs no slug: it is authenticated by your identity and must match
the attempt dispatched to you. `rescope` is human-only. When a slug is
missing, pass `--ask-human 30s` for one-shot approval instead of retrying; an
explicit deny overrides everything.

## Start and watch

```bash
tclaude process templates ls
tclaude process run release --param version=1.4.0 --group webshop
tclaude process run lint-fix --agent <self>             # bound to you
tclaude process run deploy --authorize-program-profile ci   # each program profile, explicitly
tclaude process runs ls [--group <g>] [--agent <a>] [--performer <a>] [--limit 32] [--after <run-id>]
tclaude process show <run-id> [--json]                  # state, scope, owner, what needs a human
tclaude process events <run-id> [--after <seq>] [--json-lines]   # ordered evidence trail
```

Scope binds a run for life: `--group` (by id, survives renames), `--agent`
(stable agent id, survives reincarnate), or neither (unscoped: only in
`runs ls`, not in the dashboard). If the group or agent goes away the run
pauses as `orphaned` until the human rescopes it.

## Steer

```bash
tclaude process pause <run-id>          # nothing new dispatched; in-flight work finishes
tclaude process resume <run-id>
tclaude process decide <run-id> --node <node> --verdict <outcome> [--evidence "why"] [--attempt N]
tclaude process assign <run-id> --node <node> --agent <agent>   # hand an unreported agent attempt over
tclaude process resolve-blocked <run-id> --node <node> --attempt <n> --action retry|skip|cancel [--note "why"]
```

Copy `--node` and `--attempt` from `show`; stale or wrong-attempt input is
refused. An agent decider's decision cannot be made with `decide`.

**After a daemon crash** an outstanding program command shows as
`needs_reconcile`; tclaude never re-runs it silently:

```bash
tclaude process reconcile <run-id>                          # what needs it
tclaude process reissue <run-id> [--node <node>]            # run it again (same attempt)
tclaude process record-outcome <run-id> [--node <node>] --outcome succeeded|failed [--exit-code N] [--note ...]
```

## When you are the performer

A node assigned to you arrives as an inbox message with the run id, node,
attempt and exact report command. Do the work, then report exactly once:

```bash
tclaude process report succeeded --run <run-id> --node <node> --attempt <n> --note "PR #12 is open"
tclaude process report failed --run <run-id> --node <node> --attempt <n> --note "why"
tclaude process report succeeded ... --verdict changes --note "reasoning"   # when asked to decide
```

`--run/--node/--attempt` may be omitted when you have exactly one open node.

## Triggers

Trigger rules fire on facts rather than a clock: `pr.opened`, `pr.updated`,
`pr.merged`, `ci.failed`, `ci.succeeded`, and the dwell facts `agent.idle` /
`agent.awaiting_input`. Their actions spawn a worker or send a message (they
do not start process runs). The CLI is read-only; rules are authored in the
dashboard or REST API.

```bash
tclaude agent triggers ls
tclaude agent triggers show <name-or-id>      # rule + recent firing ledger
tclaude agent triggers explain --source pr.opened --pr-url <url> --group <g>   # dry run, fires nothing
tclaude agent triggers explain --source agent.idle --agent-id <agent-id>
```

Reading needs `triggers.read` for global rules or `groups.triggers.read` for
a group's rules (group owners hold the latter).

## Example: run a review process for your group and unblock it

```bash
tclaude process run review --group webshop --param pr=42
tclaude process show run-7f3a              # node "fix" blocked at attempt 3
tclaude process events run-7f3a --after 10
tclaude process resolve-blocked run-7f3a --node fix --attempt 3 --action retry --note "flake fixed"
```

User docs: `docs/processes.md` (running, scope, performers, reconciliation)
and `docs/teams-at-scale.md` (triggers and standing orders).
