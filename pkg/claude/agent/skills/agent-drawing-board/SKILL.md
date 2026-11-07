---
name: agent-drawing-board
description: >-
  Draw on and read a Collab drawing board (Excalidraw-style, shared with the
  operator, who edits it in the dashboard) via `tclaude agent drawing-board`: create, ls,
  show (with --since / --view), snapshot, add, text, connect, move, layout,
  patch, rm, open/close your drawing board session, from-process (sketch a process
  template as a flowchart), from-fleet and fleet-changes (a staffing map of
  the fleet). Use when the operator asks to "sketch it", "draw it on a drawing board",
  "put it on the whiteboard", says "this"/"here" about one, when a
  `[drawing-board <id>]` mail arrives in your inbox, or when agreeing a design, plan or
  process with the operator visually. A drawing board of your group is yours to read
  and draw on as a member; any other drawing board needs `drawing-boards.read` /
  `drawing-boards.write` (not default-granted). Not for showing a web page you serve (that is
  agent-page), not for editing a process template (process-templates), and not
  for federation content boards (`docs/federation-boards.md`).
---

# Collab drawing boards

A drawing board is a shared drawing: the operator draws in the dashboard's drawing board
editor, you read and change the same drawing board from the CLI. Elements are a
subset of Excalidraw JSON (rectangle, ellipse, diamond, arrow, line, freedraw,
text), merged per element: higher `version` wins, a deletion is a tombstone.
Drawing boards are global, not per group (a drawing board can optionally belong to a group).

## Permissions

| Slug | Covers |
|---|---|
| `drawing-boards.read` | `ls`, `show`, `snapshot`, `fleet-changes` (also needs `fleet.read`) |
| `drawing-boards.write` | `create`, every change (`add`, `text`, `connect`, `move`, `layout`, `patch`, `rm`, `from-process`, `from-fleet`), `open`, `close` |

Neither is default-granted (a Steward gets both). They cover every drawing board.

A drawing board linked to a group (`group` in `show`/`ls`) is its members' without
either: membership confers `groups.resources.read` (show, snapshot, ls) and
`groups.resources.write` (every change, open/close, `create --group <yours>`)
in that group, and stops the moment you leave it. Without `drawing-boards.read`,
`ls` lists only your groups' drawing boards.

`from-process` also needs `process.templates.read`; `from-fleet` also needs
`fleet.read`. A 403 names the missing slug: rather than retrying, rerun with
`--ask-human 60s` (any drawing board command) to ask the operator by popup, or ask for
`tclaude agent permissions grant <you> drawing-boards.write`. A `resource_locked`
refusal is a lock on that drawing board, not a missing slug: reads still work, and
`drawing-board show`/`ls` say it is locked. Wait for the operator to unlock it. An
agent the operator granted `groups.resources.lock` for the group may change a
locked drawing board and lock or unlock it:
`tclaude agent resources lock|unlock drawing board <id>`; `tclaude agent resources ls
<group>` lists the group's resources with their locks.

## Read

```bash
tclaude agent drawing-board ls [--json]
tclaude agent drawing-board show plan            # version + one line per live element
tclaude agent drawing-board show plan --since 12 # only changes after version 12, deletions included
tclaude agent drawing-board show plan --view     # only what the operator has on screen ("this", "here")
tclaude agent drawing-board show plan --json     # raw Excalidraw elements
tclaude agent drawing-board snapshot plan -o plan.png            # last PNG the dashboard rendered
tclaude agent drawing-board snapshot plan --region 3 -o chg.png  # the change a [drawing-board plan] mail numbered
```

Remember the version `show` prints and pass it as `--since` next time.
`show` lists line ends as `api:right -> db:left` and attached text as
`on <element>`. `snapshot` needs an open dashboard and may lag your own edits
(it says by how much); working alone, read with `show`. Look at a PNG with an
image-capable tool.

## Draw

```bash
tclaude agent drawing-board create plan --title "Release plan" [--group <group>]
tclaude agent drawing-board add plan rectangle --x 100 --y 80 --text API --id api
tclaude agent drawing-board add plan rectangle --right-of api --link --text DB --id db  # beside api, arrow from it
tclaude agent drawing-board add plan arrow --from api --from-side right --to db --to-side left --routing elbow --text calls
tclaude agent drawing-board add plan text --x 100 --y 20 --text Roadmap --font-size 48 --font sans --style bold
tclaude agent drawing-board text plan api --text "API v2"        # or only --font-size/--font/--style
tclaude agent drawing-board connect plan <line> --routing curved [--to cache --to-side top | --free-to]
tclaude agent drawing-board move plan api --x 300 --y 80          # text and attached lines follow
tclaude agent drawing-board layout plan [--direction down] [--gap 60]  # arrange shapes along their arrows
tclaude agent drawing-board rm plan <elem>...                     # attached text goes too
tclaude agent drawing-board patch plan --file elements.json       # raw elements; - reads stdin
```

- `add` shapes: `--x --y --w --h` (default 160x80), `--color`/`--bg`
  (`#rrggbb`, `#rgb`, `transparent`). Relative placement: `--right-of`,
  `--left-of`, `--below`, `--above` (with `--gap`, `--link`). Give stable
  `--id`s so you can refer to elements later.
- Text on a shape or line is a second element `<id>-label`.
- `patch`: to change an element send it with `version + 1`; to delete, also
  `isDeleted: true`. `move`, `text` and `rm` bump versions for you.

## Sessions and activity

Your first change starts a **drawing board session**: the drawing board stays in the
operator's Background terminals menu with no tab open, across reincarnation
and daemon restarts. `drawing-board open <id>` starts one again when you come back
later; `drawing-board close <id>` ends yours (the drawing board stays). Changing a drawing board, or
`open`, makes you a participant: the operator's edits then reach you as
`[drawing-board <id>]` mail (at most one per 3 s per drawing board), usually naming a
`--region` snapshot. React with `show --since <last version>`. Members of a
group drawing board's group are participants automatically.

## Generators

```bash
tclaude agent drawing-board from-process flow review   # template "review" as a flowchart (p-<step> ids); redraw replaces it
tclaude agent drawing-board from-fleet fleet [--group <g>] [--all]  # column per group, card per agent (fg-/fa- ids)
tclaude agent drawing-board fleet-changes fleet [--json]  # operator's card moves/deletions/notes as staffing proposals
```

`fleet-changes` changes nothing: confirm with the operator, act, then redraw
with `from-fleet`. Changing the process itself is a template edit (see the
process-templates skill); the drawing board is where you agree on it.

## Example: sketch a design and react to feedback

```bash
tclaude agent drawing-board create cache-design --title "Cache design"
tclaude agent drawing-board add cache-design rectangle --x 80 --y 80 --text Client --id client
tclaude agent drawing-board add cache-design rectangle --right-of client --link --text Cache --id cache
tclaude agent drawing-board add cache-design rectangle --right-of cache --link --text DB --id db
tclaude agent drawing-board show cache-design        # note "version N"
# ... operator draws; a [drawing-board cache-design] mail arrives ...
tclaude agent drawing-board show cache-design --since N
tclaude agent drawing-board snapshot cache-design --region 1 -o change.png
```

User docs: `docs/drawing-boards.md`. For a live web page instead of a drawing, use
the agent-page skill.
