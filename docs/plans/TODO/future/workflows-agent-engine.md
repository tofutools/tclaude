# workflows: agent ↔ engine reflection + agent-as-engine (opt-in)

Part of the **Workflows** feature — see `docs/plans/workflows.md`. Sequenced at
the end of the roadmap, but the **agent ↔ workflow-engine interop/reflection here
is a SUPER-IMPORTANT, first-class capability** (operator emphasis), not optional
polish — it's what makes self-aware workers and the agent-as-engine mode possible.
Builds on Step 6 (engine primitives + state) and the `tclaude workflow` CLI
(`workflows-cli.md`, the reflection interface).

## Part A — the reflection/interop contract (the important part)

A **bidirectional** contract between agents and the workflow engine, both
directions first-class:

- **Agent → engine (reflection + control):** an agent can ask "**where am I**
  (which workflow / node), what's my task, what are my interpolated inputs, what
  outcome is expected, what's the history?" and can **act**: start/advance/complete
  a node, set an outcome, capture output, spawn node-agents, and — in the advanced
  case — **expand the graph** (dynamic sub-graphs, `workflows-dynamic-subgraphs.md`).
  Surfaced through `tclaude workflow` (CLI) + a skill so agents reach for it
  naturally, the way `agent-coord`/`agent-lifecycle` work today.
- **Engine → agent (notification):** the engine tells agents when their node is
  ready, delivers handoff captures as inbox messages, and escalates stuck nodes
  (the Step-6 enhancements).

This matters **even in the default system-engine mode**: a worker agent assigned
to a node must be able to reflect on its workflow context to do the node well.
So this contract ships value regardless of which engine drives.

## Part B — agent-as-engine (opt-in execution mode)

Default execution is the **deterministic system engine** (agentd, Step 6): code
advances the graph. Opt-in alternative: a designated **agent is the engine** for
an instance — it uses the reflection contract (Part A) to read state, decide the
next move with judgment, drive nodes, and adapt the flow.

- Per-template/instance flag: `engine: system | agent` (+ which agent
  profile/role drives). Default `system`.
- The system still **persists** state (SQLite) and enforces guards (max_visits,
  approval gates); the agent supplies the *decisions* the deterministic engine
  would otherwise hard-code.
- Why opt into an agent engine: advancement needs judgment hard to encode
  deterministically; adaptive re-planning; driving dynamic sub-graph expansion
  ("investigate → grow a node per object → join"); a human-style overseer that
  watches and intervenes.
- Contrast with the reference's "system is the engine" dogma: tclaude offers
  **both**, user's choice — a deliberate divergence.

## Open / to build (after Step 6 + the CLI)

1. The reflection contract: solid `tclaude workflow where/status/show --json` +
   node-control verbs + an agent skill wrapping them.
2. `engine: system|agent` in `workflow.yaml`; the engine dispatcher honours it.
3. Agent-engine harness: spawn/anchor the engine agent for an instance (in the
   instance group), feed it the reflection interface + a driving prompt, let it
   loop until the instance reaches a terminal state. Guard against runaway via the
   same caps the system engine uses.
4. Tests: a simulated agent-engine driving a small graph to completion through the
   CLI/endpoints (mock the agent's decisions).

## Relevant source files (when built)

- `pkg/claude/workflow` + `pkg/claude/agentd` engine dispatch (Step 6)
- `tclaude workflow` CLI (`workflows-cli.md`) + a new agent skill
- `pkg/claude/agent` group/spawn for anchoring the engine agent

## Open questions

- One engine agent per instance, or a shared overseer across instances?
- How much authority does an agent-engine get (can it edit the template/topology,
  or only advance/spawn within the instance)? Lean: instance-scoped control;
  topology growth only via the sanctioned dynamic-subgraph mechanism.
- Failure handling when the engine agent itself crashes — the system engine should
  be able to take over (state is in SQLite), which is a nice argument for keeping
  the deterministic engine as the substrate even in agent-engine mode.
