# Processes

Processes is an experimental feature for authoring reusable process templates
and running them through the daemon. Enable it with `features.processes` in
tclaude's config. The dashboard then shows the Templates library and
drag-and-drop editor; the daemon accepts run creation and the runtime verbs
below.

## Authoring

Template parameters, performers, edges, layout, snippets, version history,
authorship, and source-hash CAS checks are available in the dashboard editor.
Agents author the same templates through:

```bash
tclaude agent process-templates ls [--json]
tclaude agent process-templates show <template-id>
tclaude agent process-templates validate --file template.yaml
tclaude agent process-templates save --file template.yaml --expect-source-hash <hash>
```

`ls --json` emits the same bounded listing the dashboard reads as one
`{"templates": [...]}` document instead of the human table, so scripts and
agents never parse columns. It carries every field the REST response has,
including each template's full `versions` history; version `actor` and
`authoredAt` attribution is optional and omitted where absent.

Reads require `process.templates.read` — installed as an ordinary agent
default by `tclaude setup --install-default-agent-permissions`, so every agent
can list, show, and validate templates. Saves require
`process.templates.manage`, which is never a default. These permissions
authorize authoring only and do not execute a template.

## Running

```bash
tclaude process run <template-id> --param key=value --authorize-program-profile <profile>
tclaude process runs ls
tclaude process show <run-id>
tclaude process events <run-id>
tclaude process decide <run-id> --node <node> --verdict <verdict> [--attempt <n>]
tclaude process resume <run-id>
tclaude process reconcile <run-id>
tclaude process reissue <run-id> [--node <node>]
tclaude process record-outcome <run-id> [--node <node>] --outcome succeeded|failed
```

Creating a run pins the exact saved template head and its parameters, so later
edits to the template never change a run already in flight. Every program
profile a run may execute must be authorized explicitly at creation with
`--authorize-program-profile`; nothing is authorized implicitly.

Reads (`runs ls`, `show`, `events`) require `process.runs.read` and mutations
require `process.runs.manage`. Neither is an ordinary default, but **owning a
group confers `process.runs.read`**: a coordinating agent driving a validation
reads run status and evidence without a human approval per read. Plain members
need the slug granted. An explicit per-agent **deny** overrides both the
defaults and the owner grant. Ownership confers reads only — every mutating
verb above still needs `process.runs.manage` (an explicit grant or a one-shot
`--ask-human` approval), and it never grants `process.templates.manage`.

## What executes today

- Any executable node kind as the template's entry. Top-level `start` names the
  graph's single entry node; a separate `start`-typed node is optional, so a
  template may begin directly at a task, a decision, or a parallel fork.
- Sequential task chains, and exclusive decisions whose verdict selects exactly
  one authored outcome edge and closes the alternatives.
- Structured fan-out (`type: parallel`) reduced by `join: all` or `join: any`.
  Branches of one run execute concurrently, bounded to four external programs
  at a time; ready branches past that bound wait for a slot. A branch parked on
  a human decision does not block its siblings, and that decision can be
  answered while the sibling programs are still running.
- `join: any` races its branches: the first arrival wins, and only the winner
  activates the reducer and everything downstream of it. Losing branches are
  **not** cancelled, closed, or compensated — they run on to their own settled
  outcome and arrive late at the reducer, where they are recorded as honest
  evidence that cannot replace the winner or run the downstream route twice.
  The run therefore does not report a terminal status until every branch that
  was dispatched or queued has settled. A losing branch that *fails* still
  fails the run, exactly as under `join: all`.
- Program task performers (`performer.kind: program`), executed as argv without
  a shell, with a bounded environment and output.
- Human deciders on decision nodes.
- Bounded program retries. A program task may declare
  `retry.maxAttempts: <n>`, which **includes the first attempt** and is capped
  at 100 — retries here run back to back and nothing can interrupt a task that
  is still spending its budget, so an unbounded budget would be an unthrottled
  loop. (The `cancel` resolution below acts on a branch that has already
  exhausted its budget, not on one mid-loop.) A failed attempt inside
  the budget re-readies only that node and runs a fresh attempt;
  parallel siblings are untouched and are neither cancelled nor renumbered. A
  task with no `retry` stays fail-fast. Every attempt of a node carries its own
  attempt number and its own command id, so a delayed or duplicated report from
  an earlier attempt is refused as stale rather than credited to the current
  one. Attempt numbers are visible in `tclaude process show --json` (the
  checkpoint's sparse `attempts` map and each outstanding command's `attempt`)
  and in the `program_prepared` / `program_observed` rows of `tclaude process
  events`; the plain `show` table's COMMAND column carries the same attempt in
  the command id.
- Blocked branches and audited resolution. When an explicitly retry-authored
  task exhausts its budget, that branch is **parked**, not failed: the node
  becomes `blocked`, the run stays running, and unaffected siblings keep
  executing. A task with no `retry` policy still fails the run outright — so
  `retry.maxAttempts: 1` and no policy at all behave differently on purpose.
  `tclaude process show` lists each parked branch with the exact attempt it is
  blocked at and the reason from the failed attempt, and an operator resolves it
  with

  ```bash
  tclaude process resolve-blocked <run> --node <node> --attempt <n> \
      --action retry|skip|cancel [--note "why"]
  ```

  `retry` opens one more authored-size attempt window — attempt numbers keep
  going up rather than being reset or reused, so an earlier attempt's report is
  still refused as stale — and the branch parks again if that window is spent
  too. `skip` settles the task through its authored route and activates
  downstream work as a successful run would have; only the `blocked_resolved`
  evidence distinguishes the two. `cancel` gives up on the whole run: it stops
  further planning, drops every parked branch and awaited decision, lets
  already-dispatched programs drain honestly, and finishes the run `canceled`.
  The node and attempt must match exactly, so a stale, duplicated, or
  wrong-branch resolution is refused rather than applied to whatever is parked
  now. A run whose branches are all parked is left alone by the periodic
  recovery sweep, and a parked branch keeps the run open — an end node will not
  complete around a resolution that is still on offer.

- Compound tasks. A task that declares `plan`, `checks`, or `review` runs its
  stages in order: `<id>.plan`, `<id>.do`, `<id>.test.<step>`, `<id>.review`,
  and finally the engine-owned `<id>.done`. The stages are ordinary nodes —
  each is dispatched, observed, reconciled, and shown exactly like a plain
  program task, under the derived id — so nothing about a compound is a special
  case for an operator. They are derived once from the pinned template when the
  run is created, and re-derived identically on every load; no expansion is
  stored, so there is nothing that can drift from the template.

  The parent stays `running` for as long as any stage is live and settles its
  single authored route exactly once, when its `done` stage completes, so
  downstream work never starts early. A failed stage fails the run the same way
  a plain fail-fast task does; the parent is left `running` while the doomed run
  drains, because only the `done` stage ever completes it. If an exclusive
  decision routes around a compound, every derived stage is skipped with its
  parent. Because each stage is a real program, **every stage profile has to be
  authorized when the run is created**, not just the parent's `performer`.

  In this slice each stage performer must be a program with no `contact`
  schedule, and stage-level `retry` / `approvalRetry` are not executable yet.

- Human plan approval. A compound whose `plan` declares `approval: human`
  expands one extra stage between plan and do, `<id>.plan.approval`. It runs no
  program: it is an ordinary awaited decision with the fixed verdicts `approve`
  and `rework`, decided over the same `tclaude process decide` command,
  permission, and state-version CAS as an authored decision.

  `approve` readies the do stage. `rework` returns the plan stage to ready and
  closes the gate, so ordinary planning runs the plan once more and the window
  reopens when it succeeds; nothing after the approval is touched, because
  nothing after it has run. There is no approval budget: each rework is one
  explicit audited human action that buys exactly one more plan execution. A
  plan program that *fails* is still fail-fast, and a failed check or review
  gate reworks the do stage without ever reopening an approval that was already
  given.

  Because the window reopens, a verdict has to name it:

  ```bash
  tclaude process decide <run-id> --node <id>.plan.approval --attempt N --verdict approve|rework
  ```

  `tclaude process show` reports the exact `--attempt` in its next-step hint,
  and the run refuses any other value — including a verdict a person formed
  against an earlier window and submitted late. Authored decisions open exactly
  once, so they need no `--attempt` and their output is unchanged.

- A node id ceiling that only applies to *running* a template. Authoring bounds
  what characters a node id may contain, not how long it is, but a run has to
  record durable evidence per node, and that row bounds the id at 256 bytes. Run
  creation therefore refuses a template whose node ids — authored **or derived
  from a compound's stages** — exceed it, rather than creating a run that could
  never commit a transition. Editing and storing such a template still works.

Not executable yet: agent deciders, agent or human task performers, retry
backoff waits, same-session retry feedback (`retry.onFail:
feedback-same-session`), retries on compound stages, `plan.approvalRetry`,
human or agent stage performers, poison handling, wait nodes, and captures.
Template validation and run creation both refuse these with a path-specific
diagnostic rather than failing later.

## Crashes and reconciliation

A command is durable before its program starts, so after a daemon restart
tclaude cannot know whether an outstanding command's program actually ran. It
never guesses and never silently re-runs one. `tclaude process show` reports
each outstanding command as `needs_reconcile`, and an operator resolves it
explicitly with `record-outcome` (I checked out of band; here is what
happened) or `reissue` (run it again). When a run holds more than one such
command, both verbs require `--node` to name which one.

A restart preserves each node's exact attempt number, and an outstanding
retried attempt is reported `needs_reconcile` like any other — never silently
re-run. `reissue` re-runs that same durable attempt rather than spending
another one from the budget; `record-outcome --outcome failed` spends it, and
the next attempt starts if the budget allows.

A failed branch fails the run, but the run is only reported failed once every
command still in flight has been accounted for. The same holds for a `cancel`
resolution: the run reports `canceled` only after its last in-flight program
has been accounted for.

A parked branch survives a restart unchanged — the checkpoint carries the
obligation and the exact blocked attempt, so nothing has to be replayed from
evidence — and a blocked run is never re-run by itself. If some other branch
fails or is cancelled while a branch is parked, the parked obligations are
dropped: the run is over, so it stops offering a resolution nothing could act
on.

## Storage

Run state lives in SQLite under `~/.tclaude/data/`; the checkpoint there is
authoritative. Ordered human-facing evidence per run is available through
`tclaude process events`. On daemon startup, tclaude removes obsolete
filesystem run data and legacy run lock files, and deliberately preserves the
complete Processes authoring root, including all template versions, heads,
layouts, snippets, authorship records, and template locks.
