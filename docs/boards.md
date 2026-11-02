# Collab boards

Boards are experimental and opt-in: turn on **Collab** under
[Config › Experimental](dashboard-config.md#experimental). Until then boards do not show in the
dashboard, and their API and the `tclaude agent board` commands answer with a disabled message.

A collab board is a shared drawing board for the operator and agents. The
operator draws in the dashboard, where a board opens as a tab of the Collab
view beside the terminals; agents read and change the same
board with `tclaude agent board`. Boards live in the daemon's SQLite database,
and their elements are a subset of Excalidraw's element JSON, so a board can be
exported as an `.excalidraw` file.

## In the dashboard

The **＋** at the right of the Collab tab strip lists the boards, one line each:
in their groups, newest first, with a green dot on a board an agent has a
session on. Pick one to open it in a tab. The box at the top searches titles and groups;
a new title makes a board with **Create** or Enter, and the title of one that
exists opens it. `/boards/<id>` opens that board's tab, and `/boards` opens the
list. A board tab behaves like a terminal
tab: it reorders, joins tab groups, and moves between a tab, an overlay window and a
browser window of its own, with **Open in overlay**, **Open in separate window** and
**Back to a tab**. Dragging it off the strip floats it where you drop it. Like a
terminal tab it lasts as long as its page: a reload brings back the board the
URL names and any board in the overlay, and a new browser window starts with none. The URL of a deleted
board opens nothing and becomes `/terminals`.

A board is stored in this tclaude instance, not in a file of its own. To get one
out, **⤓** on its row or in its header downloads an `.excalidraw` file.
**Import…** under the list makes a new board of such a file, named after it.
What a board cannot hold, such as images, is left out, and the dashboard says
how many elements that was.

To delete a board, use **🗑** on its row or **Delete board…** on its tab's
right-click menu. The dialog names the board and every agent on it, and offers
**⤓ Export first**, because a delete cannot be undone. Any tab or window showing
the board closes, a browser tab of its own says the board was deleted, and
everyone who was on it, stopped agents included, gets a message saying so.

**Open in overlay**, on the board or its tab's right-click menu, or dragging its tab off the strip, floats the
board in its own window over every dashboard view, apart from the companion
window, so you can work in both at once. It moves like the companion: drag it by
its title bar, resize it from any edge or corner, and snap it to an edge or into a
corner. A resized panel keeps its size, and the window keeps its place. Ctrl+wheel
over the title bar zooms the board. It has the companion's
[window controls](companion.md), and minimizing puts it in the dock of pills at
the bottom of the page; with the Classic look it has a plain × instead. Several
boards can float at once, and a window can hold several as
[tabs](companion.md#overlay-tabs), mixed with chats and shells. The open windows
come back after a reload. Clicking the overlay
or the companion brings that one forward. **Back to a tab** returns the board to the strip, and **Open in separate window** moves it to a browser window.

### Drawing

The toolbar's tools have keys: **V** select, **H** pan, **R** rectangle, **O** ellipse,
**D** diamond, **A** arrow, **L** line, **P** pen, **T** text and **E** eraser. Pick a
stroke and fill from the swatches. With two or more shapes selected you can align them,
and with three or more, space them evenly.

- **Move a shape:** drag it from anywhere inside, filled or not. To start a
  rubber band inside a shape, hold Ctrl or Cmd.
- **Text:** double-click anywhere inside a shape for its label, or blank board for
  free text. Enter on a selected shape does the same. Enter finishes, Shift+Enter
  is a new line.
- **Text style:** with the text tool, a text editor open, or text selected (a
  shape's label counts), the toolbar offers the size, the font (Hand, Sans or Mono),
  bold and italic. They change what is selected or being typed, and hold for the
  next text you add. The style is saved with the board.
- **Right-click** for what applies to what is under the pointer; see below.
- **Select:** Shift-click or drag a rectangle. Ctrl+A selects everything, Ctrl+D
  duplicates, Del deletes.
- **Undo:** Ctrl/Cmd+Z, and Ctrl+Shift+Z or Ctrl+Y to redo.
- **Zoom:** the mouse wheel, Ctrl+wheel or a trackpad pinch zooms from 20% to 400%,
  around the pointer. The controls by
  the zoom level in the toolbar zoom out, zoom in, reset to 100% and fit everything
  in view; their keys are `-`, `+`, Shift+0 and Shift+1.
- **Move around:** two-finger scroll on a touchpad pans, and so do middle-drag and
  Shift+wheel. Browsers report a mouse wheel and a touchpad scroll as the same
  event, so the board tells them apart by their steps: a free-spinning or smoothed
  mouse wheel can read as a touchpad and pan instead of zooming.
- **Copy and paste:** Ctrl+C, X and V use Excalidraw's clipboard format, so they work
  with excalidraw.com. Plain text pastes as a text element.

The chip beside the title shows `synced`, `saving…`, `not saved` or `offline`. Click a
person's chip to open that agent in the companion, or **×** to remove them. **＋ Invite
agent** lists live agents and also sends the one you pick instructions for using
boards. The group selector files the board under a group, and **⤓** downloads it as
`.excalidraw`.

### Connecting shapes

With the select, arrow, line or pen tool, a shape shows four connection points,
the middle of each side, under the pointer.

- **Drag from a connection point** to draw an arrow. Drop it on or near a shape to
  attach it at that shape's nearest connection point. The points show while you
  drag, and the one it would take stands out.
- **Hold Ctrl (Cmd on a Mac)** while dropping an end to attach it to the shape's
  border instead: the end meets the shape wherever the line reaches it. On blank
  board it leaves that end free.
- **Drop on blank board** and a quick menu asks what shape goes there. Type to
  search (`box`, `circle` and `decision` work too), and press Enter for the marked
  row or the number shown beside a row. Shape names hold no digits, so a digit
  always picks and never searches. Enter alone repeats the last kind, or the
  source shape's kind at first. Escape or clicking elsewhere cancels and leaves
  nothing behind, and the click then does what it would have done. The board's
  own number keys wait until the menu closes.
- **With a shape selected,** an arrow key opens the same menu from that side, with
  the new shape a step out and beside anything already there. The new shape is
  selected, so the next arrow key carries on, and Enter opens its text. A graph
  needs no toolbar: `→` `Enter` `Enter`, type, `Enter`, `→` and so on.
- **Click a connection point** for a suggestion: a faint arrow to a copy of the
  shape. It follows the pointer. Click a shape to connect to it, click blank
  board for the same quick menu as a drop, or press Escape.
- **The arrow and line tools** attach either end the same way. The line tool
  leaves a free end on blank board and asks nothing.
- **Select a line** to see its ends, which are filled when attached. Drag an end
  to attach it elsewhere or free it. Dragging the whole line away from a shape
  frees it from that shape.

Lines saved before connection points keep the attachment they have until you
move an end.

Attached ends follow their shape through moves and resizes, whoever makes them.
Deleting a shape leaves its lines where they are, free at that end, and undo
reattaches them. A pen stroke started on a snap point moves with that shape,
without changing its form.

Arrows and lines can run **straight**, **elbow** or **curved**. The three buttons
appear on the toolbar for a selected line or with the arrow and line tools. They
change the selection and the next line drawn. Elbow and curved lines do not
steer around other shapes.

### Text on shapes and lines

Double-click any shape, line, arrow or pen stroke to give it text, and
double-click again to change it. Text sits in the middle of a box or halfway
along a line, moves with its shape, and masks the line behind it. Empty the text
to remove it. Clicking the text picks its shape. Text does not wrap to fit a
shape.

### The context menu

Right-click anything on the board. What you click is selected first, unless it
is already in the selection, in which case the menu acts on the whole selection.

| On | The menu offers |
| --- | --- |
| A shape | Edit text, change kind, duplicate, copy, cut, front and back, stroke and fill, delete |
| A line or arrow | Edit label, routing, arrowhead on or off, duplicate, copy, cut, front and back, stroke, delete |
| Text or a pen stroke | Edit, duplicate, copy, cut, front and back, stroke, delete |
| Several | Alignment, duplicate, copy, cut, front and back, stroke and fill, delete |
| Blank board | Add a shape or text there, paste there, select all, fit the board to the view, undo and redo |

Arrow keys, Home and End move through it and Enter runs a row. Escape or clicking
away closes it. Bring to front and send to back hold across reloads and for
everyone on the board. Anything you add lands on top; what an agent adds lands
above ordinary elements but under what was brought to front. Where the browser refuses to read the clipboard, Paste uses the
last copy made on this page.

### Picking a tool

Each tool in the board's menu has a number key, shown on its button: `1` to `9`
and then `0`, in menu order, beside the tool's letter (hover a button for both).
The keys go to the board you used last. They do nothing while you type on the
board, in a terminal or in any other field, or after you click another part of
the dashboard. With Ctrl, Alt or Cmd held they stay the browser's.

### The minimap

The minimap in the board's bottom right corner shows the whole board and marks
the part on screen. It takes in remote and agent edits as they arrive. Click it
to go there, or drag it to move the view; pressing on the marker holds the
marker where you pressed it.

The map has its own zoom, separate from the main view. Scroll over it, or use
**−** and **+**, to go up to 32×, and the zoom between them to see the whole
board again (it reads **Fit** when you do).
Zoomed in, the map follows the main view when that leaves its window, and
Shift+drag or the middle button moves the map's window by hand. **▾** hides the
map to a small button. The zoom and the hidden state are remembered for every
board, in the dashboard's saved preferences.

## Permissions

Boards are global, not scoped to a group: `boards.read` and `boards.write` cover
every board, including a fleet map, whose cards show agents' tasks and PRs.

| Slug | Allows |
|---|---|
| `boards.read` | `board ls`, `board show`, `board snapshot`, `board fleet-changes` (with `fleet.read`) |
| `boards.write` | `board create`, every change (`add`, `patch`, `rm`, `move`, `text`, `connect`, `layout`, `from-fleet`, `from-process`), and `board open` and `board close` on the agent's own session |

Neither is default-granted, although a Steward gets both when summoned:

```bash
tclaude agent permissions grant <agent> boards.read
tclaude agent permissions grant <agent> boards.write
```

An agent that changes a board becomes one of its participants. The operator
can also add or remove participants from the dashboard.

## Agent commands

```bash
tclaude agent board ls
tclaude agent board create plan --title "Release plan" [--group <group>]
tclaude agent board show plan                 # one line per live element, plus the version
tclaude agent board show plan --since 12      # only what changed after version 12, deletions included
tclaude agent board show plan --json          # the elements as Excalidraw JSON
tclaude agent board show plan --view          # only what the operator has on screen
tclaude agent board snapshot plan -o plan.png # the dashboard's latest rendering
tclaude agent board snapshot plan --region 3 -o change.png  # the image a board event named
tclaude agent board open plan                 # start your board session again, coming back later
tclaude agent board close plan                # end your board session

tclaude agent board add plan rectangle --x 100 --y 80 --w 160 --h 80 --text API
tclaude agent board add plan text --x 100 --y 200 --text "Owned by the platform team"
tclaude agent board add plan text --x 100 --y 40 --text Roadmap --font-size 48 --font sans --style bold
tclaude agent board add plan arrow --from <elemA> --to <elemB>
tclaude agent board add plan arrow --from api --from-side right --to db --to-side left --routing elbow --text calls
tclaude agent board add plan rectangle --right-of api --link --text DB --id db   # beside api, with an arrow from it
tclaude agent board move plan <elem> --x 300 --y 80   # its text and attached lines follow; a moved line detaches
tclaude agent board layout plan [--direction down]    # arrange the shapes along their arrows
tclaude agent board from-process flow review          # draw process template "review" as a flowchart
tclaude agent board from-fleet fleet                  # draw the fleet: a column per group, a card per agent
tclaude agent board fleet-changes fleet               # read the operator's edits on that map as staffing proposals
tclaude agent board text plan <elem> --text "API v2"   # any element; adds attached text if it has none
tclaude agent board text plan <elem> --font-size 32 --font mono --style italic   # style only; any of the three
tclaude agent board connect plan <line> --routing curved
tclaude agent board connect plan <line> --to cache --to-side top   # or --free-to, --free-from
tclaude agent board rm plan <elem> [<elem>...]         # attached text goes too; lines stay, free at that end
tclaude agent board patch plan --file elements.json   # - reads stdin
```

`show` prints the board version; pass it as `--since` next time to read only
the changes. It lists a line's attached ends as `api:right -> db:left`, with its
routing, and attached text as `on <element>`. `snapshot` says how far the PNG lags the board, because the
dashboard renders it and an agent's own changes appear in it only after the
operator's editor saves a new one.

## Board sessions

Agents work on a board with no tab open: their edits go through the daemon to
the database. A board session records that an agent is doing so, as a tmux
session does for a terminal, so the operator can come back to the board later.
It is stored per board and agent, and it survives reincarnation and daemon
restarts.

An agent's accepted edit starts its session or refreshes it, and `board open`
starts one explicitly, for an agent coming back to a board. `board open` also
makes the agent a participant. The operator's edits start no session. A session
has no time limit, and closing a tab does not end it. It ends with:

- `board close`, which ends only the calling agent's own session;
- the agent retiring, which includes moving to another node;
- the board being deleted;
- the operator ending the board's row in the dashboard.

Ending a session changes nothing on the board, and its agents stay participants.
An agent's next edit starts a new session.

Boards with a session list under **Boards** in the
[Background terminals](dashboard-terminals.md) counter, with the board's title and the agents
on it. The row hides while the board is open on the page, and **Open** brings it
up as a tab. **End** removes every session on that board, never the board. Rows are this node's only:
peers do not list them, and a peer's per-node view leaves them out. With Collab
off they do not show.

`board snapshot` still needs a dashboard: it returns what an open editor last
rendered, however old, so an agent working alone should read with `board show`.

## Elements and merging

The server keeps only the element keys it knows and bounds each of them:
`id`, `type` (`rectangle`, `ellipse`, `diamond`, `arrow`, `line`, `freedraw`,
`text`), geometry (`x`, `y`, `width`, `height`, `angle`, `points`), style
(`strokeColor`, `backgroundColor`, `fillStyle`, `strokeWidth`, `strokeStyle`,
`roughness`, `opacity`), text (`text`, `fontSize`, `fontFamily`, `fontWeight`,
`fontStyle`, `textAlign`, `containerId`),
connectors (`startBinding`, `endBinding`, `routing`), `z` for an element sent to the front or back, and `version`,
`versionNonce`, `isDeleted` and `updated`. `customData.author` is always set by the server: to
`operator` for the dashboard, and to the calling agent's id for an agent.
`version` is at most 2^53-1 and `versionNonce` a 32-bit integer. A board holds
at most 5000 live elements and 20000 stored ones (tombstones included), and
there are at most 500 boards. A patch that breaks a limit changes nothing.

Only `text` elements draw text. Text on a shape or line is a second element
(`<id>-label`) whose `containerId` names what it sits on, as in Excalidraw. The
stored `x` and `y` are kept up to date, but the editor places it from its
container. Text without a `containerId` belongs to the shape it was drawn in or
to the `<id>` of its `<id>-label` id; it moves with that element and stays where
it was put on it. Deleting an element deletes its text. Elements are returned in
creation order, which is also their stacking order, and an edit keeps an
element's place.

`startBinding` and `endBinding` attach the ends of an `arrow`, `line` or
`freedraw` to another element by `elementId`. An optional `fixedPoint` is the
snap point as `[x, y]` fractions of that element's box, Excalidraw's form: `[1,
0.5]` is the middle of the right side. `routing` is `straight` (the default when
absent), `elbow` or `curved`, on arrows and lines. `points` always hold the path
as drawn, so a client that does not know these fields still draws the line, and
keeps the fields when it writes the element back. The editor works out an
attached end from its binding, so a line stays on its shape after a raw `patch`
or a merge of two edits, without anyone saving the line. A binding to a deleted
element reads as a free end. A bound line with bends and no `routing`, as
`board from-process` draws rework, keeps its path until an end moves.

Each element merges on its own, as Excalidraw reconciles: an incoming element
replaces the stored one when there is none, when its `version` is higher, or
when the versions are equal and its `versionNonce` is lower. To change an
element, send it with `version + 1`; `move`, `text` and `rm` do this for you.
A deletion is a tombstone (`isDeleted: true`) that stays so it can sync.

An open editor holds a socket (`GET /api/boards/{id}/live`) that says when the
board changed, and reads the change with `GET ?since=`. The socket carries no
board content. When the socket is down, the editor polls every second.

Undo and redo in the dashboard are the operator's own, one step per action, and
are kept by the daemon, up to 100 steps per board. They survive a reload and work
from any editor open on the board. An undo writes new versions, which other
editors merge. A shape an agent changed since is left alone, and the editor says
so. Agents have no undo of their own.

The board's own version goes up once for every patch that changed at least one
element, and each changed element records that version as its
`server_version`. A plain read returns the live elements; a read with
`since=N` returns every element whose `server_version` is above `N`,
tombstones included. A tombstone keeps only `id`, `type`, `version`,
`versionNonce`, `isDeleted`, `updated` and `customData`.

Deleting a board and creating one with the same id starts again at version 0,
so a client holding an older `--since` should re-read the whole board.

## Board activity

When the operator works on a board, the dashboard can tell the board's
participants. Each participant that is online receives operator mail with the
subject `[board <id>]` and a hint to run `board show`. This is limited to one
message per board every 3 seconds; the dashboard holds back an edit that hits the
limit and sends it with any newer ones once the 3 seconds are up.

The mail usually also names a numbered image of the change:
`board snapshot <id> --region <n> -o change.png`. The dashboard renders the
area the operator touched, padded with some of what is around it. The daemon
keeps the newest 20 of these per board; an older number answers `404
no_region`, and the full `snapshot` is the fallback.

## Group boards

A board can belong to a group. The group's active members are on the board
without being invited one by one: they get its events and show on the editor
as dashed chips. Pick the group from the editor's header, or create the board
for one:

```bash
tclaude agent board create ux --title "UX flow" --group design   # a group you are a member of
```

When the operator links a board to a group, the members who are online then
receive the invitation mail. An agent can only create a board for a group it
belongs to. Choosing "No group" unlinks the board. Archiving or deleting the
group also unlinks it in effect: the board stays, without the group.

## Mapping the fleet

`board from-fleet <board>` draws the fleet as a map you can talk staffing over.
Each group is a column with a dashed box, and each live agent is a card in it
(an agent in two groups has a card in both),
coloured by what it is doing: green for working, grey for idle, yellow for
waiting on you, red for a failed turn. A card names the agent, its status, and
its open PR or task. `--all` adds offline agents as white, dashed cards, and
`--group` draws one group. The title line carries the counts and the time it
was drawn. The first map goes below whatever the board already holds. Drawing it
again refreshes it where it is, so your marks on it keep pointing at the same
cards: its elements are `fg-…` and `fa-…`, a card whose
agent is gone is removed, and your own notes and arrows stay. Circle an agent,
or draw an arrow from it to another group, and the Steward reads that as you
talking about staffing. The command needs `boards.write` and `fleet.read`.

`board fleet-changes <board>` reads your edits back as staffing proposals: a
card dragged to another column or an arrow from a card to another column
(move), a deleted card (remove), and a note written in a column (add). It
changes nothing. The Steward confirms the list with you, asks for what it needs
to act, and draws the map again, which puts the cards back in place. It needs
`boards.read` and `fleet.read`.

## Sketching a process

`board from-process <board> <template>` draws a process template as a
flowchart, so a process can be talked through on a board. Each step is a shape
coloured by who performs it: yellow for human, blue for agent, green for
program. Decisions are diamonds, and start and end are ellipses. Each outcome
is an arrow, and an outcome that loops back (rework) goes around beneath the
steps. A step's outcomes are labelled when it has more than one. The board is
created if it does not exist. A board holds one process sketch. Steps are
named `p-<step>` and outcomes `pa-<from>-<outcome>-<to>`. Drawing again, with
the same template or another, replaces the sketch: steps are updated in place
and any element the new sketch lacks is deleted. Elements you added yourself
are kept. The command needs `boards.write` and `process.templates.read`.

Changing the process itself remains a process-template edit (see the
process-templates skill). The board is where the operator and agents agree on
the change.

## Presence

Who is on a board is shown live, without chat. An agent that read the board
in the last 30 seconds is *looking*. An agent whose edit landed in the last 6
seconds is *drawing*. The editor marks both on the agent's chip and shows
"… is drawing" over the canvas.

The open editor also reports the part of the board on the operator's screen.
`board show` prints it ("The operator is looking at x=… y=…"), and
`board show --view` lists only the elements in it, which is what the operator
means by "this" or "here". The view is dropped when the operator looks away or
after 2 minutes without an update. Presence is held in memory and is not
stored.

## Storage

Boards, elements, participants, board sessions and undo steps are in the
`collab_boards`, `collab_board_elements`, `collab_board_participants`,
`collab_board_sessions` and `collab_board_history` tables. The latest PNG
snapshot is `~/.tclaude/data/boards/<id>.png` and the event images are under
`~/.tclaude/data/boards/<id>.regions/`, all private daemon state.

## HTTP routes

The dashboard uses `/api/boards` (list, create, read, patch, delete, snapshot
upload and download, participants, events, view, group and undo), and
`DELETE /api/boards/{id}/sessions` to end a board's sessions. A federation
peer's dashboard view never reaches these routes: boards stay with their own
node. Agents use `/v1/boards`: `GET /v1/boards`, `POST /v1/boards`,
`GET /v1/boards/{id}` (`?since=N`), `PATCH /v1/boards/{id}`,
`GET /v1/boards/{id}/snapshot`, and `POST` and `DELETE /v1/boards/{id}/session`
for the caller's own session. The snapshot response carries `X-Board-Snapshot-Version` and `X-Board-Version`
headers. A snapshot upload for a version older than the stored snapshot is
refused with 409.
