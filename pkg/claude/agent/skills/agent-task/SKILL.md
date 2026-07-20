---
name: agent-task
description: >-
  Set, clear, or show an agent's task-reference link — the clickable URL
  (a Linear issue, GitHub issue/PR, ticket, …) shown in the dashboard's Task
  column next to each agent — via `tclaude agent task set|clear|show`. Use to
  record what work item YOU are on (`self.task`, default-granted), or, when
  spawning workers, to point each one at its issue: `tclaude agent spawn --task
  <url>`. Manager pattern: `tclaude agent task set <url> --target <peer>` sets
  ANOTHER agent's link (requires the `agent.task` slug, OR being an owner of a
  group containing the target).
---

# Task-reference links

Every agent can carry an optional **task-reference link** — an http(s)
URL pointing at the work item it's on (a Linear issue, a GitHub
issue/PR, a ticket). The dashboard renders it as a clickable label in
the **Task** column next to the agent, so a human (or a lead) can jump
straight from a worker to what it's working on.

The label is derived from the URL automatically:

- `https://linear.app/acme/issue/JOH-353/…` → **JOH-353**
- `https://github.com/owner/repo/issues/42` → **#42**
- anything else → the host

You can override the label with `--label` if you want something else.

## Prerequisite: daemon must be running

If you see `Error: tclaude agentd is not running.`, ask the human to
start it:

```bash
tclaude agentd serve   # in a non-sandboxed terminal
```

## Prerequisite: self.task permission

Setting your own link is opt-in. The fastest path is
`tclaude setup --install-default-agent-permissions`, which grants
`self.task` (alongside the other self-lifecycle default slugs —
`self.rename`, `self.compact`, `self.clone`,
`self.schedule`, `self.remote-control`) in one shot. Manual alternatives:

```bash
tclaude agent permissions grant default self.task      # every agent
tclaude agent permissions grant <conv-id-or-title> self.task   # one agent
```

If you see `Error: caller is not granted permission "self.task"`, the
human hasn't opted in — quote one of the commands above so they know
exactly what to run.

## Setting your own task link

```bash
tclaude agent task set https://linear.app/acme/issue/JOH-353/wire-task-links
```

Optional custom label, and reading / clearing it back:

```bash
tclaude agent task set https://github.com/tofutools/tclaude/pull/42 --label "PR 42"
tclaude agent task show     # print your current link
tclaude agent task clear    # remove it
```

The URL must be **http(s)** — a `javascript:`/`data:`/other-scheme URL
is rejected (`invalid_task_url`, HTTP 400), because it renders as a link
in the dashboard. The link is stored on YOU (the agent), so it survives
a reincarnate/clone and shows in every group you're in — you never pass
a group name.

Good moment to set it: right after you pick up a task that maps to a
tracked issue, so the human can see at a glance which agent owns which
ticket.

## Pointing spawned workers at their issue

When you spawn a worker, hand it its task link up front with `--task`
(and optionally `--task-label`) so its Task column is populated the
moment it appears:

```bash
tclaude agent spawn my-group --name auth-worker \
  --task https://linear.app/acme/issue/JOH-360/oauth-refactor \
  --initial-message "Refactor the OAuth flow per JOH-360."
```

This is the common lead pattern: dynamically spawn N workers, each
pointed at its own Linear issue, and watch them in the dashboard with a
clickable link next to every one. The same http(s) validation applies —
a bad URL fails the spawn with a 400 rather than being silently dropped.

As with any spawn, prefer an operator-preconfigured spawn profile
(`--profile <name>`, or the group/global default) over hand-picking
launch flags — see the **`agent-coord`** skill's spawning section.

## Manager pattern: set ANOTHER agent's link

`tclaude agent task set|clear` accept an optional `--target <selector>`
that acts on a peer instead of yourself — for updating a worker's link
after it has already spawned:

```bash
tclaude agent task set https://linear.app/acme/issue/JOH-361 --target auth-worker
tclaude agent task clear --target auth-worker
```

Auth model: the caller passes if EITHER

- they hold the `agent.task` slug (granted via
  `tclaude agent permissions grant <caller> agent.task`), OR
- they own at least one group that contains the target.

The response includes `caller_conv` so the audit trail records who set
it. `--ask-human` is honored on the self path only; the cross-agent path
is opt-in via explicit grants or group ownership.

## What can go wrong

- **`invalid_task_url` (400).** The URL isn't http(s), has no host, or
  is over the 2048-char cap. Fix the URL; don't retry the same one.
- **`invalid_task_label` (400).** An explicit `--label` over 200 chars.
  Task labels are short by design (a ticket id) — shorten it.
- **`caller is not granted "self.task"` / a 403 on `--target`.** A
  permission gap — see the prerequisite section (self) or the manager
  auth model (cross-agent) above.
