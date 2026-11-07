# Group task boards

A group's **task board** is a short ordered list of tasks: what the group is on
right now and the next few things it will do. Each [agent group](agents-and-groups.md)
has one board. The operator and the group's agents read and change it with
`tclaude agent taskboard`.

> The task board is in development and is off by default. Turn it on with
> `"features": {"taskboard": true}` in `~/.tclaude/config.json`. While it is
> off, its routes answer 404, but stored tasks are kept. So far there is the
> store, the CLI, the API, importing refs from trackers, assigning tasks to
> agents, the dashboard section, and reaching a linked node's boards over
> federation.

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
tclaude agent taskboard ls --summary [--json]   # counts for every board you can read
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
- `ls --summary` prints one line of counts for every group board you can
  read, for example `crew: 9 · 1 needs you · 2 in progress · 6 next · 3 ideas`.
  These are the counts the dashboard section shows.
- If `move --before` or `move --after` names a task in the other stage, the
  moved task is promoted or demoted in the same write.
- `--ref` takes an `http(s)` URL, `owner/repo#12` for GitHub, or a bare tracker
  id. `LIN-123` is read as a Linear id, and a lower-case `tcl-a1b2c3` as an awb
  id. Prefix the id with `awb:` or `linear:` to say which one you mean. On a
  single `add`, a bare id is stored with no link. An import (below) resolves it.
- Every changing verb accepts `--ask-human <timeout>` and `--json`.

## Importing from a tracker

```bash
tclaude agent taskboard add <group> --from-awb tcl-a1b2c3,tcl-d4e5f6 [--next]
tclaude agent taskboard add <group> --from-linear LIN-1,LIN-2
tclaude agent taskboard add <group> --from-github owner/repo#12,https://github.com/owner/repo/pull/34
printf 'try the fused card\ntcl-a1b2c3\nLIN-7\n' | tclaude agent taskboard add <group> --lines-file -
```

An import adds one task per ref or line. Like any `add`, it lands in the ideas
unless you pass `--next`.

- **Titles come from the tracker.** The daemon looks each ref up through the
  matching [proxy](proxies.md): awb `issue show`, Linear `issue view`, or GitHub
  `issue view` / `pr view`. It does this as you, so you need the same rights a
  direct proxy read needs: `proxy.awb.read`, `proxy.linear.read` or
  `proxy.github.read`, within the operator's workspace, team and remote
  allow-lists. Each lookup is audited as your own proxy read. The GitHub proxy
  reads only the repository your working directory's remote points at, so a
  GitHub ref in another repository is not looked up.
- **A failed lookup still adds the task.** When no proxy is configured, the
  lookup is refused, or the tracker has no such item, the id becomes the title.
  The output says why for each item. Fix the title later with `edit --title`.
  The fallback title is the tracker id, `owner/repo#n` for GitHub, or the whole
  URL for a plain link. All lookups in one import share a one-minute budget;
  refs left over when it runs out keep their id as the title.
- **The link is filled in.** An awb ref links to the issue on the configured awb
  server, and a Linear ref to the URL Linear returns.
- **Duplicates are refused one by one.** A ref that is already on the board
  (open or blocked, in either stage) is refused, and so is a repeat within the
  same import. The rest of the import still lands. A GitHub ref is matched by
  repository and number, so `owner/repo#12` and the issue URL count as the same.
  An awb or Linear ref is matched by its id, and any other link by its whole URL.
- **`owner/repo#n`** is looked up as an issue first, then as a pull request.
- **Lines.** `--lines-file` adds one task per non-empty line. A line that is
  only an awb, Linear or GitHub id (or URL) becomes a task with that ref, and
  is looked up like the `--from-*` refs. Any other line is the title.
- An import holds at most 50 items. It takes only `--next`, not `--body`,
  `--ref`, `--after` or a position. The exit code is non-zero when any item was
  refused, and `--json` prints the result for each item.
- Titles are one line of UTF-8, up to 200 bytes, with no control characters.
  A body can be up to 8 KiB.

## Assigning and reporting

A task has at most one assignee. The assignee must be an active member of the
task's group.

- `assign --to <agent>` gives the task to a member. If the task is an idea, it
  is promoted to the end of the queue first. The assignee gets an inbox message
  from you, `[taskboard] <group> · tk_… "title" — assigned to you by <you>`,
  with the brief, the ref and the exact report commands. It is ordinary inbox
  mail, delivered like any agent message. A short, paste-safe message is
  shown inline in the agent's pane.
- `take` assigns a task to yourself. You can only take a task that is queued
  (`next`), open and unassigned. `release` gives back a task that is assigned
  to you. It never takes a task away from someone else.
- `unassign` takes a task back from its assignee. The assignee is told by inbox
  mail.
- `report done` closes the task. `report failed` counts a failure and gives the
  task back. After three failures the task is blocked with the reason
  "3 failed attempts". Unblocking or reopening the task resets the count. If an agent assigned the task, that agent is told about
  every report.
- Only the current assignee can report. A report from someone who has been
  unassigned, or whose task was reassigned, is refused with `not_assignee`
  (409). Sending the same report twice is acknowledged without being applied
  again. `--task` may be left out when exactly one live task is assigned to you.
- An assignment never retires, releases or stops the agent. `done` leaves the
  agent where it is, ready to take the next task. Retiring an agent unassigns
  its live tasks; the history records "assignee retired". Reincarnation keeps
  the assignment, because the agent id is stable. A clone does not inherit it.
- `ls --mine` lists the live tasks assigned to you, in every group you are
  still an active member of.
- Reassigning a task takes it away from its current holder, so it needs the
  right to do both: assign the new agent, and take the task back from the old
  one. A change gated against one holder is refused with `assignee_changed`
  if the task changed hands in the meantime.

Assigned tasks show `@<agent>` in `ls`, and their history records who
assigned whom.

## The writer lock

By default, every member of a group may change its board and take its tasks.
`groups set-taskboard-writers <group> <agent>…` narrows this to the listed
members. Owners of the group, the operator, and agents with an explicit
unscoped or group-scoped `taskboard.write` or `taskboard.assign` grant are not
affected. An assign grant that is also scoped to target agents does not pass
the lock. With no agents
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

Closing a task (`done` or `drop`) that is unassigned needs `taskboard.write`.
Closing your own task needs `self.taskboard`. Closing a task assigned to
someone else needs `taskboard.assign`.

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

## Across linked nodes

A board works across [federation](federation.md) the same way it works
locally, but under **peer grants** that this node's operator gives a linked
node. A linked node holds none of them by default:

```bash
tclaude federation grant bob taskboard.read --scope group=crew
tclaude federation grant bob taskboard.write --scope group=crew
tclaude federation grant bob taskboard.assign --scope group=crew
tclaude federation grant bob taskboard.manage --scope group=crew
```

| Peer grant | What the linked node may do on the group's board |
|---|---|
| `taskboard.read` | List it, show a task and its history, see it in the fused view, and count it in the node summary |
| `taskboard.write` | Add and import tasks, edit, move, promote, demote, block, unblock, drop and close them |
| `taskboard.assign` | Assign tasks to this node's members of the group, unassign them, and close a task someone else holds |
| `taskboard.manage` | Set the writer lock |

- Any of these grants also lets the node read the board.
- A grant covers only the groups in its scope. A task in another group answers
  404, so its existence does not leak.
- Unrestricted trust acts as this node's operator does.
- Taking, releasing and reporting stay with this node's own agents.
- The [writer lock](#the-writer-lock) does not apply to a linked node. Its
  peer grant is an explicit grant, like a local agent's scoped
  `taskboard.write` grant, which the lock does not narrow either. To stop a
  linked node from changing the board, revoke its grant.
- History records a linked node's change as `peer: the operator of <node>` (or
  its agent), and so does an assignment's inbox brief. This node's audit log
  records each write as `federation.peer_view`. The calling node records its
  own operator's writes as `federation.peer_view.out`, and its agents' as
  `federation.peer_view.agent.out`.
- An import's title lookups run as the linked node, which holds no proxy
  grant here, so each ref keeps its id as its title.

From the CLI, add `--peer <node>` to any verb: `ls <group>`, `ls --summary`,
`show`, `history`, `add` (including imports), `edit`, `move`, `promote`,
`demote`, `block`, `unblock`, `drop`, `done`, `assign`, `unassign`, and
`groups set-taskboard-writers`. `take`, `release` and `report` act on
yourself, so they have no `--peer`.

An agent on the calling node also needs its own grant for the linked node. It
needs the same slug, scoped with `--scope peer=<node>/<group>`. A verb that
names only a task id (`show`, `edit`, `assign` and the like) has no group in
its path, so it needs a node-wide grant there: `--scope peer=<node>`. In either
case, the linked node's own peer grants decide which groups it reaches. As
everywhere in federation, a missing peer grant is a refusal, not a popup on
the linked node. Its operator grants the access, either up front or in answer
to an [access request](federation.md).

`GET /api/node-summary` carries `taskboard`, the summed counts of the boards
the reader may see.

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

## In the dashboard

Each group card on the Groups tab has a **Task board** section (in wizard mode,
the **Quest board**), below its Agents and Resources sections.

- **Folded**, the section is one line of counts, for example
  `5 · 1 needs you · 1 in progress · 2 next · 1 idea`. *Needs you* counts the
  blocked tasks, and *in progress* counts the assigned tasks. *Next* and
  *ideas* count the unassigned open tasks in each stage. The group's summary
  line carries the same counts, so a folded group still shows what is on its
  board.
- **Open**, the section lists the queue in your order and then the Ideas band.
  Each row shows its number, title and id, a ref chip that links to the
  tracker, who holds the task, and its state. A blocked task shows its reason
  on the line below, and a dependency shows as `after #n` (amber until it is
  done). The *history* line counts the tasks done this week and the dropped
  ones. Click *history* to list the recently closed tasks.
- The section opens on its own when something needs you. If you fold or open
  it yourself, the dashboard remembers that per group.

You can change the board from the section. Each action uses the same route as
a CLI verb:

| In the section | CLI |
|---|---|
| Type a line in the capture box and press Enter | `add <group> "title"` |
| Paste a list into the capture box | `add <group> --lines-file -` |
| *+ from awb/Linear…* | `add <group> --from-awb …` / `--from-linear …` / `--from-github …` |
| Drag a row by its ⋮⋮ handle | `move <task> --before … / --after …` |
| Drag a queued task onto the Ideas line | `demote <task>` |
| *✓ Next* on an idea | `promote <task>` |
| *Block* (with a reason) / *Unblock* | `block <task> --reason …` / `unblock <task>` |
| *Done* | `done <task>` |
| *Drop* (with an optional reason) | `drop <task> [--reason …]` |
| *Edit* | `edit <task> --title … --ref …` |
| *Reopen*, on a task under *history* | `POST /v1/taskboard/tasks/{id}/status {"status":"open"}` |

- **The capture box** stays focused after each Enter, so you can add a burst of
  ideas in a row. Shift+Enter starts a new line. A line that is only an awb,
  Linear or GitHub id becomes a task with that ref, titled from the tracker
  (as in an import from a tracker). New tasks land at the end of the
  ideas. Tick *capture as next* to add them to the queue instead. Then drag
  them where you want them.
- **Dragging** a row above or below another row moves it there. If you drop it
  on the other side of the Ideas line, it is promoted or demoted in the same
  step.
- The *List | Lanes* switch in the section head is for the swim-lanes view of
  a later slice. Until then only *List* works.

The dashboard shows a board only to someone who can read it. The operator sees
every group. The section is missing while `features.taskboard` is off. A
linked node's groups, in the fused view or on that node's page, show the
boards that node lets you read. Their actions appear only where it granted
you `taskboard.write` (see [Across linked nodes](#across-linked-nodes)).

## API

| CLI | HTTP |
|---|---|
| `ls <group>` | `GET /v1/groups/{g}/taskboard[?stage=&all=1]` |
| `ls` (own group) | `GET /v1/taskboard` |
| `show` / `history` | `GET /v1/taskboard/tasks/{id}` / `…/events` |
| `add` | `POST /v1/groups/{g}/taskboard/tasks` |
| `add --from-*` / `--lines-file` | `POST /v1/groups/{g}/taskboard/tasks {"refs":[…],"lines":[…],"stage"?}` |
| `edit` | `PATCH /v1/taskboard/tasks/{id}` |
| `move` | `POST /v1/taskboard/tasks/{id}/move` |
| `promote` / `demote` | `POST /v1/taskboard/tasks/{id}/stage {"stage":"next"\|"idea"}` |
| `block` / `unblock` / `drop` / `done` | `POST /v1/taskboard/tasks/{id}/status` |
| `assign` / `take` | `POST /v1/taskboard/tasks/{id}/assign {"to":"<agent>"\|""}` |
| `unassign` / `release` | `POST /v1/taskboard/tasks/{id}/unassign` (`release` sends `{"self":true}`) |
| `report` | `POST /v1/taskboard/tasks/{id}/report {"outcome":"done"\|"failed","note":…}` → `{task, repeated, blocked}` |
| `ls --mine` | `GET /v1/taskboard/mine` |
| `ls --summary` (and the dashboard) | `GET /v1/taskboard/context` |
| `groups set-taskboard-writers` | `GET` / `POST /v1/groups/{g}/taskboard/writers {"agents":[…]}\|{"members":true}` |

The group listing also returns `locked`, and `writers` when the board is
locked. Each task carries `assignee` (`agent_id`, `name`), `assigned_by`,
`assigned_at`, `outcome`, `outcome_note` and `failures` when they are set.
Every change is recorded in the audit log as `taskboard.<verb>`, with the
group, the task id and, for an assignment, the target agent.

`status` also accepts `{"status":"open"}` on a done or dropped task. This
reopens the task in the stage and position it had before.

An import answers 200 with `{"group", "created", "results":[…]}`, with one
result per item in input order. Each result carries `input`, and either `task`
(with `resolved: true`, or a `note` saying why the title is the id) or `error`
and `code` (`duplicate`, `invalid_arg`, `cap`). The `refs` entries take the same
forms as `--ref`. The CLI prefixes `--from-awb` ids with `awb:` and
`--from-linear` ids with `linear:`.

`GET /v1/taskboard/context` is what the dashboard reads. For every group whose
board the caller can read, it returns the counts, up to 50 live rows (without
bodies) and the 8 most recently closed tasks. A group the caller cannot read is
left out entirely, not listed as empty.
