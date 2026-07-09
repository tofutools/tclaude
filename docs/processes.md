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

Create a template:

```yaml
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: manual-demo
params:
  ticket:
    type: string
    required: true
start: implement
nodes:
  implement:
    type: task
    performer:
      kind: human
      ask: Implement the change
    retry:
      maxAttempts: 2
    next:
      pass: decide
      fail: failed
  decide:
    type: decision
    performer:
      kind: human
      ask: Ship it?
    next:
      approve: end
      reject: failed
  failed:
    type: end
    result: failed
  end:
    type: end
```

Run it manually:

```bash
tclaude process run manual-demo.yaml --store-root "$STORE" --run-id demo-1 --param ticket=TCL-271
tclaude process verify demo-1 --store-root "$STORE"
tclaude process advance demo-1 implement --store-root "$STORE" --verdict fail --actor human:$USER
tclaude process advance demo-1 implement --store-root "$STORE" --verdict pass --actor human:$USER
tclaude process advance demo-1 decide --store-root "$STORE" --verdict approve --actor human:$USER
tclaude process show demo-1 --store-root "$STORE"
tclaude process show demo-1 --store-root "$STORE" --mermaid
```

If a daemon restart parks an issued performer command as
`needs_reconcile`, record the human-confirmed external result without rerunning
the side effect:

```bash
tclaude process observe demo-1 cmd_... --store-root "$STORE" --verdict pass --actor human:$USER --evidence artifact:...
```

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
      performer: { kind: agent, prompt: "Plan it" }
    checks:
      - id: tests
        performer: { kind: program, run: "go test ./..." }
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
- **Gate failure poisons, it never auto-fails the run.** A failed
  `test`/`review`/`plan.approval` gate blocks the child and its parent with a
  reason and owner; the run keeps running and a human (or later, a decision
  node) resolves it. Gate feedback loops and per-gate budgets land in the next
  phase; in this phase a gate failure blocks immediately.
- **Retry mode is policy, not routing.** `retry.onFail` selects how a retry
  re-engages the performer (`feedback-same-session` keeps the session,
  `fresh-attempt` starts clean; unset defaults to `fresh-attempt`). Failure
  routing comes only from `next` keys such as `fail`.
- The parent completes only when its `done` stage completes, which happens
  automatically after the last gate passes.

Advance the stages of a compound run manually:

```bash
tclaude process advance demo-1 implement --store-root "$STORE" --verdict pass          # expands
tclaude process advance demo-1 implement.plan --store-root "$STORE" --verdict pass --evidence artifacts/plan.md
tclaude process advance demo-1 implement.do --store-root "$STORE" --verdict pass --evidence commit:abc123
```

## Notes

- `advance` runs `verify` first and refuses dirty or inconsistent runs.
- All state changes go through `store.Append`, the manifest, and reducer events.
- Template params are validated and stored on the run record; interpolation is
  not executed by this phase.
- Retry support is node-level `retry.maxAttempts`; repair remains a later
  phase. The daemon host verifies and leases every run before advancing it,
  persists timer and rate-limit waits, and parks commands whose external side
  effect cannot be safely rediscovered after a restart.
- A manual `advance` of another ready node while a run is paused is an
  intentional human override; the paused command's own running node remains
  protected from manual advancement.
- Phase 1 treats each selected outgoing edge as an exclusive branch. Explicit
  AND-join semantics are deferred until the engine can track live paths.
- End nodes default to completed runs; set `result: failed` on a failure
  terminal node when that path should fail the run.

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
