# Processes

Processes are reusable, versioned workflow templates — task chains, decisions,
parallel fan-out, retries, human approvals — authored once and executed as
daemon-owned runs with durable state and an auditable evidence trail. The
feature is experimental and off by default: enable it with the
`features.processes` config flag (in the [dashboard's](dashboard.md) Config tab
or in config.json). While the flag is off, every process route in the daemon
returns 404 with the code `processes_disabled` and a message pointing at the
flag.

The runtime is partially implemented: a substantial subset of the engine is
live, and the rest of the CLI surface is stubbed. This page states exactly
which half is which.

!!! note "Not `tclaude agent process`"
    `tclaude agent process` shows and advances a *group's* advisory phase list
    from a task-force template — a checklist for coordination, never enforced.
    That is a different feature; see [Teams at scale](teams-at-scale.md). This
    page is about `tclaude process`: executable templates run by the daemon.

## Authoring templates

Templates are YAML documents with `apiVersion: tclaude.dev/v1alpha1` and
`kind: ProcessTemplate`. The primary authoring surface is the dashboard's
Templates library and drag-and-drop editor, which covers parameters,
performers, edges, layout, snippets, version history, authorship, and
source-hash CAS checks. Agents author the same templates from the CLI:

```bash
tclaude agent process-templates ls [--json]
tclaude agent process-templates show <template-id>
tclaude agent process-templates validate --file template.yaml
tclaude agent process-templates save --file template.yaml \
    --expect-source-hash <hash>
```

Saves are compare-and-swap: `--expect-source-hash` must match the current
stored version, so two writers cannot silently clobber each other — a stale
hash is a refused save, and the caller re-reads with `show` and merges. The
bundled `process-templates` agent skill and the CLI's `--help` are the deep
reference for the authoring shape and the safe show-edit-validate-save loop;
this page does not repeat them.

Two permissions govern authoring, and neither executes anything:

- `process.templates.read` — list, show, validate. Installed as an ordinary
  agent default by `tclaude setup --install-default-agent-permissions`.
- `process.templates.manage` — save. Never a default.

Two worked examples live in the repo under `docs/examples/`
(`code-change-with-review.yaml`, `parallel-any-review.yaml`). They are valid
authoring-shape templates and pass template validation, but note that they use
agent and human performers, `approvalRetry`, and same-session retry feedback —
features the current engine does not execute yet (see below) — so run creation
refuses them today. Read them as illustrations of the full authoring format,
not as templates you can run unmodified.

## Running

Runs are created and driven through the daemon:

![A running process in the dashboard: a decision fanning out to parallel tasks that join before the outcome is recorded](assets/process-run-graph.png)

*A running process in the dashboard: a decision fanning out to parallel tasks that join before the outcome is recorded*

```bash
tclaude process run <template-id> --param key=value \
    --authorize-program-profile <profile>
tclaude process runs ls
tclaude process show <run-id>
tclaude process events <run-id>
tclaude process resume <run-id>
tclaude process decide <run-id> --node <node> --verdict <verdict> [--attempt N]
tclaude process reconcile <run-id>
tclaude process reissue <run-id> [--node <node>]
tclaude process record-outcome <run-id> [--node <node>] \
    --outcome succeeded|failed
tclaude process resolve-blocked <run-id> --node <node> --attempt <n> \
    --action retry|skip|cancel [--note "why"]
tclaude process report succeeded|failed [--run <run-id> --node <node> \
    --attempt <n>] [--note "summary"]      # agent performers only
tclaude process templates ls
```

Creating a run pins the exact saved template head and its parameters; later
edits to the template never change a run already in flight. Every program
profile a run may execute must be authorized explicitly at creation with
`--authorize-program-profile` — nothing is authorized implicitly, and that
includes every derived stage of a compound task, not just the parent's
performer.

Permissions: reads (`runs ls`, `show`, `events`) require `process.runs.read`;
every mutating verb requires `process.runs.manage`. Neither is an ordinary
default, but owning a group confers `process.runs.read`, so a coordinating
agent can watch run status and evidence without a human approval per read.
Ownership confers reads only — mutations always need an explicit
`process.runs.manage` grant or a one-shot `--ask-human` approval — and an
explicit per-agent deny overrides everything, the owner grant included. See
[Permissions and audit](permissions-and-audit.md).

## What executes today

- **Sequential task chains** starting at any executable node kind — top-level
  `start` names the entry node, and a separate `start`-typed node is optional.
- **Exclusive decisions** with human deciders: the verdict selects exactly one
  authored outcome edge and closes the alternatives. Authored decisions open
  exactly once and need no `--attempt`.
- **Parallel fan-out** (`type: parallel`) reduced by `join: all` or
  `join: any`. Branches execute concurrently, bounded to four external
  programs at a time; ready branches past the bound wait for a slot. A branch
  parked on a human decision does not block its siblings.
- **`join: any` races its branches.** The first arrival wins and alone
  activates the reducer and everything downstream. Losing branches are not
  cancelled — they run to their own settled outcome and are recorded as
  evidence that cannot replace the winner or run the downstream route twice,
  and the run reports no terminal status until every dispatched branch has
  settled. A losing branch that *fails* still fails the run, exactly as under
  `join: all`.
- **Program performers** (`performer.kind: program`): executed as argv without
  a shell, with a bounded environment and bounded output.
- **Bounded program retries.** `retry.maxAttempts: <n>` includes the first
  attempt and is capped at 100; attempts run back to back, and nothing can
  interrupt a task still spending its budget. A failed attempt inside the
  budget re-readies only that node; parallel siblings are untouched. A task
  with no `retry` policy stays fail-fast. Every attempt carries its own
  attempt number and command id, so a delayed report from an earlier attempt
  is refused as stale rather than credited to the current one. Attempt numbers
  are visible in `show --json` and in the `program_prepared` /
  `program_observed` rows of `events`.
- **Blocked-branch parking.** When a retry-authored task exhausts its budget,
  the branch is parked, not failed: the node becomes `blocked`, the run stays
  running, and siblings keep executing. (`retry.maxAttempts: 1` and no policy
  at all therefore behave differently on purpose.) `show` lists each parked
  branch with the exact blocked attempt and the failure reason; an operator
  resolves it with `resolve-blocked`. `retry` opens one more authored-size
  attempt window (attempt numbers keep counting up); `skip` settles the task
  through its authored route as a success would, distinguished only by the
  `blocked_resolved` evidence; `cancel` gives up on the whole run — parked
  branches and awaited decisions are dropped, already-dispatched programs
  drain honestly, and the run finishes `canceled`. The node and attempt must
  match exactly, so a stale or wrong-branch resolution is refused. A parked
  branch keeps the run open: an end node will not complete around a resolution
  still on offer.
- **Compound tasks.** A task declaring `plan`, `checks`, or `review` runs its
  stages in order — `<id>.plan`, `<id>.do`, `<id>.test.<step>`, `<id>.review`,
  and the engine-owned `<id>.done`. Each stage is an ordinary node under its
  derived id: dispatched, observed, reconciled, and shown like a plain program
  task. Stages are derived from the pinned template on every load, never
  stored, so nothing can drift. The parent stays `running` while any stage is
  live and settles its single authored route exactly once, when `done`
  completes; a failed stage fails the run like any fail-fast task. In this
  slice every stage performer must be a program, and stage-level `retry` /
  `approvalRetry` are not executable yet.
- **Human plan approval.** A compound whose `plan` declares `approval: human`
  gains one extra stage between plan and do, `<id>.plan.approval`: an awaited
  decision with the fixed verdicts `approve` and `rework`, decided through the
  ordinary `decide` command and permission. `approve` readies the do stage;
  `rework` returns the plan stage to ready, and the window reopens when the
  re-run plan succeeds. There is no approval budget — each rework is one
  explicit, audited human action buying exactly one more plan execution.
  Because the window can reopen, a verdict must name it:

  ```bash
  tclaude process decide <run-id> --node <id>.plan.approval \
      --attempt N --verdict approve|rework
  ```

  `show` reports the exact `--attempt` in its next-step hint, and the run
  refuses any other value — including a verdict formed against an earlier
  window and submitted late.

- **Subprocess nodes** (`type: subprocess`) run a nested graph as one node
  of their parent. The body is either inline (`subprocess.start`,
  `subprocess.nodes`, `subprocess.params`) or another stored template
  (`subprocess.templateRef`, optionally pinned with `version: sha256:<hex>`).
  The body is its own param scope: `subprocess.inputs` binds each body param
  from a literal or `{{ params.x }}` of the parent, and an unbound param falls
  back to its default. `subprocess.outputs` maps each body end node to one of
  the subprocess node's `next` outcomes; with a single outcome, unmapped ends
  use it. Body nodes run under namespaced ids (`<id>/<inner id>`), so a body
  decision is decided with `decide --node review/gate`. The subprocess node
  stays `running` while its body is live and settles its own route exactly
  once, when a body end node is reached; on an untaken branch the whole body
  is skipped with it. Referenced templates are resolved when the run is
  created and embedded in the run's snapshot, so later edits to them never
  change a run in flight; reference cycles and nesting deeper than four levels
  are refused. Inner program profiles need authorizing at run creation like
  any other.
- **Agent performers** (`performer.kind: agent` on a plain task) — see
  [Agent performers](#agent-performers) below.

One sizing rule applies only to running: a run records durable evidence per
node, and that row bounds node ids at 256 bytes. Run creation refuses a
template whose node ids — authored, derived from compound stages, or
namespaced into a subprocess body — exceed it; editing and storing such a template still works.

## What does not execute yet

These CLI verbs are stubs that return a "process runtime is temporarily
unavailable: no engine is installed" error: `preview`, `apply`, `worklist`,
`advance`, `unblock`, `observe`, `resolve`, `verify`, `repair`.

These authoring features are valid in a stored template but refused at run
creation with a path-specific diagnostic, rather than failing later: agent
deciders; human task performers; agent performers on compound (staged) tasks;
agent performer `timeout`; retry backoff waits; same-session retry feedback
for program tasks (`retry.onFail: feedback-same-session` is executable only
for agent tasks); retries on compound stages; `plan.approvalRetry`; human or
agent stage performers; poison handling; wait nodes; captures.

## Agent performers

A plain task with `performer.kind: agent` is performed by an agent. When the
node becomes ready the daemon resolves a performer from the run's scope and
delivers the node's prompt **as a message in that agent's inbox** — never by
typing into its pane. The message carries the run id, node id, and attempt,
and the exact command to report back with:

```bash
tclaude process report succeeded --run <run-id> --node <node> --attempt <n> \
    --note "PR #12 is open"
tclaude process report failed --run <run-id> --node <node> --attempt <n> \
    --note "why it failed"
```

`--run`, `--node`, and `--attempt` may be omitted when the agent has exactly
one open node. The report is authenticated by the calling agent's identity and
must match the exact attempt that was delivered to it: another agent is
refused, a report for an older attempt is refused as stale, and a repeated
identical report is acknowledged without being applied twice. A report goes
through the same transition a program result does, so retry budgets, parking,
and fail-fast behave identically.

**Who performs the node**, by run scope:

| Scope | Resolution |
| --- | --- |
| group | the explicit `assignee`, if it names a current member; else a member whose role matches the node's `profile` (or that spawn profile's role), idle first, then busy, then offline; else — only with `spawn: allowed` and when the run owner could spawn into the group — a fresh agent spawned with the node's `profile`; else the node waits as **needs performer** |
| agent | the bound agent performs every agent node; an `assignee` naming anyone else waits as needs performer |
| none | only an explicit `assignee` (any active agent); otherwise the node waits as needs performer |

A **needs performer** node is retried on every runtime sweep (once a minute),
so it is picked up as soon as a matching member joins. `show` prints the
reason it waits.

**Retries.** `retry.onFail: fresh-attempt` (the default) hands the next
attempt to a different performer when one is available — spawning one if the
template allows it — and only falls back to the previous performer as a last
resort. `retry.onFail: feedback-same-session` hands it back to the same
performer, together with the note its failed report carried.

**Performers going away.** An offline performer keeps the node: its inbox holds
the prompt and the node simply waits. A performer that is retired or leaves
the group is replaced by re-resolving the same attempt. A run bound to an
agent is never re-resolved to someone else.

**People always win.** `record-outcome --node <node>` settles an agent's node
in its place; the performer is told the attempt no longer needs it, and its
later report is refused as stale. When a run is ending — a sibling failed, or
an operator cancelled — outstanding agent attempts are withdrawn and their
performers told to stop.

`show --json` lists every attempt's performer under `performers` (agent,
display name, live status, how it was chosen, and what it reported); outstanding
agent commands report their dispatch state (`pending`, `needs_performer`,
`delivered`, `reported`) instead of `needs_reconcile`. A daemon restart picks
in-flight dispatches back up without re-sending them. An outstanding agent
command occupies one of the run's four concurrency slots, like a program.

## Crashes and reconciliation

A command is durable before its program starts, so after a daemon restart
tclaude cannot know whether an outstanding command's program actually ran. It
never guesses and never silently re-runs one. `show` reports each outstanding
command as `needs_reconcile`, and an operator resolves it explicitly:
`record-outcome` ("I checked out of band; here is what happened") or `reissue`
("run it again"). When a run holds more than one such command, both verbs
require `--node`.

A restart preserves each node's exact attempt number. `reissue` re-runs the
same durable attempt rather than spending another from the retry budget;
`record-outcome --outcome failed` spends it, and the next attempt starts if
the budget allows. A failed branch fails the run, but the run is only reported
`failed` (or `canceled`, after a cancel resolution) once every in-flight
command has been accounted for.

Parked branches survive restarts unchanged — the checkpoint carries the
obligation and the blocked attempt. If another branch fails or the run is
cancelled while a branch is parked, the parked obligations are dropped: the
run is over, so it stops offering a resolution nothing could act on.

## Storage

Run state lives in SQLite under `~/.tclaude/data/`; the checkpoint there is
authoritative, and `tclaude process events` renders the ordered human-facing
evidence per run. On startup the daemon removes obsolete filesystem run data
and legacy lock files while preserving the complete authoring root — template
versions, heads, layouts, snippets, authorship records, and template locks.
