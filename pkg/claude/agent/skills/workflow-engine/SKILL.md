---
name: workflow-engine
description: BE the engine for a tclaude `engine: agent` workflow instance — drive the WHOLE graph with judgment via `tclaude workflow`. Use when you've been anchored as a workflow driver (spawned by `tclaude workflow drive <instance>`, or briefed that you are "the engine / driver for workflow instance N"): read the whole graph, decide which ai nodes to spawn workers into, seed each worker the upstream outputs it needs, settle nodes to advance the graph, and loop until the instance is terminal. This is the WHOLE-graph counterpart to the `workflow-node` skill (which drives a single assigned node). NOT the Claude Code harness `Workflow` tool / `/workflows` — this is the daemon-backed `tclaude workflow` CLI.
---

# workflow-engine: drive a whole `engine: agent` workflow

A **tclaude workflow** is a DAG of nodes the `tclaude agentd` daemon executes.
Most instances run in the default **`engine: system`** mode — the daemon
advances the graph deterministically (auto-spawns workers, auto-advances on
settle). An **`engine: agent`** instance is different: the daemon deliberately
steps back and **YOU supply the judgment** — which worker to spawn, what to
hand it, which branch to take, when to advance. You are the engine.

> Not the Claude Code harness `Workflow` tool or the `/workflows` command (the
> in-process JS orchestrator). This skill wraps the `tclaude workflow` CLI, a
> thin client over a running `tclaude agentd`.

If you instead need to drive a single node you were *assigned*, that's the
sibling **`workflow-node`** skill — not this one.

## What the daemon still does for you (and what it won't)

You are NOT a second engine reimplementing the daemon. The daemon stays the
substrate; you supply only the decisions it would otherwise hard-code:

- **The daemon STILL runs mechanical tool/program nodes** and settles them — you
  never shell a node's template command yourself. Leave tool nodes to it.
- **The daemon STILL enforces guards** — `max_visits` (loop/runaway cap),
  approval gates, outcome validation — on the shared settle path. You cannot
  bypass them, and you shouldn't try.
- **The daemon STILL persists everything** to SQLite and escalates a stuck
  instance to the human (the JOH-41 sweep). So if you stall or die, the human
  finds out.
- **The daemon will NOT auto-spawn ai-node workers, will NOT auto-advance, and
  will NOT auto-deliver handoffs** for your instance. Those are *your* calls.

**The seeding contract (important):** because the daemon does not auto-handoff
in agent mode, **you own data routing.** When an upstream ai node produces
output that a downstream node needs, YOU read it from `status` and seed it into
the downstream worker at spawn (`--context`). A worker also self-orients via its
own `workflow where` (interpolated inputs) + `workflow status` (peer outputs),
but `--context` is how you hand it the specific upstream result it should act
on. (Note: an ai node's reported `--output` is visible in `status` but is NOT
auto-interpolated into a downstream `{{node.output}}` — so reading it and
seeding it is on you.)

## The drive loop

Repeat until the instance reaches a terminal state (`completed` / `failed` /
`cancelled`):

```bash
# 1. READ the whole graph — node statuses, outcomes, captured outputs, vars, events.
tclaude workflow status <instance> --json

# 2. SPAWN a worker into each ready ai node, seeding the upstream outputs it needs.
#    You own data routing: summarise the relevant upstream output into --context.
tclaude workflow spawn <instance> <node> --context "<concise upstream summary>"
#    (large context? write it to a file and use --context-file <path>, or '-' for stdin)

# 3. SETTLE a node whose worker has finished, to advance the graph.
tclaude workflow node <instance> <node> done --outcome <outcome> --output "<summary>"
#    or, if it cannot be completed:
tclaude workflow node <instance> <node> fail --output "<why>"

# 4. WAIT for the frontier to change (see "Waking up" below), then loop to step 1.
```

`<instance>` is the numeric instance id you were given; `<node>` ids come
straight out of `status`.

### 1. Read — `status`

`tclaude workflow status <instance> --json` is your whole-board view: every
node with its status (`pending` / `ready` / `running` / `awaiting_verify` /
`done` / `failed` / `skipped`), its outcome and captured `output`, the shared
`vars`, and recent `events`. This is the only state you need — re-read it every
loop. **Hold no authoritative state yourself**: the instance lives in SQLite, so
you can reincarnate or be resumed mid-flight and just pick up from `status`.

`tclaude workflow show <ref>` shows the underlying template (params, node
summary, mermaid chart) if you need the topology; the `template:` line in
`status` names the ref.

### 2. Spawn — `spawn … --context`

For each node that is `ready` and is an **ai** node, decide whether to spawn a
worker now (respect any logical ordering — don't spawn a node whose inputs
aren't ready yet) and launch one:

```bash
tclaude workflow spawn <instance> <node> --context "Upstream <X> produced: <summary>. Use it to …"
```

- The worker spawns into the instance's **bound group**, is assigned to the
  node, and is briefed with the node's interpolated task prompt **plus** your
  `--context` seed.
- **Seed concisely.** Summarise large upstream outputs rather than pasting them
  whole — the brief lands in the worker's inbox. For genuinely large context,
  `--context-file <path>` (or `--context-file -` for stdin) sidesteps shell
  quoting.
- Tool/program nodes are NOT spawned — the daemon runs those. `spawn` targets a
  ready ai node; it errors otherwise.
- Tip: tell the worker, in the `--context`, to message you (the driver) when it
  finishes — that lets you react faster than polling alone.

### 3. Settle — `node … done|fail`

When a worker reports its node finished (via your inbox, or you see its node go
`done` in `status`), advance the graph. As the bound-group **owner** you have
graph-level drive authority, so you can settle any node — you don't need to be
its assignee:

```bash
tclaude workflow node <instance> <node> done --outcome <outcome> --output "<summary>"
tclaude workflow node <instance> <node> fail --output "<reason>"
```

- `--outcome` chooses the branch the engine follows next; it's validated against
  the node's allowed outcomes (`outcomes:` in `status`/`show`). Required for
  enum-verified nodes.
- On a successful settle the command reports which downstream nodes were
  **readied** or **skipped** — that's your next frontier.
- There is no `skip`: branch-skipping happens automatically on settle. To
  abandon the whole instance, `tclaude workflow cancel <instance>`.

If a worker already settled its own node (a `workflow-node` worker does this),
you don't re-settle it — you just observe it `done` in `status` and move on to
the newly-ready nodes.

### 4. Waking up (self-paced for now)

You only act when something changes (a worker finishes, a tool node completes).
How you learn of a change is a **pluggable wake mechanism**; the v1 default is
**self-paced**:

- **Watch your inbox** — read messages from workers ("done with node X") and
  the human. A `[system: new agent message #…]` line means fetch it.
- **Poll `status`** on a modest cadence when you're waiting on a node — schedule
  a periodic self-check with `tclaude agent cron add` (the `agent-schedule`
  skill) or run this drive check on a `/loop`, e.g. every ~30–60s. Re-read
  `status`, act on any newly-`done`/`ready` node, then wait again.

Don't busy-spin: a tight poll just burns tokens. Pace yourself to how fast the
nodes actually complete. (A future enhancement will have the daemon nudge you
the instant the frontier changes, so you can stop polling — but until then,
self-pace.)

## Reaching a terminal state

When `status` shows the instance `completed` (or `failed` / `cancelled`):

- Stop the loop — cancel any self-scheduled poll (`tclaude agent cron rm`).
- Report the outcome to whoever asked you to drive it (the human, or your
  spawner) — a short summary of what the graph produced and any failures.
- If it `failed` and you can see a recoverable cause, say so; don't silently
  retry past `max_visits` (the daemon halts runaway loops on purpose).

## Authority, identity & the one-driver rule

- Your drive authority is **group-ownership** of the instance's bound group —
  `tclaude workflow drive` granted it to you when it anchored you. That's what
  lets you `spawn` workers and `settle` any node.
- **One driver per instance.** Don't anchor a second driver for an instance
  you're already driving — two drivers racing the frontier will double-spawn.
  If `workflow drive` warned you that the group already has a live agent-owner,
  confirm you're not stepping on an existing driver before proceeding.
- You can be **reincarnated** safely: you hold no authoritative state, so a
  fresh successor re-reads `status` and continues. (See the `agent-lifecycle`
  skill for managing your own context.)

## Prerequisites & errors

- **Daemon must be running.** Every verb here talks to `tclaude agentd` over its
  Unix socket. `Error: tclaude agentd is not running.` → ask the human to start
  it (`tclaude agentd serve`).
- **Engine mode.** `spawn` and the drive loop assume an `engine: agent`
  instance. `workflow drive` refuses a system-mode instance (the daemon already
  drives those). Check the `engine:`/mode in `status` if unsure.
- **Permission / ownership.** A refusal exits with an auth error (distinct from
  a transport failure). If `spawn`/`node` is refused, confirm you're still a
  group owner of the instance's bound group.
- **`--json` everywhere.** Add `--json` to any read verb and pipe to `jq` to
  pull a single field — handy for scripting your decisions.
