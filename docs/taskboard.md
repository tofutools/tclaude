# Group task boards

A group's **task board** is a short ordered list of tasks: what the group is on
right now and the next few things it will do. Each [agent group](agents-and-groups.md)
has one board. The operator and the group's agents read and change it with
`tclaude agent taskboard`.

> The task board is in development and is off by default. Turn it on with
> `"features": {"taskboard": true}` in `~/.tclaude/config.json`. While it is
> off, its routes answer 404, but stored tasks are kept. Importing refs from
> trackers and the dashboard section come in later slices.

## What the board is for, and what it is not

The board is **short-term and volatile**. It shows right now and the next few
tasks, rarely more than about ten per group. It is **not the backlog**:

- The durable backlog lives in an external tracker: awb, Linear, GitHub or
  similar. A task on the board may link to its item there with `--ref`.
- New work comes onto the board in one of two ways. Either you create it
  directly, as a quick capture of an idea, or you pull it in from the tracker.
- When work is paused for longer, it goes back to the tracker and comes off the
  board. Make sure the tracker item exists, then `drop` the task. Its ref and
  its history stay, so you can still find it.

Agents should treat the board the same way. Don't use it to keep long-term
items or as a record of everything planned. Park those in the tracker.

## Stages and statuses

Every task has a **stage** and a **status**. Both are stored and set by hand.
Neither one is inferred from age or position.

| Stage | Meaning |
|---|---|
| `idea` | Captured but not queued yet. `add` puts a task here unless you pass `--next`. |
| `next` | In the queue. The top task is the next one to do. |

| Status | Meaning |
|---|---|
| `open` | On the list. |
| `blocked` | On the list, with a reason. |
| `done` | Closed as finished. |
| `dropped` | Taken off the board, for example parked back in the tracker. |

`ls` shows the queue (`next`) first, then the ideas, each in the operator's
order. Blocking, closing and reopening a task never changes its stage, and
reopening puts it back where it was. A task can depend on others (`--after`).
An open task with a dependency that is not yet done shows as `queued`.
A dropped dependency counts as not done, so the task stays queued until you
remove the dependency (`edit --rm-dep`) or reopen the dropped task.

## Commands

```bash
tclaude agent taskboard ls [group] [--stage idea|next] [--all] [--mine] [--json]
tclaude agent taskboard show <task>        # tk_… id
tclaude agent taskboard history <task>

tclaude agent taskboard add <group> "title" [--body …|--body-file f] [--ref URL|id]
                                    [--next] [--after tk_a,tk_b] [--top|--before tk_x]
tclaude agent taskboard edit <task> [--title …] [--body …|--clear-body] [--ref …|--clear-ref]
                                    [--add-dep tk_…] [--rm-dep tk_…]
tclaude agent taskboard move <task> --top|--bottom|--before tk_x|--after tk_x
tclaude agent taskboard promote <task>     # idea → end of the queue
tclaude agent taskboard demote <task>      # next → top of the ideas
tclaude agent taskboard block <task> --reason "waiting on CI"
tclaude agent taskboard unblock <task>
tclaude agent taskboard drop <task> [--reason "parked in awb"]
tclaude agent taskboard done <task>

tclaude agent taskboard assign <task> --to <agent>
tclaude agent taskboard unassign <task>
tclaude agent taskboard take <task>
tclaude agent taskboard release <task>
tclaude agent taskboard report done|failed [--task <task>] [--note "…"]

tclaude agent groups set-taskboard-writers <group> [agent…] | --members | --show
```

- If you leave out the group in `ls`, it lists your own group's board. This only
  works when you are an active member of exactly one group.
- `ls --all` also lists done and dropped tasks that are not archived yet.
- If `move --before` or `move --after` names a task in the other stage, the
  moved task is promoted or demoted in the same write.
- `--ref` takes an `http(s)` URL, `owner/repo#12` for GitHub, or a bare tracker
  id. `LIN-123` is read as a Linear id, and a lower-case `tcl-a1b2c3` as an awb
  id. Prefix the id with `awb:` or `linear:` to say which one you mean. A bare id
  is stored with no link for now; a later slice resolves it through the tracker
  proxies.
- Every changing verb accepts `--ask-human <timeout>` and `--json`.
- Titles are one line of UTF-8, up to 200 bytes, with no control characters.
  A body can be up to 8 KiB.

## Assigning and reporting

A task has at most one assignee. The assignee must be an active member of the
task's group.

- `assign --to <agent>` gives the task to a member. If the task is an idea, it
  is promoted to the end of the queue first. The assignee gets an inbox message
  from you, `[taskboard] <group> · tk_… "title" — assigned to you by <you>`,
  with the brief, the ref and the exact report commands. The message is
  delivered as inbox mail, never typed into the agent's pane.
- `take` assigns a task to yourself. You can only take a task that is queued
  (`next`), open and unassigned. `release` gives it back.
- `unassign` takes a task back from its assignee. The assignee is told by inbox
  mail.
- `report done` closes the task. `report failed` counts a failure and gives the
  task back. After three failures the task is blocked with the reason
  "3 failed attempts". If an agent assigned the task, that agent is told about
  every report.
- Only the current assignee can report. A report from someone who has been
  unassigned, or whose task was reassigned, is refused with `not_assignee`
  (409). Sending the same report twice is acknowledged without being applied
  again. `--task` may be left out when exactly one live task is assigned to you.
- An assignment never retires, releases or stops the agent. `done` leaves the
  agent where it is, ready to take the next task. Retiring an agent unassigns
  its live tasks; the history records "assignee retired". Reincarnation keeps
  the assignment, because the agent id is stable. A clone does not inherit it.
- `ls --mine` lists the live tasks assigned to you, in every group.

Assigned tasks show `@<agent>` in `ls`, and their history records who
assigned whom.

## The writer lock

By default, every member of a group may change its board and take its tasks.
`groups set-taskboard-writers <group> <agent>…` narrows this to the listed
members. Owners of the group, agents with an explicit `taskboard.write` or
`taskboard.assign` grant, and the operator are not affected. With no agents
listed, the board is locked to owners only. `--members` opens it to every
member again, and `--show` prints the current lock. A locked change is refused
with `taskboard_locked`. Reporting on your own task is never locked, because
whoever assigned it already authorised the work.

## Permissions

The operator passes every gate. For agents:

| Slug | What it allows | Who has it without a grant |
|---|---|---|
| `taskboard.read` | `ls`, `show`, `history` | Members and owners of the group |
| `taskboard.write` | Everything that changes a task. Implies read. | Members and owners of the group |
| `taskboard.assign` | Assigning another agent, taking a task back, and closing a task assigned to someone else. Scope it with `group` and `target_agent`. | Owners of the group |
| `taskboard.manage` | The writer lock | Owners of the group |
| `self.taskboard` | Taking and releasing a task for yourself, and reporting on your own task | Default-granted |

`done` on an unassigned task needs `taskboard.write`. On your own task it needs
`self.taskboard`, and on a task assigned to someone else, `taskboard.assign`.

None of the `taskboard.*` slugs are default-granted. Access to another group's
board is an ordinary scoped grant:

```bash
tclaude agent permissions grant steward taskboard.read taskboard.write --scope group=tclaude,website
tclaude agent permissions grant steward taskboard.read   # unscoped: every group, including future ones
tclaude agent permissions grant lead taskboard.assign --scope group=tclaude --scope target_agent=@descendants
```

An explicit deny wins over membership and ownership, as it does everywhere.
If an agent asks for a task in a group whose board it cannot read, the answer
is 404 with "no such task". This way the reply does not reveal that the task
exists.

## Lifecycle and retention

A done or dropped task leaves the default list at once. An hourly sweep then:

- archives it after `taskboard.archive_after_days` (default 14), which hides it
  from `ls --all`;
- deletes it, with its history, after `taskboard.retention_days` (default 90).

```json
{ "taskboard": { "archive_after_days": 14, "retention_days": 90 } }
```

A group holds at most 500 live tasks, and a task can have at most 50
dependencies. Deleting a group deletes its board.

## API

| CLI | HTTP |
|---|---|
| `ls <group>` | `GET /v1/groups/{g}/taskboard[?stage=&all=1]` |
| `ls` (own group) | `GET /v1/taskboard` |
| `show` / `history` | `GET /v1/taskboard/tasks/{id}` / `…/events` |
| `add` | `POST /v1/groups/{g}/taskboard/tasks` |
| `edit` | `PATCH /v1/taskboard/tasks/{id}` |
| `move` | `POST /v1/taskboard/tasks/{id}/move` |
| `promote` / `demote` | `POST /v1/taskboard/tasks/{id}/stage {"stage":"next"\|"idea"}` |
| `block` / `unblock` / `drop` / `done` | `POST /v1/taskboard/tasks/{id}/status` |
| `assign` / `take` | `POST /v1/taskboard/tasks/{id}/assign {"to":"<agent>"\|""}` |
| `unassign` / `release` | `POST /v1/taskboard/tasks/{id}/unassign` |
| `report` | `POST /v1/taskboard/tasks/{id}/report {"outcome":"done"\|"failed","note":…}` → `{task, repeated, blocked}` |
| `ls --mine` | `GET /v1/taskboard/mine` |
| `groups set-taskboard-writers` | `GET` / `POST /v1/groups/{g}/taskboard/writers {"agents":[…]}\|{"members":true}` |

The group listing also returns `locked`, and `writers` when the board is
locked. Each task carries `assignee` (`agent_id`, `name`), `assigned_by`,
`assigned_at`, `outcome`, `outcome_note` and `failures` when they are set.
Every change is recorded in the audit log as `taskboard.<verb>`, with the
group, the task id and, for an assignment, the target agent.

`status` also accepts `{"status":"open"}` on a done or dropped task. This
reopens the task in the stage and position it had before.
