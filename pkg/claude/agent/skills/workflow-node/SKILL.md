---
name: workflow-node
description: Self-orient and drive your assigned tclaude workflow node from the terminal via `tclaude workflow`. Use when you're an agent that has been spawned onto (or assigned to) a workflow node and need to find out which instance/node you are, what context and inputs it carries, and what outcome is expected — then report your node running / done / failed so the engine advances the graph. Triggered by being placed in a group bound to a workflow, an initial message that mentions a workflow node, or the user asking you to check / drive a tclaude workflow. NOT the Claude Code harness `Workflow` tool / `/workflows` — this is the daemon-backed `tclaude workflow` CLI.
---

# workflow-node: drive your assigned workflow node

A **tclaude workflow** is a DAG of nodes the `tclaude agentd` daemon
executes. When a node's work is an agent task, the engine spawns (or
points) an agent at that node and waits for the agent to settle it. If
that's you, this skill is your manual: **orient** (which node am I, what's
expected?), do the work, then **settle** the node so the graph advances.

> Not the Claude Code harness `Workflow` tool or the `/workflows` command
> (the in-process JS orchestrator). This skill wraps the `tclaude workflow`
> CLI, a thin client over a running `tclaude agentd`.

## The core loop

```bash
# 1. ORIENT — what node am I on, and what does it want?
tclaude workflow where

# 2. CONTEXT — the whole instance (siblings, vars, recent events)
tclaude workflow status <instance>

# 3. START — mark your node running before you begin
tclaude workflow node <instance> <node> start

#    … do the node's actual work …

# 4. SETTLE — report the result so the engine advances
tclaude workflow node <instance> <node> done  --outcome <v> --output "<summary>"
#  or, if it can't be completed:
tclaude workflow node <instance> <node> fail  --output "<why>"
```

`<instance>` is the numeric instance id; `<node>` is the node id within it.
Both come straight out of `where` / `status`.

## 1. Orient — `where`

`where` answers **"which workflow node(s) am *I* assigned to?"** It keys off
your agent identity (your conv-id), so it shows *your* frontier — not the
whole board.

```bash
tclaude workflow where                 # your live assignments
tclaude workflow where --instance 7    # limit to one instance
tclaude workflow where --all           # include finished instances / settled nodes
tclaude workflow where --json          # machine-readable (pipe to jq)
```

For each assignment it prints the instance (id, title, status), the
template ref, and your node — its id, label, status, and crucially its
**allowed outcomes** and any **output** already attached:

```
instance #7  release-cut  [running]
  template: user:release
  node:     build  "Build the release artifact"  [running]
  outcomes: ok, flaky, fail
  output:   prior attempt left ./out stale
```

Those `outcomes:` are the exact values `node ... done --outcome` will
accept (see settling, below) — a node's declared outcomes always
include `fail`, which you reach with the `fail` action rather than
`done --outcome fail`. If `where` prints *"(no caller identity …)"*
you're running without an agent identity — a human should use
`tclaude workflow ls` / `status` instead. If it prints *"(you are not
assigned to any live workflow node)"*, nothing is waiting on you; try
`--all` to see settled/finished assignments.

## 2. Context — `status`, `show`, `events`

```bash
tclaude workflow status <instance>     # full instance: every node + state,
                                       #   outcomes, params, vars, recent events
tclaude workflow show   <ref>          # the TEMPLATE behind it: params, node
                                       #   summary, mermaid flow chart
tclaude workflow events <instance> [<node>]   # the audit timeline
```

- `status` takes the numeric **instance id** — use it to see your node in
  the context of its siblings and the instance's shared `vars`.
- `show` takes a **template ref** (a bare name, or `project:` / `user:` /
  `example:` / `dir:` / `git:` qualified — the `template:` line in
  `status` tells you which). It reads templates straight off disk, so it
  works even with no daemon.
- Every verb takes `--json` for scripting.

## 3 & 4. Drive — `node <instance> <node> <action>`

```bash
tclaude workflow node <instance> <node> start
tclaude workflow node <instance> <node> done [--outcome <v>] [--output "<text>"]
tclaude workflow node <instance> <node> fail [--output "<text>"]
```

| Action  | Meaning |
|---------|---------|
| `start` | Mark the node **running** — call when you begin work on it. |
| `done`  | Settle the node **succeeded**. `--outcome` picks the branch the engine takes next. |
| `fail`  | Settle the node **failed**. Halts the instance unless the node declares `on_fail: continue`. |

Flags:

- **`--outcome <v>`** — only valid with `done`. It chooses which outgoing
  edge the engine follows, and is validated against the node's
  `allowed_outcomes` (the `outcomes:` line from `where` / `show`). For a
  node whose outcomes are an enum, it's **required** — settling without it
  is rejected. Passing `--outcome` to `start` or `fail` is an error (`fail`
  is always the failure outcome server-side).
- **`--output "<text>"`** — attach a short captured-output summary to the
  node (what you produced, where the artifact is, the key result). Visible
  in `status` and to whoever picks up downstream. Optional but good
  manners — it's how the next node, or a human, sees what you did.

On a successful settle the command reports the node's new status, the
instance status, and which downstream nodes were **readied** or
**skipped** — so you can see the graph move:

```
node build → done (instance: running)
  readied: test, package
  skipped: hotfix
```

There is deliberately **no `skip`** action: branch-skipping happens
automatically when a node settles (the unchosen branches are skipped for
you). To abandon a whole instance, use `tclaude workflow cancel
<instance>` — not a per-node skip.

## When to use this skill

- You were spawned into a group bound to a workflow, or your initial
  message references a workflow node → run `where` to find yourself.
- You're about to start your node's work → `node … start`.
- Your node's work is finished → `node … done [--outcome v] --output "…"`.
- Your node can't be completed → `node … fail --output "<reason>"`.
- The user asks you to check, report on, or advance a tclaude workflow.

**Always settle.** A node you started but never `done`/`fail` strands the
whole instance behind you — the engine is waiting on your report. If you
can't finish, `fail` it with a reason rather than going silent.

## Other verbs (for completeness)

Driving your own node rarely needs these, but the same CLI offers:

```bash
tclaude workflow ls                    # all instances + discoverable templates
tclaude workflow templates             # just the discoverable templates
tclaude workflow new <ref> [--param k=v]... [--title T] [--group G]   # instantiate
tclaude workflow cancel <instance>     # cancel an instance (skips every non-terminal node)
tclaude workflow rm <instance>         # delete an instance and its nodes/events
tclaude workflow install <dir:|git: src> [--name N] [--force]         # install a template
```

## Prerequisites & errors

- **Daemon must be running.** Every instance verb (`where`, `status`,
  `events`, `node`, `new`, `cancel`, `rm`, `ls`) talks to `tclaude agentd`
  over its Unix socket. If you see `Error: tclaude agentd is not running.`,
  ask the human to start it: `tclaude agentd serve` (in a non-sandboxed
  terminal). `show` / `templates` read disk and work without it.
- **Permission / ownership.** The daemon authorises by socket peer
  identity: you can only settle a node you're the assignee of (and only
  `new` against a group you own). A refusal exits with an auth error,
  distinct from a transport failure — re-check `where` to confirm the node
  is really yours.
- **`--json` everywhere.** Add `--json` to any verb for structured output;
  pipe into `jq` to pull a single field.

These verbs only *read and advance* node state — they don't run your node's
work. Do the work, then report the outcome.
