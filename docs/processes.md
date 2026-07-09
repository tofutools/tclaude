# Processes

Processes are an experimental, feature-flagged surface for BPMN-lite repeatable
workflows. Phase 1 has no engine: a human advances runs from the CLI, while the
store, evidence log, reducer, and verifier exercise the same paths a later
engine will use.

Enable the feature:

```json
{
  "features": {
    "processes": true
  }
}
```

Every command currently needs an explicit filesystem store:

```bash
STORE=.tclaude-processes
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

Inspect stored objects:

```bash
tclaude process templates ls --store-root "$STORE"
tclaude process runs ls --store-root "$STORE"
```

## Notes

- `advance` runs `verify` first and refuses dirty or inconsistent runs.
- All state changes go through `store.Append`, the manifest, and reducer events.
- Template params are validated and stored on the run record; interpolation is
  not executed by this phase.
- Retry support is node-level `retry.maxAttempts`; broader engine scheduling,
  automatic command execution, and repair are later phases.
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
