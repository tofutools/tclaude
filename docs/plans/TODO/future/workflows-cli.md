# workflows: `tclaude workflow` CLI

Part of the **Workflows** feature — see `docs/plans/workflows.md`. A CLI surface
so **agents (and humans) can introspect and drive workflows from the terminal**,
not only the dashboard. A thin client over the agentd socket, mirroring how
`tclaude agent` works. Depends only on Step 3 (the agentd HTTP API it wraps).

> Sequenced near the **end** of the roadmap per the operator — but the
> **agent ↔ workflow-engine reflection/interop this provides is SUPER-IMPORTANT**
> (operator emphasis; see `workflows-agent-engine.md`), and it's buildable right
> after Step 3, so it can be pulled forward if that interop is wanted sooner.

## Why

- A worker agent assigned to a node needs to answer "**where am I** in the
  workflow, what's my node, what are my inputs, what's expected of me?"
- Agents should be able to **install**, **list**, **instantiate**, and **monitor**
  workflows programmatically — e.g. the agent-as-engine mode
  (`workflows-agent-engine.md`) drives the whole flow through this CLI.
- Editing templates does **not** need a CLI verb — templates are plain files on
  disk (`~/.tclaude/workflows/<name>/`); agents edit them directly. The CLI is
  for *runtime* introspection/control + installation, not authoring.

## Proposed surface

```
tclaude workflow ls                       # discovered templates + instances (status, progress)
tclaude workflow show <ref>               # render a template: mermaid + node summary
tclaude workflow templates                # just templates (project/user/example)
tclaude workflow install <src> [--name]   # copy a dir / clone a git: source into ~/.tclaude/workflows
tclaude workflow new <ref> [--param k=v]... [--title T]   # instantiate; prints instance id
tclaude workflow status <instance>        # instance detail: per-node status, outcomes, vars
tclaude workflow where [--instance <id>]  # "which workflow/node am I (the caller) assigned to?"
tclaude workflow node <instance> <node> {start|done [--outcome v]|fail|skip} [--output ...]
tclaude workflow events <instance> [<node>]   # audit timeline
tclaude workflow cancel <instance>
tclaude workflow rm <instance>
```

- All of these are thin clients over the Step 3 agentd endpoints (over the Unix
  socket), so the daemon stays the single owner of the DB. `where` resolves the
  caller's conv-id (socket peer identity, like `tclaude agent whoami`) →
  `workflow_nodes.assignee` matches across running instances.
- `install` for a `git:` source reuses the Step 7 fetch/cache, then copies into
  the user workflows dir (or just records the ref — decide).
- Output: human table by default; `--json` for agent consumption.

## Open / to build (after Step 3)

1. `tclaude workflow` cobra subtree (boa `CmdT`), under `pkg/claude/workflow` CLI
   or a new `pkg/claude/workflowcli`, talking to agentd like `pkg/claude/agent`.
2. New agentd endpoints only where the dashboard ones don't already cover it —
   notably `where` (caller→assignee lookup) and `install`.
3. `--json` everywhere for programmatic use.
4. Tests: flow tests through the daemon mux (mirror the agent/cron flow tests).

## Relevant source files (when built)

- `pkg/claude/agent/` — the thin-client pattern to mirror (socket calls, identity)
- `pkg/claude/agentd/dashboard_workflows.go` (Step 3) — endpoints to reuse/extend
- `pkg/claude/workflow` — discovery/install helpers

## Open questions

- Should `where`/`status` also be exposed to the agent via a skill (so agents
  reach for it naturally), like the agent-coord/agent-lifecycle skills? Likely yes.
- `install` semantics for `git:` — copy-in (snapshot) vs reference-only.
