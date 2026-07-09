# Processes

Processes are an experimental, feature-flagged surface for BPMN-lite repeatable
workflows. There is not yet a continuously running engine host: a human can
advance runs from the CLI, and the executor is currently a Go library for
engine integrations and tests.

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

## Notes

- `advance` runs `verify` first and refuses dirty or inconsistent runs.
- All state changes go through `store.Append`, the manifest, and reducer events.
- Template params are validated and stored on the run record; interpolation is
  not executed by this phase.
- Retry support is node-level `retry.maxAttempts`; broader engine scheduling,
  a continuously running host/tick loop, and repair are later phases.
- Phase 1 treats each selected outgoing edge as an exclusive branch. Explicit
  AND-join semantics are deferred until the engine can track live paths.
- End nodes default to completed runs; set `result: failed` on a failure
  terminal node when that path should fail the run.
