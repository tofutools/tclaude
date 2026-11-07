# Group task boards

A group's **task board** is a short ordered list of tasks: what the group is on
right now and the next few things it will do. Each [agent group](agents-and-groups.md)
has one board. The operator and the group's agents read and change it with
`tclaude agent taskboard`.

> The task board is in development and is off by default. Turn it on with
> `"features": {"taskboard": true}` in `~/.tclaude/config.json`. While it is
> off, its routes answer 404, but stored tasks are kept. So far there is the
> store, the CLI, the API, importing refs from trackers and a read-only
> dashboard section. Assigning tasks to agents and editing in the dashboard
> come in later slices.

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
tclaude agent taskboard ls [group] [--stage idea|next] [--all] [--json]
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

## Permissions

The operator passes every gate. For agents:

| Slug | What it allows | Who has it without a grant |
|---|---|---|
| `taskboard.read` | `ls`, `show`, `history` | Members and owners of the group |
| `taskboard.write` | Everything that changes a task. Implies read. | Members and owners of the group |
| `taskboard.manage` | The writer lock and retention (later slice) | Owners of the group |
| `self.taskboard` | Taking, releasing and reporting on your own tasks (later slice) | Default-granted |

None of the `taskboard.*` slugs are default-granted. Access to another group's
board is an ordinary scoped grant:

```bash
tclaude agent permissions grant steward taskboard.read taskboard.write --scope group=tclaude,website
tclaude agent permissions grant steward taskboard.read   # unscoped: every group, including future ones
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

## In the dashboard

Each group card on the Groups tab has a **Task board** section (in wizard mode,
the **Quest board**), below its Agents and Resources sections. For now the
section is read-only.

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

The dashboard shows a board only to someone who can read it. The operator sees
every group. The section is missing while `features.taskboard` is off, and a
peer node's groups don't show their boards yet.

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
| `ls --summary` (and the dashboard) | `GET /v1/taskboard/context` |

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
