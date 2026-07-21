# Processes

Processes are an experimental, feature-flagged surface for BPMN-lite repeatable
workflows. With the feature enabled, `tclaude agentd` continuously advances
runs in the filesystem store at `~/.tclaude/data/processes`. The manual CLI remains
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
STORE="$HOME/.tclaude/data/processes"
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

`performer.profile` may use either a profile's primary name or an alias. This
supports semantic handles such as `codex-reviewer` even when the underlying
model-oriented profile name changes.

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
  `ContactState` schedule as performer obligations. Authored blocks currently
  target `human:operator`; blocked-owner scheduling accepts human/role owners
  only. Future agent or program/system ownership must add complete delivery,
  recovery, and escalation behavior before it becomes authorable. Resolving
  the block stops its schedule without erasing history.
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

Schema-8 runs in the default store use agentd for inspection, verification,
preview, and settlement. `process show` prints the current base revision and
digest needed by a preview:

```bash
tclaude process preview RUN_ID \
  --store-root "$STORE" \
  --candidate-file candidate.yaml \
  --base-revision REVISION \
  --base-digest DIGEST
```

The preview is read-only. If it returns opaque handoff blockers, repeat it with
one `--handoff TOKEN=retain` or
`--handoff TOKEN=transfer:LOCAL:RESERVATION:NODE` per blocker. If it returns
audited-settlement guidance, pass that guidance token as the second positional
argument to `process unblock`, together with the preview base:

```bash
tclaude process unblock RUN_ID GUIDANCE_TOKEN \
  --store-root "$STORE" \
  --base-revision REVISION \
  --base-digest DIGEST \
  --decision retry \
  --reason "operator-confirmed recovery" \
  --evidence incident:123
```

A valid preview also returns an opaque apply token. Apply that exact preview
atomically by repeating its base, candidate, optional reason, and handoffs:

```bash
tclaude process apply RUN_ID \
  --store-root "$STORE" \
  --candidate-file candidate.yaml \
  --reason-file reason.txt \
  --base-revision REVISION \
  --base-digest DIGEST \
  --apply-token APPLY_TOKEN \
  --handoff TOKEN=retain \
  --ask-human 30s
```

Apply requires the non-default `process.runs.unlock` permission. `--ask-human`
can request one bounded approval; timeout or denial fails closed, and this
permission cannot be made persistent through the popup. Agentd derives the
actor, UTC timestamp, and fixed `unlock_apply` reason code. Retrying the exact
committed draft after a lost response returns the original provenance, but
authorization is evaluated again on every request.

Agentd derives the schema-8 settlement actor and timestamp; `--actor` remains a
legacy-run option. Exact applied artifacts are restricted reads and require
the `process.runs.unlock.read` permission:

```bash
tclaude process show RUN_ID --store-root "$STORE" --epoch EPOCH_ID --diff
tclaude process show RUN_ID --store-root "$STORE" --epoch EPOCH_ID --reason
```

Schema-8 daemon commands intentionally reject custom store roots. Schemas 1–7
retain their existing direct/custom-root CLI behavior.

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

## Run viewer API

With Processes enabled, `GET /v1/process/runs/{id}/view` returns a dedicated,
read-only viewer projection. For schemas 1–7, the existing
`GET /v1/process/runs/{id}` contract continues to return the persisted run,
state, and verification result unchanged. Schema 8 returns the same safe
summary envelope from both routes: run status, schema/lineage metadata,
authority counts, and the current preview binding. It deliberately omits raw
runtime state and exact topology.

```json
{
  "run": { "id": "change-1", "templateRef": "...", "effectiveStatus": "running" },
  "graph": { "nodes": [], "edges": [] },
  "verification": { "effectiveStatus": "running", "dirty": false, "diagnostics": [] },
  "report": {
    "schemaVersion": 1,
    "nodes": {
      "implement.do": {
        "summary": {
          "attemptCount": 2,
          "retryCount": 1,
          "failureCount": 1,
          "completedStages": 0,
          "totalStages": 0
        },
        "timeline": []
      }
    },
    "traversedEdges": []
  },
  "viewerV2": {
    "protocol": "viewer_v2",
    "stateSchemaVersion": 7,
    "pathProtocol": "path_v1",
    "routingAvailable": true,
    "exactTopology": {
      "templateRef": "...",
      "start": "fork",
      "nodes": [{"id":"accepted","type":"end","join":"any"}],
      "edges": [{"id":"...","from":"primary-review","outcome":"pass","to":"accepted"}]
    },
    "routing": {
      "protocol": "path_v1",
      "encoding": 1,
      "edges": [{"edgeId":"...","state":"arrived","count":1}],
      "joins": [{
        "reservationId":"...","nodeId":"accepted","scopeId":"...",
        "policy":"any","state":"activated","generation":1,
        "winnerPathId":"...","detached":1,"arrived":1,"open":1,
        "impossible":0,"failed":0,"skipped":0,"canceled":0
      }],
      "stateCounts": {
        "paths":[{"state":"arrived","count":1}],
        "scopes":[{"state":"open","count":1}],
        "reservations":[{"state":"activated","count":3}],
        "propagation":[],"detachedPathCount":1,"detachedSinkCount":0
      },
      "details": {
        "generations":{"page":{"offset":0,"limit":25,"total":3,"hasMore":false},"items":[]},
        "scopes":{"page":{"offset":0,"limit":25,"total":1,"hasMore":false},"items":[]},
        "closures":{"page":{"offset":0,"limit":25,"total":0,"hasMore":false},"items":[]},
        "causeSets":{"page":{"offset":0,"limit":25,"total":0,"hasMore":false},"items":[]},
        "causes":{"page":{"offset":0,"limit":25,"total":0,"hasMore":false},"items":[]},
        "detachments":{"page":{"offset":0,"limit":25,"total":1,"hasMore":false},"items":[]},
        "detachedSinks":{"page":{"offset":0,"limit":25,"total":0,"hasMore":false},"items":[]}
      },
      "aggregate": {
        "paths":8,"scopes":2,"reservations":4,"activations":4,
        "closures":0,"propagation":0,"causeRecords":0,"causeSets":0,
        "detachments":1,"detachedSinks":0,"settled":false
      }
    }
  }
}
```

The viewer uses explicit DTOs and never serializes the stored template, state,
events, run params, performer prompts/commands, or command payloads. The report
projects narrow persisted metadata and content-addressed artifact references
only.
The additive `viewerV2` discriminator does not change the schema-v1 history
report. It publishes the declared checkpoint schema, the path protocol, an
exact-template topology with canonical path-v1 edge IDs, and one authoritative
`routingAvailable` decision. Schema-7 routing includes unpaged graph edges,
join policy/winner/count summaries, aggregate counts, and state counts. Rich
generations, scopes, candidate closures, complete cause sets and causes,
reservation-relative detachments, and detached sinks are typed pages. Request
one stable window for every detail table with:

```http
GET /v1/process/runs/change-1/view?detailOffset=25&detailLimit=25
```

`detailOffset` must be a non-negative integer. `detailLimit` must be 1–100;
omitting it uses 50. Invalid values return a sanitized `400 invalid_arg`.
Totals, state counts, topology, and edge/join overlays remain complete and do
not shrink with the selected detail page. The complete routing DTO is capped at
16 MiB, independently of the exact topology's 16-MiB encoded ceiling.

Schema-v6 runs report `legacy_schema` and omit the routing payload. Eligible
quiescent runs migrate atomically before new planning to the schema-7 path-v1
executor; runs with active legacy commands, waits, obligations, or blocks drain
on their legacy schema first. Schema-7 views expose only a bounded overlay
derived from the validated current checkpoint aggregate. Evidence and the
schema-v1 `traversedEdges` history are never routing fallbacks. Evidence may be
absent, reordered, or extended without changing viewer topology or overlays;
the dashboard renders its sanitized projection only as a separate timeline.

Every unavailable condition is explicit and fail-closed:

| Reason | Condition | Safe result |
| --- | --- | --- |
| `legacy_schema` | State schema is 1–6. | Show a verified exact topology when available; never infer routing from history. |
| `routing_absent` | A schema-7 aggregate has no routing state. | Show exact topology without an overlay. |
| `unsupported_schema` | State schema is unknown or newer than this viewer. | Omit topology/routing claims that cannot be interpreted safely. |
| `unsupported_protocol` | Routing protocol or encoding is not the supported path-v1 pair. | Preserve exact topology; omit the routing overlay. |
| `over_budget` | Exact topology, aggregate, or encoded routing DTO exceeds its limit. | Omit the over-budget surface instead of returning a partial view. |
| `inconsistent` | Template binding, identities, record relationships, safe codes, or aggregate validation disagree. | Omit routing claims until the run is repaired. |

The dashboard names the reason, keeps topology and overlay authority visible,
offers next/previous controls for the typed pages, and uses the same semantics
in its regular and wizard skins.

The schema-7 release supports direct task and decision performers, retries,
start/end routing, and duration, `until`, and signal waits. Timer schedules and
performer claims are persisted before external action. Signal satisfaction is
available at `POST /v1/process/runs/{id}/nodes/{node}/signal`, requires the
`process.advance` permission for agent callers, and is audited without copying
the signal body into audit detail. Exact observation and signal retries after
an ambiguous commit are idempotent; a changed command, node, actor, outcome, or
evidence binding is rejected.

For a serial task without an authored failure edge, schema 7 matches the
legacy terminal contract: task action aliases are normalized first, pass
outcomes succeed, and every other non-empty result consumes the retry budget.
An exhausted result becomes a `failed` / `performer_failed` terminal path and
fails the run without inventing an edge or end-node activation. Decision
labels remain exact, and templates with explicit failure edges retain their
existing routing behavior. Failed performed work is observed and completes
its contact schedule; it is not represented as command cancellation.

Obligations and blocked nodes include only their recorded wait/contact state;
missing legacy timestamps and schedules remain absent rather than being
reconstructed. A conversation reference contains only the durable agent ID
recorded on the process command. The dashboard separately decides whether that
agent currently has an online conversation.

Schema-7 worklist rows are derived from verified performer commands and real
wait/obligation/block side effects. A live parallel-any loser remains visible
when it still has an outstanding external wait; the row is marked `detached`
with its reservation-relative detachment count. A join reservation is routing
state, not work, so the worklist never invents a synthetic join task.

The graph is derived only from the exact template matching `templateRef`. A
legacy run that did not embed its template resolves that content-addressed
version without recovering an unfinished authoring save or substituting the
current template head. If the pinned version is unavailable or mismatched,
verification reports the run as inconsistent, `graph` is null, and the report
does not claim traversed graph edges.

A confirmed existing run whose checkpoint or evidence cannot be decoded still
returns HTTP 200 so inspection can show its inconsistent alarm. That degraded
response has a minimal safe run projection, `graph: null`, an empty schema-v1
report, and stable sanitized verification diagnostics. A genuinely missing run
remains 404. Permission, device, symlink, and transient process-store failures
remain sanitized 500 responses. Reading this endpoint takes the same run lock
as append, opens consumed run/template components without following symlinks,
requires regular persisted files, and never rewrites template authoring state,
run metadata, state, manifests, or evidence logs.

The schema-v1 full-history projection is deliberately bounded: each consumed
file may be at most 16 MiB; the persisted run snapshot may consume at most
64 MiB, 100,000 evidence records, and 4,096 node-directory entries. The exact
pinned template is read under its own 16-MiB file ceiling, so one successful
endpoint read consumes at most 80 MiB of persisted input. Requests that exceed
a limit or are canceled fail without holding the append lock indefinitely; the
HTTP response remains a sanitized 500 rather than returning a partial graph.
Decode work is synchronous and cooperatively checks cancellation, so it cannot
outlive the viewer request and accumulate after the coherent lock is released.

## Execution capability and migration matrix

Engine capabilities are supplied by the trusted host; neither a template nor a
run-creation request can grant one. The rollout is monotonic and ordered:
`foundation_v1` → `parallel_all_v1` → `parallel_any_v1`. `parallel_all_v1`
requires the foundation, and `parallel_any_v1` requires both earlier
capabilities. There is deliberately no fan-out-only capability: admitting a
fork without the matching reducer, durable terminal propagation, viewer, and
worklist semantics would create runs the engine could not explain or settle.

| Host capability set | Serial/exclusive template | Parallel + `join: all` | Parallel + `join: any` |
| --- | --- | --- | --- |
| `foundation_v1` | admitted | rejected | rejected |
| `foundation_v1`, `parallel_all_v1` | admitted | admitted in the executable schema-7 subset | rejected |
| `foundation_v1`, `parallel_all_v1`, `parallel_any_v1` (production) | admitted | admitted in the executable schema-7 subset | admitted in the executable schema-7 subset |

Production rejects incoherent capability combinations and parallel templates
outside the released executor subset before creating a run. The supported
subset includes direct task/decision performers (agent or human) with their
contact schedules (explicit or engine defaults), exact waits, retries,
start/end routing, serial terminal failure without an authored failure edge,
nested forks that the reducer can poison safely, complete innermost-scope
reductions, terminal cause propagation, all joins, and any joins with
winner/detachment semantics. Compound nodes, program performers, and unsafe
nested-fork shapes remain outside that parallel subset and are rejected rather
than silently falling back.

Schema-7 contact state (reminders and escalation for deferred agent/human
performers) lives in the checkpoint's `contacts` registry beside its
side-effect marker. Nil contact configuration means engine defaults, exactly
as on legacy v6, and used budgets survive restart, replay, and recovery.
Rollback limitation: binaries older than contact parity cannot read a
contact-bearing schema-7 checkpoint — the strict decoder refuses it
(fail-closed) rather than silently dropping the schedule; contact-less
checkpoints remain byte-identical and readable either way.

The allowed dependency direction is from process hosts and API/CLI entry
points through the released engine, executor, store/migration, and viewer
boundaries into `state/pathv1`. The low-level pure exclusive planner/reducer
exports in `exclusive_pure.go` are not production entry points: only their
owning `pathv1` package may compose them. A recursive architecture test scans
all non-test Go sources outside that package, including aliases, wrappers,
subpackages, and generated registration files, and fails closed when the pure
file gains an unclassified export.

Migration is checkpoint-bound:

- New production runs that match the executable subset initialize schema 7
  before their first path-v1 plan.
- A pristine or completely drained eligible schema-1–6 run upgrades atomically
  to a bound schema-7 checkpoint. Exact template ref, template-source hash,
  generation, routing authority, commands, and side effects move together.
- A legacy run with active commands, waits, obligations, blocks, admin work, or
  ambiguous progress remains on its legacy executor until those records drain.
  It is never partially converted and never uses viewer evidence as migration
  input.
- Unsupported templates continue on their compatible legacy path when that is
  safe; new parallel syntax that the production capability/subset gate cannot
  execute is rejected at instantiation.

The runnable parallel-any example is
[`parallel-any-review`](examples/parallel-any-review.yaml). It starts two cold
review agents and reduces at an `any` end join. The first passing arrival is the
winner. An already-dispatched loser is detached rather than fictionally
canceled; any real outstanding wait remains visible in the worklist.

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
- Production advertises the complete foundation/all/any capability chain and
  admits only the corresponding executable schema-7 subset described above.
- End nodes default to completed runs; set `result: failed` on a failure
  terminal node when that path should fail the run.
- A poison-resolution `cancel` settles the run directly; the authored canceled
  end marker remains pending until the resolution command grows a typed
  terminal-node target in a later phase.

### Parallel/join authoring and execution

Parallel gateways have no performer, wait, retry, result, capture, or compound
stage fields and require 2–2,046 normalized outgoing edges. A typed join is set
on the target node, requires at least two inbound edges, and accepts only `all`
or `any`. Omission on a multi-inbound node means `all` without serializing an
explicit default, so unchanged legacy templates keep their semantic hashes.

Static validation propagates causal fork/branch scope signatures. It permits a
local merge inside one unchanged scope, or a complete reduction of exactly one
innermost parallel scope. Partial, unrelated, multiple-scope, bypass, and escape
shapes are rejected before save; nested complete reductions are supported.

When editable YAML still contains the former advisory `metadata.join`, the
authoring parser promotes a valid value to typed `join`, removes only that
metadata key, and creates a new semantic version. A disagreeing typed field is
a blocking diagnostic. Immutable `template.json`, exact pinned template, and
run reads never perform this promotion or reinterpret an existing hash.

### Process-template graph budget

Normalized process graphs contain at most 2,048 authored nodes and 4,096
edges, including the synthetic edge from the graph root to `start`. The limits
are derived from the existing 2,046-way fan-out ceiling: a maximum-width fork,
one explicit branch node per lane, and its reducer use 2,048 nodes and 4,093
normalized edges. The 4,096 edge ceiling also stays on the existing routing
list, mutation, and viewer directory-entry scale while leaving a few edges for
the surrounding spine.

These are explicit graph-work limits, separate from byte limits. Agent CLI
YAML input is capped at 4 MiB, and the editor/API applies a 4 MiB JSON request
cap whose envelope includes the source or structured edit model. Those byte
caps protect transport and storage, but do not bound graph cardinality: YAML
can reuse one anchored `next` map from many nodes. The parser therefore counts
the alias-expanded graph shape with saturating counters before decoding graph
aliases, checks a materialized template again before allocating normalized
edges, and performs the exact post-normalization check before scope,
reachability, cycle, layout, canonicalization, or hashing work.

All authoring diagnostics—from duplicate/schema inspection through freeform
normalization, legacy promotion, and semantic validation—share one global
budget of 6,144 findings and a conservative encoded-wire ceiling just below 4
MiB. Wire accounting reserves the terminal response envelope and charges fixed
JSON object overhead, worst-case escaping, and a possible second copy of a path
as an editor target. Schema inspection memoizes an aliased source subtree by
its YAML-node and schema context, then instantiates occurrence-specific paths
only within that shared budget. Pre-decode saturation stops before decode;
later saturation stops before canonical hashing or persistence. Both return the
deterministic prefix plus exactly one `template_diagnostic_budget`; findings
are never silently dropped, and editor saves treat saturation as a hard
resource rejection. The API also asserts that the encoded editor-diagnostic
projection fits the same wire ceiling before writing it. The finding count is
the node-plus-edge graph-work scale, while the wire ceiling keeps one accepted
request from amplifying into a larger editor/API response.

The node/edge budget bounds the authored graph used by validation and editor
projection. It does not replace the independent execution limits: the path-v1
reducers retain their capability-specific fan-out, path, record, reference,
mutation, payload, and checkpoint ceilings, and the full-history viewer retains
its file, total-byte, evidence-record, and directory-entry budgets. Those
runtime/viewer limits intentionally describe persisted execution growth rather
than authoring-source size.

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
authoring time. When an inert field has a syntax contract, its literal param
reference also fails that validation. References in prose fields
(`name`/`description`/`doc`) are only checked for pointing at a declared param.

Duration-ish fields (`wait.duration`, `retry.backoff`, `performer.timeout`) are
parsed with Go's `time.ParseDuration` at authoring time and must be positive, so
`5m`/`1h30m`/`500ms` are valid but `banana`, `-5s`, and `0s` fail validation
rather than surfacing at runtime. Because these fields are not templatable, a
`{{ params.x }}` reference is likewise an authoring-time error.
Absolute `wait.until` deadlines are trimmed and parsed as RFC3339 timestamps at
authoring time, matching the executor's timer contract.

A template carries two distinct identifiers. Its `id` is the permanent store
key: it is part of every `id@sha256:<hash>` ref and is what pinned versions and
live runs resolve through, so it is never editable. Dashboard-created templates
never let an operator choose it — `POST /v1/process/templates` mints a compact
lowercase-hex UUID, and the dashboard's **+ new template** asks only for a
display name. Submitting that dialog persists the named scaffold, then opens
the stored template by the id returned from the backend. Templates authored as
YAML through the CLI still carry their own hand-written ids, which is why
`docs/examples/*.yaml` have readable ones. Its `name` is a free-text display
label shown wherever the template is listed, and it can be changed at any time.
A template with no name falls back to showing its id.

Generated run ids follow the template's display name rather than its id, so a
run reads as `release-train-20260719-2210` even when the template is keyed by a
UUID. The name is slugified into the run-id grammar and falls back to the id,
then to `run`, when nothing usable survives. The prefix is decoration: a run
resolves its template through the pinned `templateRef`, so renaming a template
never invalidates ids already minted under the old name. Runs list newest-first
by creation time rather than by id.

Rename a template by clicking its name in the Templates list or its title in the
open editor, and editing in place; Enter commits and Escape abandons. The
list's **rename** button opens the same edit in a dialog that also states the
id and what a rename does and does not affect, and confirms with Ctrl/Cmd+Enter
like the dashboard's other dialogs. The editor's **template settings…**
inspector carries the same field alongside the description and documentation.

The two surfaces commit differently, which is deliberate. In the editor the
name is part of the draft: it is undoable and is written when the template is
saved, like any other edit. In the Templates list there is no draft, so
committing saves immediately against the version the row published — a template
changed by an agent or another tab in the meantime is reported as a conflict
rather than overwritten.

Every path writes the name through the ordinary content-addressed save, so a
rename is a normal new version rather than a mutation of history; existing runs
and pinned refs are unaffected. The underlying endpoint requires
`process.templates.manage`, which the local operator dashboard already
satisfies.

The dashboard template editor can declare params and instantiate any clean,
saved version. Instantiation always sends the exact `id@sha256:<hash>` currently
shown; a dirty editor requires a successful save first, so a run never captures
unsaved browser state. Saved editor drafts that still have validation errors
remain editable but cannot be instantiated. Declared string, number, and
boolean params render with matching controls. Runtime values remain strings,
matching `tclaude process run --param key=value`; defaults and required checks
use the same shared creation code for both surfaces. The dashboard retains one
cryptographically strong run id for an open instantiation, so retrying after a
lost response recovers the original durable run instead of launching a second
one. The REST endpoint replays an existing explicit id only when its exact
template ref and resolved params match; reusing it for different inputs is a
conflict. This does not change the CLI's duplicate explicit-id error. When no
run id is supplied, same-second creations retry with a numeric suffix instead
of colliding.

While the template editor is active, the dashboard command palette adds graph
commands for creating nodes, editing the current selection, canvas navigation,
validation, saving, instantiation, and process navigation. Open it with the
visible **⌘K commands** button or `Ctrl-K` / `Cmd-K`, type either ordinary or
wizard vocabulary, use the arrow keys to choose, and press Enter. Unavailable
commands remain visible with a reason. The global shortcut deliberately does
not fire while an input, textarea, select, contenteditable, or embedded editor
owns the keystroke.

`Ctrl-C` / `Cmd-C` copies the nodes in the current graph selection, including
their complete node settings, relative layout, and edges whose two endpoints
are both selected. `Ctrl-V` / `Cmd-V` centers that subgraph at the current
in-canvas pointer; when no trustworthy canvas pointer is available it uses the
visible canvas center. Repeating paste at the same target creates fresh ids and
offsets each copy; moving the pointer starts a new cascade. Each paste is
one undoable editor operation; copying does not modify the draft. Neither path
imports template identity, params, save hashes, run state, or edges crossing
out of the selection. The format is versioned, bounded to 256 KiB, and carried
through the browser's
ordinary text clipboard, so it works between dashboard template editors. Text
fields, embedded editors, and open dialogs retain their native clipboard
behavior. Unrelated clipboard text is ignored, while malformed, stale, or
oversized editor payloads are rejected atomically without changing the graph.

Selected nodes can also be saved as a reusable **custom snippet**. Choose
**Save selection…** in the palette (or **save as snippet…** in the selection
inspector), give it a name, then drag it onto a canvas or use its keyboard-
accessible **Insert** button. The saved value is the same validated v1
selection envelope used by copy/paste: only selected nodes, their relative
layout, and edges with both endpoints selected are retained. Top-level
template/run identity, parameters, save metadata, and crossing edges are never
copied. Parameter or performer references are not silently rewritten for a
different template, so inserting a snippet elsewhere may produce the normal
editor diagnostics until that template declares the referenced values.

Custom snippets are global to the local operator dashboard and persist in its
SQLite store across daemon restarts and random dashboard ports. Names are
unique after trimming and case-folding; snippets can be renamed or deleted
from their palette cards. Built-in snippets remain immutable and visually
distinct. The library allows at most 128 custom entries, 256 KiB per canonical
selection, and 4 MiB total payload. A stale edit is rejected with a conflict;
an incompatible or corrupted stored entry remains visible as unavailable so
it can be renamed or deleted without breaking built-ins or other custom
snippets. Read-only process views disable saving and insertion.

The same palette can **Ask agent about selection**, **Ask agent to fix this
issue**, or **Edit / refactor with agent**. The first two require a live graph
selection or an explicitly focused validation issue. Before anything is sent,
the editor shows an editable human request beside clearly delimited, read-only
context. That context carries the template id, exact ref and source hash, node
ids, edge identities, and diagnostic code/target as applicable. Large graphs
and selections are visibly truncated to a bounded preview; retained rows keep
their stable ids and omitted counts are shown. The preview is orientation only:
the scoped process scribe must reread canonical YAML immediately before editing
and again before its validated CAS save. Sending reuses a compatible live
same-template scribe or safely summons one through the existing least-privilege
lifecycle, then opens its conversation; cancelling returns focus to the graph.
Neither path instantiates or runs a process, and no template or prompt text is
typed into a pane command.

Dragging a node port onto empty canvas opens the same searchable node-type
vocabulary at the release point. Choose with pointer or arrow keys plus Enter;
Escape, Cancel, or clicking away leaves the graph unchanged. The chosen node
is created at the release coordinate and connected in one undoable operation,
and task, decision, and wait nodes open their normal configuration editor.

With Processes enabled, the engine-hosted create endpoint is:

```http
POST /v1/process/runs
Content-Type: application/json

{"templateRef":"release@sha256:<64 hex>","params":{"issue":"TCL-300"},"runId":"optional"}
```

Success returns `201`, a minimal run identity (`id`, `templateRef`, `createdAt`,
`updatedAt`), and a `Location: /v1/process/runs/{id}/view` header. The route
does not expose the pinned template or params in its response and does not opt
the run into program execution. Agent callers need the non-default
`process.runs.create` permission. This is deliberately separate from
`process.templates.manage`: permission to author a template does not confer
permission to instantiate performers. The dashboard operator passes as the
human caller. Successful and denied creation attempts are audited as
`process.run.create`; audit rows never buffer or record the runtime params map.
