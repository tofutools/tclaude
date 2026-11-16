---
name: process-runs
description: >-
  Start, watch and steer daemon-owned process runs via `tclaude process`:
  run (start a saved template, scoped to a group or agent), runs ls, show,
  events, pause/resume, decide, assign, resolve-blocked, block, cancel, send-back, reconcile, reissue,
  record-outcome; oversee a run and raise or answer its concerns (concern,
  answer-concern, concerns, oversee); report a node you were dispatched; and read event triggers
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
| `process.runs.manage` | every mutation: unscoped `run`, `pause`, `resume`, `decide`, `assign`, `resolve-blocked`, `block`, `cancel`, `send-back`, `reissue`, `record-outcome`; and any `run` with `--authorize-program-profile` | explicit grant only (can be scoped to a template), never via ownership |
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

A group with no process yet can start from a built-in way of working
(`plan-vet-build`, `build-review-ship`, `bug-fix`, `quick-change`):
`tclaude process templates builtin ls`, then
`tclaude process templates builtin use <id> --group <group>` copies it into the
group (needs `process.templates.manage`) and prints the copy's id to run.

```bash
tclaude process templates ls
tclaude process run release --param version=1.4.0 --group webshop
tclaude process run lint-fix --agent <self>             # bound to you
tclaude process run deploy --authorize-program-profile ci   # each program profile, explicitly
tclaude process run review --task tk_…                  # for a board task: its group's run; the run's end closes the task
tclaude process runs ls [--group <g>] [--agent <a>] [--performer <a>] [--limit 32] [--after <run-id>]
tclaude process show <run-id> [--json]                  # state, scope, owner, what needs a human
tclaude process events <run-id> [--after <seq>] [--json-lines]   # ordered evidence trail
```

A run for a board task (`--task`) gets the task's text as `{{params.task}}`
and its id as `{{params.task_ref}}`; one task has one live run at a time;
`runs ls --task tk_…` lists its runs. Completed closes the task (done),
failed blocks it, canceled leaves it as it is. See the taskboard skill's
"A task that follows a process" for `taskboard add --process`.

Scope binds a run for life: `--group` (by id, survives renames), `--agent`
(stable agent id, survives reincarnate), or neither (unscoped: only in
`runs ls`, not in the dashboard). If the group or agent goes away the run
pauses as `orphaned` until the human rescopes it.

## Steer

```bash
tclaude process pause <run-id>          # nothing new dispatched; in-flight work finishes
tclaude process resume <run-id>
tclaude process decide <run-id> --node <node> --verdict <outcome> [--evidence "why"] [--attempt N]
#   at an agreement gate a person votes as --node <gate>.vote.<k>; `show` prints the gate's tally
tclaude process assign <run-id> --node <node> --agent <agent>   # hand an unreported agent attempt over
tclaude process resolve-blocked <run-id> --node <node> --attempt <n> --action retry|skip|cancel [--note "why"]
tclaude process block <run-id> --node <node> --reason "why"   # park an agent step that is out (overseer too)
tclaude process cancel <run-id> [--note "why"]                # give the whole run up
tclaude process send-back <run-id> --node <step> [--note "why"]  # run a finished step again; everything after it resets
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

## Oversight: concerns and escalation

Every run has an **overseer**: the agent set with `run --overseer` or
`oversee`, else the template's `overseer:` (`starter`, `operator`,
`role:<role>`), else the group's lead (role `lead`, else an owner), else
whoever started it. `show` prints it. Overseeing a run lets you, for that run only,
`pause`, `resume`, `assign` and `block` (park a step whose world moved:
its performer is released and it waits on `resolve-blocked`), and answer
its concerns, without `process.runs.manage`. It never lets you edit the
template, hand the run to another overseer, or `cancel` the run: raise a
concern for that, and the operator's card offers the cancel.

Anyone in the run (its overseer, its group's members, its performers, its
starter) raises a **concern** when something is off that the process cannot
handle. It climbs a chain until someone settles it:

    overseer → the group's escalation agent → operator

The escalation agent is a group member with the role `escalation` that holds
`process.runs.manage`. A holder that does not answer within 30 minutes is
passed over, and the run pauses until the next level decides. The engine
raises concerns too (source `engine`) when a step stays stuck past its
template's `patience:`: `blocked`, `no_performer`, or `no_report` (only with
an explicit patience). One per stuck spell; it settles itself (`cleared`)
when the step moves on. The operator
gets a decision card (Resume, Skip this step, Reassign, Keep paused, Cancel
the run, depending on what the run allows). The daemon applies the pick and
mails it to the overseer. If the pick is Reassign, the overseer does the
reassigning. A concern of yours that would go straight to the operator (you
oversee the run and the group has no escalation agent) needs `human.notify`.
Naming someone other than yourself with `run --overseer` needs
`process.runs.manage`.

```bash
tclaude process concern <run-id> "CI is red on main" [--node <step>]   # raise one
tclaude process concerns <run-id> [--all]                              # open (and settled) concerns
tclaude process answer-concern <run-id> <n> continue --note "why"       # settle: the run goes on (resumes if paused)
tclaude process answer-concern <run-id> <n> pause --note "why"          # settle by holding the run
tclaude process answer-concern <run-id> <n> escalate --note "why"       # pass it up the chain
tclaude process oversee <run-id> <agent|operator|default>               # process.runs.manage
```

A concern you hold arrives as `[process] concern #N on <run>` mail with
these commands. Raising a concern you would hold yourself sends it up the
chain. Every step (raised, passed on, settled, paused) is in
`tclaude process events <run-id>`.

## When you are the performer

A node assigned to you arrives as an inbox message with the run id, node,
attempt and exact report command. Do the work, then report exactly once:

```bash
tclaude process report succeeded --run <run-id> --node <node> --attempt <n> --note "PR #12 is open"
tclaude process report failed --run <run-id> --node <node> --attempt <n> --note "why"
tclaude process report succeeded ... --verdict changes --note "reasoning"   # when asked to decide
tclaude process report succeeded ... --output pr="PR #12" --note "done"   # when the step declares outputs
```

If you cannot do the step as asked, or the run looks wrong, do not just
fail it: raise a concern (`tclaude process concern <run-id> "why" --node <node>`)
and let the overseer decide.

A step with outputs must report each one the message names (with a single
output, the note stands in for it); a success without them is refused, so
report again with them.

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
