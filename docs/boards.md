# Collab boards

A collab board is a shared drawing board for the operator and agents. The
operator draws in the dashboard's board editor; agents read and change the same
board with `tclaude agent board`. Boards live in the daemon's SQLite database,
and their elements are a subset of Excalidraw's element JSON, so a board can be
exported as an `.excalidraw` file.

## Permissions

| Slug | Allows |
|---|---|
| `boards.read` | `board ls`, `board show`, `board snapshot` |
| `boards.write` | `board create` and every change (`add`, `patch`, `rm`, `move`, `text`) |

Neither is default-granted:

```bash
tclaude agent permissions grant <agent> boards.read
tclaude agent permissions grant <agent> boards.write
```

An agent that changes a board becomes one of its participants. The operator
can also add or remove participants from the dashboard.

## Agent commands

```bash
tclaude agent board ls
tclaude agent board create plan --title "Release plan"
tclaude agent board show plan                 # one line per live element, plus the version
tclaude agent board show plan --since 12      # only what changed after version 12, deletions included
tclaude agent board show plan --json          # the elements as Excalidraw JSON
tclaude agent board show plan --view          # only what the operator has on screen
tclaude agent board snapshot plan -o plan.png # the dashboard's latest rendering
tclaude agent board snapshot plan --region 3 -o change.png  # the image a board event named

tclaude agent board add plan rectangle --x 100 --y 80 --w 160 --h 80 --text API
tclaude agent board add plan text --x 100 --y 200 --text "Owned by the platform team"
tclaude agent board add plan arrow --from <elemA> --to <elemB>
tclaude agent board add plan rectangle --right-of api --link --text DB --id db   # beside api, with an arrow from it
tclaude agent board move plan <elem> --x 300 --y 80   # its label and bound arrows follow
tclaude agent board layout plan [--direction down]    # arrange the shapes along their arrows
tclaude agent board text plan <elem> --text "API v2"
tclaude agent board rm plan <elem> [<elem>...]
tclaude agent board patch plan --file elements.json   # - reads stdin
```

`show` prints the board version; pass it as `--since` next time to read only
the changes. `snapshot` says how far the PNG lags the board, because the
dashboard renders it and an agent's own changes appear in it only after the
operator's editor saves a new one.

## Elements and merging

The server keeps only the element keys it knows and bounds each of them:
`id`, `type` (`rectangle`, `ellipse`, `diamond`, `arrow`, `line`, `freedraw`,
`text`), geometry (`x`, `y`, `width`, `height`, `angle`, `points`), style
(`strokeColor`, `backgroundColor`, `fillStyle`, `strokeWidth`, `strokeStyle`,
`roughness`, `opacity`), text (`text`, `fontSize`, `textAlign`), arrow
bindings (`startBinding`, `endBinding`), and `version`, `versionNonce`,
`isDeleted` and `updated`. `customData.author` is always set by the server: to
`operator` for the dashboard, and to the calling agent's id for an agent.
`version` is at most 2^53-1 and `versionNonce` a 32-bit integer. A board holds
at most 5000 live elements and 20000 stored ones (tombstones included), and
there are at most 500 boards. A patch that breaks a limit changes nothing.

Only `text` elements carry text. A shape's label is a text element inside it,
so `board add <shape> --text ...` adds a second, centred text element
(`<id>-label`); move it along with the shape. Elements are returned in creation
order, which is also their stacking order, and an edit keeps an element's
place.

Each element merges on its own, as Excalidraw reconciles: an incoming element
replaces the stored one when there is none, when its `version` is higher, or
when the versions are equal and its `versionNonce` is lower. To change an
element, send it with `version + 1`; `move`, `text` and `rm` do this for you.
A deletion is a tombstone (`isDeleted: true`) that stays so it can sync.

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
message per board every 3 seconds.

The mail usually also names a numbered image of the change:
`board snapshot <id> --region <n> -o change.png`. The dashboard renders the
area the operator touched, padded with some of what is around it. The daemon
keeps the newest 20 of these per board; an older number answers `404
no_region`, and the full `snapshot` is the fallback.

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

Boards, elements and participants are in the `collab_boards`,
`collab_board_elements` and `collab_board_participants` tables. The latest PNG
snapshot is `~/.tclaude/data/boards/<id>.png` and the event images are under
`~/.tclaude/data/boards/<id>.regions/`, all private daemon state.

## HTTP routes

The dashboard uses `/api/boards` (list, create, read, patch, delete, snapshot
upload and download, participants and events). A federation peer's dashboard
view never reaches these routes: boards stay with their own node. Agents use
`/v1/boards`: `GET /v1/boards`, `POST /v1/boards`, `GET /v1/boards/{id}`
(`?since=N`), `PATCH /v1/boards/{id}` and `GET /v1/boards/{id}/snapshot`. The
snapshot response carries `X-Board-Snapshot-Version` and `X-Board-Version`
headers. A snapshot upload for a version older than the stored snapshot is
refused with 409.
