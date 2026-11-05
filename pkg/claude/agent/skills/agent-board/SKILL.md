---
name: agent-board
description: >-
  Draw on and read a Collab board (a shared Excalidraw-style drawing board the
  operator also edits in the dashboard) via `tclaude agent board`: create, ls,
  show (with --since / --view), snapshot, add, text, connect, move, layout,
  patch, rm, open/close your board session, from-process (sketch a process
  template as a flowchart), from-fleet and fleet-changes (a staffing map of
  the fleet). Use when the operator asks to "sketch it", "draw it on a board",
  "put it on the whiteboard", says "this"/"here" about a board, when a
  `[board <id>]` mail arrives in your inbox, or when agreeing a design, plan or
  process with the operator visually. Needs `boards.read` / `boards.write`
  (not default-granted). Not for showing a web page you serve (that is
  agent-page), not for editing a process template (process-templates), and not
  for federation content boards (`docs/federation-boards.md`).
---

# Collab boards

A board is a shared drawing: the operator draws in the dashboard's board
editor, you read and change the same board from the CLI. Elements are a
subset of Excalidraw JSON (rectangle, ellipse, diamond, arrow, line, freedraw,
text), merged per element: higher `version` wins, a deletion is a tombstone.
Boards are global, not per group (a board can optionally belong to a group).

## Permissions

| Slug | Covers |
|---|---|
| `boards.read` | `ls`, `show`, `snapshot`, `fleet-changes` (also needs `fleet.read`) |
| `boards.write` | `create`, every change (`add`, `text`, `connect`, `move`, `layout`, `patch`, `rm`, `from-process`, `from-fleet`), `open`, `close` |

Neither is default-granted (a Steward gets both). `from-process` also needs
`process.templates.read`; `from-fleet` also needs `fleet.read`. A 403 names
the missing slug: ask the operator for
`tclaude agent permissions grant <you> boards.write` rather than retrying.

## Read

```bash
tclaude agent board ls [--json]
tclaude agent board show plan            # version + one line per live element
tclaude agent board show plan --since 12 # only changes after version 12, deletions included
tclaude agent board show plan --view     # only what the operator has on screen ("this", "here")
tclaude agent board show plan --json     # raw Excalidraw elements
tclaude agent board snapshot plan -o plan.png            # last PNG the dashboard rendered
tclaude agent board snapshot plan --region 3 -o chg.png  # the change a [board plan] mail numbered
```

Remember the version `show` prints and pass it as `--since` next time.
`show` lists line ends as `api:right -> db:left` and attached text as
`on <element>`. `snapshot` needs an open dashboard and may lag your own edits
(it says by how much); working alone, read with `show`. Look at a PNG with an
image-capable tool.

## Draw

```bash
tclaude agent board create plan --title "Release plan" [--group <group>]
tclaude agent board add plan rectangle --x 100 --y 80 --text API --id api
tclaude agent board add plan rectangle --right-of api --link --text DB --id db  # beside api, arrow from it
tclaude agent board add plan arrow --from api --from-side right --to db --to-side left --routing elbow --text calls
tclaude agent board add plan text --x 100 --y 20 --text Roadmap --font-size 48 --font sans --style bold
tclaude agent board text plan api --text "API v2"        # or only --font-size/--font/--style
tclaude agent board connect plan <line> --routing curved [--to cache --to-side top | --free-to]
tclaude agent board move plan api --x 300 --y 80          # text and attached lines follow
tclaude agent board layout plan [--direction down] [--gap 60]  # arrange shapes along their arrows
tclaude agent board rm plan <elem>...                     # attached text goes too
tclaude agent board patch plan --file elements.json       # raw elements; - reads stdin
```

- `add` shapes: `--x --y --w --h` (default 160x80), `--color`/`--bg`
  (`#rrggbb`, `#rgb`, `transparent`). Relative placement: `--right-of`,
  `--left-of`, `--below`, `--above` (with `--gap`, `--link`). Give stable
  `--id`s so you can refer to elements later.
- Text on a shape or line is a second element `<id>-label`.
- `patch`: to change an element send it with `version + 1`; to delete, also
  `isDeleted: true`. `move`, `text` and `rm` bump versions for you.

## Sessions and activity

Your first change starts a **board session**: the board stays in the
operator's Background terminals menu with no tab open, across reincarnation
and daemon restarts. `board open <id>` starts one again when you come back
later; `board close <id>` ends yours (the board stays). Changing a board, or
`open`, makes you a participant: the operator's edits then reach you as
`[board <id>]` mail (at most one per 3 s per board), usually naming a
`--region` snapshot. React with `show --since <last version>`. Members of a
group board's group are participants automatically.

## Generators

```bash
tclaude agent board from-process flow review   # template "review" as a flowchart (p-<step> ids); redraw replaces it
tclaude agent board from-fleet fleet [--group <g>] [--all]  # column per group, card per agent (fg-/fa- ids)
tclaude agent board fleet-changes fleet [--json]  # operator's card moves/deletions/notes as staffing proposals
```

`fleet-changes` changes nothing: confirm with the operator, act, then redraw
with `from-fleet`. Changing the process itself is a template edit (see the
process-templates skill); the board is where you agree on it.

## Example: sketch a design and react to feedback

```bash
tclaude agent board create cache-design --title "Cache design"
tclaude agent board add cache-design rectangle --x 80 --y 80 --text Client --id client
tclaude agent board add cache-design rectangle --right-of client --link --text Cache --id cache
tclaude agent board add cache-design rectangle --right-of cache --link --text DB --id db
tclaude agent board show cache-design        # note "version N"
# ... operator draws; a [board cache-design] mail arrives ...
tclaude agent board show cache-design --since N
tclaude agent board snapshot cache-design --region 1 -o change.png
```

User docs: `docs/boards.md`. For a live web page instead of a drawing, use
the agent-page skill.
