# Collab drawing boards

Drawing boards are part of [Collab](companion.md).

A collab drawing board is a shared canvas for the operator and agents. (It is
not the group [task board](taskboard.md).) The
operator draws in the dashboard, where a drawing board opens as a tab of the Collab
view beside the terminals; agents read and change the same
drawing board with `tclaude agent drawing-board`. Drawing boards live in the daemon's SQLite database,
and their elements are a subset of Excalidraw's element JSON, so a drawing board can be
exported as an `.excalidraw` file.

## In the dashboard

**＋ New ▾** › **Drawing board…** in the Collab side panel lists the drawing boards, one line each
(**Process…** beside it lists the processes: see [Processes](processes.md)):
in their groups, newest first, with a green dot on a drawing board an agent has a
session on. Pick one to open it in a tab. The box at the top searches titles and groups;
a new title makes a drawing board with **Create** or Enter, and the title of one that
exists opens it. `/drawing-boards/<id>` opens that drawing board's tab, and `/drawing-boards` opens the
list. A drawing board tab behaves like a terminal
tab: it reorders, joins tab groups, and moves between a tab, an overlay window and a
browser window of its own, with **Open in overlay**, **Open in separate window** and
**Open in the Collab tab** (in the drawing board's **View** menu, and on its tab's
right-click menu). Dragging it off the strip floats it where you drop it. Like a
terminal tab it lasts as long as its page: a reload brings back the drawing board the
URL names and any drawing board in the overlay, and a new browser window starts with none. The URL of a deleted
drawing board opens nothing and becomes `/terminals`.

A drawing board is stored in this tclaude instance, not in a file of its own. To get one
out, **⤓** on its row or **File › Download .excalidraw** in the editor downloads an `.excalidraw` file.
**Import…** under the list makes a new drawing board of such a file, named after it.
What a drawing board cannot hold, such as images, is left out, and the dashboard says
how many elements that was.

To delete a drawing board, use **🗑** on its row, **File › Delete drawing board…** in
the editor, or **Delete drawing board…** on its tab's right-click menu. The dialog names the drawing board and every agent on it, and offers
**⤓ Export first**, because a delete cannot be undone. Any tab or window showing
the drawing board closes, a browser tab of its own says the drawing board was deleted, and
everyone who was on it, stopped agents included, gets a message saying so.

**Open in overlay**, in the drawing board's **View** menu or its tab's right-click menu, or dragging its tab off the strip, floats the
drawing board in its own window over every dashboard view, apart from the companion
window, so you can work in both at once. It moves like the companion: drag it by
its title bar, resize it from any edge or corner, and snap it to an edge or into a
corner. A resized panel keeps its size, and the window keeps its place. Ctrl+wheel
over the title bar zooms the drawing board. It has the companion's
[window controls](companion.md), and minimizing puts it in the dock of pills at
the bottom of the page; with the Classic look it has a plain × instead. Several
drawing boards can float at once, and a window can hold several as
[tabs](companion.md#overlay-tabs), mixed with chats and shells. The open windows
come back after a reload. Clicking the overlay
or the companion brings that one forward. **Open in the Collab tab** returns the drawing board to the strip, and **Open in separate window** moves it to a browser window.

### Drawing

The toolbar's tools have keys: **V** select, **H** pan, **R** rectangle, **O** ellipse,
**D** diamond, **A** arrow, **L** line, **P** pen, **T** text and **E** eraser. Pick a
stroke and fill from the swatches. With two or more shapes selected you can align them,
and with three or more, space them evenly.

- **Move a shape:** drag it from anywhere inside, filled or not. To start a
  rubber band inside a shape, hold Ctrl or Cmd.
- **Text:** double-click anywhere inside a shape for its label, or blank drawing board for
  free text. Enter on a selected shape does the same. Enter finishes, Shift+Enter
  is a new line.
- **Text style:** with the text tool, a text editor open, or text selected (a
  shape's label counts), the toolbar offers the size, the font (Hand, Sans or Mono),
  bold and italic. They change what is selected or being typed, and hold for the
  next text you add. The style is saved with the drawing board.
- **Right-click** for what applies to what is under the pointer; see below.
- **Select:** Shift-click or drag a rectangle. Ctrl A selects everything, Ctrl D
  duplicates, Del deletes.
- **Undo:** Ctrl Z (⌘ Z on a Mac), and Ctrl Shift Z or Ctrl Y to redo.
- **Zoom:** the mouse wheel, Ctrl+wheel or a trackpad pinch zooms from 20% to 400%,
  around the pointer. The controls by
  the zoom level in the toolbar zoom out, zoom in, reset to 100% and fit everything
  in view; their keys are `-`, `+`, Shift+0 and Shift+1.
- **Move around:** two-finger scroll on a touchpad pans, and so do middle-drag and
  Shift+wheel. Browsers report a mouse wheel and a touchpad scroll as the same
  event, so the drawing board tells them apart by their steps: a free-spinning or smoothed
  mouse wheel can read as a touchpad and pan instead of zooming.
- **Copy and paste:** Ctrl C, X and V use Excalidraw's clipboard format, so they work
  with excalidraw.com. Plain text pastes as a text element.

The editor's header has the title, the lock, and a chip that shows `synced`,
`saving…`, `not saved` or `offline`. Then comes a menu bar, which every resource editor
shares:

- **File:** **Export** (**Download .excalidraw**; the image export joins it there), and
  **Delete drawing board…**.
- **View:** **Zoom to fit** (Shift+1), **Zoom 100%** (Shift+0), **Zoom in**, **Zoom out**,
  and the moves: **Open in overlay**, **Open in separate window**, **Open in the Collab tab**.
- **Share:** the agents on the drawing board. Click one to open that agent in the
  companion, or **×** to remove them. **Invite agent ›** lists live agents and also sends
  the one you pick instructions for using drawing boards. The group selector files the
  drawing board under a group.

The avatars at the right of the header are the same agents, with who is drawing; a
click opens **Share**. ←/→ move along the menu bar, ↓ goes into a menu, and Escape closes it.

### Connecting shapes

With the select, arrow, line or pen tool, a shape shows four connection points,
the middle of each side, under the pointer.

- **Drag from a connection point** to draw an arrow. Drop it on or near a shape to
  attach it at that shape's nearest connection point. The points show while you
  drag, and the one it would take stands out.
- **Hold Ctrl (Cmd on a Mac)** while dropping an end to attach it to the shape's
  border instead: the end meets the shape wherever the line reaches it. On blank
  drawing board it leaves that end free.
- **Drop on blank drawing board** and a quick menu asks what shape goes there. Type to
  search (`box`, `circle` and `decision` work too), and press Enter for the marked
  row or the number shown beside a row. Shape names hold no digits, so a digit
  always picks and never searches. Enter alone repeats the last kind, or the
  source shape's kind at first. Escape or clicking elsewhere cancels and leaves
  nothing behind, and the click then does what it would have done. The drawing board's
  own number keys wait until the menu closes.
- **With a shape selected,** an arrow key opens the same menu from that side, with
  the new shape a step out and beside anything already there. The new shape is
  selected, so the next arrow key carries on, and Enter opens its text. A graph
  needs no toolbar: `→` `Enter` `Enter`, type, `Enter`, `→` and so on.
- **Click a connection point** for a suggestion: a faint arrow to a copy of the
  shape. It follows the pointer. Click a shape to connect to it, click blank
  drawing board for the same quick menu as a drop, or press Escape.
- **The arrow and line tools** attach either end the same way. The line tool
  leaves a free end on blank drawing board and asks nothing.
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

Right-click anything on the drawing board. What you click is selected first, unless it
is already in the selection, in which case the menu acts on the whole selection.

| On | The menu offers |
| --- | --- |
| A shape | Edit text, change kind, duplicate, copy, cut, front and back, stroke and fill, delete |
| A line or arrow | Edit label, routing, arrowhead on or off, duplicate, copy, cut, front and back, stroke, delete |
| Text or a pen stroke | Edit, duplicate, copy, cut, front and back, stroke, delete |
| Several | Alignment, duplicate, copy, cut, front and back, stroke and fill, delete |
| Blank drawing board | Add a shape or text there, paste there, select all, fit the drawing board to the view, undo and redo |

Arrow keys, Home and End move through it and Enter runs a row. Escape or clicking
away closes it. Bring to front and send to back hold across reloads and for
everyone on the drawing board. Anything you add lands on top; what an agent adds lands
above ordinary elements but under what was brought to front. Where the browser refuses to read the clipboard, Paste uses the
last copy made on this page.

### Picking a tool

Each tool in the drawing board's menu has a number key, shown on its button: `1` to `9`
and then `0`, in menu order, beside the tool's letter (hover a button for both).
The keys go to the drawing board you used last. They do nothing while you type on the
drawing board, in a terminal or in any other field, or after you click another part of
the dashboard. With Ctrl, Alt or Cmd held they stay the browser's.

### The minimap

The minimap in the drawing board's bottom right corner shows the whole drawing board and marks
the part on screen. It takes in remote and agent edits as they arrive. Click it
to go there, or drag it to move the view; pressing on the marker holds the
marker where you pressed it.

The map has its own zoom, separate from the main view. Scroll over it, or use
**−** and **+**, to go up to 32×, and the zoom between them to see the whole
drawing board again (it reads **Fit** when you do).
Zoomed in, the map follows the main view when that leaves its window, and
Shift+drag or the middle button moves the map's window by hand. **▾** hides the
map to a small button. The zoom and the hidden state are remembered for every
drawing board, in the dashboard's saved preferences.

## Permissions

`drawing-boards.read` and `drawing-boards.write` are global: they cover every drawing board, including a
fleet map, whose cards show agents' tasks and PRs.

| Slug | Allows |
|---|---|
| `drawing-boards.read` | `drawing-board ls`, `drawing-board show`, `drawing-board snapshot`, `drawing-board fleet-changes` (with `fleet.read`) |
| `drawing-boards.write` | `drawing-board create`, every change (`add`, `patch`, `rm`, `move`, `text`, `connect`, `layout`, `from-fleet`, `from-process`), and `drawing-board open` and `drawing-board close` on the agent's own session |

Neither is default-granted, although a Steward gets both when summoned:

```bash
tclaude agent permissions grant <agent> drawing-boards.read
tclaude agent permissions grant <agent> drawing-boards.write
```

### Group drawing boards are their members'

A drawing board linked to a group belongs to that group's members, who need neither
global slug for it. Active membership confers two group-scoped slugs in that
group:

| Slug | Allows, on the group's drawing boards |
|---|---|
| `groups.resources.read` | `drawing-board show`, `drawing-board snapshot`; `drawing-board ls` lists them |
| `groups.resources.write` | every change and `drawing-board open`/`close`, and `drawing-board create --group <group>`; implies read |

Membership is checked at every request, so an agent that joins a group can use
its drawing boards at once, and one that leaves (or whose group is archived) loses
them at once. An explicit deny of either slug for an agent wins over its
membership:

```bash
tclaude agent permissions deny <agent> groups.resources.write   # keeps read
```

Membership and the global slugs are independent sources: denying an agent
`drawing-boards.read` or `drawing-boards.write` does not keep it off its own groups' drawing boards;
deny `groups.resources.read` / `groups.resources.write` for that. Like every
`groups.*` slug, both are also covered by `groups.admin`.

A drawing board linked to no group is reachable only through `drawing-boards.read` and
`drawing-boards.write`. Without `drawing-boards.read`, `drawing-board ls` lists only the drawing boards of the
agent's groups. An operator lock on a drawing board refuses an agent's change
whatever its access: see
[The operator's lock on a resource](permissions-and-audit.md#the-operators-lock-on-a-resource).

Every drawing board command takes `--ask-human <timeout>`: when the agent lacks the
slug, the operator gets an approval popup instead of a refusal, as with other
permission-gated commands.

An agent that changes a drawing board becomes one of its participants. The operator
can also add or remove participants from the dashboard.

## Agent commands

```bash
tclaude agent drawing-board ls
tclaude agent drawing-board create plan --title "Release plan" [--group <group>]
tclaude agent drawing-board show plan                 # one line per live element, plus the version
tclaude agent drawing-board show plan --since 12      # only what changed after version 12, deletions included
tclaude agent drawing-board show plan --json          # the elements as Excalidraw JSON
tclaude agent drawing-board show plan --view          # only what the operator has on screen
tclaude agent drawing-board snapshot plan -o plan.png # the dashboard's latest rendering
tclaude agent drawing-board snapshot plan --region 3 -o change.png  # the image a drawing board event named
tclaude agent drawing-board open plan                 # start your drawing board session again, coming back later
tclaude agent drawing-board close plan                # end your drawing board session

tclaude agent drawing-board add plan rectangle --x 100 --y 80 --w 160 --h 80 --text API
tclaude agent drawing-board add plan text --x 100 --y 200 --text "Owned by the platform team"
tclaude agent drawing-board add plan text --x 100 --y 40 --text Roadmap --font-size 48 --font sans --style bold
tclaude agent drawing-board add plan arrow --from <elemA> --to <elemB>
tclaude agent drawing-board add plan arrow --from api --from-side right --to db --to-side left --routing elbow --text calls
tclaude agent drawing-board add plan rectangle --right-of api --link --text DB --id db   # beside api, with an arrow from it
tclaude agent drawing-board move plan <elem> --x 300 --y 80   # its text and attached lines follow; a moved line detaches
tclaude agent drawing-board layout plan [--direction down]    # arrange the shapes along their arrows
tclaude agent drawing-board from-process flow review          # draw process template "review" as a flowchart
tclaude agent drawing-board from-fleet fleet                  # draw the fleet: a column per group, a card per agent
tclaude agent drawing-board fleet-changes fleet               # read the operator's edits on that map as staffing proposals
tclaude agent drawing-board text plan <elem> --text "API v2"   # any element; adds attached text if it has none
tclaude agent drawing-board text plan <elem> --font-size 32 --font mono --style italic   # style only; any of the three
tclaude agent drawing-board connect plan <line> --routing curved
tclaude agent drawing-board connect plan <line> --to cache --to-side top   # or --free-to, --free-from
tclaude agent drawing-board rm plan <elem> [<elem>...]         # attached text goes too; lines stay, free at that end
tclaude agent drawing-board patch plan --file elements.json   # - reads stdin
```

`show` prints the drawing board version; pass it as `--since` next time to read only
the changes. It lists a line's attached ends as `api:right -> db:left`, with its
routing, and attached text as `on <element>`. `snapshot` says how far the PNG lags the drawing board, because the
dashboard renders it and an agent's own changes appear in it only after the
operator's editor saves a new one.

## Drawing board sessions

Agents work on a drawing board with no tab open: their edits go through the daemon to
the database. A drawing board session records that an agent is doing so, as a tmux
session does for a terminal, so the operator can come back to the drawing board later.
It is stored per drawing board and agent, and it survives reincarnation and daemon
restarts.

An agent's accepted edit starts its session or refreshes it, and `drawing-board open`
starts one explicitly, for an agent coming back to a drawing board. `drawing-board open` also
makes the agent a participant. The operator's edits start no session. A session
has no time limit, and closing a tab does not end it. It ends with:

- `drawing-board close`, which ends only the calling agent's own session;
- the agent retiring, which includes moving to another node;
- the drawing board being deleted;
- the operator ending the drawing board's row in the dashboard.

Ending a session changes nothing on the drawing board, and its agents stay participants.
An agent's next edit starts a new session.

Drawing boards with a session list under **Drawing boards** in the
[Background terminals](dashboard-terminals.md) counter, with the drawing board's title and the agents
on it. The row hides while the drawing board is open on the page, and **Open** brings it
up as a tab. **End** removes every session on that drawing board, never the drawing board. Rows are this node's only:
peers do not list them, and a peer's per-node view leaves them out.

`drawing-board snapshot` still needs a dashboard: it returns what an open editor last
rendered, however old, so an agent working alone should read with `drawing-board show`.

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
`version` is at most 2^53-1 and `versionNonce` a 32-bit integer. A drawing board holds
at most 5000 live elements and 20000 stored ones (tombstones included), and
there are at most 500 drawing boards. A patch that breaks a limit changes nothing.

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
`drawing-board from-process` draws rework, keeps its path until an end moves.

Each element merges on its own, as Excalidraw reconciles: an incoming element
replaces the stored one when there is none, when its `version` is higher, or
when the versions are equal and its `versionNonce` is lower. To change an
element, send it with `version + 1`; `move`, `text` and `rm` do this for you.
A deletion is a tombstone (`isDeleted: true`) that stays so it can sync.

An open editor holds a socket (`GET /api/drawing-boards/{id}/live`) that says when the
drawing board changed, and reads the change with `GET ?since=`. The socket carries no
drawing board content. When the socket is down, the editor polls every second.

Undo and redo in the dashboard are the operator's own, one step per action, and
are kept by the daemon, up to 100 steps per drawing board. They survive a reload and work
from any editor open on the drawing board. An undo writes new versions, which other
editors merge. A shape an agent changed since is left alone, and the editor says
so. Agents have no undo of their own.

The drawing board's own version goes up once for every patch that changed at least one
element, and each changed element records that version as its
`server_version`. A plain read returns the live elements; a read with
`since=N` returns every element whose `server_version` is above `N`,
tombstones included. A tombstone keeps only `id`, `type`, `version`,
`versionNonce`, `isDeleted`, `updated` and `customData`.

Deleting a drawing board and creating one with the same id starts again at version 0,
so a client holding an older `--since` should re-read the whole drawing board.

## Drawing board activity

When the operator works on a drawing board, the dashboard can tell the drawing board's
participants. Each participant that is online receives operator mail with the
subject `[drawing-board <id>]` and a hint to run `drawing-board show`. This is limited to one
message per drawing board every 3 seconds; the dashboard holds back an edit that hits the
limit and sends it with any newer ones once the 3 seconds are up.

The mail usually also names a numbered image of the change:
`drawing-board snapshot <id> --region <n> -o change.png`. The dashboard renders the
area the operator touched, padded with some of what is around it. The daemon
keeps the newest 20 of these per drawing board; an older number answers `404
no_region`, and the full `snapshot` is the fallback.

## Group drawing boards

A drawing board can belong to a group. The group's active members are on the drawing board
without being invited one by one: they get its events and show on the editor
as dashed chips. Pick the group from the editor's header, or create the drawing board
for one:

```bash
tclaude agent drawing-board create ux --title "UX flow" --group design   # a group you are a member of
```

When the operator links a drawing board to a group, the members who are online then
receive the invitation mail. An agent can only create a drawing board for a group it
belongs to. Choosing "No group" unlinks the drawing board. Archiving or deleting the
group also unlinks it in effect: the drawing board stays, without the group.

## A peer's drawing boards

On a linked peer's page (the Fleet map's **Open**, `?node=…`), the peer's
drawing boards show when it shares them through their groups:
`groups.resources.read` in a group reads that group's drawing boards, and
`groups.resources.write` also edits them (see
[federation-peers.md](federation-peers.md)). A drawing board in no group is never a
peer's. The page lists them in two places:

- The **Collab** panel lists each drawing board under its group, with an **edit** or
  **read-only** badge. A group you may edit has a ＋ that makes a new drawing board
  there; a group with no drawing boards yet gets its first from the Groups tab.
- Each group in the peer's **Groups** tab lists its **Drawing boards**, with **Open**,
  and **＋ Drawing board** where you may edit.

Both open the same editor, under a **Remote drawing board** banner that names the peer
and the group and says whether you can edit. The editor reads the drawing board every
second instead of keeping a live socket open. Your edits are authored
`operator@INSTANCE` on the peer. Undo, invites, presence, deleting the drawing board
and its rendered snapshot stay with the peer's own dashboard.

The drawing board is read-only here when the group grants only read, when the peer's
operator locked it, or when the peer refused an edit (the change is rolled
back, the banner says so, and the drawing board stays read-only until you reopen it).
A peer's drawing board opens as a tab only, never in an overlay window or a window of
its own. A drawing board the peer deleted or moved out of your
groups says it is no longer shared. When the peer is out of reach, the editor
keeps showing the last copy it read.

## Mapping the fleet

`drawing-board from-fleet <drawing board>` draws the fleet as a map you can talk staffing over.
Each group is a column with a dashed box, and each live agent is a card in it
(an agent in two groups has a card in both),
coloured by what it is doing: green for working, grey for idle, yellow for
waiting on you, red for a failed turn. A card names the agent, its status, and
its open PR or task. `--all` adds offline agents as white, dashed cards, and
`--group` draws one group. The title line carries the counts and the time it
was drawn. The first map goes below whatever the drawing board already holds. Drawing it
again refreshes it where it is, so your marks on it keep pointing at the same
cards: its elements are `fg-…` and `fa-…`, a card whose
agent is gone is removed, and your own notes and arrows stay. Circle an agent,
or draw an arrow from it to another group, and the Steward reads that as you
talking about staffing. The command needs `drawing-boards.write` and `fleet.read`.

`drawing-board fleet-changes <drawing board>` reads your edits back as staffing proposals: a
card dragged to another column or an arrow from a card to another column
(move), a deleted card (remove), and a note written in a column (add). It
changes nothing. The Steward confirms the list with you, asks for what it needs
to act, and draws the map again, which puts the cards back in place. It needs
`drawing-boards.read` and `fleet.read`.

## Sketching a process

`drawing-board from-process <drawing board> <template>` draws a process template as a
flowchart, so a process can be talked through on a drawing board. Each step is a shape
coloured by who performs it: yellow for human, blue for agent, green for
program. Decisions are diamonds, and start and end are ellipses. Each outcome
is an arrow, and an outcome that loops back (rework) goes around beneath the
steps. A step's outcomes are labelled when it has more than one. The drawing board is
created if it does not exist. A drawing board holds one process sketch. Steps are
named `p-<step>` and outcomes `pa-<from>-<outcome>-<to>`. Drawing again, with
the same template or another, replaces the sketch: steps are updated in place
and any element the new sketch lacks is deleted. Elements you added yourself
are kept. The command needs `drawing-boards.write` and `process.templates.read`.

Changing the process itself remains a process-template edit (see the
process-templates skill). The drawing board is where the operator and agents agree on
the change.

## Presence

Who is on a drawing board is shown live, without chat. An agent that read the drawing board
in the last 30 seconds is *looking*. An agent whose edit landed in the last 6
seconds is *drawing*. The editor marks both on the agent's chip and shows
"… is drawing" over the canvas.

The open editor also reports the part of the drawing board on the operator's screen.
`drawing-board show` prints it ("The operator is looking at x=… y=…"), and
`drawing-board show --view` lists only the elements in it, which is what the operator
means by "this" or "here". The view is dropped when the operator looks away or
after 2 minutes without an update. Presence is held in memory and is not
stored.

## Storage

Drawing boards, elements, participants, drawing board sessions and undo steps are in the
`collab_drawing_boards`, `collab_drawing_board_elements`, `collab_drawing_board_participants`,
`collab_drawing_board_sessions` and `collab_drawing_board_history` tables. The latest PNG
snapshot is `~/.tclaude/data/drawing-boards/<id>.png` and the event images are under
`~/.tclaude/data/drawing-boards/<id>.regions/`, all private daemon state.

## HTTP routes

The dashboard uses `/api/drawing-boards` (list, create, read, patch, delete, snapshot
upload and download, participants, events, view, group and undo), and
`DELETE /api/drawing-boards/{id}/sessions` to end a drawing board's sessions. A federation
peer's dashboard view reaches only the list, create, read, patch, title and
group routes, for the drawing boards of the groups it was granted (see
[A peer's drawing boards](#a-peers-drawing-boards)). Agents use `/v1/drawing-boards`: `GET /v1/drawing-boards`, `POST /v1/drawing-boards`,
`GET /v1/drawing-boards/{id}` (`?since=N`), `PATCH /v1/drawing-boards/{id}`,
`GET /v1/drawing-boards/{id}/snapshot`, and `POST` and `DELETE /v1/drawing-boards/{id}/session`
for the caller's own session. The snapshot response carries `X-Drawing-board-Snapshot-Version` and `X-Drawing-board-Version`
headers. A snapshot upload for a version older than the stored snapshot is
refused with 409.
