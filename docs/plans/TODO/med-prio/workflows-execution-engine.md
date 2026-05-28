# workflows: execution engine (Phase 2)

Part of the **Workflows** feature — see `docs/plans/workflows.md`. This is
**Phase 2**: turning the monitored, manually-driven graph into an **autonomous
runner**. Depends on all the high-prio steps shipping first (format, schema, api,
dashboard, group-integration). Med-prio on purpose — the monitoring MVP is the
priority and is useful on its own.

## Open / to build

An engine (in `pkg/claude/workflow/` + driven by `agentd`'s background loop, like
the cron scheduler tick) that advances instances without a human clicking:

1. **Scheduler tick** — periodically (or event-driven on node completion) scan
   running instances for `ready` nodes and dispatch them. Reuse the cron
   scheduler's 30s-tick pattern in `agentd` as the model.
2. **Executor dispatch** by `executor.kind`:
   - `tool`/`program`: run the interpolated `run` command, capture stdout/stderr
     into `output`/`vars`, exit code → success/failure.
   - `ai`: spawn/resume the agent in the instance group (group-integration step),
     hand it the interpolated prompt; detect completion (agent signals done /
     idle + a sentinel, TBD). Honor `mode: interactive|autonomous`.
   - `human`: leave `awaiting` for the dashboard; the engine doesn't auto-run it.
3. **Verification** by `verify.kind`:
   - `tool`/`program`: run command, exit 0 = pass.
   - `enum`: parse the produced value, must be in `values`; the value selects the
     outgoing edge.
   - `format`: regex / schema match.
   - `ai`: a judge agent rules pass/fail.
   - `human`/`none`: as today.
4. **Capture + interpolation** — `{{param}}`, `{{captured}}`, `{{node.output}}`
   resolved into prompts/commands, with **type preservation** (string/list/map)
   like agent-runner. Store in `workflow_instances.vars`.
5. **Advance / branch / join / loop** — reuse the shared advance helper from the
   api step: on node done, follow the matching labeled edge(s) (enum outcome or
   default), mark non-taken branches `skipped`, respect `join: all|any`, enforce
   `retries`/`max_visits` on back-edges, set instance terminal status.
6. **Resumability** — engine is stateless across daemon restarts: it re-derives
   what to do from the SQLite node statuses. A killed daemon resumes mid-flight.
7. **Loops/retries/fix-loops** — the canonical agent-runner pattern (run
   validator → capture → ask agent to fix → retry until pass) expressed as a
   graph back-edge + `retries`/`break-on-pass`.

## Shipped context (after Phase 1)

By the time this starts: templates parse/load, instances + node state persist,
the dashboard monitors live, nodes can be manually driven, and AI nodes can be
started/attached against an instance group. This step removes the human from the
advance loop.

## Relevant source files

- `pkg/claude/agentd/background.go` / cron tick — scheduler model to mirror
- `pkg/claude/workflow/` — advance helper, interpolation, executors, verifiers
- `pkg/claude/common/db/workflows.go` — status/vars updates, event log
- `pkg/claude/agentd/dashboard_workflows.go` — shares the advance helper

## Open questions

- AI-node completion detection is the hard problem: how does the engine know an
  agent finished a node? Options: agent calls a `tclaude agent` verb to report
  done; idle-detection + a sentinel; an explicit `verify` gate that the agent
  must pass. Lean: an explicit completion signal via `tclaude agent` + the node's
  `verify` as the real gate.
- Concurrency: how many AI nodes run in parallel per instance / globally? Cap it.
- Failure escalation: when retries exhaust, notify the human (human-notify) vs
  just mark failed.
