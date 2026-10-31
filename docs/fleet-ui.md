# Fleet UI: linked nodes

This page covers the parts of the dashboard you see once this node is linked to
others through [federation](federation.md): switching nodes, seeing several at
once, the fleet map, and **⚙ Fleet config**. To link your first two machines, follow
the [setup walkthrough](federation-setup.md).

A peer only ever shows what it shares with you: the grants it gave you decide what
its cards, views and pages can read or change.

## Node chips and switching

The right end of the tab bar picks the nodes the dashboard shows. With up to
four nodes there is a button for this node (⌂) and one per trusted peer, with a
dot for its presence; past four they fold into one dropdown that names the node
on screen, filters as you type and keeps this node first. Click a node to open
its own dashboard; ⌂ returns to this node. Alt+1 is this node and Alt+2…9 the
peers in order; the shortcut never fires while you type in a field or a web
terminal has focus. The row is always there and adds no height to the page, but
it is greyed out (its tooltip says why: federation is off, or the hub is
unreachable, still connecting or refused the connection) until this node is
connected to its federation hub, and greys out again if that connection drops;
it follows the connection without a reload (within about five seconds while
federation is on; with federation off it rechecks every couple of minutes). Once
connected, ⌂ works with no peers at all; each trusted peer adds a button.
**Fleet** ends the row: the third place you can be, after this node and
another node. It is lit on the fleet views and opens the one you used last
(the map, or ⚙ Fleet config). It is never greyed out on this node: while the
hub is not connected it opens ⚙ Fleet config, where the hub connection and the
first peer are set up. Its dot is lit while connected. On a node that was never
set up it has a **set up** pill until you have opened the setup guide once. In a peer's view it opens the map,
and hides while there is none.
**nodes ▾** stays greyed out until the hub connects; with no linked node yet it
opens with a "No linked nodes yet" note. ⌂ always
works where it leads somewhere: home from a peer's view, and back from the
fleet views.

## Fused nodes

**nodes ▾**, after the node buttons, shows several nodes together: tick the
nodes (all by default), and **Groups** lists the ticked nodes' groups as one
tree (see [Groups on several nodes](#groups-on-several-nodes)),
**Terminals** shows the open terminals on the ticked nodes, a peer's labelled
with its node — its initial on the tab, the full name in the pane header — while
this node's go unlabelled (the others stay open, out of sight), and **Messages**
their mail (see [Messages on several nodes](#messages-on-several-nodes)); the
button reads e.g. `2/3`. Clicking a node's name (or its **only**) shows that node
alone, as does unticking all but one. A fused view is always this node's page:
the peers in it show what they share with you, so this node's own groups and
controls work as on its own view. Fusing from a peer's page goes to this
node's page, and so does an address that names both a peer and a fused set.
Every other tab shows this node, says so in a strip at the top, and a peer's
own view opens from its node button. The header's quota bars show one node at
a time, because quota percentages do not add up across machines. A node pill
before them names whose they are (this node by default), and its menu lists
each ticked node's peak quota. A node that does not share usage
(`costs.read`) is locked there, with the reason. **Usage** opens with a per-node summary: each
ticked node's spend today and this month, and its peak quota. The spend is
totalled across the nodes that share it; quota is shown per node only. The
views below it show this node.
**Automations** lists this node's jobs, then each ticked peer's under its
node pill, with the same filter, kind and page. A peer that trusts you
unrestricted shares its list, and its cron jobs and standing orders run,
pause and delete in place through its routes. Editing one opens on that
node's own view. A peer's exports stay on that node, and a peer that shares
less is locked, with the reason.
**Diagnostics › Audit** gets a **node** filter: this node, one ticked peer
(paged through its routes, under `federation.audit.read`), or **all nodes**,
which merges each node's newest page newest first, with a node pill on each
row and the nodes that withhold theirs shown locked. **Logs** and
**Performance** are one daemon's own, so they get a **node** switch instead:
this node or one ticked peer, read through its routes when it trusts this
node unrestricted (anything less shows why it is refused). On **Config** and **Access**, one
machine's own settings, the strip also names each ticked peer. An
unrestricted peer's Access opens on its own view from there. A peer's Config
is always set on that node itself, so it is shown locked, with the reason.
**Plugins** and **Processes** keep each node's set intact: this node's as
before, then a section per ticked peer under its node pill. A peer that
trusts you unrestricted shows its plugins (check, run, install from its
catalog, delete, all on that node) and its process templates (delete there;
open and edit on its own view). A peer that shares less is collapsed and
locked, with the reason. The fused set is part of the address
(`?nodes=all`, or the ticked instance IDs), so a reload keeps it.

## The map

**Fleet** at the end of the node row opens the fleet views, and the tab
strip turns into their switch: **Map** and **⚙ Fleet config** (**⚙ Fleet config**
alone until a hub is connected). The right end stays the same, with Fleet lit.
`[` / `]` step back to the tab you came from.
Each ⚙ Fleet config section has its own address (`/fleet-admin/peers`,
`/fleet-admin/grants`, …) and its own history entry, so Back/Forward step
between sections and a section can be bookmarked. Peer grants narrowed to
one peer or pool is `/fleet-admin/grants/<id>`. Bare `/fleet-admin` opens
the setup guide until setup is done, else Peers. The map's shortcuts into
Fleet config (⚙, grants…, trust…, Hub page…) are links, so a middle-click
opens the section in a new browser tab.

The map draws this node and each peer as a card joined by link edges (solid:
reachable; dashed: unreachable or stale). A card shows the shared agents and
how many are online, agents waiting for input, cpu/mem/disk, the tclaude
version (↑ when a newer release is known) with its health, and how many
features the peer does not share with you (hover for the list). **Open
dashboard** opens that node's Groups page. Cards are polled only while the map
is on screen; an unreachable peer keeps its last data, marked with its age.

The toolbar above the map holds **Show hub** (remembered per browser; off, the
hub card goes and links run node to node, with the hub's state in the toolbar)
and zoom: scroll or pinch to zoom around the pointer, drag the background to
pan, or use −, the percentage (back to 100%), + and fit. A map larger than the
window fits itself until you zoom or pan. Phone-width screens keep the stacked
layout without zoom. At its right end, **💰 fleet limits** (**💰 fleet N%** once limits exist) reads this node's fleet usage limits and
opens them (see [Fleet limits](usage-and-costs.md#fleet-limits-spend-across-peers)).

Cards link to where their settings live: ⚙ opens the peer in Fleet config ›
Peers, and beside **You trust it** `trust…` opens Peers and, while the peer is
restricted, `grants…` opens Peer grants on that peer. The hub card says whether
this node is a hub admin and opens Fleet config → Hub. A peer view of the map
shows no such links: Fleet config lives on this node's own page.

Trust between two nodes can differ by direction, so the map always names both.
A peer card shows **You trust it** (what the peer may do on this node; you set
it) and **It trusts you** (what this node may do on the peer; its operator sets
it). Each link's label shows both directions on one line, such as `→ limited
← ⚠ full access`: → is what this node may do on the peer, ← what the peer may
do here (hover a direction for the node names). Click it for a plain-language
explanation and a shortcut to Fleet config. A peer on an older tclaude does not
report its side ("not reported").

**Show untrusted** (in the legend) adds the instances visible on the hub that
this node has not trusted, as dotted cards with their self-reported name,
instance ID and fingerprint. **Trust…** opens the same dialog as ⚙ Fleet
config → Peers: compare the full fingerprint with that node's operator, pick a
label and a trust level, and confirm. The box is remembered per browser.
Trusting an instance from the map also asks it to trust this node back.

An instance that asked to be trusted shows on the map even with the box off,
as an amber card first in the untrusted section with its note (its own words)
and whether it already trusts this node; the top bar counts the requests.
**Dismiss** forgets a request. **Ask to be trusted…** on any other untrusted
card sends this node's own request, with an optional note.

To update a node: click **update…** (or **manage…**) on its card, check the
version and warnings, then **Update to vX…** and confirm. The node replaces its
binaries (keeping the old ones for **Roll back…**) and restarts its daemon;
agent sessions keep running and the dialog follows the job through the
restart. Updating a peer needs its `node.update` grant. CLI:
`tclaude update [--node PEER] --check|--apply|--rollback`.

## A peer's dashboard (remote node view)

Opening a peer shows the normal per-node dashboard, served through this
daemon's peer proxy (the URL carries `?node=inst_…`). The peer's name replaces
the title, a thin line in its colour runs along the top, its chip is current,
and a peer-view pill lists what it shares.

Messages shows the peer's mail when it grants you `node.messages.read`, in
the same client as this node's: folders, filters, bulk bar, pager, reader and
attached files (agent mail's and startup briefings' too). Read state on agent
mail needs its `node.messages.manage`. Its
human notifications carry the same buttons as this node's (reply, focus, mark
read/unread, download, delete). Opening one marks it read on the peer, and
mark read/unread and the folder's mark all read go to the peer, when it grants
you `node.messages.mark-read`; reply needs its unrestricted trust. Download builds
the Markdown or zip from the peer under `node.messages.read`. Focus opens the
sending agent's terminal here through the federation bridge (as its Groups
row's attach does, so the peer's `sessions.watch` / `sessions.attach` decide),
never a window on the peer's host, and marks the notification read. Deleting
(a message, the selection, delete read) and wiping work when the peer trusts
you unrestricted. A control the peer withholds stays in place, disabled, with
a 🔒 whose hover names what it needs. Its access requests are answered in the
fused Messages view.

A group's quick options — description, default directory (typed: the folder
browser reads this machine), member cap, default profile, sandbox profile and
attachment — its rename, and the ⚙ menu's settings dialog, startup context,
notifications, remote policy and nesting work as on this node when the peer
grants `groups.settings` for that group (unrestricted trust holds it), and its
shut down and power on buttons under `groups.members.stop` and
`groups.members.resume` there. The
settings dialog sends only what you changed and disables the fields the peer
keeps. The default directory, default saved prompts and sandbox profile also
need the matching spawn grant, and the
profile pickers the peer's `node.spawn.read` ([Changing a peer's groups](federation-peers.md#changing-a-peers-groups)).

Greyed-out tabs, buttons and menu items are ones the peer does not offer you.
They stay in place so the layout does not jump; hover or click one to see why,
including the grant the peer would have to give you (or "unrestricted trust"
for what no grant opens). The Groups tab shows every group the peer shares
anything in: a roster, presence or mail grant alone shows its members, and
`agents.status.read` adds their activity, model, task and context.
The peer-view pill names both directions of trust and lists what is hidden
from you, with the grant each needs. Usage
and costs say when they are not shared.

If the peer granted you lifecycle actions, its agents' status dot (stop or
wake), restart, sandbox restart, retire and clone controls open the same
dialogs as on the peer itself, sent to the peer. Options the peer keeps for
its own operator stay in place, locked with a 🔒 naming what they need (its
unrestricted trust): deleting the worktree on retire, and a clone's group
ownership and worktree. Recreating a missing launch directory on wake is left
out. A peer that trusts you unrestricted offers them all, as on the peer.
Turning an agent's sandbox off takes unrestricted trust. With a move grant,
the ⚙ **move here…** item opens the regular move dialog, to a group on this
node. CLI: `tclaude federation action <action> --node PEER --agent ID`.

Messaging a peer's group (its ✉, or the palette) opens the regular message
dialog, titled with the node. Under the peer's `message.direct` grant it
sends one message per ticked member, from you, the operator. **From** and the
role filter stay in place, locked with a 🔒 that names what they would need
(its unrestricted trust); hover or focus the 🔒 to read it.

A group's spawn button (and any other way of opening the spawn dialog on a
peer view) opens the regular spawn dialog, aimed at the peer. Every control
stays in place; the ones the peer decides are disabled with the reason on
hover. You always set the name, display role and initial message (the brief);
a profile from the peer's selectable list; and every other launch setting
(directory, harness, model, effort, roles, sandbox profile, permission and
sandbox modes, environment, ownership, harness features, saved prompts,
worktree) only where the peer allows it (an unrestricted peer, which gets the
dialog as a local spawn, or its `groups.members.spawn.*` grants). With
`node.spawn.read` the dialog pre-fills and shows what the peer applies, and
offers the peer's and this node's profiles and saved prompts (see
[The remote spawn dialog](federation-work.md#the-remote-spawn-dialog)). Without a spawn grant the
request waits for the peer's operator, who decides those settings when
approving. The
directory is typed as a path on the peer — the folder browser reads this
machine. After submit, a toast follows the request until the peer decides.

Every dialog follows one rule for what another node keeps. Its operator's
own controls (a member's group ownership and permissions, a clone's ownership
and worktree, retire's worktree deletion) work where that node trusts you
unrestricted and are locked with a 🔒 reason elsewhere. On another node, the
spawn dialog's **Save as profile…** opens the profile editor with a **Save on**
choice: that node (under unrestricted trust or `node.spawn.profiles.create`,
which saves it operator-only) or this one. A group picker's **＋ new
profile…** stays locked there. In the palette, commands that only
unrestricted trust reaches (Snapshots…, Announce to all live agents…) show
disabled with that reason on any other peer. Controls that only
work on this machine (the folder picker, the saved prompts picker in group
settings) are locked with their reason for any other node and on a peer's
page; type the path instead.

The Groups toolbar's global default spawn profile, global sandbox profile and
**+ new group** controls work on a peer view the peer shares them in
(`node.spawn.defaults.manage`, `node.groups.create`; unrestricted trust holds
both), and are greyed with the reason otherwise. See [Federation](federation.md), Managing a peer's node.

In a fused view those three controls act on one of the listed nodes, which a
chip in front of them names in the node's colour. It starts on the page's own
node and follows the node of the group you last act on: a click on its header
or one of its controls, a right-click, or a drop onto it (hovering, or a click
on its plain text or empty space, does not move it). The chip flashes when it
moves, and picking another node in it moves it by hand. Turn off Config ›
Appearance › Header & rows › **Toolbar follows the clicked group**
(`dashboard.fused_toolbar_fixed_node: true`) to stop the following: the
toolbar then moves only when you pick a node in the chip. Another node's controls show and set that
node's defaults and create the group there, as far as it shares them with you
(the same grants as on its page); new profiles are made on their own node. The toolbar also
carries **💰 fleet limits**, this node's fleet usage limits, whichever node it follows.

A peer's agent terminal opens in the browser as a remote terminal (watch-only
or interactive, as the peer's `sessions.*` grants allow; see above).
`tclaude federation attach <agent-id>@<instance-id>` attaches from your own
terminal.

### Requesting access from a peer

Each feature in the pill's "Hidden from you" list has a **request…** link. It asks
the peer's operator for the grant that feature needs: how long it lasts (1
hour to 30 days, or permanent) and a reason. A group permission asked for
without a group covers every group on the peer, including future ones; the
peer's operator may narrow it to one group (the CLI's `--group-id` asks for one
directly). Local-only features such as terminals have no link. The
dialog follows the request until the peer's operator approves or declines it,
or it times out after 5 minutes; an approved grant shows up on the view's next
refresh. A peer only takes requests from nodes it already shares something
with. CLI: `tclaude federation access request --node <peer> --permission <slug>
[--group-id …] [--ttl …] [--reason …]`, then `access status`.

## Groups on several nodes

In a fused view, Groups lists every group on the ticked nodes, named
`group@node` with the node's colour on the suffix. It is the same Groups view
under the same toolbar: the filter, the Both / Agents / Processes lens, and
**▾ view** (offline members, columns, and the virtual groups) apply to every
node's groups, and the filter counts them all. The virtual groups (Ungrouped,
Retired, Conversations, Replaced generations) show per node, named
`Ungrouped@node` and so on: this node's work and page as on its own view,
and a peer that trusts you unrestricted shows its own (their first page, read
through its peer routes). A restricted peer's are not shared. Process runs show on this node's own groups; a peer's runs are
not shared across nodes, so its groups say so instead. On a peer's rows, the
controls that peer shares with you run through its peer routes, exactly as on
its per-node view: the status dot (stop, or wake a stopped agent), restart,
sandbox restart, retire, clone, the group's spawn button (the regular spawn
dialog, aimed at that node, as above), its ✉ message button (one agent at a
time) and the group settings it grants (`groups.settings`, as above: the
quick options, rename and the ⚙ menu's settings items), which go to that
node's own group. Under unrestricted trust a peer's rows also take what no peer
grant covers: advancing a process, re-briefing or standing down a force, group
ownership, the auto-join default, promoting, reinstating, hiding a window,
deleting a past generation, member permissions, sudo, new cron jobs, standing
orders, saving as a template (edited afterwards on that node's own dashboard),
pulling, syncing and cleaning up its repositories and worktrees, and links
(within that node: a link from its group offers no cross-node targets). Each
opens the dialog it opens here, reading and saving on that node. This node's groups work as on its own view; on a peer's
page their group settings still go straight to this node. Everything else stays an overview: click a group's `@node`
suffix to act on it in that node's dashboard. An unreachable node keeps its last rows, marked
stale; a node no longer trusted drops them. The old `/fleet` address opens this
view with every node ticked. CLI: `tclaude agent groups ls --all-nodes`; actions are
`tclaude federation action ... --node NODE`.

An agent that teleported off its home node is marked on both ends, in single-node
and fused views alike:
- **Where it runs now:** its row carries an `away from <home> · hop N` badge in
  the home node's colour.
- **At home:** it leaves a dimmed `away → on <node>` placeholder in each of its
  groups, with no controls. The placeholder is not counted in the group's
  member count; the header shows `+N away` instead. In a fused view, `↓ show`
  scrolls to its live row; in a single-node view, `open on <node> ↗` opens that
  node.
- **Back home:** once it returns, its row shows a `back home` tag for ten
  minutes.

The markers come from the agent's federation presence (`tclaude agent whoami`
shows the same).

To send an agent elsewhere without dragging, use the **send elsewhere** section of
its row's ⚙ menu (single-node and fused views). Each item opens one dialog with
a mode bar: **Move** to a group on another node, **Clone** (here, continuing in
the clone dialog), **Morph** into another spawn profile (a dry run previews the
change and what happens to the history), **Teleport** to another node (its home
keeps a placeholder), and **Recall home** (from the placeholder at home, or ⌂
`⌂ recall home…` where it runs: straight to its origin, retaking its home identity). Another
node's agent offers **move here** and **clone** (on that node), plus **Morph** when that
node trusts this one unrestricted. Dropping an agent
onto another node's group opens the same dialog on Move, pre-filled. A peer's
agent moves here, or, when that peer trusts this node unrestricted, on to a
third node: the peer sends it, and its own ⚙ Fleet config → Moves follows it. In the
fused view a drop onto a group on the agent's own node works as on that node's
Groups view (move or add to a group, retire to the bin, and Ctrl/Cmd-drop to
open the clone dialog for that group) — for this node always, and for a peer that
trusts this one unrestricted. A peer's group controls there (create a subgroup, delete a
stuck pending spawn, and the rest its grants cover) act on that peer; the few that
only its own dashboard runs (the links manager, its messages, sudo) are
greyed out; hover or focus one to see why, with the grant it needs, as on the
peer's own page. Each node's links show on its own groups there, and its triggers and queued spawns
too for this node and peers that trust it unrestricted (a queued spawn launches,
edits and discards on its node). Group headers drag the same way there: nest a
group under another on its node, pull it back out, reorder it (the fused view
keeps its own order), Ctrl/Cmd-drop to clone, or drop it on the bin to delete. Nodes a
mode cannot use stay listed with the reason; the right-hand panel says what
carries over. Until this node is connected to a hub with a linked peer, **move
to…** and **teleport to…** stay in the menu greyed out, saying why (as the
node chips do); connect under ⚙ Fleet config → Hub. CLI: `tclaude federation move-agent`, `tclaude agent clone`,
`tclaude agent morph`, `tclaude agent teleport`.

The right-hand panel lists the shown nodes' profiles, roles, saved prompts,
templates and sandbox profiles, one card per name. Picking another node's copy
on a card acts on that node: **Edit** and **Clone** open the same editors and
save there, and **Rename…** and **Delete** reach it too, while that node trusts
this one unrestricted (locked with the reason otherwise). **Open on NODE**
goes to that node's own dashboard.

Cloning on this node has one entry: **send elsewhere → clone…**, whose
**Clone** mode continues in the clone dialog. The dialog can start the clone in
another worktree, skip the conversation copy, and also copy the source's group
ownerships (**Copy group ownership**, off by default).
Ctrl/⌘-dragging an agent onto a group clones it into that group in one
request: the clone and its new membership land together or not at all. In a
peer's single-node view, the ⚙ **clone** item asks the peer instead.

## Messages on several nodes

In a fused view, Messages shows the ticked nodes' mail with the same panes and
controls as one node's Messages: the folder and message filters, the bulk bar,
the pager and page size, the reader's headers, the footer toggles (retired,
empty and previous-generation agents) and the resizable panes. The node chips
above the folders narrow everything to one node (**All nodes** shows them all
again); a chip carries the node's unread notifications and its state when it
is not answering. **All agent messages**, **Human notifications** and **Access
requests** span the nodes in view, newest first (access requests oldest
first), each row carrying its node's pill. Under them each node has a foldable
section with its own folders: its all and human folders, its groups (expand
one for its members) and its agent folders. A merged folder pages through the
nodes' newest 500 messages each; pick one node to page further back. A peer's
mail is shown only if it grants you `node.messages.read`; otherwise its chip
and heading say "not shared". An unreachable node keeps its last page, marked
with its age; a node no longer trusted drops it. Bodies are shown as text.

Actions go to the node that owns the message. Mark read/unread on agent
messages (one, the selection, or an agent folder's mark all read) needs that
node's `node.messages.manage`; deleting needs the node to be this one or to
trust you unrestricted. A reply to an agent goes through its `operator-message` route
(`message.direct`). This node's human notifications have the same buttons as
in its own Messages (reply, focus, mark read/unread, download, delete), and
so do a peer's: reply opens the same dialog (text only) and goes to the peer
when it trusts you unrestricted. Opening a notification marks it read on its
node, as in each node's own Messages, wherever mark read/unread is allowed:
on this node, and on a peer that grants you `node.messages.mark-read`
(unrestricted trust holds it). Focus on a peer's notification opens the sending
agent's terminal here through the federation bridge. What a node withholds
stays in place, disabled, with a 🔒 saying what it needs. Their files preview, play and
download as usual, read from the peer under the same `node.messages.read`.
Access requests (approve once, deny) are answered here on this node and on
peers that trust you unrestricted; approving says exactly what it lets
through on that node. Anything a node does not allow from here
says so, and **Open on NODE** shows that node's own Messages. CLI:
`tclaude federation human-inbox PEER` (with `reply` and `decide`) and
`tclaude federation action message --node NODE --agent ID --body TEXT`.

## The 🌐 marker

A group linked to federation peers carries a 🌐 after its header chips (a green
dot: a linked node is live). Click it for the links — a direct or pool grant,
or a route mirror — with what each allows and a jump to that node's dashboard.
CLI: `tclaude federation links [--group NAME]`.

## Cross-node group links

The inter-group link dialog's **To** row has a **This node | Peer node 🌐**
switch on nodes with cross-node links. **Peer node** lists only known peers you
trust at least *limited* and only the groups they share with you; an older peer
is listed but unavailable. With no hub connection the switch is greyed out with
the reason, and local links work as before. The note under the pickers says
what happens: if the peer granted you `groups.link.accept` for that group, the
link needs no prompt and shows **connecting** until the peer acknowledges this
exact edge; otherwise it sends a link request that the peer's operator approves
or declines within 24 hours. *Also ask for the reverse link* asks for the other
direction separately.

The Inter-group links list then gets **This node** and **Peer side** columns.
Each row is one directed edge. A peer that is offline shows its side as
*unknown*, with what it last reported, never as active or revoked. Deleting a
cross-node edge stops delivery on this node immediately, including queued
messages; a pending request reads **cancel request** instead. The 🌐 popover
lists the same edges.

An incoming request appears in **Messages → Access requests** as a 🌐 link
request naming the exact edge and mode, with **Approve**, **Approve + allow
future links into** the group (which also grants that peer `groups.link.accept`
for it), and **Decline**. Approving authorizes only that edge, never broader
messaging from the peer. A request this node sent shows read-only, waiting for
the peer's operator.

## Peer proposals

A proposal offers a peer several items in one go: groups of yours they may see
(name, description and member count; the member list only when you tick
*include member list*) and link edges between your groups and theirs, one
direction or both (each direction is a separate edge). Open it with
**Propose…** on a trusted peer in **⚙ Fleet config**, or **Propose several items…**
in the link dialog's Peer node mode. A preview lists exactly what is offered.
Nothing changes on either node until the peer's operator accepts.

The proposal appears on both nodes in **Messages → Access requests**. The
receiver ticks what to accept and gets **Accept all**, **Accept selected**
or **Decline all**. Unticking *member list* on a share keeps only its name,
description and count. Changing a link mode or sharing some of your own groups
back sends a **counter-proposal** instead, and the turn passes to the other
side. While the peer decides, the sender can **Withdraw**. When some edges were
already accepted, the button reads **Withdraw pending items**: accepted edges
stay until they are removed in Inter-group links. Each node applies only its own
items. Accepting grants nothing beyond them.

## Fleet administration (⚙ Fleet config)

**⚙ Fleet config** (Fleet at the end of the node row, then ⚙ Fleet config,
or "Fleet administration" in the command palette) manages this
node's federation. The identity bar at the top shows
this node's instance ID and full fingerprint (copy it to compare out of band),
the hub connection with **Disconnect** or **Reconnect**, **version & updates…** for this
node, and away cover: **away…** picks a trusted peer's operator (and an
optional end) who, while you are away, receives your agents' access requests
as they arise (and may answer each once; the first answer wins), every
message your agents send you, and waiting-session notices; **return…** ends
it. Peer operators' access requests are never
forwarded. **settings…** (or **connect…** before a hub is set) edits the hub
connection — URL, display name, a single-use invite, a CA file on this node —
and this node's labels, which automatic placement matches and peers with
`node.read` see. It also holds the fleet health notice policy
(`tclaude federation nodes health`): the defaults, or one trusted peer's own
policy, with offline/back, resource and failure notices and their thresholds
(empty = built-in default). Saving a peer's policy stops it following the
defaults. Its **Identity** section has **Rotate identity…**. Every action that widens trust, overwrites state or runs code confirms
first and says what will happen. Most pages name their CLI.

The pages, in order: **Setup guide**, **Peers**, **Harnesses**, **Run scripts**,
**Spawn requests**, **Invites & joining**, **Peer grants**, **Model gateways**, **Usage
sharing**, **Profiles & pools**, **Offers**, **Jobs & repos**, **Moves**,
[**Boards**](#boards-fleet-config-boards), **Audit** and **Hub**.

- **Setup guide** — where Fleet opens until this node is joined to a hub,
  trusts a peer and shares a group with it (the tab counts the steps done);
  afterwards it is one tab away, for linking more nodes. Each step shows its
  state, does the step in place and names its CLI. Any step, done or not,
  opens from the list to read again; a joined node keeps the join
  instructions under **How joining works**:
    1. **Set up a hub** (optional): how to run `tclaude hub serve` (with TLS,
       or plain HTTP over loopback), where its one-time admin claim token is
       written, and `tclaude hub invite` for each node's join code. Skip it
       when someone else runs the hub.
    2. **Become hub admin** (optional): once joined, whether this node is an
       admin of its hub, or the same **Claim hub admin…** dialog as Hub for
       the claim token. Before joining it says to join first.
    3. **Join a hub**: paste the join code from the hub admin; the guide
       shows what the daemon read from it (hub, space, invite expiry, the
       SHA-256 of a certificate it trusts, and the hub it replaces) before
       one Join confirmation. A hub address and a bare invite work too (a
       pasted `tchi_` invite moves to the invite field; a self-signed hub's
       CA file folds away), and this node's instance ID and fingerprint are
       there for an admin who admits nodes directly. A refused or
       unreachable hub is explained, and a refusal takes a new invite or
       join code in place.
    4. **Trust your peers**: the nodes waiting in your spaces with their full
       fingerprints, and the same Trust… dialog as Peers; your own fingerprint
       to read to the other operator.
    5. **Share groups**: a peer, one of your groups and presets of ordinary
       peer grants on it (Mail: `message.direct`, roster and presence;
       Status: `agents.status.read`, `sessions.read`; Watch terminals:
       `sessions.watch`; Spawn workers: `groups.members.spawn`, cap 2), with
       what the peer already holds there left out. Peer grants has the rest.
    6. **Let agents use peers** (optional): what each trusted peer shares with
       this node, per group, which of your groups and agents already use it,
       and **Give access…** for another group (every member) or agent. That
       merges `peer=<peer>/<group>` into their grants of the slugs that use
       it (`tclaude agent permissions peer-reach add`), keeping the rest; ✕
       removes it again. A grant held for local use is refused with the way
       out rather than replaced.
- **Peers** — while a peer views an agent terminal here, a panel on top shows
  who watches or drives which agent; **Disconnect…** closes that view (the peer
  can reopen it while any sessions.watch/attach grant covers that group, or at
  once if it is unrestricted). Below: trusted peers (level, grants, pools) and hub-visible instances
  waiting to be trusted. To trust one: **Trust…**, compare the fingerprint
  shown in full with the other operator, tick the check, pick the level and
  confirm. Unrestricted repeats that it grants every peer permission on all
  groups, including ones created later. Restrict, unrestrict and untrust from
  the row. A peer whose signing key is changing is marked next to
  its fingerprint (and on its map card): **new key pending** is information
  only — the successor waits out this node's detection window and is not yet
  accepted; **⚠ competing keys** needs explicit recovery, and its dialog shows
  both fingerprints in full plus the `tclaude federation identity recover-peer`
  preview and the apply to run only after verifying the new fingerprint with
  the peer's operator. Rotating this node's key is under **settings…**; recovery
  stays in the CLI.
- **Harnesses** — each node's harnesses in one table: version, ↑ when an
  update exists, 🔑 when a login is present. Click a cell to install or update
  (if workers are busy, choose to run now or when they are idle), or, for
  login-file harnesses, to push your login to a peer (agents there then act as
  you), back up or restore the node's login files. **Update all…** updates a
  node's harnesses when its workers are idle. A peer needs to share
  `node.harnesses.read` (and `.install`, `node.credentials.receive` to change
  anything).
- **Run scripts** — run one shell script on several nodes. Tick nodes (or
  **all online**), write the script, set the timeout, **Run** and confirm. Each
  node gets a pane with state, exit code, duration and output; **full stdout**
  / **full stderr** load the whole log, and **Re-run on N failed…** repeats it
  only where it failed. A peer runs your script only if it granted you
  `node.exec` and turned on **Accept remote scripts** on its own Run scripts
  page — that switch is full remote code execution as the tclaude user, and the
  limits beside it cap each script's memory, cpu and processes.
- **Spawn requests** — workers peers asked this node to start in its groups.
  Its sub-tab counts the requests waiting for a decision, and a request's
  notification in Messages has a **review request…** button that opens it.
  **Approve…** starts the worker in that group as you (optionally overriding
  name, profile, cwd, harness or model); **Deny…** declines with an optional
  reason. A launch that never confirmed can be abandoned back to pending, but
  the original worker may still appear late. **Request a worker on a peer…**
  sends a brief to a peer's exported group, or to automatic placement (any
  trusted node or a pool, with requirements; the dialog shows why each
  candidate was chosen or skipped). A peer that granted you
  `groups.members.spawn` on the group starts the worker right away. The outbox below shows delivery
  of the spawn requests and operator mail this node sent; decisions arrive in
  Messages. CLI: `tclaude federation spawn-request`, `requests`, `outbox`.
- **Invites & joining** — **Create invite…** issues an enrollment token for a
  node profile (uses, lifetime, the trust the joining node grants back) and
  shows it once; revoke active tokens from the list. To join a master:
  **Join with a token…**, pick the master, paste the token, check both
  fingerprints in the preview, tick the check and **Enroll**.
- **Peer grants** — pick a peer or a pool, see each grant with where it
  applies (a group, or all groups including future ones, flagged) and what it
  allows, **Revoke…** it, or add one: permission, group, cap, **Grant…**.
  Spawn and job grants have **launch settings** (profile, selectable
  profiles, harness, model, directory, requester pays, job approval) that
  decide how this node starts workers for the peer; `models.proxy` grants
  take one gateway name or cover them all. Sensitive grants (spawning,
  attach, `node.exec`, …) say what they let the peer do.
- **Model gateways** — this node's model gateways, which peers with a
  `models.proxy` (or, for requester-paid workers, `models.proxy.leased`)
  grant use to send model requests charged to this node's provider account.
  See each gateway's models and limits — a gateway missing a model allowlist,
  a budget or a cap refuses every request and says so — turn one off or on,
  block or unblock a peer on it, or **Turn off all…**. Turning anything off
  revokes the matching leases at once, so requester-paid workers using them
  lose model access. Below that are the leases (**Revoke…** one) and a day's
  usage per gateway, peer and model. **Add gateway…**, **Edit…** and
  **Remove…** manage gateways themselves (provider, credentials, limits), and
  **My Claude subscription** covers running on your own login. CLI: `tclaude federation
  models`.
- **Usage sharing** — **Shared with you** and **You share**: what subscriptions and
  gateways flow between nodes, with **Ask for more…**.
- **Profiles & pools** — pools group nodes so one grant covers every member;
  add or remove members and open a pool's grants. Profiles bundle a trust
  level, pools and grants: **Apply to peer…** previews the plan, **Make
  default…** applies it to newly trusted peers. **New profile…** / **Edit…**
  edits a definition: trust level, pools, labels, requester-pays default and
  peer grants, with worker permissions, the teleport landing and the config
  bundle as JSON. Saving makes a new revision; peers it was applied to keep
  their settings until it is applied again.
- **Offers** — config and agent bundles peers offered this node, and the ones
  it sent. **Preview…** shows what an incoming offer would change: per config
  item new / overwrites yours / unchanged with security-relevant items marked
  and a tick to leave each out, or for an agent its name, history and any
  suspected credentials in it, plus placement and the values it needs here.
  **Apply…** / **Start agent…** confirm what changes (a move or teleport says
  the peer retires its source); **Decline…** deletes the payload. **Offer my
  config…**, **Offer an agent…** and **Offer a profile's config…** send new
  ones; flagged credentials are listed and need an explicit send-anyway.
  CLI: `tclaude federation offers`, `offer-config`, `share-agent`.
- **Jobs & repos** — remote jobs: those peers sent here (⇠) and those this
  node sent (⇢), with repo@ref, group, command and state. **Approve…** runs a
  job a manual-approval `jobs.run` grant holds (jobs otherwise run on
  arrival); **Cancel…**, **Resend…** (same job ID, never runs twice),
  **Acknowledge stopped…** for a job this node lost track of, and **Output**
  once finished — each confirm says what happens. **Run a job…** sends one to
  picked nodes or an automatically placed one. Below, the repositories peers
  may run jobs in here: **Allow…**, **Edit…**, **Disable…**. **Live output**
  on a running job this node sent shows its output as it arrives (like
  `job run --follow`), read every 2 s only while the dialog is open, Fleet is
  shown and the browser tab is visible; once the job ends it shows the stored
  output. CLI: `tclaude federation job`, `repos`.
- **Moves** — agent moves and teleports in both directions (⇢ leaving, ⇠
  arriving) with peer, group, state and expiry. **Abandon…** stops an outgoing
  move that has not started retiring the agent here; the agent is not retired
  (a copy the peer already made stays there). **Freeze teleports…** stops teleports leaving or landing
  on this node until you allow them again; plain moves are not affected.
  **Move an agent to a peer…** is `tclaude federation move-agent`: it sends a
  local agent with its conversation history to a peer's receiving group, and
  the agent retires here once the peer confirms its copy is running (it leaves
  its groups and loses its grants here). The confirm says so, and keyboard
  focus starts on Cancel.
- **Audit** — this node's federation activity, newest first: a peer acting
  here (⇠) or this node acting on a peer (⇢), with actor, target, group and
  outcome; refused or failed requests in red. Filter by peer and time window.
- **Hub** — for hub admins: claim, admins, admissions, invites, settings, health,
  logs, scripts and updates. See
  [Federation](federation-hub.md#running-a-hub).

The full federation model, grants and CLI are in
[Federation](federation.md).

## Boards (⚙ Fleet config → Boards)

With at least one joined board, the page opens on **Browse content**: everything posted to every board this
node is a member of, in one list. **List** is a dense table with a detail
pane; **Cards** shows the same items as cards with facets (kind, board,
publisher) that count and filter. The filter box matches name, description,
publisher and board in both views; it also takes `kind:skill|config`,
`publisher:trusted|unknown|mine` and `board:NAME`, which the facets and kind
tabs write for you. Sort by name, kind, publisher, board, version or last
update: click a List column header (ascending, descending, then posted
order), or use the Cards **Sort** dropdown and ▲/▼. The view and the sort are
remembered. Each item shows who signed it and whether that is a peer this node
trusts (*trusted peer*), this node, or an *unknown publisher*. An unknown
publisher's detail also says that a valid signature proves the version is
unaltered, not who runs that node. Selecting an item fetches and verifies it,
then lists every file; scripts and executables are flagged and open as text.
**Install…** on a config item is the board's import preview. On a skill it
offers four targets: a harness user skill directory, a group's default
directory (as a project skill under `.claude/skills`), a custom directory
(human-only: it must exist, symlinks and `..` are refused, and the resolved
path is shown), or a plain browser **Download**. Every target but Download
previews the exact paths written and any overwrite first, and installs only
on confirm; keeping executable bits and overwriting are separate opt-ins.
**Upload…** posts to a board you can post to. **Manage boards** holds
everything below.

Boards share content with other nodes through the hub, without linking the
nodes or giving anyone access to this machine. To join, paste an invite and
press **Join**; to start one, type a name and press **Create** (you become its
owner). Each board lists your role in plain words — owner, can post, can read —
and its limits. Open a board to see its members. An owner can also invite
someone who can read or post (the invite is shown once, with **cancel
invite…**), change members' roles, remove members, and **Change key…** so that
members removed earlier cannot read anything posted afterwards. **Leave…**
ends this node's membership.

An open board also lists its **shared config**: signed, versioned roles,
profiles, sandbox profiles, templates and process templates its members
posted. **Open…** fetches and verifies a version on this node and shows its
files as text (or **Download**); **Import…** previews what would change in
this node's config — new, overwritten, security-relevant — and applies only
what you confirm, with any values it needs. Nothing on a board is applied
automatically. **Versions…** lists older versions and **Keep…** pins one (the
list then flags newer ones as *update*). Members who can post use
**Post config…** (whole sections, or named items such as `roles/reviewer`)
and **New version…**; anything that looks like a credential is refused.
Skills can also be posted as `skills/NAME`: one directory containing `SKILL.md`
and its supporting files. **Post config…** lets you select its local source
(Claude Code, shared `~/.agents/skills`, or Codex home). The import preview lists every
file, marks scripts and executables **security-relevant**, and shows their
contents as text. Choose the destination skill root(s), preview again, and
confirm. Existing skills require overwrite confirmation. Executable bits are
removed unless you explicitly confirm preserving them; scripts can still be
invoked through an interpreter. Symlinks and special files are refused. Group destinations
install project skills under the group's default directory at
`.claude/skills/NAME`; custom destinations must be existing absolute directories
without symlink components or parent traversal. The preview shows the resolved
path before confirmation. A verified archive download contains the skill files
(or `bundle.json` for configuration items), with executable bits stripped
unless explicitly confirmed.

**Post to…** (on an item or one of its versions) posts that version, unchanged
and still signed by its author, to another board you can post to. An
item whose signed details cannot be verified shows the error and cannot be
opened or imported. The hub decides who is a member: a dishonest hub
operator could block a board or keep a removed member getting new posts, but
cannot read anything or fake a post, so use a hub you trust. Hub admins
holding `hub.boards.manage` see **Boards** on the Hub page to freeze, limit
or delete boards; they never see content or keys. CLI:
`tclaude federation boards …` and `tclaude federation hub boards …`.

When an item you keep has a newer version on its board, the **Boards** tab
shows a count, and the page lists those items with **Open board**. Tick
"Also leave a note in Messages…" to get one Messages note per new version
(off by default). Neither fetches nor imports anything. CLI:
`tclaude federation boards updates [--refresh] [--notify on|off]`.

