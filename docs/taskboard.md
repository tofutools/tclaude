# Group task boards

A group's **task board** is a short ordered list of tasks: what the group is on
right now and the next few things it will do. Each [agent group](agents-and-groups.md)
has one board. The operator and the group's agents read and change it with
`tclaude agent taskboard`.

> The task board is on by default. To turn it off, set
> `"features": {"taskboard": false}` in `~/.tclaude/data/config.json`. While it
> is off, its routes answer 404 and its sweeps stop, but stored tasks are kept.

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
tclaude agent taskboard activity <task>    # comments and changes, oldest first; history is an alias
tclaude agent taskboard comment add <task> "markdown" [--body …|--file f|-|--stdin] [--key k] [--attach f]…
tclaude agent taskboard comment edit <task> <#id> "markdown" [--body …|--file f|-|--stdin]
tclaude agent taskboard comment rm <task> <#id>
tclaude agent taskboard comment ls <task> [--save <dir>]

tclaude agent taskboard add <group> "title" [--body …|--body-file f] [--ref URL|id] [--resolve-ref]
                                    [--next] [--after tk_a,tk_b] [--top|--before tk_x]
                                    [--assignee <agent>|operator|--mine] [--blocked "reason"|--done] [--attach f]…
tclaude agent taskboard add <group> "title" --assignee operator --blocks tk_mine   # ask the operator; tk_mine waits
tclaude agent taskboard ask <group> --to <agent> "what you want" [--attach f]…   # or --prompt-file f
tclaude agent taskboard attach <task> <file>…
tclaude agent taskboard detach <task> <name|#id>…
tclaude agent taskboard attachments <task> [--save <dir>]
tclaude agent taskboard edit <task> [--title …] [--body …|--clear-body] [--ref …|--clear-ref] [--resolve-ref]
                                    [--add-dep tk_…] [--rm-dep tk_…]
tclaude agent taskboard move <task> --top|--bottom|--before tk_x|--after tk_x
tclaude agent taskboard move-to-group <task> <group>[@<peer>] [--clone] [--stage idea|next]
tclaude agent taskboard promote <task>     # idea → end of the queue
tclaude agent taskboard demote <task>      # next → top of the ideas
tclaude agent taskboard block <task> --reason "waiting on CI"
tclaude agent taskboard block <task> --on tk_…        # wait on an operator task
tclaude agent taskboard unblock <task>
tclaude agent taskboard drop <task> [--reason "parked in awb"]
tclaude agent taskboard done <task>
tclaude agent taskboard reopen <task> [--next|--idea] [--assignee <agent>|operator|--unassign]

tclaude agent taskboard assign <task> --to <agent>|operator
tclaude agent taskboard unassign <task>
tclaude agent taskboard take <task>
tclaude agent taskboard release <task>
tclaude agent taskboard report done|failed [--task <task>] [--note "…"]

tclaude agent groups set-taskboard-writers <group> [agent…] | --members | --show
```

- If you leave out the group in `ls` and you are an active member of exactly
  one group, it lists that group's board. Otherwise (the operator, or an agent
  in several groups) it lists every board you can read: its counts and its
  first five live tasks.
- `ls` numbers the live tasks in board order: one sequence through the
  queue and then the ideas. Done and dropped tasks are not
  numbered.
- `ls --all` also lists done and dropped tasks that are not archived yet.
- `ls --summary` prints one line of counts for every group board you can
  read, for example `crew: 9 · 2 needs you (1 stalled, 1 blocked) · 2 in progress · 6 next · 3 ideas`.
  These are the counts the dashboard section shows.
- If `move --before` or `move --after` names a task in the other stage, the
  moved task is promoted or demoted in the same write.
- `--ref` takes an `http(s)` URL, `owner/repo#12` for GitHub, or a bare tracker
  id. `LIN-123` is read as a Linear id, and a lower-case `tcl-a1b2c3` as an awb
  id. Prefix the id with `awb:` or `linear:` to say which one you mean. A
  ref's URL is what it is: the board shows it under a short label (the awb
  or Linear id, `owner/repo#12`) as a link.
  - An awb id is linked on the configured awb server
    (`agent.awb_proxy.url`, as `<url>/#/issues/<id>`) wherever it is given:
    `add`, `edit`, an import, a move to another group. An awb ref stored with no
    link before that is linked as the board reads it.
  - A Linear id has no link until it is looked up: pass `--resolve-ref` to
    `add` or `edit`, and it is looked up through the Linear proxy, as an
    import does. The title stays yours. An import (below) always looks it up.
    `edit <task> --resolve-ref` with no `--ref` links the task's own ref.
  - A ref with no link shows as a plain chip ("not linked" in its tooltip).
- `ask` mails a member of the group a request in your own words. The member
  is told to turn it into tasks with `taskboard add`, to reply when it is
  done, and not to start the work. The request is inbox mail, never keys sent
  into its pane. You need `taskboard.write` on the group. The agent asked
  must be an active member or owner that holds `taskboard.write` there and
  passes the [writer lock](#the-writer-lock). A member that only tracks its
  own work (`taskboard.own`) cannot be asked.
- Every changing verb accepts `--ask-human <timeout>` and `--json`.

### Where a new task starts

`add` creates a task in any column of the board, in one call:

- **Ideas**, the default, or **Next** with `--next`.
- **In progress** with `--assignee <agent>`. The task is assigned as it is
  created, so it lands in next, and the assignee gets the brief in its inbox,
  as with `assign`. Naming yourself (or `--mine`) takes the task. Anyone else
  needs `taskboard.assign`, and the assignee must be an active member of the
  group.
- **In the assignee's Ideas or Next, not started**, with `--assignee <agent>
  --idea` or `--assignee <agent> --next`. See [Holding a task without
  starting it](#holding-a-task-without-starting-it).
- **Blocked** with `--blocked "reason"`. The reason is required. An
  `--assignee` makes it a blocked task that agent holds.
- **Done** with `--done`, for logging work after the fact. The task is closed
  now and counts in *done this week*. An `--assignee` records who did it, and
  nobody is mailed. A done task does not count toward the live cap.

An import (`--from-*`, `--lines-file`) lands in Ideas or Next only.

These start states are for `taskboard.write` holders. A member that holds only
[`taskboard.own`](#tracking-your-own-work-taskboardown) adds its own tasks:
each one is assigned to it as it is created. A plain `add` starts in progress,
`--blocked` starts blocked and held by you, and `--done` logs work you did. An
unassigned task (`--next`, or an idea) and an import need `taskboard.write`.

## Files on a task

A task can carry files: a screenshot, a log, a spec. `add --attach` (repeatable)
creates the task and its files in one step, and `attach` adds files to an
existing task. `show` lists them, and so does the brief an assignee gets,
with the command to fetch them.

- A task holds at most 10 files, of at most 4 MiB each.
- Files have the types mail attachments may have: images (PNG, JPEG, GIF,
  WebP), text, markdown, CSV, JSON, YAML, diff/patch and PDF. HTML, SVG,
  archives and executables are refused.
- A second file with a name the task already has gets a numbered prefix
  (`2-shot.png`).
- The files are stored where agents cannot reach them. `attachments <task>
  --save <dir>` writes them into a directory (it never overwrites a file).
  `detach` removes one, by name or by the `#id` the list shows.
- Attaching and detaching need `taskboard.write` on the task's group, or
  `taskboard.own` on a task assigned to you. Reading the files needs
  `taskboard.read`. Both are recorded in the task's history
  (`attached`, `detached`).
- `ask --attach` sends the files with the request, as attachments of the
  mail. The asked member is told to put them on the tasks it creates.
- A clone or a move to another group, on this node or another, takes the
  files with it. A clone shares the stored bytes. Bytes no task refers to
  any more are removed by the hourly sweep.
- In the dashboard, the *Write it myself* form, *Ask an agent* and the edit
  form take files as the spawn dialog does: 📎 *Choose files…*, a drop of
  files anywhere on the dialog, or Ctrl+V (Cmd+V on macOS) of copied files or
  a screenshot. A file over the limits or of a refused type is left out, and
  a note names it. The edit form shows a task's files as tiles (an image
  with its thumbnail); click one to open it, and ✕ removes it on *Save*.
  This works for a linked node's board too: the files go through this
  node's staged upload.

### Reopening a task

`reopen` puts a done or dropped task back on the board when it turns out to
need more work. By default it comes back where it was: in its stage, at its
place, with the assignee it had (unless that agent has been retired since).
`--next` or `--idea` puts it in that stage instead, `--assignee` gives it to a
member or the operator, and `--unassign` takes the assignee off. An assigned
task is always in next, so `--idea` on an assigned task needs `--unassign`. A
kept assignee who has left the group is taken off.
It is one write, and the history records each part: `reopened`, then
`assigned` or `unassigned`, then `promoted` or `demoted`. A reopen resets the
failure count, as an unblock does.

Reopening is gated as closing is: your own task needs `taskboard.own`, any
other needs `taskboard.write` (owners have it). Changing the assignee is
gated as `assign` and `unassign` are. Someone else who is given the task gets
its brief; an assignee who had it gets a `reopened by` mail. Across linked
nodes, `reopen` takes `--peer` like the other verbs.

## Comments and activity

Each task has one append-only activity stream, oldest first. `activity <task>`
shows it (`history` is an alias), and `show` gives the header: created by,
last edited by, assigned to, and the comment count.

- **Changes** carry a `changes` list of `{field, before, after}`: `title`,
  `body`, `ref_*`, `depends_on`, `stage`, `status`, `assignee` and `group`.
  An action on a resource (`attached`, `detached`, `assigned`, `unassigned`,
  `taken`, `released`, `reported`, `regrouped`, `cloned`, `imported`,
  `archived`) also names itself as `action`. A failed or no-op operation
  records nothing, and every entry names its actor.
- **Comments** are Markdown, up to 16 KiB, from agents and the operator. A
  comment may carry up to 10 files, as a task's own may. They belong to the
  comment, not the task's own list, and `comment ls --save <dir>` writes them
  out, one directory per comment.
- **Reasons** for a block, a close, a drop, and a report's note are typed
  comments in the same stream (`block_reason`, `done_reason`, `drop_reason`,
  `report_note`).
- **Keys.** A comment takes an idempotent client key, unique per task and
  author. Sending the same key and body again returns the first comment; the
  same key with another body is refused (`key_conflict`). The CLI makes a
  fresh key per call unless `--key` names one.
- **Editing and deleting** is for a comment's author only. An edit keeps the
  earlier body as a `comment_edited` entry. A delete leaves a tombstone,
  blanks the earlier bodies, and removes the comment's files.
- **Mail.** A comment by someone else mails the task's assignee. It also
  mails the agent that created the task, when an agent did.
- Commenting needs `taskboard.write`, or `taskboard.own` on your own group's
  board. Reading the stream needs `taskboard.read`.

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
- **Lines.** `--lines-file` adds one task per line, skipping blank lines and
  lines that start with `#`, so a list can be kept as a commented file. A line that is
  only an awb, Linear or GitHub id (or URL) becomes a task with that ref, and
  is looked up like the `--from-*` refs. Any other line is the title.
- An import holds at most 50 items. It takes only `--next`, not `--body`,
  `--ref`, `--after` or a position. The exit code is non-zero when any item was
  refused, and `--json` prints the result for each item.
- Titles are one line of UTF-8, up to 200 bytes, with no control characters.
  A body can be up to 8 KiB.

## Assigning and reporting

A task has at most one assignee. The assignee must be an active member of the
task's group, or the operator (see [The operator as an
assignee](#the-operator-as-an-assignee)).

- `assign --to <agent>` gives the task to a member. If the task is an idea, it
  is promoted to the end of the queue first. The assignee gets an inbox message
  from you, `[taskboard] assigned to you by <you>: <group> · tk_… "title"`,
  with the brief, the ref and the exact report commands. It is ordinary inbox
  mail, delivered like any agent message. A short, paste-safe message is
  shown inline in the agent's pane.
- `take` assigns a task to yourself. You can take any open, unassigned task.
  An idea is promoted to the end of the queue as you take it. A task that is
  assigned to someone else cannot be taken: that is `taskboard.assign`'s
  reassign. `release` gives back a task that is assigned to you. It never
  takes a task away from someone else.
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
  its live tasks; the history records "assignee retired". Removing an agent
  from a group unassigns its live tasks on that group's board ("assignee left
  the group"); its tasks on other groups' boards stay. Reincarnation keeps
  the assignment, because the agent id is stable. A clone does not inherit it.
- `ls --mine` lists the live tasks assigned to you, in every group you are
  still an active member of.
- Reassigning a task takes it away from its current holder, so it needs the
  right to do both: assign the new agent, and take the task back from the old
  one. A change gated against one holder is refused with `assignee_changed`
  if the task changed hands in the meantime.

Assigned tasks show `@<agent>` in `ls`, and their history records who
assigned whom.

### Holding a task without starting it

Any assignee, an agent or the operator, can hold a task without starting it:
an idea to triage, or a task queued for it in Next. A held task sits in its
stage in the assignee's lane. It is *in progress* only once it is started, and
only a started task can stall.

- `add --assignee <agent> --idea` (or `--next`) and `assign <task> --to
  <agent> --idea` (or `--next`, or `--hold` to keep its stage) assign without
  starting. The assignee gets a short note, `[taskboard] an idea for you to
  triage` or `[taskboard] queued for you`, instead of the brief.
- With no flag, `add --assignee` and `assign` start the task, as before.
  `take` starts it too.
- `start <task>` starts a held task. An idea is promoted to the end of the
  queue. Started by someone other than the assignee, the assignee gets the
  brief, as on an assign.
- `hold <task> [--idea|--next]` puts a started task back, held in its stage or
  the one named. It keeps its assignee.
- The assignee starts and holds its own task under `taskboard.own`. Doing it
  for someone else, or for the operator, needs `taskboard.assign`.
- In `ls`, a held task shows `@<agent> (not started)`.
- The operator: `add --assignee operator` with no flag is active and counts as
  *waiting on you*, as before. `--next` queues it, still waiting on you.
  `--idea` puts it in your Ideas: it counts as an *idea for you*
  (`ideas_for_you` in the context route), not as waiting on you, and no task
  can wait on it (`block --on` refuses it). `--blocks` always lands the task
  in Next.

### The operator as an assignee

A task can be assigned to the **operator**: something an agent needs answered
or done before its work can go on. Name it `operator` wherever an agent goes:
`add --assignee operator`, `assign --to operator`, the dashboard's
⋯ *Assign ›* and the forms' assignee (*You (operator)*), or a drag onto the
operator's lane. This complements `notify-human` and `--ask-human`; nothing
there changes.

- `operator` means the operator of the node that holds the board. On a linked
  node's board (`--peer bob`, or bob's board in the fused view) it is bob's
  operator, under the peer grants bob gave this node, as for any assign.
- **Permissions.** Giving the operator a task needs `taskboard.assign` on the
  group. A grant of it narrowed to target agents does not cover the operator.
  An agent asking about its own work needs only `taskboard.own`: it may add a
  new task assigned to the operator, or hand the operator a task it holds. The
  writer lock applies, as to any own write. The agent that asked may edit,
  block and drop (withdraw) that task as its own. Only the operator, or a
  `taskboard.assign` holder, closes it as done, and agents do not `report` on
  it.
- **Waiting on it.** `add … --assignee operator --blocks tk_mine` creates the
  operator task and, in the same write, adds it as a dependency of `tk_mine`
  and blocks `tk_mine` with the reason `waiting on operator: <title>`. The
  change to `tk_mine` is gated as any change of it is. `block tk_mine --on
  tk_…` does the same for an operator task that already exists.
- **Auto-unblock.** When the task is closed as done, every task blocked
  waiting on it (`--blocks` or `block --on`) is reopened, and history records
  `unblocked: tk_… is done`. A task that depends on it but is blocked for
  another reason stays blocked. Their assignees get inbox mail, `[taskboard]
  unblocked: tk_… is done` or `[taskboard] still blocked: tk_… is done`, with
  the closing reason. When the task is dropped instead, the tasks waiting on
  it stay blocked and their assignees get `[taskboard] still blocked: tk_… is
  dropped`. Removing the task from a waiting task's dependencies ends the
  wait. Close it
  with a reason (`done --reason`, or the edit form) or a comment to answer;
  the operator's comments on it reach the agent that asked.
- Nobody is mailed when the operator is given a task; it shows in the
  dashboard. It never stalls.
- A move to another group of this node keeps the operator as the assignee. A
  move to another node does not carry the assignee, as for any task.
- **Counts.** An open task the operator holds counts as *waiting on you*, which
  is part of *needs you*: `ls --summary` prints `2 needs you (1 blocked, 1
  waiting on you)`, and `tclaude agent fleet` lists the board. The context
  route's counts carry `waiting_on_you`.

### Idle without report

An open task is **stalled** when its assignee has sat idle for
`taskboard.idle_grace` (default 5 minutes) without reporting: its session is
idle, with no subagents, background shells or monitors running, and has been
since the later of its last activity and the start of its current stretch of
work: the assignment, or the task being unblocked or reopened. Time spent
blocked or closed does not count. Stalled is worked out
on every read. It is not a status, and it clears as soon as the agent starts a
new turn or reports.

A sweep every 30 seconds announces each stall once per assignment (an unblock
or a reopen starts a new stretch, which may be announced again):

- the history records a `stalled` event;
- the assigner is told,
  `[taskboard] <agent> went idle without reporting (7m): <group> · tk_… "title"`:
  an agent by inbox mail, the operator in Messages. A task the agent took for itself has no one else to tell;
- the assignee gets an inbox nudge with its report commands. It is never sent
  as keys into the pane.

Stalled never fails, blocks or unassigns a task. Ask the agent how it is going,
or take the task back with `unassign`.

```json
{ "taskboard": { "idle_grace": "5m" } }
```

`idle_grace` is a duration (`"90s"`, `"10m"`); `"0"` turns the check off.

Stalled and blocked tasks are what the board needs you for. `ls --summary`, the
dashboard and the context route count them as *needs you*, and
`tclaude agent fleet` (the Steward's brief) lists every group whose board has
any, for example `crew board: 2 tasks need you (1 stalled, 1 blocked)`. The
brief's JSON carries the same counts per group under `taskboards`.

## The writer lock

By default, every member of a group may track its own work on its board
(`taskboard.own`) and take its tasks.
`groups set-taskboard-writers <group> <agent>…` narrows this to the listed
members. Owners of the group, the operator, and agents with an explicit
unscoped or group-scoped `taskboard.write`, `taskboard.assign` or
`taskboard.own` grant (a group grant included) are not affected. Since every
source of `taskboard.write` passes the lock, the lock in practice narrows
members' own work: adding, changing and taking their own tasks. An assign grant that is also scoped to target agents does not pass
the lock. With no agents
listed, the board is locked to owners only. `--members` opens it to every
member again, and `--show` prints the current lock. A locked change is refused
with `taskboard_locked`. Reporting on your own task is never locked, because
whoever assigned it already authorised the work.

## Permissions

The operator passes every gate. For agents:

| Slug | What it allows | Who has it without a grant |
|---|---|---|
| `taskboard.read` | `ls`, `show`, `activity`, `comment ls` | Members and owners of the group |
| `taskboard.write` | Everything that changes a task: unassigned tasks, imports, `ask`, reordering, and any task's edits and status. Implies read. | Owners of the group. Members only through a grant. |
| `taskboard.own` | Tracking your own work: adding tasks that are assigned to you, changing and closing your own tasks, taking any unassigned task, releasing yours, reporting, and commenting on any task of the board | Members of the group, on their own group's board only |
| `taskboard.assign` | Assigning another agent, taking a task back, and closing a task assigned to someone else. Scope it with `group` and `target_agent`. | Owners of the group |
| `taskboard.manage` | The writer lock, and moving a task you do not hold to another group | Owners of the group |
| `taskboard.transfer` | Moving or cloning a task into the group from another group. Scope it with the target `group`. | Owners of the group |
| `self.taskboard` | Deprecated alias of `taskboard.own`. An existing grant keeps working and adds nothing. An explicit deny of it still denies `taskboard.own`. | No longer default-granted |

Moving a task to another group (`move-to-group`) needs `taskboard.transfer` on
the target group. On the source it needs `taskboard.write` when the task is
yours (assigned to you), or `taskboard.manage` when it is not. A clone
(`--clone`) needs `taskboard.read` on the source and `taskboard.transfer` on
the target.

Closing a task (`done` or `drop`) that is unassigned needs `taskboard.write`.
Closing your own task needs `taskboard.own`. Closing a task assigned to
someone else needs `taskboard.assign`.

### Tracking your own work (taskboard.own)

`taskboard.own` lets an agent track its own work on its group's board without
owning the group. Every active member holds it on its own group's board. It
never reaches the board of a group the agent is not an active member of,
whatever a grant's scope says. Under it an agent may:

- add a task, which is assigned to it as it is created: in progress, or
  blocked or done with `--blocked` / `--done`;
- edit, promote, demote, block, unblock, reopen, close and report on the tasks
  assigned to it;
- take any open, unassigned task, an idea included, and release its own;
- comment on any task of the board.

It may not add an unassigned task or import, ask a member for tasks, reorder
the board, or change a task that is someone else's or nobody's. Those need
`taskboard.write`. Taking a task from someone else needs `taskboard.assign`.
The [writer lock](#the-writer-lock) narrows `taskboard.own`, except a report.

To have an agent track its own work, spawn it into the group and tell it to:

```bash
tclaude agent taskboard add crew "fix the flaky retry test" --ref tcl-a1b2c3   # assigned to you, in progress
tclaude agent taskboard take tk_1a2b3c4d                                    # or take an unassigned one
tclaude agent taskboard report done --note "merged #4200"
```

### Migration: members no longer write the board

Members used to hold `taskboard.write` on their group's board through
membership. They now hold `taskboard.read` and `taskboard.own`. Owners keep
`taskboard.write`, `taskboard.assign`, `taskboard.manage` and
`taskboard.transfer`. For a plain member, with no grant, this changes:

- `add` assigns the task to the member (in progress). `--next`, ideas,
  imports and `ask` need `taskboard.write`.
- Editing, promoting, demoting, blocking, unblocking and reopening work only on
  the member's own tasks. Reordering (`move`) and closing an unassigned task
  need `taskboard.write`.
- Moving a task you hold to another group needs `taskboard.write` on the
  source.
- A plain member can no longer be asked for tasks (`ask`, and *Ask an agent*
  in the dashboard).
- `take` now also takes an unassigned idea, which it promotes.

To give the members of a group the old, broad rights back, give the group a
[group grant](#giving-a-whole-group-access-to-another-groups-board) of
`taskboard.write` scoped to the group itself. In the dashboard, open the
group's ⚙ permissions, add `taskboard.write`, and set its `group` scope to the
group's own name. Like any explicit grant, it also passes the writer lock.

`self.taskboard` is no longer installed as a default permission. An existing
grant of it does no harm, and an explicit deny of it still denies
`taskboard.own`.

None of the `taskboard.*` slugs are default-granted. Access to another group's
board is an ordinary scoped grant:

```bash
tclaude agent permissions grant steward taskboard.read taskboard.write --scope group=tclaude,website
tclaude agent permissions grant steward taskboard.read   # unscoped: every group, including future ones
tclaude agent permissions grant lead taskboard.assign --scope group=tclaude --scope target_agent=@descendants
```

### Giving a whole group access to another group's board

To let every member of one group read or change another group's board, give
the first group a **group grant**: the ordinary `taskboard.read` or
`taskboard.write` slug, held by the group and scoped to the board's group.
There is no separate sharing feature and no special slug. In the dashboard,
open the group's ⚙ permissions, add the slug, and set its `group` scope to
the board's group. For example, to give website read access to crew's board,
and ops read and write access:

| Group | Group grant | Its members may |
|---|---|---|
| `website` | `taskboard.read`, scope `group=crew` | List crew's board, show its tasks and their history |
| `ops` | `taskboard.write`, scope `group=crew` | Also add, edit, move, promote, demote, block and close crew's tasks |

A group grant applies to every current and future member, and ends for an
agent as soon as it leaves the group. Like an explicit per-agent grant, it
also passes the board's [writer lock](#the-writer-lock). Assigning and the
lock itself stay on `taskboard.assign` and `taskboard.manage`, which a group
can be granted in the same way. A board on a linked node works the same way,
with a `peer` scope instead (see [A linked node's board, for a whole
group](#a-linked-nodes-board-for-a-whole-group)).

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
| `taskboard.read` | List it, show a task and its activity and comments, see it in the fused view, and count it in the node summary |
| `taskboard.write` | Add and import tasks, ask this node's members for tasks, edit, move, promote, demote, block, unblock, drop and close them, and comment (editing and deleting only its own comments) |
| `taskboard.assign` | Assign tasks to this node's members of the group, unassign them, and close a task someone else holds |
| `taskboard.manage` | Set the writer lock |
| `taskboard.transfer` | Move or clone a task into the group from another group of this node. A move also needs `taskboard.manage` on the source group, and a clone needs only a read of it |

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
`show`, `activity` (`history`), `comment add|edit|rm|ls`, `add` (including imports), `ask`, `edit`, `move`, `promote`,
`demote`, `block`, `unblock`, `drop`, `done`, `reopen`, `assign`, `unassign`, and
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

### A linked node's board, for a whole group

Giving the agents of a local group access to a group's board on a linked
node takes two grants, one on each node. Each one bounds the other:

1. **On the linked node (bob).** Its operator gives this node a peer grant
   on the board's group, or trusts it unrestricted:
   `tclaude federation grant <this node> taskboard.read --scope group=crew`.
   This decides what any of this node's agents can reach on bob.
2. **On this node.** Its operator gives the local group a group grant of the
   same slug, scoped `peer=bob/crew`: in the group's ⚙ permissions, add
   `taskboard.read` and pick **bob / crew** in its `peer` scope. The picker
   lists each linked node, and the groups that node grants this node anything
   on. This decides which of this node's agents may use that reach.

Every member of the local group can then run
`tclaude agent taskboard ls crew --peer bob`. With `taskboard.write` on both
nodes it can also run `add crew … --peer bob` and the other writes. On this
node, write does not imply read for a linked node's board, so give the group
both slugs, each scoped `peer=bob/crew`. A local `group=`
scope, an unscoped grant and group ownership never reach a linked node, and
a `peer=` scope never reaches a local board. A verb that names only a task id
(`show`, `edit`, `move`, `done` and the like) needs the node-wide scope
`peer=bob` on this node, as described above.

A refusal names a missing layer. When both are missing, the linked node's
layer is reported first:

| What is missing | What the agent is told |
|---|---|
| The linked node grants this node nothing on the group | `bob has not granted this node anything on group "crew"`, with the `tclaude federation grant` command its operator would run |
| The linked node grants other things on the group, but not this slug | `requires taskboard.read for group:crew, a peer grant the node you called has not given your node`, also with the command |
| This node's grant | `caller is not granted permission "taskboard.read" for peer=<bob's instance id>/crew`, with the scope to grant |

`GET /api/node-summary` carries `taskboard`, the summed counts of the boards
the reader may see.

### Moving a task to another node

`move-to-group <task> <group>@<peer>` moves a task of this node to a group on
a linked node, and `--clone` copies it there. The linked node must give this
node the peer grant `taskboard.transfer` on that group:

```bash
tclaude federation grant alice taskboard.transfer --scope group=crew   # on bob: alice may move tasks into crew
```

A move happens in two steps, so a failure never leaves a silent duplicate.
First, the drop's permissions are checked here. If this node would refuse
the drop, nothing is created anywhere. Then:

1. The linked node creates the task (`transfer-in`), with the same title,
   brief and ref, in the ideas or in next. Its history notes where it came
   from (`moved from alice/web tk_…`), and the node name is the one the
   linked node knows the caller by. In a move between two linked nodes,
   driven from a third, the source node is the caller's claim, marked with
   who sent it (`moved from carol/web tk_… (via alice)`). If this step is
   refused, nothing changes. If it times out, the target may have the task
   already, so check it before you retry.
2. Only then is the original dropped here, with the reason
   `moved to bob/crew`. Its assignee, if any, is told. This takes the same
   source-side permission as a local move. If this step fails for a
   transient reason, it is retried once. A retry that finds the original
   already closed counts as done. If it still fails, the command says so
   and names both copies, and the original is left for you to drop.

The task's files go with it, in the same `transfer-in` request. The task's
history, assignee and dependencies stay behind with the dropped
original. A clone runs only the first step. Only an open or blocked task moves
to another node; a done or dropped one can be cloned.

An agent running the command also needs its own grant for the linked node,
as for any `--peer` verb: `taskboard.transfer` scoped with
`--scope peer=bob/crew`.

In the fleet view the dashboard does the same when a task is dragged onto a
group on another node, from this node to a linked one, back, or between two
linked nodes. Its confirmation says it is a move across nodes. If the drop
fails, a warning toast names both copies.

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

- The section line shows the number of live tasks. Open, the ＋▾ add button
  and the List | Lanes switch sit on a row below it; folded, the ＋▾ moves up
  onto the section line. The group's summary line carries the breakdown,
  for example `task board: 1 waiting on you · 1 needs you · 1 in progress · 2 next · 1 idea`, so
  a folded group still shows what is on its board (the count's tooltip has it
  too). *Waiting on you* counts the open tasks assigned to the operator, and
  the section line shows it as a tag beside the count. *Needs you* counts the
  blocked and the stalled tasks, and *in progress* counts the other assigned
  tasks. *Next* and *ideas* count the
  unassigned open tasks in each stage.
- **Open**, the *List* view is a table like the Agents and Resources tables
  above it. It lists the queue in your order, then the Ideas band, then the
  *history* line. Each row starts with a cluster of controls: the ⋮⋮ drag
  handle, the one action the task's state calls for (✓ *Done*, ✓ *Next* on an
  idea, ↺ *Reopen* in history), ✎ *Edit*, and ⋯ for the rest. Then come
  *Name* (the task's title), *State*,
  *Last* (its last change) and *Age*, in line with the Agents table's
  columns of the same names. After those come *Who* holds it (the tooltip
  says who assigned it, and when), *Ref* (a chip that links to the tracker),
  *Deps* (`after` and the task's title, amber until that task is done; the
  tooltip has its whole title and id) and *Files*. A blocked
  task shows its reason under its title. The *history* line counts the tasks
  done this week and the dropped ones. Click *history* to list the recently
  closed tasks in the same table.
- **Filter ▾**, next to the view switch in the *List* view, picks which task
  states the list shows: *open*, *queued*, *ideas*, *in progress*, *waiting*
  (on you, the operator), *stalled*, *blocked*, *done* and *dropped*. By
  default it hides *done* and *dropped*, as before. Tick *done* to see this week's
  finished tasks in the list. The *history* toggle shows or hides *done* and
  *dropped* together. The button counts how many states differ from the
  default, and a line under the list says how many tasks the filter hides,
  with *show all*. *Reset* puts the default back. The dashboard remembers the
  filter per group, for you only.
- The section opens on its own when something needs you. If you fold or open
  it yourself, the dashboard remembers that per group.

You can change the board from the section. Each action uses the same route as
a CLI verb:

| In the section | CLI |
|---|---|
| *+ New task › Write it myself* | `add <group> "title" --body … --ref … --resolve-ref [--next] --after … [--assignee …] [--blocked "…"\|--done]` |
| A pasted list in *Write it myself* | `add <group> --lines-file -` |
| *+ New task › Ask an agent* | `ask <group> --to <agent> "…"` |
| *+ New task › From awb/Linear/GitHub…* | `add <group> --from-awb …` / `--from-linear …` / `--from-github …` |
| *+* in a lanes cell (opens the add menu; the choice starts from the cell) | as the menu's items, with the cell's column and the lane's agent |
| Drag a row by its ⋮⋮ handle | `move <task> --before … / --after …` |
| Drag a queued task onto the Ideas line | `demote <task>` |
| ✓ *Next* on an idea | `promote <task>` |
| ⋯ *Block…* (asks the reason in a dialog) / ⋯ *Unblock* | `block <task> --reason …` / `unblock <task>` |
| ✓ *Done* | `done <task>` |
| ⋯ *Drop…* (asks an optional reason in a dialog), or drag the task onto the Groups view's bin | `drop <task> [--reason …]` |
| ✎ *Edit*, or a click on the task's title (in the list or on a lanes card): the edit form | `edit <task> --title … --body … --ref …`, and the verbs below for where it is, the assignee and its files |
| Files in the edit form, *Write it myself* or *Ask an agent* | `attach <task> <file>…` / `detach <task> <name>` · `add --attach` · `ask --attach` |
| *Comment* in the edit form's Activity, and a comment's *Edit* / *Delete* | `comment add <task> …` · `comment edit` · `comment rm` |
| ↺ *Reopen* or ⋯ *Reopen*, on a task under *history* | `reopen <task>` |
| ⋯ *Reopen in Next* / *Reopen as an idea* | `reopen <task> --next` / `reopen <task> --idea [--unassign]` |
| Drag a task under *history* among the live tasks, onto the Ideas line, or onto an agent's row | `reopen` beside that task, in its stage, in one write (`POST …/reopen {"before"\|"after"}`), or `reopen <task> --assignee <agent>` |
| ⋯ *Assign ›* and a member, or drag a row onto an agent's row in the Agents section | `assign <task> --to <agent>` |
| ⋯ *Assign › You (operator)*, or drag a card onto the *You* lane | `assign <task> --to operator` |
| ⋯ *Unassign*, or drag an assigned row onto *drop here to unassign* | `unassign <task>` |
| Drag a task onto another group's Task board or header row (Ctrl/Cmd clones), or ⋯ *Move to group ›* / *Clone to group ›* | `move-to-group <task> <group> [--clone] [--stage next]` |

- **＋ ▾** (*New task*; in wizard mode, *New quest*) in the section head is
  the one way in, in both views. It opens a menu:
  - *Write it myself* opens a form: a title (required, one line, as for
    `add`), a brief, a ref, where it starts, dependencies picked from the
    board's live tasks, and, if you may assign, an assignee from the group's
    members. It can start in any column (see
    [Where a new task starts](#where-a-new-task-starts)): *Idea* (the
    default), *Next*, *In progress* (only if you may assign; it needs the
    assignee), *Blocked* (it asks for the reason) or *Done* (the assignee,
    optional, is who did it). A bare awb or Linear ref gets its
    link, as with `--resolve-ref`. If you paste a list into the title, the
    form adds one task per line instead. A title that is only an awb, Linear
    or GitHub id becomes that ref, titled from the tracker. Files go with the
    task (see [Files on a task](#files-on-a-task)). Enter or Ctrl+Enter adds
    the task, and Esc or *Cancel* closes the form.
  - *Ask an agent* sends a request in your own words to a member, as `ask`
    does. It picks the group's owner when one is online. You can pick any
    other online member who holds `taskboard.write` there and passes the
    writer lock. With no such member online, the item is off, and says why. Files go with the request.
  - *From awb/Linear/GitHub…* opens the import: one task per tracker id.
- **✎ *Edit*** on a row, or a click on a task's title (in the list or on a
  lanes card; Enter on a focused title does the same), opens the edit form.
  Renaming happens there. A press on the title never starts a drag. The
  form's header shows the task's id, and ⧉ copies it. It has the title, the
  brief, the ref (its URL, with the ref as a link beside it while unchanged;
  a new bare awb or Linear id is linked on *Save*, as `edit --resolve-ref`
  does), *Where it is* (*Idea*, *Next*,
  *In progress*, *Blocked* with its reason, *Done* or *Dropped*), the
  assignee (if you may assign), the dependencies (✕ takes one off, and
  *＋ dependency* adds one of the board's other live tasks) and the files.
  Picking an assignee for an unassigned idea or queued task makes it *In
  progress*; picking *Idea* or *Next* for an assigned task keeps its assignee,
  who holds it there, not started. *Save* writes only what
  changed, each through the route its CLI verb uses. If one write is refused
  (say, a close that needs `taskboard.assign`), the rest still land, and the
  form stays open and names what was not saved. A done or dropped task
  reopens when you pick another place for it.
- **Hovering a title** shows the task's card: its title, its id, where it
  is (and why it is blocked), who holds it, who made it and when, its last
  change, its ref and dependencies, its files, and 💬 with the latest comment.
  Once the pointer rests on the title (or at once for keyboard focus), the
  card reads the brief, shown as Markdown and clamped, and the latest comment;
  on a linked node's board they are read from that node. The card shows only
  while the pointer is on the title: it hides a moment after the pointer
  leaves it, and at once when another hover card opens. It never takes the
  pointer, so the tasks under it stay usable, and a click on the title never
  pins it. The list and the cards show no number or id; the card and the edit
  form's header carry the id.
- **Activity** is at the foot of the edit form: the task's
  [stream](#comments-and-activity), oldest first. Changes show as one line
  each, and comments as Markdown, with their files as tiles and a block,
  close, drop or report reason marked as such. A comment is sent at once with
  *Comment* (or Ctrl+Enter), not with *Save*; 📎 or a paste into the comment
  attaches files to it. Your own comments have *Edit* and *Delete*. A row or a
  lanes card with comments shows 💬 and the count; a click opens the edit
  form. On a linked node's board, your comments are your node's.
- In the lanes view, a cell's **+** opens the same menu, anchored at the
  **+** (see below).
- **Dragging** a row above or below another row moves it there. If you drop it
  on the other side of the Ideas line, it is promoted or demoted in the same
  step.
- **Dragging a task to the bin.** While a task is dragged, the Groups view's
  bin shows, labelled *Drop*. A task dropped there is dropped, after a
  confirmation: closed as dropped, it leaves the board and stays in history.
  The bin shows only for a live task you may drop, the same gate as
  ⋯ *Drop…*. Ctrl/Cmd means nothing there.
- **Dragging a task to another group**, a row from the list or a card from
  the lanes, onto that group's Task board section or its header row moves it
  there. Hold Ctrl (Cmd on macOS), as for the Groups view's other clones, to
  clone it instead: the target group turns to the green clone look while the
  key is held, and back when you let go. Both ask first, in the Groups view's
  confirmation. The
  task lands in the ideas, or in Next when dropped on a Next cell of the
  other group's lanes.
  - A move keeps the task and its history. Its dependencies are dropped both
    ways, because they never cross groups, and the confirmation names them.
    An assignee who is not a member of the other group is unassigned and told
    so. Anything but a done task lands open, so a block is lifted.
  - A clone is a new task with the same title, brief and ref, and no assignee
    or dependencies. The original stays, and each task's history notes the
    other.
  - In the fleet view a group on another node is a target too: the task is
    created there first and only then dropped here (see
    [Moving a task to another node](#moving-a-task-to-another-node)).
- Each agent's row in the Agents section shows the tasks assigned to it
  (`☰ 2 tasks · first title`), in amber when one of them is blocked.

#### Links between agents and tasks

The board and the Agents section link to each other. On the board, an agent's
name in its lane head, its avatar on a card (in *triage* and *one row*) and
its name under *Who* in the List are links to the agent's row. On an agent's
row, the first task's title in `☰ n tasks` is a link to that task on the
board.

- **Click** (or Enter) unfolds whatever hides the target, such as the group,
  the section or a lanes cell past its *+ n more*, then scrolls there with a
  short smooth scroll and a brief flash.
- **Hover** (or focus) never unfolds. It outlines the target. When something
  folded hides it, the folded header or the cell's *+ n more* is outlined
  instead, with a dashed line. When the target is off-screen, a small bar on
  the top or bottom edge of the window points the way.

With *reduce motion* set in your system, the scroll jumps and the flash only
fades.

### Swim lanes

The *☰ List | ⇶ Lanes* switch in the section head picks the view. In *Lanes*,
*lanes agent | role | subgroup | triage | one row* picks the lanes: one per
agent, grouped by agent, role or subgroup (by agent unless you pick another),
or the two modes without a lane per agent, described below. The dashboard
remembers both per group, for you only.

The lanes view lays the same board out as a grid. It has five columns: *Ideas*,
*Next*, *In progress*, *Blocked · needs you* and *Done this week*. *Blocked ·
needs you* also holds stalled tasks.

A cell shows its first three cards. When it holds more, *+ n more* opens it in
place, so one busy cell does not stretch its whole row. An open cell grows up
to about a dozen cards, then scrolls inside. *▴ Show less*, under its cards,
closes it again. The column heads still count every task. When the card is too
narrow for every column, the grid scrolls sideways and the lane names stay
pinned on the left.

The operator's lane comes first: *You*, tinted, with how many tasks wait on
you (on a linked node's board, *operator of <node>*). It shows while the
operator holds a task, or when you may assign. Its tasks stand in *In
progress*, or in *Ideas* or *Next* while held there. A card dragged onto it
is assigned to the operator, and its **+** starts a task assigned to the
operator. In the *List*, *Who* says *you* and *State* says *waiting* (an idea
you hold says *idea*).

The *Unassigned* lane comes next, then one lane per agent, grouped by role
(the owner's role first), by subgroup, or not grouped. Each lane head shows the
agent's state and how many tasks it holds. A name too long for the lane column
ends in "…"; hover it for the full name.

You can arrange the grid for yourself. The dashboard remembers the
arrangement per group, for you only. It is not part of the board, so nobody
else sees it.

- **Column widths.** Drag the right edge of a column head, including the
  *Lane* column, to resize it. Every column keeps a minimum width. Double-click
  an edge to reset every width. A focused edge also moves with ← and →.
- **Lane order.** Drag a lane head onto another lane to move it there. The
  *Unassigned* lane always stays first, and a new agent's lane joins at the
  end. With lanes by role or by subgroup, a lane moves within its heading, and
  you drag a heading to reorder the headings. A focused lane head or heading
  also moves with Alt+↑ and Alt+↓. *reset order*, next to the *lanes*
  switch, puts the default order back.

A lane group whose agents are all offline folds; click *show offline lanes* to
see it. The one striped cell, which cannot hold a card, is the Unassigned
lane's *In progress*. An agent's *Ideas* and *Next* hold the tasks it was
given and has not started.

Dragging a card is one action, the same as its CLI verb:

| Drag | CLI |
|---|---|
| Ideas → Next, in the same lane | `promote <task>` |
| Next → Ideas | `demote <task>` |
| Onto *Blocked* (asks the reason) | `block <task> --reason …` |
| Blocked → Next or In progress | `unblock <task>` |
| Onto *Done* | `done <task>` |
| Done → Next or In progress, in the same lane | `reopen <task>` (an idea comes back in next) |
| Done → another lane's Next or In progress, or the operator's lane | `reopen <task> --next --assignee <agent>` (`--unassign` onto the Unassigned lane) |
| Done → the Unassigned lane's *Ideas* | `reopen <task> --idea --unassign` |
| Done → *Blocked* (asks the reason) | `reopen <task>` (and the lane's assignee), then `block <task> --reason …` |
| Onto an agent's *In progress* | `assign <task> --to <agent>` (an idea is promoted) |
| Onto an agent's *Ideas* or *Next* | `assign <task> --to <agent> --idea` / `--next` (not started) |
| In an agent's lane, Ideas or Next → In progress | `start <task>` |
| In an agent's lane, onto Ideas or Next | `hold <task> --idea` / `--next` |
| Onto the Unassigned lane's *Next* | `unassign <task>` |
| Onto an agent's *Blocked* (asks the reason) | `assign <task> --to <agent>`, then `block <task> --reason …` |
| From an agent's lane onto the Unassigned lane's *Ideas* | `unassign <task>`, then `demote <task>` |
| From an agent's lane onto the Unassigned lane's *Blocked* (asks the reason) | `unassign <task>`, then `block <task> --reason …` |

Where a drag asks the reason, it asks in the same reason dialog as ⋯
*Block…*: the reason is required and may take several lines (Enter adds a
line), Ctrl/Cmd+Enter blocks, Escape cancels and nothing is written. A
refused write keeps the dialog open with the reason typed. A multiline
reason keeps its line breaks under the task's title and on its hover card.

A drag to another lane and another column at once runs both writes in that
order, the lane first. A move the board cannot make in one step, such as
unassigning and closing a task, is not accepted.

Each cell that a new task can start in has a small grey **+** in its
lower-right corner, which brightens when you hover the cell. That is every
cell but the striped ones. An agent's lanes show no **+** unless you may
assign. The **+** opens the add menu, and whichever item you pick starts from
the cell:

- *Write it myself* has the cell's column picked as where the task starts.
  An agent's lane also picks that agent as the assignee: its *In progress*
  starts the task, its *Ideas* and *Next* hold it there, not started.
- *Ask an agent* opens with a first line that says where the tasks should
  land, such as "Land the tasks in Next for builder", for you to add your
  request to.
- *From awb/Linear/GitHub…* imports into the queue from a Next cell or an
  agent's lane. An import lands in Ideas or Next only, so you assign, block or
  close the tasks on the board afterwards. Order within the queue is set in the
*List* view.

#### Triage and one row

Two *lanes* modes do without a lane per agent, for a board where you mostly
want to see what nobody holds and what waits on you:

- **triage** has three lanes: *You* (the operator's tasks, tinted as the
  operator's lane is), *Unassigned*, and *Agents*, every agent's tasks
  together.
- **one row** puts every task in a single row.

In these modes each card says who holds it in its top-right corner: a yellow
**!** when nobody does, a red **!** when you (the operator) do, otherwise the
agent's avatar. The two **!** cards also get an edge in their colour. A done
card carries no **!**. A cell shows its first five cards, then *+ n more*.

Drags work as in the lanes. In *triage*, a card dropped onto *You* is
assigned to you, and one dropped onto *Unassigned* is unassigned. *Agents*
does not say which agent, so it takes only an agent's own card, moving it to
another column; assign a task to an agent in its edit form (click its
title) or in *lanes agent*. In *one row*, a drag changes the column and keeps the assignee. A
cell's **+** starts an unassigned task, or in *You* one assigned to you.

The dashboard shows a board only to someone who can read it. The operator sees
every group. The section is missing while `features.taskboard` is off. A
linked node's groups, in the fused view or on that node's page, show the
boards that node lets you read. Their actions appear only where it granted
you `taskboard.write` (see [Across linked nodes](#across-linked-nodes)). Assigning,
unassigning, and *Done* or *Drop* on a task assigned to someone else also need
`taskboard.assign`. Without it, those actions don't appear in the list and
those drags aren't accepted in the lanes.

## API

| CLI | HTTP |
|---|---|
| `ls <group>` | `GET /v1/groups/{g}/taskboard[?stage=&all=1]` |
| `ls` (own group) | `GET /v1/taskboard` |
| `show` | `GET /v1/taskboard/tasks/{id}` |
| `activity` / `history` | `GET /v1/taskboard/tasks/{id}/activity` → `{id, group, title, entries}` (`…/events` answers the same) |
| `comment add` | `POST /v1/taskboard/tasks/{id}/comments {"body","key"?}` (multipart: a `comment` JSON part and `file` parts) → 201 `{id, group, comment}`, 200 on a repeated key |
| `comment edit` / `rm` | `PATCH` `{"body"}` / `DELETE /v1/taskboard/tasks/{id}/comments/{cid}` → `{id, group, comment}`; 403 `not_author` for anyone else |
| `comment ls` | `GET /v1/taskboard/tasks/{id}/comments` → `{id, group, comments}`; a comment's file is read through `…/attachments/{aid}` |
| `add` | `POST /v1/groups/{g}/taskboard/tasks` (`--resolve-ref` sends `"resolve_ref":true`; `--assignee` sends `"to"`, `--blocked` sends `"status":"blocked","block_reason"`, `--done` sends `"status":"done"`, `--blocks` sends `"blocks"`) |
| `ask` | `POST /v1/groups/{g}/taskboard/ask {"to":"<agent>","prompt":…}` |
| `add --from-*` / `--lines-file` | `POST /v1/groups/{g}/taskboard/tasks {"refs":[…],"lines":[…],"stage"?}` |
| `edit` | `PATCH /v1/taskboard/tasks/{id}` (`--resolve-ref` sends `"resolve_ref":true`) |
| `move` | `POST /v1/taskboard/tasks/{id}/move` |
| `move-to-group` | `POST /v1/taskboard/tasks/{id}/move-to-group {"group":…,"clone"?,"stage"?}` → `{task, group, from, clone, dropped_deps, dropped_dependents, unassigned}` |
| `move-to-group … <group>@<peer>` | on the peer `POST /v1/groups/{g}/taskboard/transfer-in {"title","body","ref","ref_label","stage","clone","origin":{"group","task"}}` → the new task; then here `POST /v1/taskboard/tasks/{id}/moved-away {"to":{"node","group","task"}}` |
| `promote` / `demote` | `POST /v1/taskboard/tasks/{id}/stage {"stage":"next"\|"idea"}` |
| `block` / `unblock` / `drop` / `done` | `POST /v1/taskboard/tasks/{id}/status` (`block --on` sends `{"status":"blocked","on":"tk_…"}`) |
| `reopen` | `POST /v1/taskboard/tasks/{id}/reopen {"stage"?: "next"\|"idea", "to"?: "<agent>"\|"operator"\|"", "before"?\|"after"?: "tk_…"}` (no `to` keeps the assignee; `""` takes it off; `before`/`after` places it beside a live task, in its stage, and needs `taskboard.write`) |
| `assign` / `take` | `POST /v1/taskboard/tasks/{id}/assign {"to":"<agent>"\|""}` |
| `unassign` / `release` | `POST /v1/taskboard/tasks/{id}/unassign` (`release` sends `{"self":true}`) |
| `assign --idea` / `--next` / `--hold` | `POST /v1/taskboard/tasks/{id}/assign {"to":…,"start":false,"stage"?:"idea"\|"next"}` |
| `add --assignee X --idea` / `--next` | `POST /v1/groups/{name}/taskboard/tasks {…,"to":"X","start":false,"stage":"idea"\|"next"}` |
| `start` / `hold` | `POST /v1/taskboard/tasks/{id}/start {"start":true}` / `{"start":false,"stage"?:"idea"\|"next"}` |
| `report` | `POST /v1/taskboard/tasks/{id}/report {"outcome":"done"\|"failed","note":…}` → `{task, repeated, blocked}` |
| `ls --mine` | `GET /v1/taskboard/mine` |
| `ls --summary` (and the dashboard) | `GET /v1/taskboard/context` |
| `groups set-taskboard-writers` | `GET` / `POST /v1/groups/{g}/taskboard/writers {"agents":[…]}\|{"members":true}` |
| `attach` | `POST /v1/taskboard/tasks/{id}/attachments` (multipart `file` parts) → the task |
| `attachments` | `GET /v1/taskboard/tasks/{id}/attachments` → `{id, group, attachments}` |
| `attachments --save` | `GET /v1/taskboard/tasks/{id}/attachments/{aid}` (the bytes; `?format=json` → base64 `data`) |
| `detach` | `DELETE /v1/taskboard/tasks/{id}/attachments/{aid}` → the task |

`add --attach`, `ask --attach` and a move to another node with files send
their usual JSON as the `task` part of a multipart request, with one `file`
part per file. Each task carries `attachments` (`id`, `name`, `size`,
`content_type`, `sha256`, `image`, `added_by`, `added_at`) when it has files.
An image is served inline and anything else as a download. A peer view
carries JSON only, so a linked node reads a file with `?format=json`. A file
larger than one peer view request (32 KiB) reaches a linked node through the
staged upload, `POST /api/peer-upload/{node}?path=…` from the dashboard or
`POST /v1/federation/peer-upload/{node}?path=…` from the operator's CLI. It
runs under the same grants as a direct request.

An activity entry carries `id`, `at`, `actor`, `actor_name`, `kind`, and
`detail` / `detail_text`, `changes`, `action`, and `comment` when they apply. A
comment carries `id`, `kind`, `body` (its current body, empty once deleted),
`key`, `author`, `author_name`, `at`, `edited_at`, `deleted_at` and
`attachments`. A `comment_edited` entry carries `comment_id` and
`previous_body`. Each task carries `comment_count`, `created_by_name`,
`last_edited_by`, `last_edited_by_name` and `last_edited_at`.

The group listing also returns `locked`, and `writers` when the board is
locked. Each task carries `assignee` (`agent_id`, `name`; for the operator
`{"agent_id":"operator","name":"operator","operator":true}`), `blocked_on`
when it waits on an operator task, `assigned_by`,
`assigned_at`, `outcome`, `outcome_note` and `failures` when they are set.
Every change is recorded in the audit log as `taskboard.<verb>`, with the
group, the task id and, for an assignment, the target agent.

`status` also accepts `{"status":"open"}` on a done or dropped task. This
reopens the task in the stage and position it had before, as a plain
`reopen` does.

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
