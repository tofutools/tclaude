# Processes

Processes are an experimental, feature-flagged surface for BPMN-lite repeatable
workflows. With the feature enabled, `tclaude agentd` continuously advances
runs in the filesystem store at `~/.tclaude/processes`. The manual CLI remains
available for instantiation, inspection, verification, and repair workflows.

Enable the feature:

```json
{
  "features": {
    "processes": true
  }
}
```

Every command currently needs an explicit filesystem store. Use the agentd
default when you want the daemon to host the run:

```bash
STORE="$HOME/.tclaude/processes"
mkdir -p "$STORE"
```

## Quickstart

Start `agentd` after enabling the flag, and make sure the `dev` and `reviewer`
spawn profiles referenced by the bundled
[`code-change-with-review`](examples/code-change-with-review.yaml) example
exist (create them in the dashboard Profiles editor or with `tclaude agent
profiles create`). Then instantiate the run in the daemon's default store:

```bash
tclaude agentd serve

# In another terminal:
tclaude process run docs/examples/code-change-with-review.yaml \
  --store-root "$STORE" \
  --run-id change-1 \
  --param issue=TCL-278 \
  --allow-programs
while true; do
  clear
  tclaude process show change-1 --store-root "$STORE"
  sleep 1
done
```

No `process advance` is needed. The engine expands the task, launches the plan
and implementation agents, runs `go test ./...` as a real program performer,
launches the cold reviewer, and waits only at the two human obligations. Those
obligations appear in the dashboard Messages channel; reply with the advertised
action (for example `approve`) and the engine continues on its next tick.

Stopping `agentd` with Ctrl-C and starting the same command again resumes the
run from `state.json`. Issued commands carry deterministic command IDs and
idempotency keys; the daemon rediscovers bound agents and claimed internal
commands instead of spawning or executing them twice.

If a daemon restart parks an issued performer command as
`needs_reconcile`, record the human-confirmed external result without rerunning
the side effect:

```bash
tclaude process observe demo-1 cmd_... --store-root "$STORE" --verdict pass --actor human:$USER --evidence artifact:...
```

## Agent and human performers

Agent performers resolve `performer.profile` against the saved agent spawn
profiles managed by `tclaude agent profiles`. The daemon launches a
process-owned, ungrouped agent and records the deterministic process command id
on its stable agent metadata. On restart, that metadata is used to rediscover
the live attempt rather than spawning it again.

Process agents currently start in the agentd daemon's working directory. The
template and saved spawn profile do not yet provide a per-slot cwd/worktree
override.

The agent receives a fixed reporting protocol in its startup brief. A
successful result must include an evidence reference:

```bash
tclaude process report RUN NODE \
  --command cmd_... \
  --verdict pass \
  --evidence commit:abc123
```

The daemon authenticates the calling pane and derives `agent:agt_...` itself;
the report cannot claim another actor identity.

A human performer creates a durable obligation in run state. Obligations carry
the run/node/attempt, assignee, due time, summary, available actions, evidence
link, and visible contact schedule. Resolve one from the CLI:

```bash
tclaude process resolve RUN NODE \
  --verdict pass \
  --actor human:johan \
  --evidence approval:change-42
```

The obligation also appears in the dashboard Messages channel. Replying to its
message starts with an advertised action. On task obligations, `approve` maps
to `pass`, while `reject` and `ask-changes` map to `fail`; decision obligations
advertise and preserve their actual edge names. Unknown actions are rejected
instead of silently settling the wrong edge. The dashboard reply is recorded
as approval evidence. `process show` renders both the obligation and its nudge
state.

Asynchronous performer slots use kind defaults (humans: every 30 minutes, five
nudges; agents: every five minutes, three nudges) or a per-slot override:

```yaml
performer:
  kind: human
  profile: johan
  ask: Approve merge?
  contact:
    cadence: 15m
    budget: 4
    escalationTarget: human:operator
```

Nudge exhaustion sends one human escalation, names the configured escalation
target in that dashboard message, and leaves the node waiting; it never fails
the node. The budget resets when an agent becomes active again.
If the human interacts directly with a live process agent, automation for that
node pauses after the same five-second grace used by the task runner, so an
automated nudge never competes with the human for the session.

## Program performers

Program performers execute local commands and therefore require an explicit
opt-in on each run:

```bash
tclaude process run program-demo.yaml --store-root "$STORE" --run-id program-1 --allow-programs
```

The opt-in is stored on the run record and only becomes executable after its
admin audit event is committed through the log, manifest, and state checkpoint.
The executor refuses a program command when its run was instantiated without
`--allow-programs`. The opt-in's integrity is only as strong as the filesystem
permissions protecting the process store root.

`performer.run` is an executable name or path; `performer.args` is passed as a
literal argument vector, without a shell. `performer.timeout` accepts a Go
duration such as `30s` or `5m` and defaults to 10 minutes. Program commands
receive `TCLAUDE_PROCESS_COMMAND_ID` and
`TCLAUDE_PROCESS_IDEMPOTENCY_KEY` in their environment. Only `PATH`, `HOME`,
`TMPDIR`, `LANG`, and `LC_*` are inherited from the parent process. Exit code
and bounded stdout/stderr tails are stored as an evidence artifact; exit code
zero settles as pass and every other exit code settles as fail.

This phase does not provide command allowlists or process sandboxing. Treat
templates as untrusted input and only enable program execution when you have
reviewed the commands. Allowlists and sandboxing are planned for a later phase.

Inspect stored objects:

```bash
tclaude process templates ls --store-root "$STORE"
tclaude process runs ls --store-root "$STORE"
```

## Compound task nodes

A task node that declares `plan`, `checks`, or `review` is a compound node.
Activating it expands it into explicit child stage nodes, recorded in run
state (logical zoom is the data model, not a UI trick):

```
implement  ->  implement.plan
               implement.plan.approval   (only when plan.approval: human)
               implement.do
               implement.test.<check-id> (one per checks entry, in order)
               implement.review
               implement.done
```

```yaml
nodes:
  implement:
    type: task
    performer: { kind: agent, profile: dev, prompt: "Implement {{ params.issue }}" }
    plan:
      id: plan
      approval: human            # human | auto (default auto)
      approvalRetry:
        maxAttempts: 3           # approval gate failed-verdict budget
      performer: { kind: agent, prompt: "Plan it" }
      retry:
        maxAttempts: 3           # plan attempts, including approval rework
    checks:
      - id: tests
        performer: { kind: program, run: "go test ./..." }
        retry:
          maxAttempts: 3         # gate budget: failed verdicts before poison
    review:
      id: review
      performer: { kind: agent, profile: reviewer, prompt: "Cold-review the diff" }
    retry:
      maxAttempts: 2             # budget for the do stage
      onFail: feedback-same-session   # retry mode: feedback-same-session | fresh-attempt
    next:
      pass: done
```

Rules that make compound runs trustworthy:

- **Expansion is recorded state.** The `node_expanded` event lists the derived
  children; `verify` re-derives the expansion from the pinned template and
  flags any mismatch (`expansion_template_mismatch`).
- **Claimed done is not done.** A stage child can only settle as completed
  with an `--evidence` ref; a pass claim without evidence flips to failed.
- **Gate failure feeds back within budgets.** A failed gate whose budget is
  not exhausted routes its feedback to the stage it re-enters
  (`plan.approval` re-enters `plan`; `test`/`review` re-enter `do`): the work
  stage re-readies with the gate's feedback pending, and every gate between
  the work stage and the failing gate resets to pending so the loop re-runs
  them against the new work. A gate's budget is its own `retry.maxAttempts`
  (default 1) and counts failed verdicts in the current loop window; gates of
  a different stage kind inside the reset span get their counters zeroed too
  (a review failure restarts the testing window). The feedback loop is also
  bounded by the work stage's `retry.maxAttempts`.
- **Gate retry policies use `maxAttempts` only.** Gate stages ignore the
  `onFail` and `backoff` fields in `plan.approvalRetry` and in the `retry`
  policy on authored check and review steps.
- **Plan approval has its own gate budget.** `plan.approvalRetry.maxAttempts`
  controls how many failed approval verdicts are allowed (default 1). A live
  approval feedback loop also needs enough `plan.retry.maxAttempts` for the
  plan stage to revise and re-propose.
- **Exhausted budgets poison, they never auto-fail the run.** When a gate's
  budget (or the target work stage's attempt budget) is spent, the gate blocks
  itself and its parent with a reason and owner; the run keeps running and a
  human through `process unblock` or the authored escalation decision resolves
  it. New blocks record when the wait began and use the same kind-scoped
  `ContactState` schedule as performer obligations to nudge the typed block
  owner; resolving the block stops that schedule without erasing its history.
- **A poisoned fail-edge decision is a resolution surface, not failure
  routing.** When a blocked compound node's authored `fail` edge targets a
  human decision node, the engine readies that decision while leaving the
  parent and poisoned child blocked. The v1 bridge accepts only the strawman's
  `retry` edge back to the blocked parent and `cancel` edge to a canceled end.
  The planner emits a generation-bound `resolve_block` command; the executor
  claims it and applies the same audited poison-resolution funnel as `process
  unblock`. A restart between the human verdict and the resolution append
  rediscovers that command. Non-decision fail targets stay inactive, so poison
  cannot silently turn into failure or continuation.
  Reserved poison-escalation decisions cannot be completed with `process
  advance`, because ordinary decision-edge activation would bypass that
  funnel; answer the engine worklist obligation or use `process unblock`.
  Decision nodes are single-use in v1: if a decision-driven retry later
  poisons again, the completed escalation node is not reset; use the explicit
  `process unblock` path for that later generation.
- **Poison resolution is explicit and audited.** Resolve the blocked stage
  child (or its blocked parent mirror) with `process unblock`. The engine
  clears both mirrors in one append batch and records the actor, decision,
  reason, evidence reference, and blocked attempt. `retry` opens a fresh gate
  budget window and starts a new attempt, `skip` completes the stage by
  decision, and `cancel` settles the run as canceled. A replayed resolution is
  idempotent, and the recorded attempt generation prevents a delayed poison
  command from silently re-blocking the node.
- **Unchanged evidence short-circuits a re-entered gate.** When a gate
  re-enters but the work stage settled with the same evidence hash its
  previous verdict evaluated, the engine does not re-run the gate's performer:
  it appends a decision record by the `engine:evidence-unchanged` actor that
  stands the prior verdict (and its evidence ref). Manual `advance` verdicts
  are always honored — the short-circuit is planner/executor-only.
- **Retry mode is policy, not routing.** `retry.onFail` selects how a retry
  re-engages the performer (`feedback-same-session` keeps the session,
  `fresh-attempt` starts clean; unset defaults to `fresh-attempt`). The mode
  and any pending gate feedback ride the work stage's start commands, so
  adapters see them. Failure routing comes only from `next` keys such as
  `fail`.
- The parent completes only when its `done` stage completes, which happens
  automatically after the last gate passes.

Advance the stages of a compound run manually:

```bash
tclaude process advance demo-1 implement --store-root "$STORE" --verdict pass          # expands
tclaude process advance demo-1 implement.plan --store-root "$STORE" --verdict pass --evidence artifacts/plan.md
tclaude process advance demo-1 implement.do --store-root "$STORE" --verdict pass --evidence commit:abc123 --evidence-hash "$(sha256sum diff.patch | cut -d' ' -f1)"
tclaude process advance demo-1 implement.test.tests --store-root "$STORE" --verdict fail --feedback "unit tests fail on TestFoo"   # re-readies implement.do with feedback
```

Resolve a stage after its retry budget poisons it:

```bash
tclaude process unblock demo-1 implement.test.tests \
  --store-root "$STORE" \
  --decision retry \
  --actor human:$USER \
  --reason "transient CI outage confirmed" \
  --evidence incident:ci-2026-07-10
```

`--decision` accepts `retry`, `skip`, or `cancel`. `--reason` and
`--evidence` are required so the reconstructed event log always explains the
release; `--actor` defaults to the current human user.

`--evidence-hash` records the content hash of the settle's evidence — on a
work stage it is the hash later gate verdicts evaluate, which powers the
evidence-unchanged short-circuit; `--feedback` is the gate payload the next
work attempt answers.

## Reconstruct a run from disk

The run directory is the audit boundary. After a run completes—or with no
engine running—you can reconstruct it using only these files:

```bash
RUN_DIR="$STORE/runs/change-1"

# Materialized lifecycle, node attempts, obligations, decisions, blocks,
# outstanding command IDs, and the final run status.
less "$RUN_DIR/state.json"

# Immutable run metadata, including the pinned canonical template snapshot,
# and the ordered checksum chain over every event.
less "$RUN_DIR/run.json"
less "$RUN_DIR/manifest.jsonl"

# Per-node and run-scoped evidence logs, including performer verdicts,
# feedback loops, human decisions, and admin block resolutions.
find "$RUN_DIR/nodes" "$RUN_DIR/run" -name log.jsonl -type f -print

# Content-addressed program evidence and any other stored artifacts.
find "$RUN_DIR/artifacts" -type f -print

# Re-check the checksum chain and semantic invariants without starting agentd.
tclaude process verify change-1 --store-root "$STORE"
```

`state.json` answers what the latest materialized state was; the template
snapshot in `run.json` preserves the exact workflow semantics; the node/run logs
answer which actors and verdicts produced it; `manifest.jsonl` proves ordering
and integrity; artifact filenames are their content hashes. Together they are
enough to explain retries, human approvals, escalation decisions, crash
recovery, and the terminal result without consulting SQLite or a live daemon.

## Notes

- `advance` runs `verify` first and refuses dirty or inconsistent runs.
- All state changes go through `store.Append`, the manifest, and reducer events.
- Template params are validated and stored on the run record. Performer
  prompt/ask/run/args interpolation is bound, with the parameter snapshot, into
  the issued command so recovery replays the exact request.
- Retry support is node-level `retry.maxAttempts`; repair and poison release
  are always explicit audited operations. The daemon host verifies and leases every run before advancing it,
  persists timer and rate-limit waits, and parks commands whose external side
  effect cannot be safely rediscovered after a restart.
- A manual `advance` of another ready node while a run is paused is an
  intentional human override; the paused command's own running node remains
  protected from manual advancement.
- Phase 1 treats each selected outgoing edge as an exclusive branch. Explicit
  AND-join semantics are deferred until the engine can track live paths.
- End nodes default to completed runs; set `result: failed` on a failure
  terminal node when that path should fail the run.
- A poison-resolution `cancel` settles the run directly; the authored canceled
  end marker remains pending until the resolution command grows a typed
  terminal-node target in a later phase.

## Param templating surface

Templates may reference params with `{{ params.<name> }}`. Only these performer
fields are templatable — the engine interpolates them before dispatch:

- `performer.prompt`
- `performer.ask`
- `performer.run`
- `performer.args[]`

Everywhere else a `{{ params.x }}` reference is used **literally** and is never
interpolated. In particular `performer.profile`, `performer.timeout`,
`retry.backoff`, and `wait.duration`/`until`/`signal` are config values, not
templates; a param reference there raises an `inert_param_ref` warning at
authoring time. References in prose fields (`name`/`description`/`doc`) are only
checked for pointing at a declared param.

Duration-ish fields (`wait.duration`, `retry.backoff`, `performer.timeout`) are
parsed with Go's `time.ParseDuration` at authoring time and must be positive, so
`5m`/`1h30m`/`500ms` are valid but `banana`, `-5s`, and `0s` fail validation
rather than surfacing at runtime. Because these fields are not templatable, a
`{{ params.x }}` reference is likewise an authoring-time error.
