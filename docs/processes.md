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
tclaude agent process-templates ls
tclaude agent process-templates show <template-id>
tclaude agent process-templates validate --file template.yaml
tclaude agent process-templates save --file template.yaml --source-hash <hash>
```

Reads require `process.templates.read`; saves require
`process.templates.manage`. These permissions authorize authoring only and do
not execute a template.

## Running

```bash
tclaude process run <template-id> --param key=value --authorize-program-profile <profile>
tclaude process runs ls
tclaude process show <run-id>
tclaude process events <run-id>
tclaude process decide <run-id> --node <node> --verdict <verdict>
tclaude process resume <run-id>
tclaude process reconcile <run-id>
tclaude process reissue <run-id> [--node <node>]
tclaude process record-outcome <run-id> [--node <node>] --outcome succeeded|failed
```

Creating a run pins the exact saved template head and its parameters, so later
edits to the template never change a run already in flight. Every program
profile a run may execute must be authorized explicitly at creation with
`--authorize-program-profile`; nothing is authorized implicitly.

Reads require `process.runs.read` and mutations require `process.runs.manage`.

## What executes today

- Sequential task chains, and exclusive decisions whose verdict selects exactly
  one authored outcome edge and closes the alternatives.
- Structured fan-out (`type: parallel`) reduced by `join: all`. Branches of one
  run execute concurrently, bounded to four external programs at a time; ready
  branches past that bound wait for a slot. A branch parked on a human decision
  does not block its siblings, and that decision can be answered while the
  sibling programs are still running.
- Program task performers (`performer.kind: program`), executed as argv without
  a shell, with a bounded environment and output.
- Human deciders on decision nodes.

Not executable yet: `join: any`, agent deciders, agent or human task
performers, retries and poison handling, wait nodes, captures, and compound
plan/check/review stages. Template validation and run creation both refuse
these with a path-specific diagnostic rather than failing later.

## Crashes and reconciliation

A command is durable before its program starts, so after a daemon restart
tclaude cannot know whether an outstanding command's program actually ran. It
never guesses and never silently re-runs one. `tclaude process show` reports
each outstanding command as `needs_reconcile`, and an operator resolves it
explicitly with `record-outcome` (I checked out of band; here is what
happened) or `reissue` (run it again). When a run holds more than one such
command, both verbs require `--node` to name which one.

A failed branch fails the run, but the run is only reported failed once every
command still in flight has been accounted for.

## Storage

Run state lives in SQLite under `~/.tclaude/data/`; the checkpoint there is
authoritative. Ordered human-facing evidence per run is available through
`tclaude process events`. On daemon startup, tclaude removes obsolete
filesystem run data and legacy run lock files, and deliberately preserves the
complete Processes authoring root, including all template versions, heads,
layouts, snippets, authorship records, and template locks.
