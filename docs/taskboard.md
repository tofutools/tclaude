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
- Work that is deliberately deferred but should stay in view, such as a task
  that waits on time rather than on work, can be parked in the board's
  [backlog](#the-backlog) instead. It is hidden by default.

Agents should treat the board the same way. Don't use it to keep long-term
items or as a record of everything planned. Park those in the tracker.

## Stages and statuses

Every task has a **stage** and a **status**. Both are stored and set by hand.
Neither one is inferred from age or position.

| Stage | Meaning |
|---|---|
| `backlog` | Parked. Deliberately deferred: never started, queued or nudged. Hidden by default. See [The backlog](#the-backlog). |
| `idea` | Captured but not queued yet. `add` puts a task here unless you pass `--next`. |
| `next` | In the queue. The top task is the next one to do. |

| Status | Meaning |
|---|---|
| `open` | On the list. |
| `blocked` | On the list, with a reason. |
| `done` | Closed as finished. |
| `dropped` | Taken off the board, for example parked back in the tracker. |

`ls` shows the queue (`next`) first, then the ideas, each in the operator's
order. The backlog comes last, and only with `ls --backlog` or `--all`. Blocking, closing and reopening a task never changes its stage, and
reopening puts it back where it was. A task can depend on others (`--after`).
An open task with a dependency that is not yet done shows as `queued`.
A dropped dependency counts as not done, so the task stays queued until you
remove the dependency (`edit --rm-dep`) or reopen the dropped task. A task is
also queued while it waits in line for a full lane, on a board with
[work-in-progress limits](#work-in-progress-limits).

### The backlog

The backlog is a stage before the ideas, for parked work: tasks that should
stay on the board but are deliberately not up for consideration now. A
typical one waits on time, not on work.

```bash
tclaude agent taskboard add crew "One-time prompt in doctor" --backlog
tclaude agent taskboard backlog tk_…              # park a task (move --stage backlog)
tclaude agent taskboard promote tk_…              # back to the top of the ideas
tclaude agent taskboard move tk_… --stage next    # or straight into the queue
tclaude agent taskboard ls crew --backlog         # list it, after the ideas
```

- **Never in play.** A parked task is never started, queued for a full lane
  or nudged. The idle check, the work-in-progress limits, auto-start and the
  lanes all ignore it. Parking a started task holds it: it keeps its
  assignee, not started. Parking takes a task out of its custom column and
  out of any lane's queue. `start`, `take` or an assign that starts it takes
  it out of the backlog, to next.
- **Out of the way.** A plain `ls` leaves the backlog out and ends with a
  `+N in backlog` line (`backlog_hidden` in `--json`). The counts keep it
  apart: it is not in the live count, nor in *needs you*, even when a
  parked task is blocked or held by the operator. `ls --summary` adds
  `N in backlog`. Nothing can wait on a parked operator task (`block --on`).
- **Step by step.** `promote` and `demote` move one stage: backlog ⇄ idea ⇄
  next. So `demote` on an idea parks it, and `promote` on a parked task brings
  it back to the ideas. `backlog <task>` parks a task from any stage.
- **Order.** The backlog keeps its own order. `move --top`, `--before` and
  `--after` reorder it, and a move beside a parked task parks the moved one.
- **Epics.** An epic's tasks may be parked. Its progress counts them as not
  started (`N in backlog`). An epic itself is not parked.
- **Who.** Parking and bringing back is a change of the task:
  `taskboard.write` defers any task, `taskboard.own` only your own.
- **Moves.** A parked task moved to another group stays parked unless the
  move names a stage. An unassigned task reopens in the stage it was
  closed in, so a parked one dropped and reopened is parked again (an
  assigned one comes back started, in next, as any reopen does).
- **Columns.** `backlog` is a built-in name, so a custom column cannot take
  it, and `--stage backlog` always names the stage.

### Custom columns

A group's board may also have up to four **custom columns** of its own, such
as *In review* or *In CI*. They sit between *In progress* and *Blocked*, in
the group's order. The built-in columns (*Ideas*, *Next*, *In progress*,
*Blocked*, *Done this week*) are fixed.

```bash
tclaude agent taskboard columns add crew "In review"       # slug in-review
tclaude agent taskboard columns add crew "In CI" [--slug ci] [--before in-review]
tclaude agent taskboard columns ls crew
tclaude agent taskboard columns rename crew in-review "Awaiting review"
tclaude agent taskboard columns move crew in-ci --before in-review | --last
tclaude agent taskboard columns rm crew in-ci

tclaude agent taskboard move tk_… --stage in-review         # into the column
tclaude agent taskboard move tk_… --stage next              # back out
tclaude agent taskboard add crew "Ship it" --stage in-review    # start there
tclaude agent taskboard reopen tk_… --stage in-review       # reopen into it
```

- **Who.** Adding, renaming, reordering, removing, hiding and cordoning
  columns needs `taskboard.columns` on the group. `taskboard.manage` covers
  it, and owners hold both. Listing them needs
  `taskboard.read`. Moving a task in or out is a change of that task:
  `taskboard.write`, or `taskboard.own` on a task assigned to you. A worker
  that opens its PR moves its own task to *In review*.
- **Slugs.** A column's slug is made from its label (`In CI (macOS)` →
  `in-ci-macos`) unless you give `--slug`, and it stays the same across a
  rename. `--stage` takes the slug or the label. The names of the built-in
  columns and states (`next`, `blocked`, `done`, `stalled` and the like) are
  refused.
- **A task in a column** is a live task in stage `next`. It keeps its
  assignee and its lane, and it may be unassigned. An idea moved into a
  column is promoted first. `--stage next` (or `promote`) takes it out again, and
  restarts the idle clock: a started task
  is in progress once more, and a held one is queued. Blocking a task keeps
  its column, and unblocking it puts it back there. Closing it (done or
  dropped), demoting it to the ideas, or moving it to a group that has no
  column with the same slug takes it out. A reopened task comes back in next,
  unless `reopen --stage <column>` puts it straight into a column. `add
  --stage <column>` starts a new task in one. Either is one write; the add
  records one *created* entry, the reopen its reopen and the move into the
  column.
- **It does not stall.** A task in a custom column waits on something outside
  its agent's turn, such as a review or CI, so the [idle
  check](#idle-without-report) skips it, unless the board lists the column in
  `idle.check_columns`.
- **Removing a column** puts its tasks back in next. Each one's history
  records the change. The history records every move in or out as a `column`
  change.
- **Counts.** `ls` marks a task in a column with the column's slug where the
  state goes, and lists the board's columns. `ls --summary` counts each column
  (`2 in progress · 1 In review · 3 next`). Those tasks are not counted in
  progress or next. A blocked task still counts as blocked.

A move to another node does not carry the column; the task lands in the
stage the move names.

### Hiding and cordoning columns

A board can hide a column, or cordon a custom one:

```bash
tclaude agent taskboard columns hide crew in-ci          # also: ideas, done
tclaude agent taskboard columns show crew in-ci
tclaude agent taskboard columns cordon crew in-review    # alias: disable
tclaude agent taskboard columns uncordon crew in-review  # alias: enable
```

- **Hidden.** The board does not draw the column for anyone. Its tasks keep
  their column, and the lanes count them in a *◌ N in hidden columns* tag.
  *Ideas*, *Done this week* and the custom columns can be hidden. *Next*, *In
  progress* and *Blocked* are always shown. In the *List*, a hidden column's
  tasks are filtered out by default, until you change the *Filter*.
- **Ideas hidden.** A new or imported task that does not name its stage
  starts in next, not in the ideas. A named stage (`--idea`) still holds.
- **Cordoned.** A custom column is still drawn, greyed and hatched, but takes
  no new task. A move, an `add --stage`, a `reopen --stage` or a drop into it
  is refused, and a task moved in from another group lands outside it. The
  tasks already in it stay and may leave. The stage pickers do not offer it.
- **Hidden for you.** In the dashboard you can also hide a column only for
  yourself (its ⋯ *Hide for me*, or *Columns ▾*). That choice is kept in your
  browser, per group, and does not change the board for anyone else.
- `columns ls` and `ls` mark each column's state, and `ls --summary` counts
  a hidden or cordoned column with its state (`1 In review (cordoned)`).

## Commands

```bash
tclaude agent taskboard ls [group] [--stage backlog|idea|next] [--backlog] [--all] [--mine] [--json]
tclaude agent taskboard ls --summary [--json]   # counts for every board you can read
tclaude agent taskboard show <task>        # tk_… id
tclaude agent taskboard activity <task>    # comments and changes, oldest first; history is an alias
tclaude agent taskboard comment add <task> "markdown" [--body …|--file f|-|--stdin] [--key k] [--attach f]…
tclaude agent taskboard comment edit <task> <#id> "markdown" [--body …|--file f|-|--stdin]
tclaude agent taskboard comment rm <task> <#id>
tclaude agent taskboard comment ls <task> [--save <dir>]

tclaude agent taskboard add <group> "title" [--body …|--body-file f] [--ref URL|id] [--add-ref URL|id]… [--resolve-ref]
                                    [--next|--backlog|--stage <custom column>] [--after tk_a,tk_b] [--top|--before tk_x]
                                    [--assignee <agent>|operator|--mine] [--blocked "reason"|--done] [--attach f]…
tclaude agent taskboard add <group> "title" --assignee operator --blocks tk_mine   # ask the operator; tk_mine waits
tclaude agent taskboard ask-operator <task|group> "question?" [--option "A: why"]… [--block|--blocks tk_mine] [--wait 10m]
tclaude agent taskboard answer <task> [--question n] --pick <key|label|n>[,…] [--text "…"]
tclaude agent taskboard ask <group> --to <agent> "what you want" [--attach f]…   # or --prompt-file f
tclaude agent taskboard attach <task> <file>…
tclaude agent taskboard detach <task> <name|#id>…
tclaude agent taskboard attachments <task> [--save <dir>]
tclaude agent taskboard edit <task> [--title …] [--body …|--clear-body] [--ref …|--clear-ref] [--resolve-ref]
                                    [--add-ref …]… [--rm-ref #n|link|label]… [--first-ref #n|link|label] [--add-dep tk_…] [--rm-dep tk_…]
tclaude agent taskboard move <task> --top|--bottom|--before tk_x|--after tk_x
tclaude agent taskboard move <task> --stage backlog|idea|next|<custom column>
tclaude agent taskboard columns ls|add|rename|move|rm <group> …   # custom columns
tclaude agent taskboard move-to-group <task> <group>[@<peer>] [--clone] [--stage backlog|idea|next]
tclaude agent taskboard promote <task>     # idea → end of the queue; backlog → top of the ideas
tclaude agent taskboard demote <task>      # next → top of the ideas; idea → top of the backlog
tclaude agent taskboard backlog <task>     # park it: top of the backlog
tclaude agent taskboard block <task> --reason "waiting on CI"
tclaude agent taskboard block <task> --on tk_…        # wait on an operator task
tclaude agent taskboard unblock <task>
tclaude agent taskboard drop <task> [--reason "parked in awb"]
tclaude agent taskboard done <task>
tclaude agent taskboard reopen <task> [--next|--idea|--stage <custom column>] [--assignee <agent>|operator|--unassign]

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
  queue and then the ideas (and, with `--backlog`, the backlog). Done and
  dropped tasks are not numbered.
- `ls` hides the backlog and ends with `+N in backlog` when it has any;
  `ls --backlog` lists it.
- `ls --all` also lists the backlog, and the done and dropped tasks that are
  not archived yet.
- `ls --summary` prints one line of counts for every group board you can
  read, for example `crew: 9 · 1 waiting on you · 1 blocked · 1 stalled · 2 in progress · 6 next · 3 ideas`.
  These are the counts the dashboard section shows.
- If `move --before` or `move --after` names a task in the other stage, the
  moved task is promoted or demoted in the same write. Through the API a
  move may also name the stage it expects (`"stage":"next"`): if the task
  it is placed beside has crossed to the other stage meanwhile, the move is
  refused as `stale` (409) and nothing changes. The dashboard always sends
  it.
- A run of moves of one task by the same actor, less than ten minutes apart,
  is one `moved` entry in its history, saying where the last one put it. A
  move mails nobody.
- `--ref` takes an `http(s)` URL, `owner/repo#12` for GitHub, or a bare tracker
  id. `LIN-123` is read as a Linear id, and a lower-case `tcl-a1b2c3` as an awb
  id. Prefix the id with `awb:` or `linear:` to say which one you mean. A
  ref's URL is what it is: the board shows it under a short label (the awb
  or Linear id, `owner/repo#12`) as a link.
  - An awb id is linked on this node's awb server, as `<server>/#/issues/<id>`.
    The server is the configured awb proxy's (`agent.awb_proxy.url`) or, without
    one, the server the operator's own `awb` CLI is configured for (what
    `awb status` shows, read from `AWB_CONFIG_FILE` or awb's config files; the
    daemon asks it at most every few minutes). The link is stored wherever the
    id is given: `add`, `edit`, an import, a move to another group.
  - A Linear id is linked in its team's Linear workspace
    (`https://linear.app/<workspace>/issue/<id>`). The workspace is learned from
    any link of one of the team's issues: a Linear lookup, or a ref given by its
    URL. Until then the id has no link: pass `--resolve-ref` to `add` or `edit`,
    and it is looked up through the Linear proxy, as an import does. The title
    stays yours. An import (below) always looks it up. `edit <task>
    --resolve-ref` with no `--ref` links the task's own ref.
  - A ref stored without a link is linked as the board is read, whenever its
    tracker's link is known by then. The link is derived again on every read and
    not written back, so a server configured later links every old ref. A stored
    link always wins. `--resolve-ref` stores the link.
  - A ref that still has no link says why (`link_note`): no awb server is
    configured, the Linear team's workspace is not known yet, and so on. `show`
    prints it under the ref, and the dashboard shows it in the ref's tooltip. A
    `--resolve-ref` that links nothing prints why on stderr (`resolve_notes` in
    the JSON answer).
  - A linked node's board shows its refs as that node links them, with its own
    awb server and Linear workspaces.
- A task can have several refs, at most 10: its tracker issue and the pull
  request that does it, say. `--ref` is always the task's first ref, so
  everything that reads one ref keeps reading that one.
  - `add --add-ref` (repeatable) adds refs after `--ref`. Without `--ref`,
    the first one added is the first ref.
  - `edit --add-ref` appends refs, and leaves out one the task already has
    (the same link, issue or pull request however it is written).
    `edit --rm-ref` removes refs, each named by `#position` (`#2`), link or
    label. Removing the first ref makes the next one first.
  - `edit --first-ref` makes a ref the first, named the same way, after any
    `--add-ref` and `--rm-ref` in the same edit (so a ref just added can be
    made first). The others keep their order. The first ref is the one `ls`,
    the List and every reader of one ref show.
  - `edit --ref` replaces the first ref and keeps the rest; one the task
    already has is not kept twice. `--clear-ref` removes them all.
    `--resolve-ref` links the added refs too.
  - Through the API, `clear_ref` (or `"ref":""`) removes only the first ref,
    and the next one becomes first. So a form that shows one ref never drops
    refs it does not show. `clear_refs` removes them all; `--clear-ref` sends
    both.
  - `show` lists every ref; `ls` shows the first and how many more (`#12 +1`).
  - A GitHub pull request or GitLab merge request ref carries its status:
    state, title, CI and reviews, as the agent rows show them. It is read
    from the same caches the agent rows read, so a board read sends no
    request to the forge. A pull request no agent shows has whatever the
    hover cards last read for it, and at least its number.
  - An awb, Linear or GitHub issue ref carries its issue's title and status
    (`tracker`) as its last lookup found them. A lookup happens on
    `--resolve-ref`, an import, and the dashboard's ref lookup. A board read
    asks no tracker. A ref never looked up has none.
  - In the dashboard each ref shows by its kind. A pull request is the agent
    rows' chain: #N, CI and review, with their hover cards. An awb, Linear or
    GitHub issue is its tracker's badge, its id as a link that opens in a new
    tab, its status and (in the tooltip) its title. Anything else is a link. A
    lanes card shows every ref; the List's *Ref* column shows the first and
    `+n`. The edit form's *Refs* row has one line per ref, with its title;
    ↑ makes it the first and ✕ takes it off, on Save. Its add box adds one
    (Enter or *+ Add*), and Save links a bare awb or Linear id. The New task
    form's *Refs* row works the same way: the first becomes the task's ref,
    the rest follow it. Opening the form looks each awb or
    Linear issue up once, so its status is current there and on the board
    after. A linked node from before several refs takes only one ref in the
    form.
  - A linked node from before several refs keeps only the first: `add` and
    `edit` with `--add-ref`, `--rm-ref` or `--first-ref` on it fail, saying so. A move to
    such a node leaves the original where it is, with all its refs, and says so.
    The dashboard's move does the same.
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
- A drawing-board export can be put on a task as a PNG with `drawing-board
  export <board> --attach-to <task>` (see [Drawing boards](drawing-boards.md)).
  It links back to the board: the file's tile has *✎ Edit in drawing board*,
  and exporting the same part again updates the file in place.
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
  with its thumbnail), and ✕ removes one on *Save*. Click a tile to open it
  in the attachment viewer the Messages tab uses: an image with zoom and
  ‹ › to step through the task's other images, a Markdown file as the
  rendered document with its source a toggle away. A file the viewer cannot
  show opens as before: a PDF in a new tab, anything else as a download.
  A comment's files work the same way.
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

## Epics

An epic groups the tasks of one piece of work: a feature, a migration, a
release. It sits on the board like a task (an idea, then promoted to next),
but nobody works on it directly. It has no assignee and no column, and it is
never blocked: its tasks carry the work, and its progress is theirs.

```bash
tclaude agent taskboard add crew "Taskboard epics" --epic
tclaude agent taskboard add crew "Schema and migration" --parent tk_1a2b3c4d
tclaude agent taskboard edit tk_5e6f7a8b --parent tk_1a2b3c4d   # into an epic
tclaude agent taskboard edit tk_5e6f7a8b --rm-parent            # out of it
tclaude agent taskboard epics crew                              # open epics, with progress
tclaude agent taskboard ls crew --epic tk_1a2b3c4d              # one epic's tasks
tclaude agent taskboard show tk_1a2b3c4d                        # the epic, its tasks and progress
```

- A task is in at most one epic, of its own group. There is one level: an
  epic is never inside another epic.
- Moving a task into, out of or between epics is recorded in its history and
  in each epic's. To undo a move, move the task back.
- Progress counts the epic's tasks: done, in a custom column (In review and
  the like), in progress, blocked, next and ideas. A dropped task is not
  counted in the total.
- When the last open task of an epic closes, the board asks "close epic?",
  and the agent that created the epic gets one `[taskboard] close epic?`
  mail. Nothing closes an epic by itself. A task that is added or reopened
  takes the question back.
- `done` on an epic with open tasks is refused unless you pass `--force`.
  `drop` on an epic keeps its tasks: they stay on the board, out of the
  epic, and each one's history records it.
- An epic is archived with its tasks, never before them: archiving it by
  hand also archives its closed tasks, and is refused while any task is
  open. The sweep and `archive --done` leave an epic until all of its tasks
  are archived. A task taken out of the archive brings its epic out too.
  The retention purge deletes an epic only once none of its tasks is left,
  so a task never loses its epic.
- Only an open epic takes new tasks; reopen a closed one first. Reopening an
  epic brings it back with no assignee and in no column.
- `move-to-group` takes a task out of its epic, because an epic holds only
  tasks of its own group. An epic that still has tasks cannot move, and an
  epic never moves to another node. A clone of an epic is an epic, without
  the tasks.

Epics use the same routes and grants as tasks, so a linked node works with
them under the `taskboard.read` and `taskboard.write` it was given.

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
- **Markdown.** A brief, a comment and a question's body are Markdown,
  shown rendered wherever the board shows them (the card, the edit form, the
  activity): headings, emphasis, lists and task lists,
  quotes, code, tables and links. `![name](file.png)` shows one of the
  task's (or the comment's) files inline, by its name, so a file added with
  `--attach` can illustrate the brief.
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
  mails the agent that created the task, when an agent did. A board may change
  who is mailed, or turn comment mail off ([the board's
  messages](#the-boards-messages)).
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
- **Counts.** A live task the operator holds counts as *waiting on you*, a
  blocked one too (it also counts as blocked):
  `ls --summary` prints `1 waiting on you`, and `tclaude agent fleet` lists
  the board. The context route's counts carry `waiting_on_you`, and
  `needs_you` is the same count.

### Questions for the operator

An agent can ask the operator a question, with choices to pick, and get the
answer back (`ask-operator`). A question is asked **on a task**: it is an
entry of the task's activity, numbered per task (Q1, Q2…), and the answer is
recorded right under it. Scrolling a task's activity shows every question
asked on it and how each was answered.

```bash
# On a task (yours, or any task of your group's board):
tclaude agent taskboard ask-operator tk_4f0a "Lane highlight colour?" \
  --body "The lane needs a tint that reads as yours." \
  --option "Amber: warm, matches the ★" --option Teal --option "No tint" \
  --recommend 1 --block --wait 10m

# With images for the operator to look at: attach them.
tclaude agent taskboard ask-operator tk_4f0a "Which layout?" \
  --attach mock-a.png --attach mock-b.png --option A --option B

# On a group: a new task in the operator's lane, made for the question.
tclaude agent taskboard ask-operator crew "Ship behind a flag?" --option Yes --option No --blocks tk_mine
```

**To show the operator an image (or any file), attach it.** `--attach
<file>` (repeatable; at most 10, 4 MiB each, the types `taskboard attach`
takes) sends it with the question: it becomes a file of the task, as
`taskboard attach` makes it, and the question lists it, so the Answer dialog
previews it with the question, an image large, a click enlarging it.
`--attach-existing <#n|name>` (on a task) names a file already on the task
the same way. A local file path pasted into the body is no use to the
operator, who reads the question in the dashboard, maybe on another device:
`ask-operator` warns (on stderr; the question is still asked) when the body
names a local image file and nothing is attached. Over HTTP, `POST
/v1/taskboard/tasks/{id}/questions` takes the question's JSON as the "task"
part of a multipart request with "file" parts, and `attach_existing:
["#13", "name.png"]`; the question's `attachment_ids` list the task's files
it shows.

- `--option "Label"` or `--option "Label: description"`, repeated: none, or
  2 to 6. `--options-file q.json` takes them as JSON instead,
  `[{"label","description","preview","key"}]`, where a preview is
  monospace text of up to 2 KiB that the answer dialog shows beside the
  options. `--multi` lets the operator pick several; `--recommend` marks
  one, by number, key or label. Without options the operator answers in
  words. Free text is always allowed: a pick, words, or both.
- **On a task.** The task keeps its assignee and stays in its lane. The
  question shows as a card of its own in the operator's lane, or Triage's
  *You* row (In progress),
  until it is answered, and the task's card says `❓ n asked you`.
  `--block` blocks the task (`waiting on operator: Q2 …`) until the
  question is answered or withdrawn; the history then records `unblocked:
  Q2 answered`. Asking is gated as commenting is (`taskboard.own` on your
  own group's board, or `taskboard.write`); `--block` as a change of the
  task.
- **On a group.** A new task is made for the question, assigned to the
  operator, titled with it: its first activity entry is the question.
  Answering it closes that task as done with the answer as its reason, and
  the tasks waiting on it (`--blocks`) reopen. A question task that is
  answered is not reopened: ask again instead. `--idea` puts it in the
  operator's ideas, not waiting on them (no `--blocks` or `--wait` then).
- The operator answers in the dashboard: *Answer…* on the question's card,
  on the task's card, in the List's ⋯ menu, or in the task's edit form
  (its *Questions*, at the top, and each open question in its activity).
  Every one opens the same Answer dialog, which shows the question with
  everything needed to decide: its context rendered as Markdown, the files
  that go with it opening in the attachment viewer (first, large, the
  files the question was asked with or named; then the task's other
  files), and each
  option with its description and preview, the recommended one marked.
  Or with `answer <task> [--question n] --pick
  <key|label|n>[,…] [--text "…"]`; `--question` is needed only when the
  task has several questions open. The agent that asked gets `[taskboard]
  answered Q2: <group> · tk_… "<task>"` (`answered:` for a question task)
  with the picks, the words and the answer as JSON. A question is answered
  once. The asker is fixed when it is asked, so the answer goes back to it
  whoever holds the task by then.
- Answering is the operator's, or a `taskboard.assign` holder's, never the
  asking agent's. The asker withdraws an open question by deleting its
  entry (`comment rm <task> <#id>`); an answered question and its answer
  stay. A task that closes (done, dropped, reported, moved away) closes its
  open questions with it.
- `--wait <duration>` holds the command until the answer comes, prints it
  (`--json` for the answer state), and exits 6 if it does not come in time.
  The question stays open, and the answer still comes by mail. While an
  agent waits, its card says *⏳ waiting*.
- **Counts.** Each open question on a task the operator does not already
  hold counts as *waiting on you*, in `ls --summary`, the context route and
  `tclaude agent fleet`.
- With `--peer <node>` the question goes on a linked node's board, and the
  answer comes back as remote mail from that node. Your node records the
  question when it is asked and lets in that one answer, from that node, for
  you, within 30 days; it needs no `message.direct` grant, and any other
  mail from the node is refused as before. Asking on a linked node's task
  takes `taskboard.write` there, answering `taskboard.assign`; files go
  with it under the same grant (`taskboard attach --peer`): small ones
  directly, larger ones through the operator's staged upload. `--wait` and
  `show <task> --peer <node>` read the answer too.
- `show` prints each question, its options and, once answered, the answer;
  `activity` shows them in the stream. The board stays quiet: a question
  raises no popup and no Messages entry.

#### Questions from Messages and access requests

The operator's other inboxes put their questions on the board too, so
everything waiting on the operator is in one place. The originals stay
exactly where they are; the board's question is a mirror of each, answered in
either place.

- A **notify-human message that asks something** (it has `--option`s, or its
  subject, or with no subject its first line, ends in `?`) is a 📨 question.
  A plain notification is not mirrored. Answering it on the board is the
  Messages reply or decision: the agent gets the answer only as it would
  from Messages (`Re: …`, the decision mail), never a `[taskboard]` mail.
  A decision takes a pick (a note may go with it); a question in words is
  replied to, and only while the agent is online, as in Messages. Answered
  in Messages, the board's question is answered; dismissed, withdrawn or
  deleted there, it closes.
- An agent's pending **`--ask-human` prompt** is a 🔐 question, *`<agent>`
  asks for `<slug>`*, with Allow, Always allow (where the prompt offers it,
  scoped too) and Deny. Picking one decides the prompt as Messages →
  Access requests does. However it ends (decided anywhere, by away cover, or
  timed out), the board records it: the pick, or `Timed out: denied`. A
  daemon restart ends pending prompts; the next start closes their
  questions (`Expired: the daemon restarted`). A linked node's access
  request is not mirrored.
- **Where it lands.** On the board of the group the original names (a
  message's group, a prompt's target group) if the agent is a member there;
  else the group of the task it started most recently; else its first
  group. An agent in no active group, or a board that is off, is not
  mirrored. In that group it is asked on the agent's current task (the one
  it started most recently), or, when it works on none, on a new question
  task for the operator. A question task made for a prompt is archived
  when it closes, so prompts do not fill *Done this week*.
- Only the operator answers a mirrored question on the board; the agent
  that asked does not withdraw it there, and a task that closes or reopens
  leaves the original as it is. Each original is mirrored once.
- **Linked nodes.** A node acting as this node's operator (unrestricted
  trust) sees mirrored questions as the operator does. A node reading the
  board under its taskboard grants sees only that the operator has a
  question there, not what it asks or the answer. Either answers the
  original in this node's human inbox, under its rules, not on the board.

### Idle without report

The idle check is on unless a board turns it off: it is one of the
[board's options](#board-options) (`idle.enabled`), and
`taskboard.idle_check: false` turns it off for every board that does not
turn it on itself. With it on, an open task
is **stalled** when its assignee has sat idle for the board's grace
(`idle.grace`, else `taskboard.idle_grace`, else 5 minutes) without
reporting: its session is idle, with no subagents, background shells or
monitors running, and has been since the later of its last activity and the
start of its current stretch of work: the assignment, or the task being
unblocked or reopened. Time spent blocked or closed does not count. Stalled
is worked out on every read. It is not a status, and it clears as soon as the
agent starts a new turn or reports.

A sweep every 30 seconds announces each stall once per assignment (an unblock
or a reopen starts a new stretch, which may be announced again):

- the history records a `stalled` event;
- the board's `idle.tell` recipients are told,
  `[taskboard] <agent> went idle without reporting (7m): <group> · tk_… "title"`.
  By default that is the assigner: an agent by inbox mail, the operator in
  Messages. A task the agent took for itself has no one else to tell. The
  message names the command that turns the check off;
- unless `idle.nudge` is off, the assignee gets an inbox nudge with its report
  commands. It is never sent as keys into the pane.

Stalled never fails, blocks or unassigns a task. Ask the agent how it is going,
or take the task back with `unassign`.

A **queued** task, one that depends on a task not done yet, never stalls: its
agent is waiting on that work. That wait does not count as idling either: the
clock starts when the last dependency is closed. Nor does a task waiting in
line for a full lane ([limits](#work-in-progress-limits)): one for In progress
is held, and one for a column waits on the board, not on its agent.

A task in a [custom column](#custom-columns), such as *In review*, does not
stall either. Its agent is waiting on a review or CI, not idling on the work.
Move the task there when you open its PR. A board that wants a column checked
all the same lists it in `idle.check_columns`.

A stalled or a blocked task is only that: it does not need you. What needs
you is what is assigned to you, the tasks given to the operator and the open
[questions](#questions-for-the-operator), and that shows in your lane
whatever its column. `ls --summary`, the dashboard and the context route
count stalled and blocked tasks on their own, and count what is yours as
*waiting on you* (`waiting_on_you`; the older `needs_you` field is the same
count). `tclaude agent fleet` (the Steward's brief) raises every group whose
board has tasks waiting on you, for example `crew board: 2 tasks wait on
you`. The brief's JSON carries the counts per group, blocked and stalled
too, under `taskboards`. A stalled task mails its assigner instead (above).

## Board options

Each board has options of its own: how that board behaves. They are stored
with the board, so a [linked node's board](#across-linked-nodes) has its own
and they read the same there. A board stores only what it sets. Every value
it leaves unset is inherited, from the operator's global config where there
is one, else from the built-in default, and every read says where each value
comes from. In the dashboard they are the board's
[⚙ Board options](#board-options-in-the-dashboard) drawer.

```
$ tclaude agent taskboard options show crew
crew board options (version 2, changed by human:operator 2026-11-12T21:30:00Z)
idle check: on
  idle.enabled        on        default
  idle.grace          10m       set on this board (global: 5m)
  idle.nudge          on        default
  idle.tell           assigner  default
  idle.check_columns  none      default
messages:
  events.assigned       on   assignee         default
  events.comment        off  assignee,creator set on this board
  …
$ tclaude agent taskboard options set crew idle.grace=10m
$ tclaude agent taskboard options set other idle.enabled=off   # no idle check on that board
$ tclaude agent taskboard options set crew idle.tell=assigner,owners
$ tclaude agent taskboard options reset crew idle.grace    # inherit it again
$ tclaude agent taskboard options reset crew all
```

| Option | Values | Default |
|---|---|---|
| `idle.enabled` | `on` / `off`: the [idle check](#idle-without-report) | `taskboard.idle_check`, else on |
| `idle.grace` | a duration, `1m` to `168h` | `taskboard.idle_grace`, else `5m` |
| `idle.nudge` | `on` / `off`: nudge the idle assignee | on |
| `idle.tell` | any of `assigner`, `owners`, `operator`; `none` for no one | `assigner` |
| `idle.check_columns` | custom column slugs whose tasks are checked too | none |

`set` takes several `key=value` pairs and applies them as one change; a value
of `default` is the same as `reset`. `reset` takes keys, an event
(`events.comment`), a lane's limit (`limits.in_progress`,
`limits.columns.in-review`), a section (`idle`, `events`, `limits`), or `all`. Recipients are
told once each, and never about their own task: an owner that also assigned
the task hears once. `owners` are the group's owner agents; `operator` is the
operator's Messages. A column named in `idle.check_columns` drops out of it
when the column is removed.

### The board's messages

The board mails people when things happen to a task. Each of these events is
an option of the board: `events.<event>.enabled` (`on` / `off`) and
`events.<event>.to`, who gets it. Every event is on by default and goes where
it always has.

| Event | When | Default `to` |
|---|---|---|
| `assigned` | A task is assigned or reassigned to an agent (also when it is added with an assignee, or taken) | `assignee` |
| `started` | A held task is started for its assignee | `assignee` |
| `unassigned` | A task is taken back from its assignee, released, or reassigned away | `assignee` (the one that held it) |
| `reopened` | A closed task is reopened | `assignee` |
| `moved_away` | A task is moved to another group or node | `assignee` |
| `comment` | Someone comments on a task | `assignee`, `creator` |
| `report_done` | The assignee reports done | `assigner` |
| `report_failed` | The assignee reports failed | `assigner` |
| `unblocked` | A task another task waits on is closed (`unblocked` / `still blocked`) | `assignee` (of the waiting task) |
| `epic_close` | Every task of an epic is closed: close the epic? | `creator` |
| `limit_reached` | A task meets a full lane: [queued, refused or let in over the limit](#work-in-progress-limits) | `assignee`, `assigner` |

`to` is any of `assignee`, `assigner`, `creator`, `owners` and `operator`, or
`none`. In an event message, `assignee`, `assigner` and `creator` name agents;
the operator hears only as `operator` (in Messages). On a task the operator
holds, the `assignee` copy of a comment goes to the agent that asked the
operator, as it always has.

A recipient the message was written for gets it as before: the assignee its
brief, the assigner the report. A recipient the board adds gets a short note
of what happened (`[taskboard] assigned to builder by lead: crew · tk_… "title"`),
or for a comment, a report and the close-epic prompt, the message itself. Nobody hears
twice. Whoever caused the event gets no note of it.

```
$ tclaude agent taskboard options set crew events.comment.enabled=off
$ tclaude agent taskboard options set crew events.report_failed.to=assigner,owners,operator
$ tclaude agent taskboard options reset crew events.comment
```

The answer to an operator question, a task request (`ask`) and the idle
check's messages are not events: an answer always reaches the agent that
asked, a request is sent on purpose, and the idle check has its own options.

The global settings in `~/.tclaude/data/config.json` are what a board
inherits:

```json
{ "taskboard": { "idle_check": false, "idle_grace": "5m" } }
```

`idle_check` is the check's default for every board that does not say
otherwise (absent means on): `false` turns it off fleet-wide, and a board can
still turn it on for itself. `idle_grace` is a duration (`"90s"`, `"10m"`); `"0"` turns
the check off on **every** board, whatever a board sets, and the board's
options then read `idle check: off on every board (taskboard.idle_grace is 0)`.

### Who may read and change them

A board's options are its setup, not its tasks, so they have slugs of their
own. `taskboard.read` and `taskboard.write` cover the tasks and do not reach
the options.

| Slug | What it allows | Who has it without a grant |
|---|---|---|
| `taskboard.options.own.read` | Reading the options of the board of a group you are an active member of. It never reaches another group, whatever its scope. | Members of the group, on their own group's board only |
| `taskboard.options.own.write` | Changing them, on the same terms. It never reaches another group, so it can be a default permission. Implies `own.read`. | No one: the operator grants it |
| `taskboard.options.read` | Reading any board's options. Scope it with `group=<name>`; unscoped, it reaches every group. | Owners of the group |
| `taskboard.options.write` | Changing any board's options, a column's limit included. Scope it with `group=<name>`. Implies `taskboard.options.read`; `taskboard.manage` implies it. | Owners of the group |

```bash
tclaude agent permissions grant <agent> taskboard.options.write --scope group=crew
tclaude agent permissions grant <agent> taskboard.options.own.write   # its own group's board, wherever it is a member
tclaude agent permissions slugs | grep taskboard.options
```

- The operator passes every gate.
- On a locked board ([the writer lock](#the-writer-lock)), a change of the
  options passes only for the group's owners, its listed writers, and an
  explicit `taskboard.options.write` or `taskboard.options.own.write` grant
  (the agent's own, a group grant or a sudo) whose scope names the group.
  An unscoped grant or a default permission does not pass it, and nor do the
  task slugs (`taskboard.write`, `taskboard.assign`, `taskboard.own`).
- An explicit deny of `taskboard.options.read` or `taskboard.options.write`
  also denies the matching `taskboard.options.own.read` or `.own.write`: the
  deny wins, for members and owners alike, and a copy of the board's setup
  leaves it out.
- A column's limit and its idle-check choice are options. Adding, renaming,
  moving, removing, hiding and cordoning a column stay
  [`taskboard.columns`](#custom-columns).
- The board's context (the dashboard's section line) carries the options
  summary only for a caller that may read the options. A task's own queue
  position and the refusal or queue message at a full lane are facts about
  the task and read with the board under `taskboard.read`.
- An agent copies a board's setup (an export, a clone, a template made from
  the group) only when it may read the board's options; otherwise the copy
  leaves the setup out. See [Carrying a board's setup](#carrying-a-boards-setup).
- A linked node reads and changes them under the peer grants
  `taskboard.options.read` and `taskboard.options.write`, or
  `taskboard.manage` (see [Across linked nodes](#across-linked-nodes)).
  These grants read no tasks. A local agent acts on a linked node's board
  options with the same slugs scoped `peer=<node>/<group>`. Reading a linked
  node's board context, the agent gets a board's options summary only where
  it also holds `taskboard.options.read` for that node and group.

**Changed in this release.** `taskboard.write` used to change a board's
options and `taskboard.read` used to read them. They no longer do, and there
is no alias. Owners keep both through ownership, and members read their own
group's board options through membership. An agent or a linked node that
changed options under an explicit `taskboard.write` grant needs
`taskboard.options.write` (or `taskboard.options.own.write` for its own
group's board) now.

Before board options, the idle check ran on every board with
`taskboard.idle_grace` (default 5 minutes). Board options first made it
opt-in; it is on by default again, now that a task waiting on purpose never
stalls: a queued task (on a dependency or in line for a full lane), a blocked
task, and a task in a custom column the board does not check. Turn it off per
board (`idle.enabled=off`), or for every board at once with
`taskboard.idle_check: false`.

### Work-in-progress limits

A board may cap how many tasks a lane holds at once. The lanes are **In
progress** (the started tasks of agents, in no custom column) and each
[custom column](#custom-columns). A board has no limits until it sets one.

| Option | Values | Default |
|---|---|---|
| `limits.in_progress.max` | how many tasks In progress holds, `1` to `999`; `none` (or `0`) for no limit | none |
| `limits.columns.<column>.max` | the same, for one custom column (its slug) | none |
| `limits.at_limit` | what a full lane does: `queue`, `refuse` or `warn` | `queue` |
| `limits.in_progress.at_limit`, `limits.columns.<column>.at_limit` | the same, for that lane only | the board's `limits.at_limit` |
| `limits.per` | `board`: count every task in the lane; `assignee`: count each assignee's tasks in it on their own | `board` |

A change that would put a task into a full lane meets the lane's `at_limit`:

- **queue** — the change is made short of the lane, and the task waits in line
  for it. A task for In progress (an assign, take, start, reopen, an add with an
  assignee, a move to this group) is held in Next for its assignee, marked
  *queued*; a task for a column (`move --stage`, or an add or reopen into it)
  stays where it is. When a slot frees, the first in line goes in: a task for
  In progress is started for its assignee, who then gets its brief
  (`[taskboard] started for you: a slot freed in In progress`); a task for a
  column moves there. The line is in the order the tasks got in it, and a
  slot that frees while tasks wait is theirs: a newcomer joins the line
  behind them. A task still waiting on a dependency is passed over and keeps
  its place. A task a full column keeps out that would land in In progress
  instead (an add or reopen with an assignee) meets In progress's limit, and
  waits there when it is full too. A task already in a column never waits for
  it: handed to an assignee whose column is full (per assignee), or moved to
  another group into a full column of the same slug, it stays, over the limit.
- **refuse** — the change is refused, `409 limit_reached`, and nothing changes.
- **warn** — the task goes in over the limit; the answer says so.

Every time a task meets a full lane the board sends its `limit_reached`
message (to the assignee and the assigner by default). An assignee that is
told of the change anyway (an assign) hears once: its mail says it waits
(`[taskboard] queued for you, 1st in line for In progress`), not its brief.

The operator may go over any limit: `--over-limit` on `add`, `assign`, `start`,
`move --stage` and `reopen` (`"over_limit": true` on the wire). In the
dashboard a refused change asks the operator *Go over the limit?* and, on
*Go over*, sends that change again over the limit. Agents follow the rule: an agent's `over_limit` is refused (403).

A linked node's operator may go over a limit on this node's board too, from
its dashboard or with `--peer <node> --over-limit`, when the linked node may
change that board's options: it holds the peer grant
`taskboard.options.write` or `taskboard.manage` on the group, or unrestricted
trust. Whoever may change the limit may also exceed it once. Without it the
linked node's `over_limit` is refused (403), and its dashboard is not offered
*Go over the limit?*: a refusal offers it only to a caller who may go over.
A linked node's agent follows the rule whatever its node holds. The board's
`limit_reached` message names who went over (`let in over the limit by peer:
the operator of <node>`).

```
$ tclaude agent taskboard options set crew limits.in_progress.max=3 limits.columns.in-review.max=2
$ tclaude agent taskboard assign tk_… --to builder
assigned tk_…: …  [next, open]
  queued: In progress is full (3 of 3); 1st in line, it goes in when a slot frees
$ tclaude agent taskboard options set crew limits.per=assignee     # three each
```

A queued task reads `queued` with why: `queued_reason` is `In progress is full,
1st in line` (or, for a task with a dependency not done, `waiting on tk_…`,
the two joined when both hold), `queued_for` is the lane (`in_progress` or a
column slug) and `queued_position` its place in line. A task leaves the line
when it gets into its lane, and whenever what it waited as changes: it is
closed, moved to another group, unassigned or reassigned, demoted to the
ideas, moved to another column, or started or held by hand: `hold` takes a
task out of the line, so it does not start when a slot frees. The [idle check](#idle-without-report) never stalls a
queued task.

What counts: a blocked task keeps its slot (it is still the lane's work); a
held task, an idea and a task the operator holds count in no lane. Per
assignee, a task nobody holds counts against no one, so an unassigned task
moves into a column freely. Raising or lifting a limit, removing a column
(its limit and its line go with it) and uncordoning a column let the line in
at once; the daemon also looks every 30 seconds, for a slot freed by
something else (an assignee retired or gone from the group). Removing a
column puts its tasks back in next, In progress for started ones, even over
In progress's limit.

### Carrying a board's setup

A board's setup is its options, its custom columns, and the columns it hides
or cordons. It travels with its group, but its tasks never do:

- A group export carries it, and the import lands it. Before the import, the
  preview shows the columns and the option keys the file sets.
- A clone copies it.
- A group template made from a group keeps it, through the template's export
  and import too, and a group deployed from the template starts with it.

Each of these can leave the setup out, and the new board then starts as any
new board does. In the dashboard, untick *Board options* in the export,
import, clone, save-as-template or template-import dialog. In the CLI, pass
`--no-board-options` to `groups export`, `groups import`, `groups clone`,
`templates from-group` or `templates import`. Over the API, it is
`board_options=false` on an export or an import, or `"no_board_options": true`
in a clone's or a from-group's body. Re-snapshotting a template
(`from-group --update`) with `--no-board-options` keeps the setup the
template already has.

A setup from a file or a template is checked before it lands, and the import
says what it leaves out:

- a column with a bad slug or label, one the board already has, or one past
  the board's cap of columns;
- a state other than hidden or cordoned;
- a hidden built-in column other than Ideas or Done;
- options this tclaude cannot read.

Options for a column that doesn't land are dropped. The setup is the board's
options, so a copy carries it only for a caller that may read them. A peer's
clone, export or template made from a group carries it only when the peer
holds `taskboard.options.read`, `taskboard.options.write` or
`taskboard.manage` on the source group. An agent's needs
`taskboard.options.read` there, or `taskboard.options.own.read` on its own
group's board. Otherwise the copy leaves the setup out.

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
| `taskboard.write` | Everything that changes a task: unassigned tasks, imports, `ask`, reordering, and any task's edits and status. Not the board's options. Implies read. | Owners of the group. Members only through a grant. |
| `taskboard.options.*` | Reading and changing the [board's options](#who-may-read-and-change-them) | `own.read`: members; `read` and `write`: owners |
| `taskboard.own` | Tracking your own work: adding tasks that are assigned to you, changing and closing your own tasks, taking any unassigned task, releasing yours, reporting, and commenting on any task of the board | Members of the group, on their own group's board only |
| `taskboard.assign` | Assigning another agent, taking a task back, and closing a task assigned to someone else. Scope it with `group` and `target_agent`. | Owners of the group |
| `taskboard.manage` | The writer lock, the board's columns (it implies `taskboard.columns`) and options (it implies `taskboard.options.write`), and moving a task you do not hold to another group | Owners of the group |
| `taskboard.columns` | Adding, renaming, reordering, removing, hiding and cordoning the board's custom columns, and hiding *Ideas* and *Done*. Grant it to let an agent shape the board's columns without the writer lock. | Owners of the group |
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
- order the tasks assigned to it among themselves: `move` one of them
  `--before` or `--after` another one of them. That orders its own lane:
  the task always lands beside one of its own (passing any others' tasks
  between them). `--top` and `--bottom` are of the whole stage, so they take
  `taskboard.write`;
- take any open, unassigned task, an idea included, and release its own;
- comment on any task of the board.

It may not add an unassigned task or import, ask a member for tasks, reorder
the board beyond its own tasks, or change a task that is someone else's or nobody's. Those need
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
  the member's own tasks. Reordering (`move`) needs `taskboard.write`, apart
  from ordering the member's own tasks among themselves, and so does closing
  an unassigned task.
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
tclaude federation grant bob taskboard.options.write --scope group=crew
```

| Peer grant | What the linked node may do on the group's board |
|---|---|
| `taskboard.read` | List it, show a task and its activity and comments, see it in the fused view, and count it in the node summary |
| `taskboard.write` | Add and import tasks, ask this node's members for tasks, edit, move, promote, demote, block, unblock, drop and close them, and comment (editing and deleting only its own comments) |
| `taskboard.options.read` | Read the [board's options](#board-options), and carry its setup in a clone, an export or a template it makes from the group |
| `taskboard.options.write` | Change the board's options, and let its operator [go over a limit](#work-in-progress-limits). Implies `taskboard.options.read` |
| `taskboard.assign` | Assign tasks to this node's members of the group, unassign them, and close a task someone else holds |
| `taskboard.manage` | Set the writer lock, and manage the board's columns and options |
| `taskboard.columns` | Add, rename, reorder, remove, hide and cordon the board's columns |
| `taskboard.transfer` | Move or clone a task into the group from another group of this node. A move also needs `taskboard.manage` on the source group, and a clone needs only a read of it |

- Any of these grants, except the options grants, also lets the node read the board.
- A grant covers only the groups in its scope. A task in another group answers
  404, so its existence does not leak.
- Unrestricted trust acts as this node's operator does.
- Taking, releasing and reporting stay with this node's own agents.
- A linked node's changes meet the board's
  [work-in-progress limits](#work-in-progress-limits) as a local caller's
  do: a full lane queues, refuses or warns, and a slot the linked node frees
  lets the first in line in, its assignee briefed. A task's queue facts
  (`queued_reason`, `queued_for`, `queued_position`) read under
  `taskboard.read`. How full each lane is, in the board's context, is part of
  the options summary, so it needs `taskboard.options.read`. Going over a
  limit takes the linked node's operator and `taskboard.options.write` (or
  `taskboard.manage`) on the group.
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

### Archiving by hand

You can archive a task before the sweep does. An archived task is on no list
and not in the history, but it is kept, and it is purged with the other
closed tasks after `taskboard.retention_days`.

- `archive <task> [--reason "…"]` archives a done or dropped task: your own
  under `taskboard.own`, any other under `taskboard.write`. A live task is
  dropped first, with the reason, and then archived; that is gated as a drop
  (someone else's live task needs `taskboard.assign`), and the tasks that
  wait on it are told, as for a drop.
- `archive <group> --done [--older-than 7d]` archives every done task on the
  board, or only those closed longer ago than the age (`1d`, `7d`, `36h`).
  `--dropped` does the same for the dropped tasks, and `--done --dropped`
  for both; `--older-than` applies to either. Live tasks stay. It needs
  `taskboard.write`.
- `ls <group> --archived` lists the archive, most recently closed first.
- `unarchive <task>` puts a task back in the history, done or dropped as it
  was. One closed longer ago than `taskboard.archive_after_days` is archived
  again by the next sweep. `reopen` puts an archived task straight back on
  the board.

In the dashboard, a done or dropped task's ⋯ menu has *Archive*: in the
List's history and on a lanes card in *Done*. Right-clicking a List row or a
lanes card opens that menu. A live task is dropped first (⋯ *Drop…*, which
asks the reason), then archived. The
history line's *Archive closed ▾* archives the done tasks, the dropped ones,
or both: every one, or those closed over a day, a week or 30 days ago. Bringing a task back is CLI-only.

A group holds at most 500 live tasks, and a task can have at most 50
dependencies. Deleting a group deletes its board.

## In the dashboard

Each group card on the Groups tab has a **Task board** section (in wizard mode,
the **Quest board**), below its Agents and Resources sections.

- The section line shows the number of live tasks. Open, the ＋▾ add button,
  the List | Lanes switch and, last, ⚙ *Board options* sit on a row below it;
  folded, the ＋▾ and an icon-only ⚙ move up onto the section line. Beside the
  count, the section line says what the board's
  [options](#board-options-in-the-dashboard) change: *⏱ idle check off* when
  the idle check does not run there (the board or `taskboard.idle_check: false`
  turns it off, or `taskboard.idle_grace` is `0`; *idle check off globally*
  when the board sets `idle.enabled` but `taskboard.idle_grace` is `0`),
  *⏱ idle check on* when the board tunes the check's options, and *✉ 2 messages changed* when the
  board changes the messages it sends. The count's tooltip carries the breakdown, for
  example `5 · 1 waiting on you · 1 blocked · 1 in progress · 2 next · 1 idea`.
  *Waiting on you* counts the live tasks assigned to the operator (blocked
  ones too) and the open questions, and the section line shows it as a tag beside the count.
  *Blocked* and *stalled* count those tasks, and *in progress* counts the
  other assigned tasks. *Next* and *ideas* count the
  unassigned open tasks in each stage.
- **Open**, the *List* view is a table like the Agents and Resources tables
  above it. It lists the queue in your order, then the Ideas band, then the
  *history* line. Each row starts with a cluster of controls: the ⋮⋮ drag
  handle, the one action the task's state calls for (✓ *Done*, ✓ *Next* on an
  idea, ↺ *Reopen* in history), ✎ *Edit*, and ⋯ for the rest. Then come
  *Name* (the task's title), *State*,
  *Last* (its last change) and *Age*, in line with the Agents table's
  columns of the same names. After those come *Who* holds it (the tooltip
  says who assigned it, and when; a live task nobody holds has a yellow **!**,
  and one you hold a red **!**, as cards in *one row* do), *Ref* (the first ref and how many more, `+1`),
  *Deps* (`after` and the task's title, amber until that task is done; the
  tooltip has its whole title and id) and *Files* (📎 and the count; a
  click opens the task's first viewable file in the attachment viewer). A blocked
  task shows its reason under its title. The *history* line counts the tasks
  done this week and the dropped ones. Click *history* to list the recently
  closed tasks in the same table.
- **Filter ▾**, next to the view switch in the *List* view, picks which task
  states the list shows: *open*, *queued*, *ideas*, *in progress*, each of
  the board's [custom columns](#custom-columns), *waiting* (on you, the
  operator), *stalled*, *blocked*, *done* and *dropped*. By
  default it hides *done* and *dropped*, as before. Tick *done* to see this week's
  finished tasks in the list. The *history* toggle shows or hides *done* and
  *dropped* together. The button counts how many states differ from the
  default, and a line under the list says how many tasks the filter hides,
  with *show all*. *Reset* puts the default back. The dashboard remembers the
  filter per group, for you only.
- The section opens on its own when something waits on you. If you fold or open
  it yourself, the dashboard remembers that per group.

You can change the board from the section. Each action uses the same route as
a CLI verb:

| In the section | CLI |
|---|---|
| *+ New task › Write it myself* | `add <group> "title" --body … --ref … --resolve-ref [--next] --after … [--assignee …] [--blocked "…"\|--done]` |
| A pasted list in *Write it myself* | `add <group> --lines-file -` |
| *+ New task › Ask an agent* | `ask <group> --to <agent> "…"` |
| *+ New task › From a tracker…*, one ticket | `add <group> "title" --ref <url> --ref-label … --body … --resolve-ref [--next]` |
| *+ New task › From a tracker…*, several ids | `add <group> --from-awb …` / `--from-linear …` / `--from-github …` |
| *+* in a lanes cell (opens the add menu; the choice starts from the cell) | as the menu's items, with the cell's column and the lane's agent |
| Drag a row by its ⋮⋮ handle | `move <task> --before … / --after …` |
| ⋯ *Move to top* / *Move up* / *Move down* / *Move to bottom* on a row or a lanes card, or Alt+Home / Alt+↑ / Alt+↓ / Alt+End on it | `move <task> --before … / --after …`, among the rows or the cell it is in |
| Drag a queued task onto the Ideas line | `demote <task>` |
| ✓ *Next* on an idea | `promote <task>` |
| ⋯ *Block…* (asks the reason in a dialog) / ⋯ *Unblock* | `block <task> --reason …` / `unblock <task>` |
| ✓ *Done* | `done <task>` |
| ⋯ *Drop…* (asks an optional reason in a dialog), or drag the task onto the Groups view's bin | `drop <task> [--reason …]` |
| ✎ *Edit*, or a click on the task's title (in the list or on a lanes card): the edit form | `edit <task> --title … --body … --ref …`, and the verbs below for where it is, the assignee and its files |
| ⋯ *Copy link*, or the title's own link (see [A task's link](#a-tasks-link)) | `show <task>` and `add` print it |
| Files in the edit form, *Write it myself* or *Ask an agent* | `attach <task> <file>…` / `detach <task> <name>` · `add --attach` · `ask --attach` |
| *Comment* in the edit form's Activity, and a comment's *Edit* / *Delete* | `comment add <task> …` · `comment edit` · `comment rm` |
| ↺ *Reopen* or ⋯ *Reopen*, on a task under *history* | `reopen <task>` |
| ⋯ *Reopen…* (the stage picker: where it was, *Idea*, *Next* or a custom column) | `reopen <task>` / `--idea [--unassign]` / `--next` / `--stage <column>` |
| ⋯ *Move to…*, on a board with custom columns (the stage picker: *Idea*, *Next* or a column) | `move <task> --stage idea\|next\|<column>` |
| Drag a task under *history* among the live tasks, onto the Ideas line, or onto an agent's row | `reopen` beside that task, in its stage, in one write (`POST …/reopen {"before"\|"after"}`), or `reopen <task> --assignee <agent>` |
| ⋯ *Assign ›* and a member, drag a row onto an agent's row in the Agents section, or drag an agent's row onto a task (a List row or a lanes card) | `assign <task> --to <agent>` |
| ⋯ *Assign › You (operator)*, or drag a card onto the *You* lane | `assign <task> --to operator` |
| ⋯ *Unassign*, or drag an assigned row onto *drop here to unassign* | `unassign <task>` |
| Drag a task onto another group's Task board or header row (Ctrl/Cmd clones), or ⋯ *Move to group ›* / *Clone to group ›* | `move-to-group <task> <group> [--clone] [--stage next]` |

- **＋ ▾** (*New task*; in wizard mode, *New quest*) in the section head is
  the one way in, in both views. It opens a menu:
  - *Write it myself* opens a form: a title (required, one line, as for
    `add`), a brief, a ref, where it starts, dependencies picked from the
    board's live tasks, and, if you may assign, an assignee from the group's
    members. *Starts in* picks any column (see
    [Where a new task starts](#where-a-new-task-starts)) in a type-to-filter
    list: *Idea* (the default), *Next*, *In progress* (only if you may
    assign; it needs the assignee), *Blocked* (it asks for the reason),
    *Done* (the assignee, optional, is who did it), then the board's [custom
    columns](#custom-columns), as `add --stage <column>`. A bare awb or Linear ref gets its
    link, as with `--resolve-ref`. If you paste a list into the title, the
    form adds one task per line instead. A title that is only an awb, Linear
    or GitHub id becomes that ref, titled from the tracker. Files go with the
    task (see [Files on a task](#files-on-a-task)). Enter or Ctrl+Enter adds
    the task, and Esc or *Cancel* closes the form.
  - *Ask an agent* sends a request in your own words to a member, as `ask`
    does. It picks the group's owner when one is online. You can pick any
    other online member who holds `taskboard.write` there and passes the
    writer lock. With no such member online, the item is off, and says why. Files go with the request.
  - *From a tracker…* opens a form for a ticket. Paste its full URL (awb,
    Linear, GitHub…) first; the title is looked up through the tracker and
    stays yours to change. A label for the link (the text the Task column
    would show) and a brief (Markdown, in the same editor as the other forms) are
    optional, and *Starts in* picks Idea or Next in the stage picker.
    *Several ids* switches the form to the bulk import: one task per tracker
    id.
- **✎ *Edit*** on a row, or a click on a task's title (in the list or on a
  lanes card; Enter on a focused title does the same), opens the edit form.
  Renaming happens there. A press on the title never starts a drag. The
  form's header shows the task's id, and ⧉ copies it. It has the title, the
  brief, the ref (its URL, with the ref as a link beside it while unchanged;
  a new bare awb or Linear id is linked on *Save*, as `edit --resolve-ref`
  does), *Where it is* (*Idea*, *Next*,
  *In progress*, *Blocked* with its reason, *Done*, *Dropped*, or one of the
  board's custom columns, picked in the same type-to-filter list), the
  assignee (if you may assign), the dependencies (✕ takes one off, and
  *＋ dependency* adds one of the board's other live tasks) and the files.
  Picking an assignee for an unassigned idea or queued task makes it *In
  progress*; picking *Idea* or *Next* for an assigned task keeps its assignee,
  who holds it there, not started. *Save* writes only what
  changed, each through the route its CLI verb uses. If one write is refused
  (say, a close that needs `taskboard.assign`), the rest still land, and the
  form stays open and names what was not saved. A done or dropped task
  reopens when you pick another place for it.
- **The brief and comments are written in a Markdown editor**, in *Write
  it myself*, the edit form and a comment (new or edited). Its toolbar has a
  heading level, bold, italic, inline code, strikethrough, bullet, numbered
  and task lists, quote, code block, table, rule, link, 📎 and a syntax
  reference (?); a comment's toolbar has the everyday ones. *Write*,
  *Preview* and (for a brief) *Split* switch between the text, the rendered
  brief as the card shows it, and both side by side; the choice is
  remembered. Ctrl+B, Ctrl+I, Ctrl+E and Ctrl+K format (Cmd on macOS), Enter
  continues a list, Tab and Shift+Tab nest a list item, and Ctrl+Enter adds
  or saves the task, or sends the comment. A screenshot or file pasted or
  dropped on the text is added to the task's (or comment's) files and linked
  where it landed: `![name](name.png)` shows an image inline, wherever the
  brief or comment is rendered. A task list (`- [ ]`, `- [x]`) renders as
  checkboxes. The editor (CodeMirror) loads the first time one opens; until
  then, or if it cannot load, it is a plain text box with the same toolbar.
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

### Board options in the dashboard

⚙ *Board options* opens a drawer on the right, over the board, so the board
stays in view. It has three tabs:

- **Triggers**: the [idle check](#idle-without-report) (its switch, the grace
  period, the nudge, who is told, and the custom columns whose tasks are not
  checked) and the [board's messages](#the-boards-messages), one row per event
  with its switch and a box per recipient. *all off* turns every message off;
  *↺ defaults* puts them all back.
- **Limits**: the [work-in-progress limits](#work-in-progress-limits): a
  limit for In progress and for each custom column (empty for none), with how
  many tasks the lane holds now; whether a limit counts the whole board or
  each assignee; and what a full lane does (*Queue*, *Refuse*, *Warn only*).
  A lane's own choice is on the Columns tab; without one it follows the
  board's.
- **Columns**: the board's columns, and for each custom column whether its
  tasks are skipped by the idle check (`idle.check_columns`). A custom
  column's ⋯ renames, moves or removes it, and *＋ column* adds one. Those
  column changes apply at once, as on the board; the rest of the drawer saves
  with *Save*. Each lane's limit and its own at-limit choice are here too.

Every value says where it comes from: *default*, *global* (the operator's
`taskboard.*` config) or *set on this board*, with the value it would inherit
beside it. ↺ makes a value the board sets inherit again. A change not yet
saved has a dot, and the footer counts them. *Save* sets and resets the keys
you changed, in one change, as `options set` and `reset` do; options the
drawer does not show stay as they are.
*Cancel*, Esc, ✕ or a click beside the drawer ask before they drop unsaved
changes. When the options changed elsewhere while the drawer was open (the
CLI, another viewer), it says so, and *Save* puts your changes over the newer
options, for you to check and save again. When `taskboard.idle_grace` is `0`,
the idle check's switch is greyed and the drawer says the check is off on
every board.

Without `taskboard.options.write` on the board, as on a linked node's board
that granted you only `taskboard.options.read`, the drawer is read-only and
says so, for example *Read-only: no taskboard.options.write on node-b*. Its
column actions follow `taskboard.columns` (or `taskboard.manage`) instead, so
either part can be read-only while the other is not. Without a read of the
options there is no ⚙ and no summary on the section line. A linked node's
board reads and saves its options through that node, under its grants.

On the board, a limited lane's head says how full it is: `2 / 3`, amber
with *full* at the limit, red with *over* past it (per assignee, `≤ 3 each`),
and `⏳ 2` when two tasks wait in line for it. The count is the board's own,
so a blocked task that keeps its slot counts, though it shows under Blocked.
A task waiting in line has a `⏳ 1st` chip on its card and in the List, and
its tooltip says why. The section line lists the limited lanes and how many
wait. *Set limit…* on In progress's or a custom column's ⋯ sets that lane's
limit and what it does when full, at once.

The link `/groups?board-options=<group>` opens the drawer on a board, and
`#idle`, `#events`, `#columns` or `#limits` after it scrolls to that part. A linked node's
board is `?board-options=<node-id>:<group>`. Back closes the drawer.

### A task's link

Every task has its own dashboard link:

```
http://127.0.0.1:<port>/groups?task=<task-id>:<group>
```

A linked node's task shown here adds that node's id after the task's id
(`?task=<task-id>@<node-id>:<group>`). The group name comes last and as it
is, so a name holding `:` or `@` still reads back.

Opening the link unfolds the group and its Task board, scrolls to the task
and opens its edit form. That works for a task in history or archived as
well. You can share it with an agent or paste it back to ask about the task.
A link to a task that has moved to another group since, or is gone, opens
nothing and leaves the address bar.

- A task's title is that link, in the List and on a lanes card. A plain
  click still opens the edit form in place. A middle click or a Ctrl/⌘-click
  opens the task in a new browser tab, and the browser's *Copy link address*
  copies it. The hover card says so; the card itself is not clickable.
- ⋯ *Copy link* copies the whole link, from a row's ⋯ (or a right-click on
  the row) and from a lanes card's ⋯.
- Opening the edit form adds a browser history step, and closing it steps
  back. **Back** closes an open form, **Forward** opens it again, and a
  reload keeps it open. If the form has unsaved input, Back first asks
  *Discard input?*, as *Cancel* does. Choose *Cancel* there to keep editing
  with the form's entry back in the history.
- `tclaude agent taskboard show <task>` and `add` print the link as
  `link:`, and every `[taskboard]` mail ends with it (*Open it in the
  dashboard: …*). The CLI prints none for a linked node's task (`--peer`):
  that node's dashboard is not this one.

The port is the dashboard listener's. By default it is random and changes
when the daemon restarts, so a printed or mailed link works only until then.
The task id beside it keeps working. To keep links working, fix the port with
**Dashboard port** in [Config › Daemon & network](dashboard-config.md#daemon-network).

### Epics on the board

The board shows each open epic once, in the **epic strip** above the stage
columns: its name in its colour, a progress bar (done, in progress, blocked)
and its counts. An epic is never a card in a stage column, because it is not
work of its own. An epic that is still an idea is a card in Ideas, marked
◆ epic, until it is promoted.

- Each task in an epic carries the epic's **chip**, in the epic's colour, on
  its Lanes card and its List row. A click on the chip shows only that epic's
  tasks; a second click shows every task again.
- **Epic ▾** in the toolbar picks the epic to show, or *No epic* for the tasks
  in none. The other tasks dim, or with *hide*, leave the board. The choice is
  this viewer's, kept per group.
- A click on an epic's name in the strip opens it in the edit form. In place
  of the assignee and stage, the form shows its **progress** and its
  **tasks**, each with where it is and who holds it. A click on a task opens
  it. *+ add a task* adds a new task to the epic, *add an existing task* puts
  in one of the board's tasks that is in no epic, and × takes a task out.
  These changes are made at once, not with Save. An epic has no stage, only a
  **Status**: open, done or dropped. Done on an epic with open tasks closes
  the epic and leaves the tasks open (`done --force`). While an epic is an
  idea, *Promote to open epic* moves it to Next, where its strip card shows.
- A task's edit form, and the New task form, have an **Epic** picker.
  *＋ ▾ → New epic* adds an epic, in Ideas or Next.
- When every task in an epic is closed, its strip card asks **close epic?**:
  *Close epic* closes it, and *Not yet* hides the question until another of
  its tasks changes. Nothing closes an epic by itself.
- *lanes epic* gives each open epic a lane, with *No epic* last. The lane
  head is the epic: its name, progress and *open ›*. A card there carries no
  epic chip, since the lane names the epic. Dragging a card into another
  epic's lane moves the task to that epic. Dragging it into *No epic* takes
  it out of its epic. Either way a toast offers *Undo*, which puts the task
  back in the epic it was in. A drag to another column in the same move also
  moves the task there, as in *one row*, and Undo puts back only the epic.
  The epic strip is hidden in this mode, since the lane heads are the epics;
  an epic's **close epic?** shows in its lane head instead.
  An epic still an idea is a card in *No epic*'s Ideas, and gets a lane of
  its own only once it holds a task. A cell's *+* puts the tasks the form
  adds (one, or a pasted list) in that lane's epic; an import from a tracker
  and *Ask an agent* do not.
- In the List, *Filter ▾ → group by epic* puts the queue under its epics,
  each under a head row that folds it, with *No epic* last. A row dragged
  among another epic's rows moves to that epic too, with *Undo*. Only the
  queue is grouped; the ideas and the history stay as they are.

### Swim lanes

The *☰ List | ⇶ Lanes* switch in the section head picks the view (*Lanes*
unless you pick *List*). In *Lanes*, *lanes agent | role | subgroup | triage |
one row | epic* picks the lanes: one per agent, grouped by agent, role or
subgroup, the two modes without a lane per agent, described below, or a lane
per epic ([Epics on the board](#epics-on-the-board)) (*triage* unless you
pick another). The dashboard remembers both per group, for you only.

The lanes view lays the same board out as a grid. It has five columns: *Ideas*,
*Next*, *In progress*, *Blocked* and *Done this week*. *Blocked* also holds
stalled tasks. A blocked task is only blocked; what needs you is what stands
in your lane (*You*), in any column, with the open questions on other tasks
as cards in its *In progress*. A board's [custom
columns](#custom-columns) come after *In progress*, with blue heads; their
cards say *⏸ no idle alarm* while an agent holds them.

A custom column's head has a ⋯ with *Rename…*, *Move left*, *Move right* and
*Remove*, and the last head has *＋ column*, while the board has room for
another. They are `taskboard columns rename`, `move`, `add` and `rm`, and need
`taskboard.columns`. Every hideable column's ⋯ (custom ones, *Ideas*, *Done*)
also has *Hide for me*, and for a viewer who manages the columns *Hide on the
board* and, on a custom column, *Cordon* or *Lift the cordon*, then *Column
in Board options ↗*, which opens the board's options at that column. The
toolbar's
*Columns ▾* lists the same choices for every column, and brings a hidden one
back. See [Hiding and cordoning columns](#hiding-and-cordoning-columns). In the *List*, the *State* says the column, and a row's ⋯
*Move to…* opens the stage picker, where you can move the task into a column,
or back out to *Next* or *Idea* (`taskboard move --stage`). Every place a
stage or column is picked by hand (*Starts in*, *Where it is*, *Move to…* and
*Reopen…*) uses the same picker.

A cell shows its first three cards. When it holds more, *+ n more* opens it in
place, so one busy cell does not stretch its whole row. An open cell grows up
to about a dozen cards, then scrolls inside. *▴ Show less*, under its cards,
closes it again. An open cell stays open until you close it: opening a task's
editor, switching views or reloading the dashboard keeps it, per group and per
lanes view, for this viewer. The column heads still count every task. When the card is too
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
| Onto a custom column, in the same lane | `move <task> --stage <column>` (a blocked task is unblocked first) |
| Onto a custom column of another lane | `assign <task> --to <agent>` (or `unassign`), then `move <task> --stage <column>` |
| Out of a custom column onto *Next* or *In progress*, same lane | `move <task> --stage next` (onto *In progress*, a held task is started too; onto *Next*, a started one is held back) |
| Out of a custom column onto another lane's *Next* or *In progress*, or a blocked card in a column onto either | that drop's own write (`assign`, `unassign`, unblock), then `move <task> --stage next` |
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

#### Ordering the cards

A cell lists its cards in the board's order, top first: what to do next is
on top. You can change it in every cell where the order means something:

- **Drag** a card up or down in its own cell. A blue line shows the slot
  it will land in, and the cell says *Move before “…”*. The card moves at
  once; if the board refuses (someone moved the card next to it to the
  other stage meanwhile), it goes back and a toast says why.
- **Drop at a slot in another cell.** Where a drag into another cell lands
  the task in Ideas, Next, In progress or a custom column, the line shows
  there too, and the task is put at that slot after the drop's own write
  (a promote, an assign, a move into the column), as `move --before` /
  `--after`. A drop onto *Blocked* or *Done* keeps the task's place.
- **⋯ Move to top / Move up / Move down / Move to bottom** on a card, or
  **Alt+Home / Alt+↑ / Alt+↓ / Alt+End** with the card's title focused. The
  top and bottom are of the cell you see the card in (this lane's Next, an
  epic's lane), not of the whole stage. Items that would not move it are
  greyed. The List's rows have the same items and keys, among the rows
  they are listed with (the queue, an epic's rows, or the ideas).

A task's order is its place among the live tasks of its stage, Ideas or
Next; In progress and the custom columns are Next's tasks, in Next's order.
In *Blocked*, which holds blocked tasks from both stages, a card moves among
the cards of its own stage only, so reordering never promotes or demotes
it; an unblocked task goes back to its place. *Done this week* is ordered
by when each task closed, newest first (its head says *by close time*): its
cards have no move items, and a drag inside it says so and does nothing.

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
- *From a tracker…* imports into the queue from a Next cell or an
  agent's lane. An import lands in Ideas or Next only, so you assign, block or
  close the tasks on the board afterwards.

#### Triage and one row

Two *lanes* modes do without a lane per agent, for a board where you mostly
want to see what nobody holds and what waits on you:

- **triage** has three lanes: *You* (the operator's tasks, tinted as the
  operator's lane is), *Unassigned*, and *Agents*, every agent's tasks
  together.
- **one row** puts every task in a single row.

In *one row* each card says who holds it in its top-right corner: a yellow
**!** when nobody does, a red **!** when you (the operator) do, otherwise the
agent's avatar. The two **!** cards also get an edge in their colour. A done
card carries no **!**. In *triage* the *You* and *Unassigned* lanes already
say who holds their cards, so those carry no marker and no coloured edge;
only the *Agents* lane's cards show their agent's avatar. A cell shows its first five cards, then *+ n more*.

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
| `ls <group>` | `GET /v1/groups/{g}/taskboard[?stage=&all=1]` (the API lists the backlog with the rest; `ls` leaves it out unless `--backlog`, `--all` or `--stage backlog`) |
| `ls` (own group) | `GET /v1/taskboard` |
| `show` | `GET /v1/taskboard/tasks/{id}` |
| `activity` / `history` | `GET /v1/taskboard/tasks/{id}/activity` → `{id, group, title, entries}` (`…/events` answers the same) |
| `comment add` | `POST /v1/taskboard/tasks/{id}/comments {"body","key"?}` (multipart: a `comment` JSON part and `file` parts) → 201 `{id, group, comment}`, 200 on a repeated key |
| `comment edit` / `rm` | `PATCH` `{"body"}` / `DELETE /v1/taskboard/tasks/{id}/comments/{cid}` → `{id, group, comment}`; 403 `not_author` for anyone else |
| `comment ls` | `GET /v1/taskboard/tasks/{id}/comments` → `{id, group, comments}`; a comment's file is read through `…/attachments/{aid}` |
| `add` | `POST /v1/groups/{g}/taskboard/tasks` (`--resolve-ref` sends `"resolve_ref":true`; `--assignee` sends `"to"`, `--blocked` sends `"status":"blocked","block_reason"`, `--done` sends `"status":"done"`, `--blocks` sends `"blocks"`; `--stage <column>` sends `"stage":"<column slug or label>"`; `--add-ref` sends `"add_refs":[…]`) |
| `ask` | `POST /v1/groups/{g}/taskboard/ask {"to":"<agent>","prompt":…}` |
| `add --from-*` / `--lines-file` | `POST /v1/groups/{g}/taskboard/tasks {"refs":[…],"lines":[…],"stage"?}` |
| `edit` | `PATCH /v1/taskboard/tasks/{id}` (`--resolve-ref` sends `"resolve_ref":true`; `--add-ref` sends `"add_refs":[…]`, `--rm-ref` sends `"rm_refs":[…]`, `--first-ref` sends `"first_ref"`, `--clear-ref` sends `"clear_ref":true,"clear_refs":true`) |
| `move` | `POST /v1/taskboard/tasks/{id}/move {"top"\|"bottom"\|"before"\|"after", "stage"?: "backlog"\|"idea"\|"next"}`: with `stage`, a place that would land the task in the other stage answers 409 `stale` |
| `epics` | `GET /v1/groups/{g}/taskboard?kind=epic[&all=1]`. Each epic carries `kind:"epic"`, `progress:{total,done,dropped,blocked,in_progress,in_column,columns,next,ideas}` and, once all of its tasks are closed, `close_suggested_at` |
| `ls --epic tk_…` | `GET /v1/groups/{g}/taskboard?parent=tk_…` (a task in an epic carries `parent_id`) |
| `add --epic` / `--parent` | `POST /v1/groups/{g}/taskboard/tasks {…,"kind":"epic"}` / `{…,"parent":"tk_…"}` |
| `edit --parent` / `--rm-parent` | `PATCH /v1/taskboard/tasks/{id} {"parent":"tk_…"}` / `{"clear_parent":true}` |
| `done --force` | `POST /v1/taskboard/tasks/{id}/status {"status":"done","force":true}`. Without `force`, an epic with open tasks answers 409 `epic_open_children` |
| `move-to-group` | `POST /v1/taskboard/tasks/{id}/move-to-group {"group":…,"clone"?,"stage"?}` → `{task, group, from, clone, dropped_deps, dropped_dependents, unassigned}` |
| `move-to-group … <group>@<peer>` | on the peer `POST /v1/groups/{g}/taskboard/transfer-in {"title","body","ref","ref_label","add_refs","stage","clone","origin":{"group","task"}}` → the new task; then here `POST /v1/taskboard/tasks/{id}/moved-away {"to":{"node","group","task"}}` |
| `promote` / `demote` / `backlog` | `POST /v1/taskboard/tasks/{id}/stage {"stage":"next"\|"idea"\|"backlog"}` (`promote` and `demote` read the task first and send the stage one step up or down) |
| `move --stage` | `POST /v1/taskboard/tasks/{id}/stage {"stage":"backlog"\|"idea"\|"next"\|"<column slug or label>"}` |
| `columns ls` | `GET /v1/groups/{g}/taskboard/columns` → `{group, columns:[{slug,label}], cap}` |
| `columns add` | `POST /v1/groups/{g}/taskboard/columns {"label","slug"?,"before"?}` |
| `columns rename` / `move` | `PATCH /v1/groups/{g}/taskboard/columns/{slug} {"label"?,"before"?,"to_end"?}` |
| `columns rm` | `DELETE /v1/groups/{g}/taskboard/columns/{slug}` → `{group, columns, cap, moved:[tk_…]}` |
| `options show` | `GET /v1/groups/{g}/taskboard/options` → `{group, options, resolved, inherited, globals, version, updated_by, updated_at, keys}`: `options` is what the board sets, `resolved` what is in force, each value as `{value, source}` with source `default`, `global` or `board`; `resolved.events` lists every event as `{name, label, enabled, to}`; `resolved.limits` is `{at_limit, per, lanes:[{lane, max, at_limit}]}`, In progress (`in_progress`) first, then each column. `inherited` has the shape of `resolved` and is what each value would be if the board set nothing: what a reset falls back to |
| `options set` / `reset` (and the dashboard's *Save*) | `PATCH /v1/groups/{g}/taskboard/options {"set":{"idle.grace":"10m","events.comment.enabled":"off",…},"reset":["idle.tell"\|"idle"\|"all"],"version"?}`: resets first, then sets, in one change |
| (whole document) | `PUT /v1/groups/{g}/taskboard/options {"options":{"idle":{…}},"version":n}`: replaces the whole document (an unknown key is a 400); a `version` other than the stored one answers 409 `stale` with the `current` options. A `PATCH` on a document written by a newer tclaude that this one cannot read answers 409 `unreadable` |
| `block` / `unblock` / `drop` / `done` | `POST /v1/taskboard/tasks/{id}/status` (`block --on` sends `{"status":"blocked","on":"tk_…"}`) |
| `archive` | `POST /v1/taskboard/tasks/{id}/archive {"reason"?}` |
| `unarchive` | `POST /v1/taskboard/tasks/{id}/unarchive` |
| `archive <group> --done` | `POST /v1/groups/{name}/taskboard/archive-done {"older_than"?: "7d"}` (answers `{"group","archived":[ids],"count"}`) |
| `archive <group> --dropped [--done]` | `POST /v1/groups/{name}/taskboard/archive-closed {"statuses": ["done"?, "dropped"?], "older_than"?: "7d"}` (answers `{"group","statuses","archived":[ids],"count"}`; a node without it answers 404) |
| `ls --archived` | `GET /v1/groups/{name}/taskboard?archived=1` |
| `reopen` | `POST /v1/taskboard/tasks/{id}/reopen {"stage"?: "next"\|"idea"\|"backlog"\|"<column slug or label>", "to"?: "<agent>"\|"operator"\|"", "before"?\|"after"?: "tk_…"}` (no `to` keeps the assignee; `""` takes it off; `before`/`after` places it beside a live task, in its stage, and needs `taskboard.write`) |
| `assign` / `take` | `POST /v1/taskboard/tasks/{id}/assign {"to":"<agent>"\|""}` |
| `--over-limit` (operator) | `"over_limit": true` on `add`, `assign`, `start`, `stage`, `reopen`, `status` (a reopen) and `move-to-group`; from an agent, 403. A write that met a full lane answers the task with `limit: {lane, label, max, count, per, action, position?}`, `action` being `queued`, `warned` or `over`; a refused one answers 409 `limit_reached` |
| `unassign` / `release` | `POST /v1/taskboard/tasks/{id}/unassign` (`release` sends `{"self":true}`) |
| `assign --idea` / `--next` / `--hold` | `POST /v1/taskboard/tasks/{id}/assign {"to":…,"start":false,"stage"?:"idea"\|"next"}` |
| `add --assignee X --idea` / `--next` | `POST /v1/groups/{name}/taskboard/tasks {…,"to":"X","start":false,"stage":"idea"\|"next"}` |
| `ask-operator <task>` | `POST /v1/taskboard/tasks/{id}/questions {"title","body"?,"options"?:[{"label","description"?,"preview"?,"key"?}],"multi"?,"recommend"?,"block"?}` → `{task, question}` |
| `ask-operator <group>` | `POST /v1/groups/{name}/taskboard/tasks {…,"to":"operator","question":{"options":[…],"multi"?,"recommend"?}}` |
| `answer --question n` | `POST /v1/taskboard/tasks/{id}/questions/{n}/answer {"picked":["key"\|"label"…],"text"?}` |
| `answer` | `POST /v1/taskboard/tasks/{id}/answer {…}` (the task's only open question) |
| `ask-operator --wait` | `GET /v1/taskboard/tasks/{id}/questions/{n}?wait=30s` (or `…/answer` for a question task; long poll, at most 30s per request: `{"task","n","status","state","answered","question"}`) |
| (dashboard: *From a tracker…*; no CLI verb) | `POST /v1/groups/{name}/taskboard/ref-lookup {"ref"}`: what the ref would be (`{"kind","url","label","title","status","resolved","note"}`; it also keeps the title and status for the board to show on that ref), titled through the tracker proxy as an import is; adds nothing, needs board read |
| `start` / `hold` | `POST /v1/taskboard/tasks/{id}/start {"start":true}` / `{"start":false,"stage"?:"idea"\|"next"}` |
| `report` | `POST /v1/taskboard/tasks/{id}/report {"outcome":"done"\|"failed","note":…}` → `{task, repeated, blocked}` |
| `ls --mine` | `GET /v1/taskboard/mine` |
| `ls --summary` (and the dashboard) | `GET /v1/taskboard/context` |
| `groups set-taskboard-writers` | `GET` / `POST /v1/groups/{g}/taskboard/writers {"agents":[…]}\|{"members":true}` |
| `attach` | `POST /v1/taskboard/tasks/{id}/attachments` (multipart `file` parts) → the task |
| `attachments` | `GET /v1/taskboard/tasks/{id}/attachments` → `{id, group, attachments}` |
| `attachments --save` | `GET /v1/taskboard/tasks/{id}/attachments/{aid}` (the bytes; `?format=json` → base64 `data`) |
| `detach` | `DELETE /v1/taskboard/tasks/{id}/attachments/{aid}` → the task |
| `drawing-board export --attach-to` | `POST /v1/taskboard/tasks/{id}/drawings {"board","elements"?,"region"?,"scale"?,"name"?,"comment"?,"key"?,"replace"?}` → `{id, group, attachment, replaced?, comment?}` (`key` goes with `comment`; a retry answers 200 with the first comment) |

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

The group listing also returns `locked`, `columns` (the custom columns,
`[{slug,label}]`), and `writers` when the board is locked. A task in a custom
column carries `column` (its slug). `GET /v1/taskboard/context` gives each
group `columns`, `[{slug,label,count}]`, beside its `counts`. Each task carries `assignee` (`agent_id`, `name`; for the operator
`{"agent_id":"operator","name":"operator","operator":true}`), `blocked_on`
when it waits on an operator task, `assigned_by`,
`assigned_at`, `outcome`, `outcome_note` and `failures` when they are set.
A queued task carries `queued: true` and `queued_reason`; one in line for a
full lane also `queued_for`, `queued_position` and `queued_at`.
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
bodies) and the 8 most recently closed tasks, and `options`, a summary of the
board's options: `{version, idle_active, idle_globally_off, idle_source, set}`,
where `set` lists the option keys the board sets. When the board limits a
lane, `options.limits` is `{per, lanes:[{lane, max, at_limit, count, counts?,
queued}]}`, one entry per limited lane: `count` is the tasks it holds (per
assignee, the fullest assignee's; `counts` has each one's), `queued` how many
wait in line. A group the caller cannot read is
left out entirely, not listed as empty.
