# Terminals and Collab

This tab is where you work inside agents' sessions from the browser. It is
called **Terminals**. With [Collab](companion.md#turning-collab-on) on it is called
**Collab**, and [boards](boards.md) open as tabs here too. It is always shown,
and its badge reads `N +M`: N open terminals plus M sessions running in the
background.

A web terminal is the real thing, attached to the agent's tmux session. There is
no read-only mode here: what you type lands in the agent's session.

## Opening a terminal

- **From [Groups](dashboard-groups.md#opening-a-terminal):** the focus icon on a
  row, `terminals & windows…` in its ⚙ menu, or click its directory.
- **From the group:** `🖥 open web terminal` in the group's ⚙ menu, or Ctrl/Cmd-click
  its default directory.
- **From the keyboard:** Ctrl/Cmd-K, then `terminal <group>` or `shell <group>`.
- **In the background:** Ctrl/Cmd-click opens a pane without switching to it.

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
  "<group>"`, `Remove from group`, `Detach tab`, `Close tab`, `Close other tabs`
  and `Close all tabs`. Boards, directory shells and group shells add `Open in overlay`.
- **Right-click a group** for `Expand group` or `Collapse group`, `Rename group…`,
  `Ungroup tabs` and `Close tabs in group`. Double-click or press F2 to rename.
- **Detach** a tab by dragging it off the strip, which opens its own browser
  window, or with **⧉ tab** or `Detach tab`, which open it in a new browser tab.
  **↩ dashboard**, or dragging the pop-out's header back, reattaches it.
- **Float** a directory or group shell with **Open in overlay** (needs Collab). It
  moves, snaps and splits like a board's window, on the same tmux session, and
  **Back to a tab** returns it. Closing the window leaves the shell running. A peer's
  terminal stays a tab, and an agent's own terminal floats from the companion.
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

- **✉ Message** (Ctrl/Cmd+M) queues a message instead of typing into the pane.
  It has an **Insert prompt…** picker for
  [saved prompts](dashboard-library.md#saved-prompts).
- **Files.** Paste them (Ctrl/Cmd+V) or drag them in. The dashboard uploads them
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

The counter at the right of the tab strip shows sessions running with no terminal
attached, so idle ones aren't left running by accident. It's hidden at zero. Hover
to peek, or click to pin it open.

Its sections are `Agents`, `Sessions`, `Shells`, `Group shells`, `Boards` and
`Other tmux sessions`. Each row shows the idle time.

- **Attach** (or click the title) opens it as a tab. **Attach all** appears with
  two or more.
- **Kill** ends a shell or a leftover session. It asks again as `Kill?` for three
  seconds.
- **Open** and **End** on a board row open the board, or remove its
  [board sessions](boards.md#board-sessions) and leave it.

With linked nodes, peers' unattached agents and shells appear under each node's
pill; which ones depends on your [grants from that peer](fleet-ui.md).

## Terminals on other nodes

A remote terminal has the node's pill on its tab and a chip in its header naming
the peer and the mode, `⌨ interactive` or `👁 watch-only`. A watch-only view takes
no input or files. When someone else is attached you see `Also attached on
<peer>: …` with **Take over**. **Files** in the header browses, previews and
downloads files there.

To open a peer's shell, click a member's directory on that peer's row, or
Ctrl/Cmd-click its group's default directory. The peer must grant `sessions.shell`
on the group; without it the path is dimmed and its tooltip says why.

## Processes in terminals

With `features.processes` on, a pane whose agent is in a process run shows run
icons at its top right and on its tab. An icon opens the run's mini-graph, with
actions to decide, reassign, promote, pause and resume. See
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
- With Collab off, every terminal works like a plain shell.

The wake phrase works here too, for the terminal with focus.
