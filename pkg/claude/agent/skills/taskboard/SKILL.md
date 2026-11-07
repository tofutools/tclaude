---
name: taskboard
description: >-
  Read and work a group's Task board via `tclaude agent taskboard`: see what
  your group is doing now and next (ls, show, activity, comment), capture and order tasks
  (add, edit, move, promote, demote, block, drop, done), take a queued task or
  have one assigned to you, and report done or failed on it (take, release,
  report). Use when a `[taskboard]` mail arrives in your inbox, when the
  operator or your lead says "put it on the board", "what's next for the group",
  "take the next task", or when you finish or give up on a task you were
  assigned. Not the backlog: long-term work belongs in the external tracker
  (awb, Linear or similar). Not for Collab drawing boards (that is agent-drawing-board)
  or the task-reference link (agent-task).
---

# Task board

The group's Task board is the short-term view of what is happening now and next (about ten tasks). It is not the backlog. The backlog is the external tracker (awb, Linear or similar). Add a ref when you create a task from the tracker. If a task is paused for longer, drop it from the board and park it in the tracker; its ref stays in the board's history. Report done or failed on tasks assigned to you; reporting does not end your session.

## The loop

```bash
tclaude agent taskboard ls                 # your group's board: the queue (next), then ideas
tclaude agent taskboard ls --mine          # the tasks assigned to you, in every group
tclaude agent taskboard show tk_1a2b3c4d   # brief, ref, dependencies, who holds it
tclaude agent taskboard take tk_1a2b3c4d   # take a queued, unassigned task yourself
# … do the work …
tclaude agent taskboard report done --note "merged #4126"
tclaude agent taskboard report failed --note "CI image lacks bubblewrap"
tclaude agent taskboard release tk_1a2b3c4d   # give it back without a report
```

- When a task is assigned to you, an inbox message from the assigner
  (`[taskboard] assigned to you by …: <group> · tk_… "title"`) carries the brief
  and the exact report commands. You don't need anything else.
- `report` names the task with `--task`. You can leave it out when exactly one
  live task is assigned to you.
- `done` closes the task. `failed` gives it back and counts a failure; after
  three failures the task is blocked for the lead to decide. A repeated report
  is acknowledged, not counted twice. A report after you were unassigned is
  refused (`not_assignee`).
- Reporting never ends, retires or stops you. You stay in the group and can
  take the next task.
- If you sit idle on an assigned task without reporting (5 minutes by
  default), the task shows as stalled, whoever assigned it is told, and you
  get a `[taskboard] you went idle without reporting: …` nudge. Report, or carry
  on if you are still on it; starting a new turn clears it. Nothing fails or
  is taken from you.
- A task can carry files (screenshots, logs, specs). `show` and the brief list
  them; `tclaude agent taskboard attachments tk_… --save <dir>` writes them out
  for you to read.
- Talk about a task on the task itself: `tclaude agent taskboard comment add tk_… "markdown"`
  (`--attach f`, `--file note.md`). `activity tk_…` shows the comments and every change,
  oldest first. A `[taskboard] … comment by …` mail means someone commented on a task you hold
  or created; answer with `comment add`. You can edit or remove only your own comments
  (`comment edit|rm tk_… #id`).
- You can only take a task from the queue (`next`), not an idea. If the right
  next step is an idea, ask whoever leads the group to promote it.

## Capturing and ordering

```bash
tclaude agent taskboard add <group> "title" [--ref tcl-a1b2c3|LIN-12|URL] [--next] [--body-file brief.md] [--attach shot.png]…
tclaude agent taskboard add <group> "title" --assignee <agent>      # starts in progress (assigned); --blocked "why" starts blocked; --done logs finished work
tclaude agent taskboard attach tk_… <file>…   ;   detach tk_… <name>   # at most 10 files, 4 MiB each
tclaude agent taskboard move tk_… --top | --before tk_… | --after tk_…
tclaude agent taskboard promote tk_…   ;   tclaude agent taskboard demote tk_…
tclaude agent taskboard block tk_… --reason "waiting on review"   ;   unblock tk_…
tclaude agent taskboard drop tk_… --reason "parked in awb"
tclaude agent taskboard move-to-group tk_… <other-group>[@<peer>] [--clone] [--stage next]   # @peer: created there, then dropped here
```

New tasks land in the ideas unless you pass `--next`. Keep the board short.
When something will not happen soon, make sure the tracker has it, then
`drop` it.

### When you are asked for tasks

A `[taskboard] task request from …: <group>` message (the dashboard's
*+ New task › Ask an agent*, or `taskboard ask`) carries a request in prose.
Turn it into one or more well-formed tasks with `taskboard add`: a one-line
title, a short `--body` brief, a `--ref` where there is one, and `--next` only
when it is clearly next. If the request came with files, save them
(`tclaude agent inbox attachment <message id>`) and put each on its task with
`--attach`. Create the tasks only; do not start the work. Then reply to the
message with the task ids.

## Permissions

- Members read and change their own group's board and take its tasks. If the
  board is locked (`groups set-taskboard-writers`), only the listed writers,
  owners and agents with an explicit grant may change it or take tasks. Reports
  are never locked.
- Assigning another agent, taking a task back from it, and closing a task
  assigned to someone else need `taskboard.assign`. Group owners have it for
  their groups.
- Moving a task to another group needs `taskboard.transfer` on that group (its
  owners have it) and, on the source, `taskboard.write` for your own task or
  `taskboard.manage` for someone else's. `--clone` needs only read on the
  source. A move drops the task's dependencies and unassigns an assignee who
  is not a member of the new group.
- Another group's board needs `taskboard.read` / `taskboard.write`, scoped with
  `--scope group=<name>`. Without access, a task id from that group is reported
  as not found.
- Every changing verb accepts `--ask-human 60s` to ask the operator on a
  denial.

See `docs/taskboard.md` for the full reference.
