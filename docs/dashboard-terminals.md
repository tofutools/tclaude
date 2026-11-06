# Terminals and Collab

This tab is where you work inside agents' sessions from the browser. It is
called **Collab**, and [boards](boards.md) open as tabs here too. It is always shown,
and its badge reads `N +M`: N open terminals plus M sessions running in the
background.

A web terminal is the real thing, attached to the agent's tmux session. There is
no read-only mode here: what you type lands in the agent's session.

## The home

With no tab open on the strip, the Collab view says **Nothing open**. It offers
**Open side panel**, where everything is listed, and it names the agents that
need you: waiting on an approval or on input. Click one to focus it. With none
waiting, it says **No agents need you.**

A group shell that was never started is not listed in Collab. Start it from
its group on the [Groups](dashboard-groups.md#resources) tab.

## The side panel

The right side panel lists what you can open here, by group. It is the same
panel and handle as the palette on the Groups tab. It starts folded: open it with the
handle on the right edge. Each tab remembers its own panel's state.

While a panel is folded, resting the pointer at the right edge for a moment
(within about 32 pixels of the page's edge, or on the page's scrollbar; no
button held) floats it in over the page, on this tab and on Groups. The
scrollbar beside it counts as part of it. It goes again shortly after the
pointer leaves it, after the same delay as a popover, but not while something
in it has focus, one of its menus is open, or you are dragging its scrollbar;
**Esc** sends it away. Its handle still
unfolds it for good.

A pointer flung at the edge peeks the panel's leading edge in before it gets
there. The peek widens as the pointer closes in. Moving onto it, crossing
where the open panel's edge would be, or stopping there reveals the panel. A
motion that brakes, as one aimed at something on the page does, lets the peek
shrink back and go. A motion that starts close to the panel is left to the
edge zone. The closer to the panel a motion starts, the less speed it needs.

To turn this off, uncheck **Side panel at the edge** on the Config tab
(`dashboard.side_panel_edge_reveal`). The peek and its growth have switches of
their own there, and **Advanced** holds the values that tune them. None of
this does anything on touch screens.

- **Filter ▾** picks what is listed. Tick states (Needs you, Working, Idle,
  Background, Open, Closed) and kinds (Agents, Shells, Boards, Pages,
  Processes). By default everything but Closed is listed. The button shows a
  count of the boxes changed from the default, and **Reset** puts them back.
  The browser remembers the filter. A search finds everything, whatever the
  filter is. It also counts the [background sessions](#background-sessions).
- **＋ New ▾** makes something: **Agent…** (the spawn dialog), **Shell…** and
  **Page…** (pick the group), **Board…** (the board list: open one, or create
  one), **Process…** (the process list, with Processes on) and **Import
  board…** (an `.excalidraw` file). Each group's header has a **＋** that opens
  the same menu for that group: a **Shell** in its directory, a **Board** and a
  **Page** in the group, and an agent spawned into it.
- In each group, the agents come first, in a fixed order: needs you, working
  (green light), idle (amber), then by name. Closed items come last.
- A group with an agent that needs you comes first, then groups with something
  open here.
- Each agent goes under one group: the group it was spawned into, or else its
  first group. Agents in no group are under **Not in a group**.
- Click a group's header to fold it. The panel remembers which groups are
  folded.
- Click an item to open it, or bring it forward where it already is; a closed
  page reopens. Drag it onto the tab strip or the home to open it as a tab, or
  anywhere else to float it in a window. A terminal that cannot float stays a
  tab.
- Drag an agent, board or page onto another group's section in the panel to
  move it there, as you would in the Groups tab. An agent goes through the same
  confirm as there. To take an agent or a board out of every group, drop it on
  **Not in a group**. A page always stays in a group, and only moves to a group
  its agent is in.
- Hover a tab to pop it out or close it, and hover a window to minimize it or
  close it.
- **⋯** on a row, or a right click, opens its menu: **Attach** or **Open**,
  **Kill session…** for a background session or a group's shell, and on a
  board **Download .excalidraw**, **End agents' sessions…** and **Delete
  board…**. Each destructive one asks first.
- A board, page or process row has a lock at its right end: an amber closed
  lock, always shown, while agents can only read it, and a dim open lock on
  hover or keyboard focus while they can edit it. Click it to lock or unlock;
  see [Groups › Resources](dashboard-groups.md#resources). Agent rows have none.

## Opening a terminal

- **From [Groups](dashboard-groups.md#opening-a-terminal):** the focus icon on a
  row, `terminals & windows…` in its ⚙ menu, or click its directory.
- **From the group:** `🖥 open web terminal` in the group's ⚙ menu, or Ctrl-click (⌘-click on a Mac)
  its default directory.
- **From the keyboard:** Ctrl K (⌘ K on a Mac), then `terminal <group>` or `shell <group>`.
- **In the background:** Ctrl-click (⌘-click on a Mac) opens a pane without switching to it.

## Working with tabs

Each tab shows a live status glyph and, for agents, a **⚙** with the same menu as
the agent's ⚙ in Groups, so you can morph, move or clone without leaving the
terminal. A `!` on a tab opens the agent's newest unread notification. With a
pending permission request, a **🔑** opens the
[approval panel](companion.md#answering-permission-requests).

- **Reorder** by dragging. Drag one tab onto another to make a named, collapsible
  group. Like the tabs in them, groups belong to their browser window and end
  with it, so a reload starts without them.
- **Right-click a tab** (or Shift+F10) for `New group from this tab`, `Add to
  "<group>"`, `Remove from group`, `Open in separate window`, `Close tab`, `Close other tabs`
  and `Close all tabs`. Boards, directory shells, group shells and running local agents add
  `Open in overlay`.
- **Right-click a group** for `Expand group` or `Collapse group`, `Rename group…`,
  `Ungroup tabs` and `Close tabs in group`. Double-click or press F2 to rename.
- **Move** a terminal between three places, one at a time: a tab, an overlay window
  and a separate browser window. Each place offers the other two. A tab has **Open in
  overlay** and **Open in separate window**, an overlay window has **Open in the Collab
  tab** and **Open in separate window**, and a separate window has **Open in the Collab
  tab** and **Open in overlay**. Every
  overlay tab's right-click menu has the same two. Dragging a separate window's header
  back works like its button back to the dashboard. A separate window offers **Open in overlay** only while the dashboard that
  opened it is there to take it, and a terminal that can't float any more lands in a tab.
- **Drag a tab off the strip** to float it where you drop it. Once clear of the strip,
  the snap targets an overlay window's drag shows appear: let go on one, a room edge, an
  empty cell's box or a window's marker, and it snaps there instead. What can't float opens in
  a separate window instead: a peer's terminal and an offline agent. Drop it on an overlay window's title bar instead to join that window,
  where a marker shows. Overlay tabs drag back the same way, onto this strip or onto
  **Collab** in the main nav.
- **Float** a directory or group shell with **Open in overlay**. It
  moves, snaps and splits like a board's window, on the same tmux session, and
  **Open in the Collab tab** returns it. Closing the window leaves the shell running. A peer's
  terminal stays a tab.
- **Float** a running local agent with **Open in overlay**. It moves to the
  same chat window a tab dragged off the companion gets, and leaves the Collab tab: a
  terminal is in one place at a time. **⚙ > Open in the Collab tab**, or the same item on
  its tab's menu, returns it to the Collab tab.
- **`$`** after the last tab shows or hides costs next to agent names. It works
  separately from the one in Groups. Green is API spend, and muted italic `≈$…` is
  a subscription what-if estimate. A tab's tooltip always has its cost.

**⌨** in an agent terminal's floating tools, in this tab and in the companion, opens a
shell tab where the agent is working now: its worktree root, the directory its row shows.
Its tooltip names the directory. This is the same shell a click on that directory opens,
with the same group environment. It runs as you, outside the agent's sandbox, and the
agent can write to that directory, so look before running something it left there. On a
peer's agent the button needs `sessions.shell` on one of the agent's groups, and that peer
showing you the group's members, and is hidden otherwise; a restricted peer does not share
the directory, so the tooltip says where in words. A plain shell has no ⌨, and neither does a pop-out, which has no tabs to open
one in.

**Zoom** a terminal by pinching or Ctrl+scrolling over it or its floating tools, or with **−**, **+** and
**⟲** (reset) in the floating tools in its upper right corner, which show the current
size. The page stays as it is. Each terminal keeps its size, separately in a Terminals
tab and in the companion, across reloads. A zoom changes the rows and columns once the
gesture ends, so the harness redraws to the new size, and a session shared with other
viewers fits the smallest of them. A remote terminal normally fits its font to the
peer's grid; a zoom replaces that fit, scrolling if the grid no longer fits, and reset
returns to it.

A browser attach takes the session over from any other web view. Opening an agent
in the companion closes its pane here, and moving it here parks the companion's
tab until you bring it back.

## Sending things to an agent

- **✉ Message** (Ctrl M) queues a message instead of typing into the pane.
  It has an **Insert prompt…** picker for
  [saved prompts](dashboard-library.md#saved-prompts).
- **Files.** Paste them (Ctrl V, or ⌘ V on a Mac) or drag them in. The dashboard uploads them
  next to the session, inside its sandbox if it has one, and types their paths
  into the prompt. Folders are skipped. A plain shell only accepts PNG, JPEG and
  WebP images.

A slow upload shows a progress card with **Cancel** and **▁** to shrink it to a
small pill. For a remote node, the bar tracks bytes reaching this node, then the
hop to the other node with no percentage.

## Copying text

Select with Option-drag on macOS, or Shift-drag on Linux and Windows, to make a
browser selection. Harness copy commands reach your clipboard through OSC 52
after a supported copy gesture, although not every harness command works that way.
Windows created before passthrough support need `tmux -L tclaude set-option -w
-t SESSION:WINDOW allow-passthrough on`, or a new session.

## When a terminal drops

Reconnecting is deliberately careful. The terminal retries a few times, then only
reattaches on its own after a proven `agentd` restart. Otherwise you get a
**Reconnect** button.

When the tmux client goes away, the pane says why: **session ended** (the shell or
agent exited) or **detached** (the session lives on). By default, shell tabs close
when their session ends, because reconnecting would only start a fresh shell.
Agent windows and detached terminals keep their tab. Change this under **Auto-close
tabs** in [Config › Terminals & windows](dashboard-config.md#terminals-windows).

When the last tab closes, the dashboard moves to Groups, unless you turn on
**Stay after last close** in the same section.

## Background sessions

Sessions running with no terminal attached are counted on the side panel, so
idle ones aren't left running by accident: on its **Filter ▾** button, on the
**Background** box in it, and, while the panel is folded, on its handle. The
count is hidden at zero.

The panel lists them, with **Background** ticked, under their agent's group,
each with its idle time (amber after a few hours).

- Click one, or **Attach** in its menu, to open it as a tab. **Attach all
  background** at the bottom of the Filter menu opens every one that can be
  attached, one at a time.
- **Kill session…** in its menu ends it, after asking.
- A board with [board sessions](boards.md#board-sessions) has **End agents'
  sessions…** in its menu, which removes them and leaves the board.
- Sessions the dashboard cannot attach (an earlier generation, a tmux session
  tclaude did not start, or one that is locked) are listed last, under **Other
  sessions**. Their tooltip has the `tmux attach` command to reach them from a
  shell, and their menu can kill them.

With linked nodes, peers' unattached agents and shells are listed too; which ones
depends on your [grants from that peer](fleet-ui.md).

## Terminals on other nodes

A remote terminal has the node's pill on its tab and a chip in its header naming
the peer and the mode, `⌨ interactive` or `👁 watch-only`. A watch-only view takes
no input or files. When someone else is attached you see `Also attached on
<peer>: …` with **Take over**. **Files** in the header browses, previews and
downloads files there.

To open a peer's shell, click a member's directory on that peer's row, or
Ctrl-click (⌘-click on a Mac) its group's default directory. The peer must grant `sessions.shell`
on the group; without it the path is dimmed and its tooltip says why.

## Processes in terminals

A pane whose agent is in a process run shows run icons at its top right and on
its tab. An icon opens the run's mini-graph, with actions to decide, reassign,
promote, pause and resume. See
[Processes](processes.md#in-the-dashboard).

## Dictation

The [companion's](companion.md#voice-and-dictation) **view**, **🎤** and **✎** float at
a terminal's top right, dimmed until the pointer is on them. Switching terminals,
leaving the tab or losing the connection stops the mic; the draft stays with its
terminal.

- **Agent terminals** work like the companion: the draft goes to the agent as a
  message, you can say "send now", and [send after silence](companion.md#voice-and-dictation)
  applies if you turned it on.
- **Plain shells** and shell sessions show only the mic. Enter or **Paste** puts the
  draft at the prompt as one line with no newline, so nothing runs until you press
  Enter in the terminal. Spoken send and send after silence are off.

The wake phrase works here too, for the terminal with focus.
