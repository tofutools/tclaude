# Companion and Steward

The **companion** is a floating window that keeps agents' terminals with you on
every dashboard tab. The **Steward** is a helper agent that lives there: ask it
what you can do, how things are set up, or to get work started.

## Turning Collab on

The companion, the Steward, [boards](boards.md), the fleet brief and decisions
are experimental and off by default. Turn them on with **Collab** under
[Config › Experimental](dashboard-config.md#experimental), or `features.collab` in
`config.json`. You don't need to restart anything; the next dashboard refresh picks
it up.

| With Collab | Off | On |
|---|---|---|
| Tab name | Terminals | Collab |
| Companion and its shortcuts | Absent | Available |
| Access banner's **Review** | Opens Messages | Opens the [approval panel](#answering-permission-requests) |
| Boards, Config › Steward | Hidden | Shown |
| `tclaude agent board`, `fleet`, `guide` | Refused with `collab_disabled` | Work |
| `notify-human --option` | Refused with `collab_disabled` | Sends a [decision](#decisions) |

The companion isn't available in a peer's per-node view or in pop-out windows.

## Using the companion

Each pinned agent shows as a pill at the bottom right, with its status. With
nothing pinned, you'll see a **✦ Steward** pill. A pill can show:

- **needs you**: a permission prompt or question is waiting.
- **🔑 approval**: the agent asked for permission.
- **decide**: the agent asked you to [choose](#decisions).

Hover a pill to peek at the agent's last lines. It's read-only, and your keyboard
stays with the page.

Click a pill, or press **Ctrl+.** anywhere, to open the window. It stays above the
page without blocking it, and comes back when you reload the tab.

- **Move it** by its title bar. Resize it from a corner.
- **Snap it** by dragging against the left, right or bottom edge to make a side
  panel or drawer.
- **Each agent is a tab**, up to eight. **＋** summons the Steward or pins another
  live agent, and **×** unpins one. The agent keeps running.
- **Scribes and Stewards** get a tab and a pill by themselves. One you close stays
  closed until you bring it back with **＋**.

An offline agent's tab has **Resume**. A tab that moved elsewhere has **Bring it
back here**.

### Title bar buttons

| Button | What it does |
|---|---|
| 🔈 / 🔊 | Turn [voice](#voice-and-dictation) on or off for this agent |
| ⤢ | Move it to the Collab tab |
| 🔑 N | Open the [approval panel](#answering-permission-requests) |
| 🛡 | Edit permissions (Steward only) |
| sudo | Grant sudo (Steward only) |
| ✉ | Send a queued message (Ctrl/Cmd+M) |
| ⧉ | Detach it into its own browser window |
| ⇄ | Morph it, with Move, Teleport, Clone and Recall home a pick away |
| – | Minimize (Ctrl+.) |

### Talking to an agent

Type in its terminal, just like the Collab tab. You get the harness's own
streaming, prompts and Esc interrupts.

The row under the terminal holds **view**, the mic **🎤** and **✎**. **view** is
ticked by default and attaches the page you're on to dictated messages, so the
agent knows what you're looking at. A text field appears while you dictate.

## Answering permission requests

When an agent asks for access, click **🔑 approval** on its pill, **🔑** in the title
bar, **🔑** on its terminal tab, or **Review** on the access banner. A panel titled
**🔑 Waiting for your OK** opens at the right, over whatever you're doing. **🔑 N to
approve** covers agents with no pill.

Each card has a countdown, **+5 min**, **Deny** and **Approve** (or **Approve
plan**). At zero it declines by itself. **Always allow? Open in Messages** takes
you to the full card. Requests that need a closer look, such as those from peers,
are collected in a link to [Messages](dashboard-messages.md#access-requests). Esc
closes the panel.

## Decisions

When an agent asks you to choose (`tclaude agent notify-human --option`, two to
six options), its tab shows the question, any mock, and a button per option.
The agent's recommendation has a **★**. You can **Add a note (optional)**. Your
choice goes back to the agent as mail, even if it's offline.

Decisions from every agent also wait in one queue: a **◆ N to decide** pill, and a
tab of the same name in the window. The same buttons are in the
[Messages](dashboard-messages.md) reader, without the note.

## Voice and dictation

**Voice.** **🔈** turns voice on for that agent's tab (**🔊**). When its turn ends,
its last message is read aloud. Code is skipped, tables are dropped, links become
their text, and long replies are cut short. It also says when the agent needs you,
and reads decisions with their options. Typing in the terminal stops speech. It uses
your browser's built-in voices. It reads the agent's transcript, so it doesn't work
on OpenCode.

**Dictation.** **🎤** dictates into a draft. Pause and say "send" on its own to send
it, or press the button again to stop and edit. Typing in the draft also stops it.
No reply is read while the mic is open. It uses your browser's speech recognition,
and in Chrome the audio is recognized by Google's service. The mic is hidden in
browsers without it.

## Marking up a page

**✎**, or **Ctrl+Shift+.**, puts a drawing layer over the page. Circle, underline or
draw an arrow at what you mean, add a note, and press **Send**. The agent can't see
your screen, so each mark goes as words: what it is and which control is under it,
such as `1. circled "Approve plan" (button)`, along with the page you're on. **Esc**
cancels and **Ctrl+Z** undoes the last stroke.

## The Steward

The Steward is an agent for all of tclaude. Summon it by clicking **✦ Steward**,
then **Summon the Steward**. It lives in the `steward` scribe group. Ask it what you
can do on a page, how your groups are configured, or to help set up groups, profiles,
processes or your sandbox.

### What to ask it

- **"Brief me."** It tells you what needs you, most urgent first, and can open the
  agent that's waiting in your companion window.
- **An idea.** It restates the idea, asks what it can't find out itself, writes a
  brief, then starts a worker or a task force once you approve. It follows the work
  through to presented PRs and puts taste calls to you as [decisions](#decisions).
- **Setup.** It inspects first, asks only what it can't infer, shows a numbered plan,
  and asks for the permissions for the whole plan once, for a limited time.

It follows the bundled `tclaude-fleet` and `tclaude-setup` skills. After upgrading,
run `tclaude setup --install-agent-skills` to install new skills.

The Steward can also point at the dashboard. It can see which view you're on,
switch you to another, ring a control with a note, mark things up, and open an agent
in your companion window. A cue only points. It never presses anything, and it
can't mark approval or decision controls. It plays once and fades after about 14
seconds. While you're typing or dictating, a hand-over only pins the agent, so your
words still go where you meant them.

### What the Steward may do

Set this under [Config › Steward](dashboard-config.md#steward). The daemon grants it
when a Steward is summoned, and a running one keeps what it started with, so retire
it to apply a change.

| Setting | Options |
|---|---|
| **Reads** | **Operator level (default)**: fleet, boards, the dashboard view, processes, triggers, usage, costs, host and federation activity. **Fleet and boards only**: just those. Both list every agent and group. |
| **Writes** | **Message agents (default)**: can reach any agent. **Boards and notifying you only**. **Message agents and start workers**: can also spawn and set task links. |
| **Default permissions** | Extra permissions a new Steward starts with. `permissions.grant` and `permissions.revoke` aren't allowed. |

Every level can draw on boards, point at the dashboard and notify you. Anything else
it asks for with `--ask-human`, and you approve it in the dashboard. **Message
agents** also reaches agents on linked instances that accept mail, so use **Boards
and notifying you only** to rule that out.

To change a running Steward's access, use **🛡** (permissions) and **sudo** in its
title bar. They need it to be running.

While an agent in the window holds elevated access, its tab and pill show **🔓**, and
a band above its terminal lists what it holds. **End now** revokes what the agent
holds itself, with no confirmation; it can ask again. Access from a group or from
owning one is listed but stays, since other members share it. Change that in Groups.

### Other Steward settings

- **Spawn profile**: what a new Steward launches with. Unset, it uses the global
  default profile.
- **Name**: what you call it, up to 32 characters. The companion shows it, and agents
  can message it by that name as well as by `steward`.
- **Startup prompt**: your own instructions, up to 10,000 bytes, added to what every
  new Steward is told.

These settings can only change on this node's Config tab. Agents and linked instances
can't read or write them, and config bundles don't carry them.

### Messaging the Steward

An agent can answer a message from the Steward with `tclaude agent reply <id>`,
whatever its groups and permissions. Writing to it first, with `tclaude agent message
steward`, needs `message.direct` or a shared group, like any other agent. With Collab
on, `steward` and your chosen name always mean the Steward, even if another agent has
that name.

## Commands behind this

**`tclaude agent fleet`** is the brief. It lists agents in every group, including
ungrouped ones, with status, task link and presented PRs, then ranks what needs you:
approvals, permission prompts, questions, failed turns, agents being recovered,
presented PRs, unread messages, and agents idle for ten minutes or more. Offline
agents are counted, and `--all` lists them. `--group <name>` narrows it, and `--json`
gives the raw brief. It needs `fleet.read`.

**`tclaude agent guide`** drives the pointing. `guide view` needs
`dashboard.view.read`. `guide show` needs `dashboard.guide`:

```bash
tclaude agent guide show --tab <tab> --target "<visible text>" --note "<note>"
tclaude agent guide show --path <path> --circle "<text>" --underline "<text>" \
  --arrow "<from> -> <to>"
tclaude agent guide show --open <agent>
```

With Collab on, `fleet.read` also lifts the group scoping on `tclaude agent ls`
and shows group default directories in `tclaude agent groups ls`, so the
Steward can name any agent or group you have open. It changes only what is
listed: who it can message is still decided at send, and group environment
values stay with you.

A cue holds up to six marks. These slugs and `fleet.read` are given to a Steward when
it's summoned, and aren't granted to other agents by default.

### Capture

`tclaude agent guide capture -o view.png` shows the Steward what
you see, with `dashboard.capture`, which it gets when summoned and other agents
do not. It saves an image of your dashboard and prints the cursor position and
what is under it, the focused control, any selected text, the viewport, and the
open board with the region on screen. `--json` gives the same as data.

The tab renders its own page, so a capture holds only that dashboard page,
never another window or your desktop, and needs no browser prompt. Terminals are
rendered as their real text. Every capture shows you a notice naming the agent,
which is left out of the image. With several tabs open it takes the focused
one, else the visible one heard from last. A hidden tab is never captured,
because you would not see the notice, so the command then fails and says so; it
also fails when no dashboard is open. The daemon stores a capture only when
asked to with `--save`.

A capture shows whatever is on screen, including open messages and terminal
output, which reaches the Steward even though it cannot read agents' mail
through the API. Password fields are masked. Hover styles are not reproduced,
and the cursor is drawn as a marker. An iframe shows as a labelled box. It works
in Chrome, Edge and Firefox; Safari refuses to read back the render, so the
command fails there. It needs Collab, like the rest of the Steward.

### Marks

`tclaude agent guide mark <kind>` lets the Steward draw on your view
to get your attention, with `dashboard.mark`, which it gets when summoned and
other agents do not. A mark is a `highlight`, `circle`, `underline`, `box`,
`arrow`, `freehand` stroke or `label`, on a control named by visible text
(`--target`) or selector (`--ref`), or on a screen region (`--region`). `--blink`
pulses it, unless your system asks for reduced motion. Marks follow their
control as the page scrolls and resizes; a region follows the page's scroll.
They never take a click: only the buttons on a mark's tip, and **Dismiss all
marks**, respond to the pointer, and clicks on the rest of a tip reach the page
under it. Neither a mark nor its tip lands on an approval control.

Every mark has a tip with **✓** (got it), **×** (dismiss) and **Reply**. Reply
sends your answer to the agent as mail naming the mark and its tip, so you need
not say which one you mean, and the mark stays up. Each mark times out on its
own: 60 seconds by default, 10 minutes at most. At most 10 marks are up at once,
and a tip is at most 280 characters; past a limit the request is refused. A
tab shows marks only while on screen.

Marks are stored, so `tclaude agent guide marks` (paged, with `--state`) lists
active, expired, dismissed, acknowledged, cleared and replaced marks, with who
ended each and how. The newest 1000 ended marks are kept. Captures show marks
and report whether each was drawn, so the Steward can confirm one landed.

### Saved captures

`guide capture --save` keeps a capture on this machine
under `~/.tclaude/data/captures`, and `guide captures` and `guide captures get
<id>` read it back, so the Steward can recover what has been going on, for
example after compaction. The newest 50 are kept, up to 256 MiB in all; the
oldest go first.
