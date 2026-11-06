# Connect and share

Link nodes, decide what each peer may see and do, and see your fleet. For a first setup, follow the [walkthrough](federation-setup.md).

## Joining

Connection, trust and peer-grant commands are human-only.
`spawn-request` is also callable by agents with a peer-scoped spawn grant.

```bash
tclaude federation identity                  # your instance id: give it to the hub admin
tclaude federation connect wss://hub.example:8470 [--invite tchi_…] [--name alice]
tclaude federation join tchj_… [--name alice] [--preview]   # or --code-file / --code-stdin
tclaude federation status
```

A **join code** (`tchj_…`) bundles the hub address, an optional invite and an
optional PEM CA bundle into one string. `tclaude hub invite --url … [--ca-file
…]` mints one, and so do `tclaude federation hub invites create` and the
dashboard's Hub page on a hub-admin node (`join_code` beside `token`, built
from that node's own `hub_url` and `hub_ca_file`). `federation join` shows what
the code holds and what it replaces, then stores the CA under
`~/.tclaude/data/federation/hub-ca/` (content-addressed, 0600) and writes the
same configuration as `connect`. Whitespace in a pasted code is ignored. A
code is a bearer secret that also decides TLS trust for the hub: accept one
only from the hub admin. The route is human-only `POST /v1/federation/join`
(`/api/federation/join` from the dashboard) with `{code, name?, preview?}`;
an expired invite is refused with `join_code_expired`, anything malformed
with `invalid_join_code`.

Connection settings live under `federation` in
`~/.tclaude/data/config.json` (`enabled`, `hub_url`, `name`, `invite`,
`hub_ca_file`). Use `--ca-file` when the hub's certificate is signed by a
private CA. `tclaude federation disconnect` turns the connection off but
keeps peers and grants.

## Pairing

The hub shows you instances that share a space with you. Trust is your own
decision:

```bash
tclaude federation peers                     # visible instances + fingerprints
tclaude federation trust inst_… --label bob  # compare the fingerprint out of band first
tclaude federation untrust bob
```

The label is the short name you use in addresses (`member@bob`). Envelopes
from untrusted instances are dropped unanswered, except enrollment (below) and
trust requests.

A **trust request** asks a hub-visible instance's operator to trust your node,
or to trust it back (the dashboard's Fleet map sends one when you trust an
instance from it, and offers **Ask to be trusted…** with an optional note of up
to 200 characters). It is small, signed and sealed, carries no authority and
bypasses the outbox; the receiving node keeps it in memory only (at most 64,
for 7 days or until a restart), rate-limits senders, and ignores it from an
instance it already trusts. It only shows on the receiver's map and in
⚙ Fleet config → Peers; trusting the sender still needs the receiver's own
fingerprint comparison. The note is the sender's own words. API:
`POST /v1/federation/peers/trust-request` `{instance, note?}` (one per instance
a minute) and `POST /v1/federation/peers/trust-request/dismiss` `{instance}`;
status rows carry `trust_request` `{note, trusts_you, received_at}` and
`trust_requested_at`. Untrusting a peer also
removes its peer grants and cached catalog.

Trust defaults to **restricted**, including for existing peers: receiving peer
grants and requesting agents' `peer=` grants control access. For your own
machines, opt in locally to **unrestricted**:

```bash
tclaude federation trust bob --level unrestricted
# Later, downgrade immediately:
tclaude federation trust bob --level restricted
```

Upgrading asks for confirmation and repeats the fingerprint and permissions;
`--yes` confirms for scripts. `peers` and `status` show the level. Re-running
`trust` updates the existing peer, including while disconnected.

An unrestricted peer holds every peer-grantable permission across all live
groups, including future groups. Spawn auto-approves using each group's default
launch settings. The live automatic worker cap defaults to 8 per peer; set
`federation.unrestricted_max_live` in `~/.tclaude/data/config.json` to change it.
Towards that peer, local agents' unscoped grants (including group grants and
config defaults) also count. `group=` scopes and group ownership never confer
remote authority; denies still apply. **Interactive attach permits answering harness approvals and prompts, just
as a keyboard at the target pane does.** `--ask-human` remains a local
operator workflow; remote keyboard access does not grant its daemon API (only
an unrestricted peer, or one holding `human.inbox.answer` within its groups,
answers it from this node's human inbox). Each side chooses its own level;
the hub cannot set it. Downgrading or untrusting affects subsequent authorization
reads immediately, including reads by in-flight operations.

## Peer grants: what a peer may see and do

Trusted peers are permission principals. The receiving operator grants a
peer regular permission slugs on local groups:

```bash
tclaude federation grant bob message.direct --scope group=builders
tclaude federation grant bob groups.roster.read --scope group=builders
tclaude federation grant bob groups.presence.read --scope group=builders
tclaude federation grants bob
tclaude federation revoke bob message.direct --scope group=builders
```

| Peer slug | Allows |
|---|---|
| `groups.roster.read` | member names and roles |
| `groups.presence.read` | online/offline per member |
| `message.direct` | mail to members; shares their names and ids so they are addressable |
| `message.attachments` | attachments, together with `message.direct` on the same group |
| `groups.members.spawn` | automatic worker spawning with receiving operator launch settings and caps |
| `groups.members.spawn.cwd`, `.harness`, `.model`, `.effort` | choose that launch setting per spawn request, with `groups.members.spawn` (see [Launch settings a peer may choose](federation-work.md#launch-settings-a-peer-may-choose)) |
| `groups.members.spawn.profile` | select any enabled spawn profile that is not operator-only, with `groups.members.spawn` |
| `groups.members.spawn.roles` | choose the spawn's roles from this node's roles (roles carry permissions here), with `groups.members.spawn` |
| `groups.members.spawn.sandbox` | add one of this node's sandbox profiles to the spawn, with `groups.members.spawn` |
| `groups.members.spawn.permissions` | choose the approval, tool and harness sandbox modes, the auto-reviewer, question timeout, trust-folder answer and per-slug permissions, and launching with unenforceable sandbox rules or no sandbox profiles, with `groups.members.spawn` |
| `groups.members.spawn.environment` | set environment overrides on the spawn, with `groups.members.spawn` |
| `groups.members.spawn.owner` | make the spawn a group owner at birth, with `groups.members.spawn` |
| `groups.members.spawn.features` | choose the harness features (remote control, auto memory, peer messaging, fast mode, app server, SSH workaround, group context), startup context, window settings, description and task link, with `groups.members.spawn` |
| `groups.members.spawn.prompts` | choose the spawn's saved prompts from this node's, replacing the profile and group defaults, with `groups.members.spawn` |
| `groups.members.spawn.worktree` | launch the spawn in a git worktree: list and create worktrees in any repository on this node, with `groups.members.spawn` and `.cwd` |
| `groups.members.spawn.all` | every `groups.members.spawn.*` launch setting above |
| `groups.settings` | change the group's ordinary settings from the peer's dashboard: description, startup context, member cap, notifications, remote-control policy, attachment, name and nesting; its default directory, default profile, default saved prompts and sandbox profile also need the matching spawn grant (see "Changing a peer's groups") |
| `groups.settings.environment` | change the group's spawn environment, whose values reach every agent spawned into it |
| `groups.resources.read` | list the group's resources (saved processes, boards, live pages) with their agent locks on the peer's dashboard, and read the group's collab boards (the board API through the peer proxy; `boards.read` is the global slug a peer reaches only this way). Before it existed, this list came with `groups.roster.read`; a peer that held only roster read (and no unrestricted trust) no longer sees it until granted this. That includes a peer granted `groups.resources.lock`: its lock chips sit on this list, so grant both for the lock to be usable from its dashboard |
| `groups.resources.write` | edit the group's collab boards: their elements and titles, create a board in the group, and move one to another group you hold this in (never unlink it). It implies reading them, as locally. Edits are authored `operator@INSTANCE`, and a new board takes its id from its title. Peers' creates leave this node's last 100 board slots to its own operator and agents. A peer's board read carries no presence and no invited agents, and the group's members only with `groups.roster.read`. An operator lock on a board refuses agents only, so a peer edits a locked board as this node's operator would. Deleting a board, its live view and presence, activity mail, undo, its rendered snapshot, and inviting agents stay with this node's dashboard. A board linked to no group is no peer's. Sensitive |
| `groups.resources.lock` | lock and unlock the group's resources (saved processes, boards, live pages) against agent changes from the peer's dashboard; a process pinned to several groups needs it in each. Locks stay the operator's: no agent reaches them, granted or not |
| `groups.members.manage` | change member agents' ordinary settings from the peer's dashboard: name, tags, task link and notifications, and role and description in the group; an agent's own settings need it in every group the agent is in |
| `routes.consume` | lists ready group routes and permits opening them |
| `sessions.read` | live agent sessions, harness, state and waiting reason |
| `agents.status.read` | dashboard-aligned agent activity, model, task and numeric context summaries |
| `sessions.watch` | read-only terminal view of a group member agent |
| `sessions.attach` | terminal view and full keyboard input, including harness approvals |
| `sessions.takeover` | take over a member agent's terminal: disconnect its other viewers, including this node's local terminals; used with `sessions.watch` or `sessions.attach`; default off |
| `sessions.shell` | open this node's tclaude shells from the peer's dashboard: the group's default-directory shell and its member agents' directory shells, attached in the browser over the federation terminal stream. This is host shell access as the tclaude user; default off |
| `sessions.files.read` | bounded non-secret project file downloads through a live terminal viewer; default off, independent of watch/attach |
| `groups.visibility.read` | a group's name, description and member count, with no message, presence, launch or roster grant. A grant that includes members also gives the roster. See [Group links](#group-links-and-peer-proposals) |
| `groups.link.accept` | pre-authorize incoming group link proposals on that group |
| `groups.members.add` | add an agent the peer sees to the group, holding this grant in every group the agent is already in too (joining puts it under the group's context, settings and owners). Moving a member in takes it in the target. Sensitive |
| `groups.members.remove` | remove a member from the group, or move it out with `groups.members.add` in the target; keeping its permissions is this node's operator's. Leaving can hand the agent another group's sandbox profile or environment, which takes `groups.members.spawn` and `.sandbox` or `.environment` in that group (for every group when it leaves its last one). Sensitive |
| `groups.link.add` | link the group to another group on this node, or change a link's mode, holding this grant in both; a link to another node takes unrestricted trust. Sensitive |
| `groups.link.remove` | remove a link that starts in the group. Sensitive |
| `groups.archive` | archive and unarchive the group; an archived group is found for its unarchive by this grant on it. Sensitive |
| `groups.delete` | delete the group; a sandbox or environment its members may take from their other groups instead takes that group's spawn grant, as on a remove. Sensitive |
| `groups.owners.manage` | remove the group's owners, and make a member an owner when the peer holds everything an owner holds there. An owner holds the group's whole owner tier, spawning and the member lifecycle included, which no peer may confer (see [The peer ceiling](#the-peer-ceiling)), so in practice only unrestricted trust adds owners. Sensitive |
| `groups.settings.member-permissions` | read and replace the permissions every member of the group holds. A row the peer adds or widens must be within its own grants ([The peer ceiling](#the-peer-ceiling)); a row it leaves as shown, or drops, needs nothing more. The whole list is replaced, so without this grant a peer can't replace it at all, since it would replace rows it was never shown. Sensitive |
| `groups.settings.owner-scopes` | read and narrow what the group's owners hold. Loosening or dropping a constraint widens every owner's grant of that slug, which takes the peer holding that grant itself ([The peer ceiling](#the-peer-ceiling)). The dashboard's group permissions editor saves the member rows with them, so using it also takes `groups.settings.member-permissions`. Sensitive |
| `permissions.grant`, `permissions.revoke` | edit a member agent's own permission overrides from the peer's dashboard, holding the grant in every group the agent is in. `permissions.grant` shows the agent's overrides and grants more: a grant the peer adds or widens must be within its own grants ([The peer ceiling](#the-peer-ceiling)), and narrowing one it was shown needs nothing more. `permissions.revoke` denies an override, or drops an unscoped grant. An override shadows what the agent inherits from its groups, roles and defaults, so dropping a deny or a scoped grant can widen what it holds: that takes `permissions.grant` and the ceiling for the slug unscoped. A stored scope that names anything the peer can't see (a hidden group, or a profile, resource or remote) is shown as unreadable, and only your operator changes it. Either grant is needed even to save a batch that changes nothing, and a peer's save never answers with the agent's overrides. A Steward's permissions, and keeping a grant for future Stewards, stay your operator's. An override the peer writes is recorded as the peer's, and while its `group=` scope names a group, the peer may not rename or delete that group. Sensitive |
| `groups.members.stop`, `.resume`, `.retire`, `.clone` | stop, resume, retire or clone member agents from the peer's dashboard. See [Remote operator actions](federation-reference.md#remote-operator-actions) |
| `process.runs.start`, `groups.members.process` | start a process run scoped to the group, or bound to one member agent (`groups.members.process` in every group the agent is in). The run is the peer's (owner `peer:<instance>`), never your operator's: it tasks only the members in its scope and spawns no performer. It cannot authorize program profiles, so a template with a program step is refused, and it cannot pick an older template version or a run ID. A node that would need a spawned performer fails its attempt rather than wait; a retry meets the same refusal, so such a step only completes once a member who can take it joins. Every node prompt it delivers opens with an untrusted-content banner that names the peer, because the peer's params reach the prompt. Starting from the peer's dashboard also needs the template list, which no peer grant serves yet; use `tclaude process run --remote`. Unrestricted trust starts runs as your operator does |
| `process.runs.manage` | pause any process run here; resume, decide, reassign and resolve a blocked branch of a run that executes no host programs and spawns no performers on your authority (a run with program authorizations, or one of your runs whose agent steps may fall back to a spawn, stays your operator's, pause excepted). This includes your operator's own agent-only runs, in every group. Decisions are human gates: only the peer's operator decides them, never its agents. A reassignment goes only to an agent the peer could start an agent-bound run for (`groups.members.process` in every group it is in), inside a run scoped to a group or an agent. The actor recorded is the peer, and the evidence or note it writes, which a later performer can be shown, opens with the untrusted-content banner. Re-scoping a run stays your human's, and reissuing or recording a program command's outcome has no peer route |
| `process.advance` | advance the group's advisory process to the next or a named phase. The transition is recorded as the peer's (`peer: the operator of linked node …`), and the members it nudges are told which node advanced it. Any peer that sees the group reads its process |
| `groups.messages.schedule`, `groups.members.schedule` | schedule recurring messages to the group (`groups.messages.schedule`) or to a member agent (`groups.members.schedule` in every group the agent is in), and list, read the run history of and disable your own cron jobs aimed there. A job the peer schedules is the peer's, never your operator's: it sends messages only and never spawns; each one arrives as the peer's mail under the untrusted-content banner; and every fire rechecks the peer's grants, recording `peer_denied` and delivering nothing once they no longer reach the target. The peer changes, enables, fires and deletes only its own jobs; on yours it reads and disables (disabling is reversible), and creating, editing, enabling, firing and deleting stay with their owner and your operator. A peer keeps at most 50 jobs here (10 for each of its agents); a run replaces the job's still-unread earlier one, as local cron does; your operator may reschedule, disable or delete a peer's job but never rewrites what it says; another peer's jobs are not in reach; a group export leaves them out. Unlinking or revoking the peer deletes its jobs. Jobs an unrestricted peer creates are ordinary local jobs and stay when it is downgraded or unlinked. The peer sees no conversation IDs and no run error text, and a spawn job's worker instructions stay yours. `agent.schedule` is granted as `groups.members.schedule` |
| `groups.triggers.read` | list and inspect the group's trigger rules and their recent firings, without a spawn's instruction template or any error detail. Global rules stay yours, and no peer grant authors, edits, enables or disables a rule: trigger rules are only ever your operator's (`groups.triggers.manage` is local). `triggers.read` is granted as `groups.triggers.read` |
| `groups.members.interrupt`, `.compact`, `.reincarnate`, `.context-info`, `.pr` | from the peer's dashboard: interrupt a member Codex agent's running turn, compact a member agent, ask it to reincarnate itself, read its context window use, and list or present its pull requests. Each needs the grant in every group the agent is in. No peer text is typed into the agent's pane: a peer's compact takes no follow-up, and a peer's reincarnate is the agent's own (mode `self`, no focus hint; the inbox request names your node and is not operator-authored). A peer presenting a PR names no repository directory. Unrestricted trust keeps every option |
| `groups.members.delete` | permanently delete a member agent from the peer's dashboard, holding this grant in every group the agent is in or owns, archived ones included (a delete purges those memberships and owner seats). Deleting its git worktree as well takes unrestricted trust: it can destroy uncommitted work on this machine, so the peer's delete dialog offers no worktree option. Sensitive |
| `groups.members.debug-export` | download a member agent's recorded launch and sandbox configuration, holding this grant in every group the agent is in. Every environment value, the recorded PATH and each sandbox-profile pre-launch script are redacted (names kept, each field listed under `redactions`); unrestricted trust gets the full export. Sensitive |
| `groups.members.remote-control` | turn a member agent's Claude Code Remote Access on or off, holding this grant in every group the agent is in. Remote Access exposes the session to this node's claude.ai account, not to the peer. Sensitive |
| `agents.receive` | offer agents into that group. See [Sharing an agent](federation-moving.md#sharing-an-agent-with-a-peer) |
| `agents.receive.permissions` | opt-in permission carry into a receiving group, optionally limited to named slugs. It never transfers ownership or sensitive authority. See [permission carry](federation-moving.md#receiver-approved-permission-carry) |
| `agents.teleport.receive` | automatic teleport landing in that group, with an applied node profile that sets `teleport_landing` |
| `agent.move` | move a member agent to a peer, with `groups.members.retire` on the source |
| `jobs.run` | one-shot remote jobs in that group, under a launch policy. See [Remote one-shot jobs](federation-work.md#remote-one-shot-jobs) |
| `node.read` | platform, harness versions, labels and numeric node resources (unscoped only) |
| `fleet.read` | the fleet brief: every active agent with its status, groups, task link and PRs, and what waits on the operator (unscoped only; read-only) |
| `process.runs.read` | list and inspect every process run here (unscoped only; read-only). Sensitive: it covers runs in every group, including groups the peer cannot otherwise see. The peer's view is an allowlist that leaves out how programs run: no program command lines, working directories or sandboxes, program authorizations, blocked-branch error details, bound task prompts or event payloads. It does show run params and performer reports. A group's `groups.resources.read` does not read runs |
| `agent.sandbox-impl` | read a member agent's sandbox implementation and, with `groups.members.spawn.sandbox`, only add confinement to it: `off` to `resource-only`, either of those to `harness-builtin` or `tclaude-layer`. Both grants must be held in every group the agent is in. Moving an agent onto less confinement, switching between the two that confine access, pinning a harness sandbox mode, or accepting a move-out's consequences takes unrestricted trust; restarting a running agent onto it also takes `groups.members.stop` and `.resume` there. Sensitive |
| `human.inbox.read` | read your unread human notifications that an agent the peer sees sent in the group, never a senderless one, and the pending ask-human requests of agents that have the grant in every group they are in. See [Human inbox across your own nodes](federation-work.md#human-inbox-across-your-own-nodes). Sensitive |
| `human.inbox.answer` | reply to the notifications `human.inbox.read` shows, and answer those requests once. Deny needs only this grant. Approve also needs the requested action to be within the peer ceiling, checked when it answers. "Always allow" and deadline extensions stay yours. Sensitive |
| `node.spawn.read` | the spawn dialog's catalogs, read-only: spawn profiles and the default, launch defaults, saved prompts, roles, sandbox profiles and defaults, spawn harness policies, the default model, the harness and sandbox implementation catalogs; group templates, in full (a template snapshotted from a group carries that group's context and briefs, even one the peer cannot see); the fused Groups view's right panel lists them; shares profile environment values and paths (unscoped only; see [The remote spawn dialog](federation-work.md#the-remote-spawn-dialog)) |
| `node.spawn.defaults.manage` | set or clear the global default spawn profile and global sandbox profile from the peer page's Groups toolbar, choosing among existing profiles (`node.spawn.read` lists them; unscoped only) |
| `node.spawn.profiles.create` | save a new spawn profile here from the peer's spawn dialog (**Save as profile…** → **Save on**); a restricted peer's profile is saved operator-only, so it opens nothing its spawn grants keep closed until this node's operator lifts that; editing and deleting profiles take `profiles.manage` (unscoped only) |
| `sandbox-profiles.manage` | create sandbox profiles that narrow one of yours, and edit, rename and delete only the ones it created. A peer's profile includes exactly one existing profile, fixed once created, and adds only filesystem denies and `network_access: none`, so it never confines less than its include: it follows your operator's edits of the include, and it fails to resolve rather than falling back to anything if the include is gone (deleting an included profile is refused, and renaming one moves the reference). Your own profiles, the default profile and implementation, and profile import, directories, harness-config repairs, read exclusions and enforcement take unrestricted trust. The dashboard's sandbox profile editor stays unrestricted trust's; a restricted peer saves its profiles through the API (unscoped only). Sensitive |
| `config.export` | download a config bundle, staged like every peer download, carrying only the sections the peer reads otherwise: roles, spawn and sandbox profiles and saved prompts with `node.spawn.read` (saved prompts also with `prompts.manage`), templates with `templates.manage`, process templates with `process.templates.read`. Your node's config keys and default permissions are never in it, and neither is an item flagged as a possible credential (no `allow_flagged`; skip it instead). Naming a section in `only` that the peer can't read is refused (unscoped only). Sensitive |
| `profiles.manage` | edit and delete this node's spawn profiles from the peer's dashboard. Sensitive: an edit reaches every new spawn that uses the profile, your default profile and your own spawns included. A restricted peer changes a launch value only where it holds the matching remote spawn grant for every group (`groups.members.spawn` plus `.harness`, `.model`, `.effort`, `.roles`, `.permissions` for approval, sandbox, tools and per-slug permissions, `.environment`, `.owner`, `.features`, `.prompts`, `.worktree`, or `.all`), so it never sets what it could not launch itself; the startup context and first message take `.prompts`; disabling, enabling, renaming or deleting a profile counts as changing every value it set; a group-scoped spawn grant that opens less in one group narrows what the peer may set; making a profile selectable by agents, its relaunch policy and its model gateway take unrestricted trust (unscoped only) |
| `prompts.manage` | create, edit and delete this node's saved prompts. Sensitive: they are text your own spawns start from (unscoped only) |
| `roles.manage` | create, edit and delete this node's roles: name, description and brief. A role's permission grants take unrestricted trust. Sensitive: a role's brief reaches every spawn that names it (unscoped only) |
| `templates.manage` | create, edit, delete and import this node's group templates, and snapshot a group the peer can see into one. A template agent's launch settings (harness, model, effort, roles, profile, approval, sandbox, ownership, inline profile, first message) are held to the peer's own remote spawn grants as a profile edit is, and its permission grants take unrestricted trust; so does a snapshot of a group whose members carry more. A snapshot or its preview carries the group's owners only under `groups.owners.manage` there, its members' own permission grants never, and its owner scopes only under `groups.settings.owner-scopes`; without that last grant a preview leaves them out and a save from a group that has them is refused, since a template without them would deploy owners holding more. Changing a template's owner scopes, and an import that embeds spawn profiles or roles, take unrestricted trust; instantiating one does not come with this grant (unscoped only) |
| `process.templates.read` | list, show and validate this node's process templates; exporting a workflow file, which this node signs, stays this node's (unscoped only; read-only) |
| `process.templates.manage` | create, edit, delete, import and list this node's process templates, and pin or unpin one in a group's process list where the peer also holds `groups.settings`. Starting a run takes `process.runs.start` (unscoped only) |
| `node.groups.create` | create groups from the peer page's **+ new group**: name, parent (a visible group), description, context, attachment, member cap. A launch setting (directory, environment, default profile, repository clone, remote-control policy) takes `groups.members.spawn` and the matching `.cwd`, `.environment`, `.profile` (a profile it could pick), `.worktree` or `.features` grant for every group (unscoped only; see [Managing a peer's node](federation-work.md#managing-a-peers-node)) |
| `templates.instantiate` | deploy a team from this node's templates. Scoped to a group: re-brief a deployed group, and reinforce it (with `groups.members.spawn` there). Unscoped, with `node.groups.create` and an unscoped `groups.members.spawn`: instantiate or deploy into a new group. Every agent the template launches, whoever wrote it, must be one the peer could spawn itself (its harness, model, effort, roles, profile, approval, sandbox, environment and first message within the peer's spawn grants, no permission grants); a new group's owner seats and owner scopes take unrestricted trust (an owner holds the whole owner tier; see [The peer ceiling](#the-peer-ceiling)); the request's own directory, worktree, environment or context take their spawn grants. A request that needs more is refused whole. The task or mission text arrives marked as a remote request |
| `groups.clone` | clone the group, with `node.groups.create`. Its settings copy from the source; a setting the request overrides takes its spawn grant as on create. Cloning its members takes `groups.members.clone` in every group of each; copying its owners takes unrestricted trust, so a restricted peer sets `copy_owners` to false |
| `groups.import` | import a group archive as a new group, with `node.groups.create`, through the staged upload only. The group's context and default prompts take `.prompts`, a legacy default model `.model` and `.profile`, and the target directory `.cwd`, each with `groups.members.spawn` for every group; saved prompts the file carries join this node's library and take `prompts.manage`. Every member's landing harness and model take `.harness` and `.model` for every group: a member that needs more refuses the whole import, which lists each such member with what it needs (the preview lists them too). The import keeps nothing the file asks for: owner seats, environment, stored grants, sudo grants and cron jobs drop, listed in the reply, and the landing is written to this node's audit log (`federation.group_import`) with what dropped. Keeping any of them (`keep*`) is this node's operator's. A file whose mail is addressed to, or whose head aliases point at, an agent it does not carry is refused, so an import cannot reach this node's own agents; an import the check cannot judge is refused too (unscoped only) |
| `groups.export` | download the group as an archive (`.paf`, or `?format=zip`), with `sessions.files.read` in the group, and `groups.settings.environment` when the group has an environment, whose values the peer view otherwise withholds. Carrying workflows and a flagged export (`allow_flagged`) take unrestricted trust. The grants are held again at every chunk of the staged download, so revoking one stops it. The transcripts take `sessions.files.read` in this group only, including those of members that are also in groups the peer has no grant in. Sensitive: the archive holds every member's conversation, the group's settings and its stored grants |
| `agent.bundle.export` | download a member agent as a portable archive, held in every group the agent is in; its private transcript (`history=true`) also takes `sessions.files.read` in each, and a launch environment it carries takes `groups.settings.environment` in each (and `node.spawn.read` when it comes from a spawn profile). Only the staged download serves a restricted peer. Other export options (carried workflows, a flagged export, a summary) take unrestricted trust, and the grants are held again at every chunk |
| `costs.read` | complete node-wide cost collection in peer UI requests (unscoped only) |
| `usage.read` | usage totals (today, week, month), limit readings and budget counts, which the peer's fleet usage limits count (unscoped only; see [Fleet limits](usage-and-costs.md#fleet-limits-spend-across-peers)) |
| `usage-rules.manage` | this node's usage limits, usage profiles, assignments and per-agent budget adjustments from the peer's dashboard (unscoped only) |
| `usage-rules.enforce` | the peer's fleet usage limits notify, wrap up, stop and block spawns of member agents; this node performs and records each action; an unscoped grant also covers agents in no group |
| `federation.audit.read` | complete node-wide dashboard audit collection in peer UI requests (unscoped only) |
| `approvals.answer` | one-shot access-request answer while selected as away cover (unscoped only) |
| `node.update` | check and apply tclaude updates here. See [Updating nodes](federation-admin.md#updating-local-and-linked-nodes) (unscoped only) |
| `node.harnesses.read` | installed harness binary paths and versions, and whether credentials exist, as a yes or no (unscoped only) |
| `node.harnesses.install` | install and update harnesses here (unscoped only) |
| `node.credentials.receive` | receive harness credentials copied from a peer (unscoped only) |
| `node.exec` | **full remote code execution as the agentd user** (unscoped only) |
| `node.messages.read` | this node's agent mail and human notifications; this exposes private bodies across the whole node. See [Message history](federation-moving.md#message-history-across-nodes) (unscoped only) |
| `node.messages.manage` | change agent mail read state (unscoped only) |
| `node.messages.mark-read` | mark this node's human notifications read or unread (unscoped only) |
| `config.offer` | offer configuration to this node, for you to review (unscoped only) |
| `snapshots.receive` | keep encrypted resilience snapshots of the peer's agents here; nothing is imported (unscoped only) |
| `models.proxy` | use a named model gateway here (`--scope http_proxy=<name>`) |
| `models.proxy.leased` | lease-only access to a named gateway here: only leases this node issued, for a [requester-paid worker](federation-models.md#requester-paid-workers), never ordinary opens (`--scope http_proxy=<name>`) |

Rows marked unscoped only are instance-wide and refuse a scope. For the rest,
prefer `--scope group=<local group>` to limit access. For the same slug, a
group-scoped peer grant takes precedence over an unscoped grant, including
its spawn launch policy. Revoking the scoped row leaves any unscoped grant
in force. `agents.receive`, `agents.teleport.receive` and
`agents.receive.permissions` require a group scope. An unscoped one, which a
node profile saved by an older tclaude can still hold, authorizes nothing:
`federation grants` and the dashboard's grant list mark it inert, and
`federation profile show` warns about it, until it is granted per group.

Some local permissions have no peer grant at all. Three examples:
`settings.default-model` edits the host's `~/.claude/settings.json`, which
reaches Claude sessions outside tclaude. `relaunch-policy.manage` is refused
to every peer on purpose, unrestricted trust included. `groups.pages.*`: a
live page is this node's stage origin, shown to the local operator's own
dashboard and its agents, so a peer has no view of it.

Nothing is granted by default; agent defaults and group ownership never grant
peer authority. Any grant covering a live group makes it visible in the peer's
signed **catalog**. Archived groups are hidden. Group scopes follow the group's
identity through renames; deleting the group does not authorize a replacement.
Omitting `--scope` grants authority on **every active group, including future
groups**; the CLI warns explicitly. There is no grant to all peers at once.

### Time-limited grants

A peer grant can end on its own. `--for` takes a duration (`90m`, `8h`, `7d`,
`2w`); `--until` takes an RFC 3339 time or a date (local midnight). The
limit is at most a year; without either flag the grant is permanent.

```bash
tclaude federation grant bob message.direct --scope group=builders --for 7d
tclaude federation extend bob message.direct --scope group=builders --for 30d
tclaude federation extend bob message.direct --scope group=builders --never
```

`extend` changes only when the grant ends and keeps its launch settings; it
also renews a grant that has already expired. Re-running `grant` replaces the
grant, so without `--for` or `--until` the replacement is permanent. Every
check reads the expiry, so an expired grant stops working on time. This node also re-sends its catalog when a grant
ends, so the peer's view drops it without waiting for another change.
`federation grants` shows each timed grant's end. In the dashboard, the
**Add grants** dialog has a **For** choice, and the grant list shows how long
each timed grant has left; updating a timed grant there keeps its limit unless
a new one is picked. Node-pool grants cannot be timed yet; grant the peer
directly. `approvals.answer` cannot be timed either; away mode ends the cover.
Applying a node profile or accepting a peer's proposal makes the grants it
writes permanent. An approved operator access request is a timed grant too (see
[Operator access requests](federation-work.md#operator-access-requests)).

### Changing a peer's groups

A peer's operator changes a visible group from the peer's dashboard (its
per-node view, or its groups in the fused Groups view) through the same
dashboard routes this node's operator uses, and the same per-field checks an
agent caller meets. One grant covers what an operator decides together:

```bash
# Bob: let Alice's operator edit builders' settings, including its sandbox profile
tclaude federation grant alice groups.settings --scope group=builders
tclaude federation grant alice groups.members.spawn.sandbox --scope group=builders
```

`groups.settings` opens the ordinary settings, from the quick options to the
⚙ menu's settings dialog, startup context, notifications, remote policy and
nesting (under another group this node shows the peer). What every spawn into
the group inherits also needs the peer's say over that part of a spawn: the
default directory needs `groups.members.spawn.cwd`, the default saved prompts
`groups.members.spawn.prompts`, the sandbox profile
`groups.members.spawn.sandbox` (`groups.members.spawn.all` covers them), and a
default profile must be one the peer may pick for a spawn there (see
[Launch settings a peer may choose](federation-work.md#launch-settings-a-peer-may-choose)). The environment needs
`groups.settings.environment`. The dashboard's profile pickers list this
node's profiles under `node.spawn.read`. A group's shut down and power on
buttons stop or wake every member under `groups.members.stop` and
`groups.members.resume` for that group. An agent's name, tags, task link and
notifications change under `groups.members.manage`, held in every group the
agent is in; its role and description in a group under that group's grant.
Deleting an agent takes `groups.members.delete`, its debug export
`groups.members.debug-export` and its remote access
`groups.members.remote-control`, each held in every group the agent is in (a
delete also in every group it owns). Deleting its git worktree with it, switching its
fast mode and assigning its sandbox implementation take unrestricted trust.
Cloning a group with retiring its members first, and cleaning up its offline
members, take unrestricted trust: no peer grant covers them.

Unrestricted trust makes a peer's dashboard about equivalent to this node's
own. It holds every peer grant, and every dashboard route without a peer
grant of its own reaches this node's handler as the local operator's request
would, every option included: ownership and permissions, links, standing
orders, cron jobs, templates, repositories and worktrees, retiring with the
worktree, and so on. A few things stay with this node even then:
- its dashboard login and browser-side preferences;
- its configuration, which holds the remote-script and federation settings
  that bound every peer, whether edited directly or imported in a config
  bundle;
- its remote-script settings (updates have their own `node.update` grant);
- its files outside an agent's own routes: a terminal's file reads and
  uploads, and the folder picker;
- its federation administration;
- the peer proxy (no peer reaches a third node through this one);
- terminal streams;
- its human inbox, which has its own peer routes.

Each change is in this node's federation audit as `federation.peer_view`.

### The peer ceiling

What a peer confers on this node's agents (a member permission row, a
loosened owner scope, an owner seat) never exceeds what the peer itself
holds here. This node's operator grants without that bound; a peer's
request does not. A row is within the ceiling only when:

- its slug is one a peer may confer (below);
- it names no peer (`peer=`): such a row acts on other nodes, which no
  grant here measures;
- the peer holds the slug's peer grant in every group the row reaches: the
  groups its `group=` scope names, or, with no group scope, every group,
  so the grant must be unscoped. An instance-wide slug is always held
  unscoped.

The slugs a peer may confer are local permissions whose peer grant of the
same name reaches the same operation here, through a peer route that adds no
bound of its own:

- agent controls: `groups.members.interrupt`, `.context-info`, `.pr`,
  `.remote-control`, `.rename`, `.tags`, `.task`, `.update`;
- a group's reads and plain settings: `groups.roster.read`,
  `groups.presence.read`, `groups.resources.read`,
  `groups.settings.environment`, `.description`, `.max-members`,
  `.notifications`, `.remote-control-policy`, `groups.attachment`,
  `groups.rename`, `groups.archive`, `message.attachments`,
  `process.advance`;
- this node's libraries and reads: `prompts.manage`, `costs.read`,
  `usage.read`.

Every other slug stays this node's operator's to confer, even when the peer
holds a grant of the same name. Some examples of why:
- the peer route refuses an option the local gate allows (stop's, resume's
  and restart's options, retire's worktree deletion);
- the peer route redacts (the debug export);
- the peer route checks a wider footprint (delete, add, remove, links,
  nesting);
- it takes extra grants (exports, clone, templates, process runs);
- the local gate reaches what the peer route keeps back
  (`process.templates.read` also exports a workflow file this node signs);
- the peer form carries a policy a local row can't hold (spawning,
  one-shot jobs, permission carry);
- it can put text into another agent's context (compact's focus,
  reincarnate's follow-up, cron jobs);
- it is a global `agent.*` gate, which also reaches agents in no group;
- its local gate opens more than the peer route: the group context's gate
  also sets default saved prompts, the resource lock's reaches resources in
  no group, and the role library's lets an agent attach its own grants to
  roles.

A `group=` scope names a group by name, so a peer may not rename a group
from or to a name that a permission it conferred names, nor delete such a
group: the permission would follow the name onto another group.


A peer view request is one exchange of at most 15 seconds. Longer operations,
a repository pull or sync and a worktree cleanup, run as jobs instead: the
peer starts the work and the requesting node waits on it, so the dialog sees
its usual answer (a pull or sync shows its results when the batch is done
rather than one repository at a time). A peer runs at most two at once for
one node.

Catalogs go directly to each trusted peer and are never published to the hub.
They refresh when grants change, when the peer connects, and every few minutes
for presence. Untrusting a peer deletes its grants.

## Shared agent status

```bash
tclaude federation grant bob agents.status.read --scope group=builders
tclaude agent permissions grant lead agents.status.read --scope peer=bob/builders
tclaude agent ls --remote
tclaude agent ls --remote --json
```

The peer grant shares only current enrolled members of that group. Unrestricted
peers hold it implicitly. The separate agent grant limits which received groups
an agent may inspect; the local operator can inspect every received status.
Status access does not grant mail, session discovery or terminal access.

Summaries include stable agent addresses, names, group-local roles, liveness,
activity and coarse waiting state, harness/model/effort, live subagent,
background-shell and monitor counts, task links/labels, numeric context usage,
last activity and coarse exit/recovery state. Unknown context is `null`. Task
links must be HTTP(S), contain no credentials, and exclude session links;
query strings and fragments are removed. Paths, pane handles, prompts, tool
contents, raw errors, costs, permissions and launch configuration are excluded.

Dashboard, agent listing, context tools and peer publication share one gathered
status cache. Concurrent consumers join an in-progress gather. Its default
freshness is 1500 milliseconds; set `status_snapshot.freshness_ms` in the local
config to a value from 1 to 60000 to change it. Authorization is checked again
when each consumer projects the data. Known local state writes invalidate the
cache immediately, including hooks, session/agent lifecycle, group membership,
task changes and daemon-owned pane actions. The window coalesces repeated polls;
it does not hide a change the daemon has observed. Peer updates reuse the existing session
observer, debounce changes for at least two seconds (or the configured freshness
window, whichever is greater), and gather once for all authorized peers. They
add no independent polling loop. Regular catalogs carry the full status set.

For debugging, `agent ls --no-cache`, `agent context-info --no-cache` (including
`--target` and `--group`) and `agent task-force status --no-cache` force a fresh
local gather and populate the shared cache for subsequent readers. Dashboard
`GET /api/snapshot?fresh=1` and `/api/conversations?fresh=1` do the same; normal
UI polling stays cached. Forced reads share a per-caller limit across status and
host APIs: one in flight and at most one every 1.5 seconds; excess requests
return HTTP 429 with `Retry-After`. Debug logs and forced perf phases identify
these reads. An older in-flight gather is never returned to a forced read.

If the shared cache itself misbehaves, set `status_snapshot.disabled: true` in
`~/.tclaude/data/config.json` to bypass reuse globally. Remove it or set it to
false to restore normal behavior. `freshness_ms: 0` still means the default
1500ms window. These are escape hatches, not recommended polling settings.

Remote observations cannot be refreshed by bypassing a local cache.
`federation nodes` and `federation sessions` do not support `--no-cache`; they
show the latest received peer updates and their freshness. Likewise,
`agent ls --remote --no-cache` refreshes its local portion only.

Remote JSON includes source observation and local receipt timestamps. Status is
marked stale when the peer is offline or either timestamp is over six minutes
old. Idle time is derived from last activity; stale observations freeze it at
the source observation time. A late full catalog cannot replace newer status
updates. Revoking status access removes status independently of other grants.

## Agent grants: what your agents may do remotely

Your operator grants agents ordinary slugs with a required `peer=` scope:

| Agent slug | Remote action |
|---|---|
| `message.direct` | send to a member or group |
| `groups.members.spawn` | request a worker in a peer’s group |
| `agent.spawn` | request workers in any visible group on a peer; peer-only scope |
| `routes.consume` | open a route in a peer’s group |
| `sessions.read` | list live sessions in a peer’s shared groups |
| `agents.status.read` | read shared agent status summaries in authorized peer groups |
| `node.read` | read a peer’s shared instance-wide node metadata (peer-only scope) |
| `agent.share` | share an agent's portable configuration, and optionally its transcript, with a peer; another agent's also needs `agent.bundle.export` |
| `agent.move` | move an agent to a peer, with ordinary retire authority over it |
| `self.teleport` | move or clone only yourself to a peer; no grants travel |
| `self.teleport.permissions` | opt in to carrying permissions on your own teleport; needs receiver approval |
| `sessions.watch` | watch a shared remote agent's terminal |
| `sessions.attach` | full keyboard access to it, including approval prompts |
| `sessions.takeover` | disconnect its other viewers, with `sessions.watch` or `sessions.attach` |
| `sessions.files.read` | download bounded non-secret project files through a live terminal viewer |
| `jobs.run` | request and inspect remote one-shot jobs |
| `models.proxy` | use a peer's named model gateway (scope `peer=` and `http_proxy=`) |

Peer grants for roster, presence and attachments control what the receiving
instance shares. Agents do not need separate roster, presence or attachment
grants for these remote operations.

### Agent peer views: lifecycle and settings on a peer

An agent can also act on a peer's agents and groups through the peer view
API (`/v1/federation/peer/{node}/{tail...}`), the way its operator can from
the dashboard. Two gates apply, and **both** must pass:

1. **On your node**, the agent holds the route's slugs with an explicit
   `peer=<peer>/<remote-group>` (or `peer=<peer>`) grant covering every
   group the route touches. For an agent route that is every group your
   node's latest catalog of the peer shows the target in, plus a clone's
   destination group. Nothing is granted by default. An unscoped grant, a
   `group=` grant, ownership and agent defaults never count, even toward a
   peer you trust unrestricted. An explicit deny wins. One missing group
   can still be approved once with `--ask-human`; more than one is refused.
2. **On the peer**, the request runs under an *agent principal*. The peer's
   own grants to your node still decide, as for your operator. An agent
   never gets the peer's local-operator paths, not even when the peer trusts
   your node unrestricted. Your node sends the groups it checked, and the
   peer refuses a request that touches any other group (`agent_footprint`),
   so a stale catalog cannot widen what the agent reaches. Its operator can turn agent views off for your
   node with `tclaude federation agent-views <peer> deny`. The default is
   `allow`, because gate 1 already requires an explicit grant per slug.

| Route on the peer | Agent slug (`peer=` scope) |
|---|---|
| `GET snapshot`, `groups`, `groups/{name}`, `agents/{id}` | `agents.status.read` |
| `GET fleet` | `fleet.read` (scope `peer=<peer>`; the peer grants `fleet.read`) |
| `GET v1/process/runs`, `v1/process/runs/{id}`, `…/events` | `process.runs.read` (scope `peer=<peer>`; the peer grants `process.runs.read`) |
| `POST v1/process/runs` with `group` · with `agent` | `process.runs.start` (that group) · `agent.process` (every group the agent is in). The run is owned by your node's agent (`peer:<instance>/agent:<id>`) |
| `GET groups/{name}/process` · `POST groups/{name}/process/advance` | `agents.status.read` · `process.advance` |
| `POST v1/process/runs/{id}/pause` · `resume` · `resolve-blocked` | `process.runs.manage` (scope `peer=<peer>`). Not `decide` (a decision is a human gate, the peer's operator's) and not `assign` (the assignee is named in the body, outside the footprint your node checks): both are the peer's operator's |
| `POST v1/cron` | `groups.messages.schedule` for a `group:NAME` target, `agent.schedule` for an agent target (scope `peer=<peer>/<group>`, in every group of the agent). The job is the agent's own there |
| `GET v1/cron` · `GET v1/cron/{id}/logs` · `PATCH`/`DELETE v1/cron/{id}` · `POST v1/cron/{id}/enable` · `disable` · `run-now` | `self.schedule` (scope `peer=<peer>`): an agent lists and manages only the jobs it scheduled there; its node's operator manages them too |
| `POST agents/{id}/stop` · `resume` · `restart` · `retire` · `clone` | `agent.stop` · `agent.resume` · both for restart · `agent.retire` · `agent.clone` |
| `POST agents/{id}/rename` · `tags` · `task` · `notify` | `agent.rename` · `agent.tags` · `agent.task` (notify too) |
| `POST agents/{id}/interrupt` · `compact` · `reincarnate` | `agent.interrupt` · `agent.compact` · `agent.reincarnate` |
| `GET agents/{id}/context-info`, `GET`/`POST agents/{id}/prs` | `agent.context-info`, `agent.pr` |
| `PATCH groups/{name}/members/{conv}` | `groups.members.update` |
| `POST groups/{name}/shutdown` · `power-on` | `groups.members.stop` · `groups.members.resume` |
| `PATCH groups/{name}` with `descr`, `max_members`, `notify_enabled` | `groups.settings.description`, `.max-members`, `.notifications` |
| `POST groups/{name}/rename`, `GET`/`POST groups/{name}/attachment` | `groups.rename`, `groups.attachment` |

```bash
tclaude agent permissions peer-reach add ops-lead --peer bob/builders --slug agent.stop --slug agent.resume
tclaude agent permissions peer-reach add ops-lead --peer bob --slug agents.status.read
```

`peer-reach add` adds the peer to the agent's existing grant of each slug.
It refuses a grant the agent holds for local use, rather than replacing it.
Both `peer-reach add` and `permissions grant --scope peer=…` warn for
`agent.stop`, `agent.retire` and `groups.members.stop`, which end sessions
on the peer, and for `agent.resume`, which a restart also needs.

From the CLI, the member verbs take `member@peer`, where the member is an
`agt_…` ID or a name the peer's catalog shows exactly once:

```bash
tclaude agent stop builder@bob
tclaude agent resume agt_0123456789abcdef@bob
tclaude agent retire builder@bob --no-shutdown
tclaude agent rename "builder (night shift)" --target builder@bob
tclaude agent task set https://example.com/t/42 --target builder@bob
tclaude agent compact --target builder@bob
tclaude agent interrupt --target builder@bob
tclaude agent context-info --target builder@bob
tclaude agent present-pr https://github.com/org/repo/pull/7 --target builder@bob
tclaude agent reincarnate "unused" --target builder@bob
tclaude agent fleet --remote bob
tclaude process runs ls --remote bob
tclaude process show run_0123 --remote bob
tclaude process run release-check --group builders --param issue=42 --remote bob
tclaude process pause run_0123 --remote bob
tclaude process decide run_0123 --node approve --verdict approve --remote bob
tclaude agent cron ls --remote bob
tclaude agent cron logs 12 --remote bob
tclaude agent cron disable 12 --remote bob
tclaude agent cron add --target group:builders --interval 1h --body "post your status" --remote bob
tclaude agent cron rm 14 --remote bob
tclaude agent triggers ls --remote bob
tclaude agent triggers show reviews --remote bob
```

The same commands work for the operator, through the operator's own peer
view and the peer's grants. A local agent whose title is `builder@bob`
always wins over the peer, and two of them are an ambiguous selector: the
verb never falls through to the peer.

Options that stay the peer operator's are refused locally:
- `resume --recreate-dir` and `--send-keys`;
- `rename --auto`;
- `task show` (read the agent with `agent ls --node`);
- a follow-up on `compact` (it would be typed into the peer's pane).

A remote `reincarnate` asks the agent to reincarnate itself with its own
handoff: the follow-up the CLI requires is not sent, and the reply says
so. A remote `present-pr` does not send your working directory.

`retire` never deletes a peer's worktree or records a reason; the reply
says so. Replies are marked as remote and untrusted: the CLI prints an
untrusted-content line first, and `fleet --remote --json` carries
`remote.untrusted`. Control characters are stripped from replies and errors.
An agent's view metadata (`peer_view`) names `principal: agent` and lists
only what agent views reach, for the groups its node checked its grants for.

An agent that already holds `fleet.read` for this node, such as the Steward,
keeps that grant: one grant per slug cannot hold both a local and a peer
scope. Give it the peer reach through one of its groups instead:
`tclaude agent permissions peer-reach add --group <its group> --peer bob --slug fleet.read`.

Everything else on the peer view stays the operator's: moves and teleports,
spawns (agents use the spawn request), sessions and shells, node management,
environment and sandbox, and every group setting that shapes future members
(startup context, saved prompts, default directory, profile and spawn target,
member permissions, owners, nesting).

Other limits:

- An agent gets at most 60 peer view attempts a minute, refused ones
  included, and 4 at once. Past the limit, one refusal per minute is
  audited.
- A peer that does not advertise agent views (an older tclaude) is refused
  locally with `peer_lacks_agent_views`. It never receives the request,
  because it would serve it as your operator's. A peer that answers without
  confirming the agent principal, for example one downgraded since its last
  catalog, is refused the same way, before the request is sent.
- An agent's reads carry no group or member settings and no spawn catalogs
  or defaults, even where the peer's grants would show them to its operator.
- Both nodes audit the agent, reads and refusals included:
  `federation.peer_view.agent.out` on yours, and
  `agent:<name>/<agent-id>@<node>` on the peer's.
- A reply carries `X-Tclaude-Remote-Content: untrusted`. What a peer returns
  is remote content, so treat it like remote mail.

## Discovery: what your agents may address

```bash
tclaude federation remote                         # what peers share with you
tclaude agent permissions grant lead message.direct --scope peer=bob/builders
```

Agents see remote members next to local ones with `tclaude agent ls --remote`.
The remote section shows each member's address, harness, role, presence and
remote group. An agent sees catalog groups covered by its effective remote
mail, spawn or route grants; the operator sees all received catalogs.
Peer-scoped grants assigned to a local group give its current members that
remote reach. Presence is marked
stale when the peer is offline or its catalog has not refreshed for several
minutes. With `--json` the output is `{"local": [...], "remote": [...]}`.

Federation scopes use `peer=<label-or-instance-id>[/<remote-group>]`.
The daemon resolves labels at grant time and stores the full trusted instance
id, so renaming or reassigning a label cannot redirect the grant. Unknown peers
are refused. Remote group names need not appear in the current catalog when
granting: a catalog may be stale. A peer-only scope covers all that peer's
groups, including future groups. `peer=` cannot be combined with another
scope dimension; put the remote group after `/`, not in a separate `group=`
scope. Git and GitHub scopes continue to use `remote=`.

An agent or group holds one grant per slug, so `grant --scope peer=…` replaces
the peer groups it held for that slug. To add or remove one peer group and
keep the rest, use peer reach (human-only):

```bash
tclaude agent permissions peer-reach add lead --peer bob/builders --slug message.direct --slug sessions.watch
tclaude agent permissions peer-reach add --group ops --peer bob/builders --slug message.direct
tclaude agent permissions peer-reach remove lead --peer bob/builders --slug sessions.watch
tclaude agent permissions peer-reach ls [--peer bob]
```

`add` merges the peer group into each slug's grant in one transaction; a
grant that holds the slug for local use, or a deny, is refused (409
`peer_reach_conflict`, nothing written) instead of being replaced, and the
usual way out is to give the reach to a group the agent is in. `remove`
drops the grant when its last peer group goes. An agent's own grant of a
slug takes precedence over its groups' grants of it, so a member with its
own peer reach (or a deny) for a slug does not get a group's: `add --group`
names such members in its `warnings` (printed on stderr, and shown in the
dashboard). The route is
`GET|POST /v1/permissions/peer-reach` (`/api/federation/peer-reach` from the
dashboard, local-only) with `{target|group, slugs, peer, remove?}`; both
answer `{rows:[{kind, group|conv, agent_id, title, slug, peers}]}`.

Unscoped grants, local `group=` scopes, defaults and group ownership never
authorize remote actions. Peer-scoped grants never authorize local actions.
Denies are unscoped and block the slug on both local and remote actions.
Operator actions and replies keep their own authority, and one-shot
`--ask-human` approval remains available.

## Peer nodes

Share a path-free node advertisement with a peer, then find suitable machines:

```bash
tclaude federation grant bob node.read
tclaude federation node-labels --add gpu --add test-rig
tclaude federation node-labels --remove test-rig
tclaude federation nodes --match os=darwin,label=gpu,harness=codex
tclaude federation nodes --json
```

`node.read` is an unscoped **instance** peer grant; group scopes are rejected.
Unrestricted peers hold it implicitly. The ordinary agent `node.read` grant
requires a peer scope to read a restricted peer's node:

```bash
tclaude agent permissions grant lead node.read --scope peer=bob
```

A group-specific scope such as `peer=bob/builders` cannot expose instance-wide
metadata. Reading node metadata does not grant spawn, mail, session or approval
rights. `GET /v1/federation/nodes?match=…` applies the same scope checks. Matches
are ANDed; supported keys are `os`, `arch`, `label` and `harness`. Unknown keys
are errors. An empty list can mean no peer shares metadata or no authorized
peer matches. Offline and stale nodes remain visible with explicit markers.

The optional catalog node block contains OS/version/architecture, tclaude
version, installed registered harnesses and versions, a label set, configured
maximum live agents, and numeric CPU/load, RAM, data-disk, work-disk and agent
counts from [local host status](host.md). It includes no hostname, local paths,
raw probe errors or group names. A failed version probe leaves the installed
harness's version empty. Labels contain 1–64 letters, digits, dots, dashes or
underscores, with at most 64 labels. The local set uses incremental add/remove
operations and can also be updated by future node-profile writers.

The regular catalog includes the advertisement; a separate `node_update`
refreshes it every 30 seconds while connected with an online peer holding
`node.read`. Updates reuse the local host cache. Installed harnesses and OS
version are probed at most once every five minutes while sharing is active;
API reads never run probes. An unreadable local config withholds the node
advertisement rather than publishing an unknown maximum as unlimited. Missing resource readings are JSON null, not zero.
Work-disk byte and percentage minima are independent summaries across required
work roots, unavailable if a required root could not be measured. Available
macOS RAM is explicitly marked as an estimate. Source observation time and a
receiver-owned receipt time prevent an old observation from becoming fresh
through an update. Nodes are stale when offline, the update or host observation
is over 90 seconds old, or the source marks the snapshot warming/stale.
Node updates do not refresh group roster/presence timestamps.

Configure capacity in `~/.tclaude/data/config.json`:

```json
{"federation":{"max_live_agents":8,"node_labels":["gpu"]}}
```

Zero (the default) means unlimited. The maximum is enforced when admitting spawns, jobs and
teleports, although it isn't a resource reservation. `capacity.max_live_agents` is the same
cap for the whole instance and takes precedence when set; see
[Capacity limits and the spawn queue](spawning-and-lifecycle.md#capacity-limits-and-the-spawn-queue). Existing peers without a node block
continue to work and do not appear in `nodes`.

## Local node pools

Pools collect trusted peers under a local name. They are operator-managed and
are never published to a peer or hub. Pools cannot contain other pools.

```bash
tclaude federation nodes groups create test-rigs
tclaude federation nodes groups add test-rigs bob
tclaude federation nodes groups ls --json
tclaude federation grant group:test-rigs node.read
tclaude federation grant group:test-rigs groups.members.spawn --scope group=builders
tclaude federation grants bob
tclaude agent permissions grant lead agent.share --scope peer=group:test-rigs/builders
tclaude federation nodes groups rm test-rigs bob
tclaude federation nodes groups rm test-rigs
```

`group` is an alias for `groups`. Adding a member immediately applies every pool
grant; removing or untrusting a peer immediately removes inherited authority.
Direct grants remain additive. Listing a concrete peer's grants shows inherited
policies and their pool names; listing `group:test-rigs` shows the pool policy.
Untrusting also deletes membership, so trusting the same peer again does not
restore it. Each pool has an immutable ID. Deleting and recreating its name
cannot revive old grants or agent permission scopes.

Agent `peer=group:<pool>` scopes are resolved against current trusted membership
on every check, after a concrete peer has been chosen. An optional `/builders`
suffix restricts the remote group. Missing pools, removed members and database
errors fail closed. Stored scopes use the immutable pool ID rather than its name.
The placement seam accepts `group:<pool>` and reports an explicit error for a
missing or empty pool; placement still applies the requested capability filters.

For spawn policies, the most specific local group scope wins. Among equally
specific matches, an explicit peer policy takes precedence over pool policies.
Unequal pool policies at the winning specificity block spawning until the
operator resolves the conflict. `max_live` remains a per-peer limit, rather than
a shared pool budget. Pool authority changes also invalidate pending delegated
approval epochs, preventing a removed and re-added member from answering an
old approval request.

## Node profiles and trust defaults

A local node profile describes a peer's trust level, local pool memberships,
peer grants and launch policies, requested labels, an optional config bundle,
and permission overrides for workers that peer spawns here. Profiles and their
immutable IDs/revisions stay on this instance. Pool references bind to immutable
pool IDs and use the pool's live policy; deleting and recreating a pool name
does not retarget a profile.

Create a profile from JSON:

```json
{
  "definition": {
    "trust_level": "restricted",
    "pools": ["test-rigs"],
    "peer_grants": [
      {"slug": "message.direct", "scope": "group=lobby"},
      {"slug": "groups.members.spawn", "scope": "group=builders",
       "spawn_policy": {"max_live": 2}}
    ],
    "labels": ["test-rig"],
    "worker_permissions": {
      "groups.members.stop": {"effect": "grant", "scope": {"group": ["builders"]}},
      "self.rename": "deny"
    }
  }
}
```

```bash
tclaude federation profile create rigs --file rigs.json
tclaude federation profile show rigs > rigs.json
tclaude federation profile apply rigs bob                 # preview
tclaude federation profile apply rigs bob --apply         # commit local policy
tclaude federation profile default rigs
tclaude federation trust <instance> --label rig2          # displays default profile
tclaude federation trust <instance> --profile other
tclaude federation trust <instance> --no-default-profile
```

Defaults apply only when first trusting a peer. Updating an existing peer's trust
does not silently reapply the current default. A profile selecting unrestricted
trust still requires the peer fingerprint confirmation. `profile default none`
clears the default; `profile ls` lists profiles and the selected default.

Edit the JSON returned by `show`, preserving its revision, then use
`profile update rigs --file rigs.json`. `profile apply rigs --all` previews
changes for assigned peers; add `--apply` to commit each peer's displayed plan.
The plan highlights security changes, includes live pool grants, and binds the
profile revision and current peer state. Stale plans are refused. Re-apply
removes obsolete profile-managed grants and memberships while preserving
unrelated manual entries. Conflicting manual edits are reported and refused;
resolve them before applying. Changing a profile alone does not change a peer.
Deleting a default or assigned profile is refused until it is no longer
referenced; assign a replacement profile or untrust its peers first.

Labels and config are a separate, retryable offer:

```bash
tclaude federation profile offer rigs bob
```

Applying a profile never imports settings remotely. The receiver uses the
existing config-offer preview/import path and decides whether to accept it.
An unchanged live or applied offer is not resent by re-apply or another `offer`
command. Expired, declined or removed offers can be retried with `offer`.
`labels: null` leaves labels unspecified; `labels: []` requests clearing them.
Only `federation.node_labels` is portable here; federation credentials, trust
and other authority settings remain excluded from config bundles. Optional
`config_bundle` content uses the ordinary versioned config-bundle envelope,
including its credential checks and path placeholders.

Worker defaults apply to both automatically approved and operator-approved
peer spawn requests. They overlay role and spawn-profile overrides, are
persisted before the subprocess starts, and record the originating peer and
profile revision. They are frozen at birth: re-applying a profile affects future
workers and leaves existing workers' permissions intact. Harnesses without
enrollment before launch are refused only when the applied profile has nonempty
worker permission overrides. Peers with no worker defaults retain ordinary
spawn behavior on every harness.

An omitted worker slug inherits the normal receiving-group and global defaults.
A scoped worker grant narrows a broader receiving-group grant **because the
existing per-agent permission tier takes precedence over the group tier**;
these two scopes are not unioned. A worker deny takes precedence over group,
global-default and owner grants. Ordinary explicit operator sudo keeps its
higher precedence, and existing owner-derived scope behavior is unchanged.
Delegation still uses the normal attenuation rules. Remote requesters cannot
supply or override this receiver-owned permission map.

## Enroll a node with a token

Enrollment sets up reciprocal trust after both machines have joined the hub.
Hub admission remains a separate step; an enrollment token is not a hub invite.
On the master, create a node profile and issue a short-lived bearer:

```bash
tclaude federation enroll-token create --profile test-rig --uses 1 --ttl 24h
```

The token is printed once on stdout; its public terms and master fingerprint
appear on stderr. Deliver it privately to the node. Prefer a private file or
stdin over `--token` to avoid shell history and process argument exposure:

```bash
tclaude federation enroll <master-instance-id> --token-file /private/enrollment-token
# Inspect consent terms without making changes:
tclaude federation enroll <master-instance-id> --token-stdin --preview
```

Running `enroll` is consent. Before sending, it displays both fingerprints, the
profile's name, immutable ID and revision, expiry, and the trust level granted
to the master on this node. `--trust-level unrestricted` when creating the
bearer proposes full master authority: every peer permission on every live
local group, automatic worker spawning, and local unscoped grants towards that
peer. The default is restricted, which requires explicit local grants.

The named profile controls the node's authority **on the master**, including
pool memberships, peer grants and defaults for workers that node spawns there.
The node grants its master only the reciprocal trust level displayed by the
token. Enrollment does not apply the node's default peer profile and does not
automatically accept config offers. Offer/import those separately. Profile
edits invalidate unused tokens; pool grants remain live membership policy.

`tcle1` tokens contain signed public terms, a pinned master public key and a
random bearer secret. The master stores only the public terms and secret hash.
Enrollment requests and replies are signed and sealed end to end; they bypass
the durable outbox and are never logged with bearer content. Only these two
small, rate-limited message kinds can arrive from an untrusted directory peer.
A reply must match an active request nonce and the token's pinned master key.
Trust on the node is written only after authenticated master confirmation.

First use binds a token use to a node key and applies its exact profile revision
atomically. `--uses N` permits N distinct node keys. Retry with the same bearer
and node key after an interrupted exchange: the master returns the existing
receipt without spending another use or reapplying permissions. Manual trust
changes invalidate an in-flight local preview; completed retries preserve
manual downgrades. Untrust retires bindings permanently, so replay cannot
restore trust, even if that peer is subsequently trusted manually.

```bash
tclaude federation enroll-token ls
tclaude federation enroll-token revoke <token-id>
tclaude federation enrollments
```

Lists expose public token IDs, use counts, expiry, keys and retired bindings.
Revocation prevents new bindings and does not untrust enrolled nodes. Completed
master-side retries can recover receipts after revocation or expiry; a node
must present an unexpired bearer to initiate the command. A new enrollment
requires a new token after untrust. Identity rotation changes the pinned key
and therefore requires a newly issued token.

## Stable group grant identities

Grant listings (`tclaude federation grants`, `GET /v1/federation/grants`, and
`GET /api/federation/grants`) return group scopes as `group_id=<id>`, with
`group_id`, `group_name` for a live group, and `group_deleted: true` for an
orphan left by an older version. Use the returned scope unchanged to revoke:

```sh
tclaude federation revoke laptop groups.roster.read --scope group_id=12
```

`group=<name>` explicitly selects a current local group by name, even if that
name is numeric. `group_id=<id>` explicitly selects its stable identity. Grant
creation accepts either form and requires an active group; revocation by ID
also works after deletion. Deleting a group now removes its direct peer and
inherited node-group grants transactionally. Legacy orphaned rows remain
listed and can be revoked by ID. Display names are separate from identity so
renames and numeric-name collisions cannot retarget a listed grant's revoke.

## Per-group federation links

`tclaude federation links [--group NAME] [--json]` reads the same aggregation as
local dashboard groups' `federation_links`: group-scoped direct/pool grants,
route mirrors, and peer trust/online/last-seen. Unscoped grants and unrestricted
trust are not per-group links. Groups without links have an empty array.

The local-only `GET /api/federation/links?group=NAME` (CLI:
`GET /v1/federation/links`) returns `{groups: [{group_id, name,
federation_links: [...]}]}`. The optional filter is an exact local group name;
an unknown name returns 404. Each link has the existing snapshot shape:
`peer`, `label`, `level`, `kind`, `direction`, `slugs?`, `pool?`, `remote?`,
`online`, and `last_seen?`. Peers cannot read this node's other trust links,
including through unrestricted trust.

Additional stable action concept keys are `spawn.inline`, `lifecycle.stop`, `lifecycle.resume`, `lifecycle.restart`, `lifecycle.sandbox-restart`, `lifecycle.retire`, `lifecycle.clone`, `lifecycle.move`, `lifecycle.teleport`.

## Group links and peer proposals

Cross-node group links are exact directed edges between two shared groups. They
allow the source group's current members (or owners, with `owners->members`) to
message the destination group without a broader `message.direct` grant. They
are not transitive. A two-way link has two independent approvals and receipts;
one direction can be active while the other is still pending. The receiving
operator can pre-authorize ingress with `groups.link.accept` scoped to a local
group. Otherwise the proposal appears in Messages > Access requests for 24
hours. Revoking an edge takes effect immediately on that node, including mail
still waiting in its outbox. An offline peer's displayed state is last known.

An operator can create a link with
`tclaude agent groups link add SOURCE REMOTE_GROUP --peer NODE [--bidir]`.
`tclaude agent groups links` lists the edge IDs; use
`tclaude agent groups link rm LOCAL_GROUP xlink_…` to revoke one.
`tclaude agent groups why-can-i-message group:REMOTE_GROUP@NODE` explains the
current path. Older daemons keep their existing local links and federation mail;
new links and proposals require a peer advertising proposal support.

`tclaude federation proposals targets` lists shared groups and their opaque
identities. `proposals create --peer NODE --file items.json` offers a JSON array
of `share_visibility` or `group_link` items; `--file -` reads standard input.
`proposals list` and `proposals show --id ID` show each revision and whose turn
it is. Decisions require the exact `--revision`: `accept --item ITEM_ID`
(repeatable), `decline`, or `counter --file replacement-items.json`. A counter
replaces all items, preserves the original deadline, and requires new approval
for changed authority. Applied items cannot be changed by a counter. The
originator can `withdraw` pending items; already applied shares or edges must be
revoked separately.

A visibility share grants `groups.visibility.read` for that exact group: name,
description and member count, with no message, presence, launch or roster grant.
Proposal counts are snapshots; proposal payloads never contain member names.
`include_members` defaults to false and enables the roster only after acceptance.
The receiver can narrow an accepted share with `--without-members ITEM_ID`.
A share item uses `host: "us"` or `"them"` and `group_id`; a link additionally
uses `source`, `source_group_id` and `mode`, with `host` naming the receiving
node. Local group names are accepted on input; wire identities survive renames
and cannot select a replacement group after deletion.

## Fleet health notices and watching

```bash
tclaude federation nodes --watch
tclaude federation nodes --watch --json
tclaude federation nodes health
tclaude federation nodes health --peer bob --set '{"resources":true,"failures":true}'
tclaude federation nodes health --peer bob --set '{"presence":false}'
tclaude federation nodes health --peer bob --silence
tclaude federation nodes health --peer bob --part-time
tclaude federation nodes health --peer bob --always-on
```

These commands are operator-only. `--watch` prints the current node list, then
live transitions; `--json` emits one initial `{"nodes":[…]}` object followed by
one event object per line. Watch cannot be combined with `--match`. It adds no
polling, resource probes or status-snapshot gathers. Presence comes from hub
directory changes; resource checks consume the existing shared node snapshots;
job and spawn outcomes feed failure counters. The existing federation tick
flushes debounces. A hub connection outage alone is not proof that every peer
went offline. The initial directory establishes a baseline without notices.

Trusted-peer offline/back notices are enabled by default. Watch reports the
transition after a 15-second debounce that suppresses short flaps, but the
operator inbox escalates instead: the first offline notice arrives after the
node has been offline for 5 minutes, then at 30 minutes and 4 hours, then once a
day. Restarts shorter than 5 minutes, such as upgrades, therefore never reach
Messages. "Back online" reaches Messages only when an offline notice went out.
Reminders stop when the node reconnects, when its trust is removed, or when the
operator silences the current disconnection with `nodes health --peer <name>
--silence` or the silence button on the node's Fleet map card. A silenced
disconnection ends without a back-online notice, and the next disconnection
notifies again. Offline nodes' cards show the notices sent and when the next is
due. A node that is often off on purpose, such as a laptop or a machine only
powered up now and then, can be marked part-time: its offline/back notices are
off for good, not just for one disconnection. Use the "Offline alerts: notify |
part-time" switch on its Fleet map card, or `nodes health --peer <name>
--part-time` (`--always-on` undoes it). This sets only `presence: false` in the
peer's own policy, copying the rest from the policy it followed; a peer without
an override therefore gets one, and switching back to notify keeps that
override rather than following the defaults again. The card then shows a "part-time" tag and a grey
"asleep" instead of "offline". Hub-unreachable notices are unaffected. Resource and repeated-failure signals are off by default. Enabled notices use the same operator Messages inbox and desktop
notification channel as `agent notify-human`. Identity rotation observations and
acceptances, teleport lease loss requiring recovery, and successful backup
resumption also appear in watch. Identity notices retain their existing inbox
messages; teleport recovery adds a fleet notice.

`nodes health --set` replaces the selected policy; omitted fields use built-in
defaults, rather than inheriting individual fields from the defaults policy.
Without `--peer` it replaces the defaults for peers without an override. Policies
live under `federation.health.defaults` and `federation.health.peers` in the local
config. Peer keys are immutable instance IDs, resolved from the CLI label.
Accepted identity successors inherit the nearest ancestor settings; a direct
successor override wins. Deleting/re-pairing an unrelated identity does not inherit them.

Available settings (durations are seconds; zero selects the built-in default):

| Setting | Default | Meaning |
| --- | --- | --- |
| `presence` | true | Offline/back notices |
| `resources` | false | Low disk and sustained high memory |
| `failures` | false | Repeated confirmed job/spawn failures |
| `debounce_seconds` | 15 | Delay before a stable transition is emitted (offline inbox notices follow the fixed schedule above) |
| `disk_free_percent` | 10 | Minimum data/work disk free percentage |
| `ram_free_percent` | 10 | Minimum available RAM percentage |
| `memory_seconds` | 120 | Continuous fresh high-memory observations required |
| `failure_count` | 3 | Distinct failed attempts in the window |
| `failure_window_seconds` | 600 | Rolling failure-count window |
| `cooldown_seconds` | 600 | Minimum interval between failure notices |

Resource recovery emits a transition too. Missing, warming, stale, replayed or
withdrawn readings never imply recovery or count toward sustained memory use.
An offline interval or observation gap over 90 seconds breaks memory continuity.
Disk checks use the least available percentage across data/work disks; unknown
disks do not count as zero. Memory readings retain the source platform's available
RAM estimate. Threshold oscillation is debounced.

Failures include validated job execution failure/timeout, interrupted or missing
output, terminal spawn capacity refusal, and confirmed failed spawn launch
attempts. Human denials, cancellations and uncertain startup timeouts do not
count. The optional `spawn_attempt_failed` frame correlates to a local outgoing
request and deduplicates the reserved worker attempt; it never changes the final
spawn decision. Older peers ignore that frame and cannot report launch-attempt
failures. Failure counters and debounce state reset on daemon/federation restart.

Watch events are live, not a durable history. Slow viewers disconnect rather
than holding up federation; rerun `--watch` after disconnection or daemon restart.
Important notices remain in the operator inbox, and identity/teleport audits
retain their existing durable records. Watch survives an in-process federation
reload, including an accepted identity rotation. Revoked peers are checked again
before event delivery.

## Live peer views from the CLI

The operator CLI uses the same pinned peer transport and authorization as the
per-node dashboard. It does not introduce remote administrator authority.

```bash
tclaude federation status --summary --json
tclaude federation nodes --summary --json
tclaude federation nodes --node bob --json
tclaude federation nodes --node self --json
tclaude federation view snapshot --node bob
tclaude federation view 'node-summary' --node bob
tclaude federation view 'costs?page=1' --node bob
tclaude federation view 'audit?page=1' --node bob
tclaude agent ls --node bob --json
tclaude agent ls --node bob --group builders --json
tclaude agent groups ls --node bob --json
tclaude agent groups ls --all-nodes --json
```

`--node` accepts a pinned instance ID, an unambiguous ID prefix of at least
8 characters, or a locally assigned peer label. Summary listings additionally
accept `self` for the local instance. Hub-reported names do not select peers.
Normal `federation nodes` still reads the cached node capability catalog under
its existing `node.read` policy; live summaries and live peer views are
operator reads (an agent reaches only the routes in
[Agent peer views](#agent-peer-views-lifecycle-and-settings-on-a-peer)). The local daemon APIs are `GET /v1/federation/node-summary`
and `GET /v1/federation/peer/{node}/{tail...}`.

A summary listing explicitly fetches self and trusted peers in the CLI, with
at most four concurrent requests, no retries, and deterministic output order.
Peers without `node.read` still supply their authorized shared-group counts;
resource health remains withheld. Each JSON row contains identity, local
label, trust level, directory presence and last-seen time, and either `summary`
(including the peer's omission metadata) or `error` (including its stable code,
reason, and last-seen when supplied). Partial failures return a nonzero exit
status while preserving successful rows. A successful live read marks the
row online even if the cached directory has not caught up.

`agent groups ls --all-nodes` is the CLI side of the dashboard's
fused Groups view with every node ticked: this node's groups and every trusted peer's shared
groups, named `group@node`. Peers are read like a summary listing (at most four
at a time, no retries). JSON carries `nodes` (an `error` on any unreachable
peer) and `groups` rows with `group`, `node`, `node_id`, `members` and
`online`. An unreachable peer keeps the other rows and makes the exit status
nonzero.

For a how-to tour of the dashboard side, see
[Fleet UI](fleet-ui.md).

The dashboard's **⚙ Fleet config** view covers these commands; see
[Fleet administration](fleet-ui.md#fleet-administration-fleet-config).

`federation view` returns the full JSON response, including `peer_view`, and
preserves structured failure JSON on stderr. The `agent ls --node` and
`agent groups ls --node` JSON outputs are envelopes with `agents` or `groups`
and `peer_view`; local listing output retains its existing schema. Remote
text listings describe reported status rather than treating withheld fields
as offline. Remote listings reject `--state`, since permissions can withhold
presence for individual groups. `--archived`, `--no-cache`, and `--remote`
retain their existing local meanings and cannot combine with the remote
listing flags. The peer dashboard audit is read with `federation view audit`;
`federation audit` continues to read the local federation activity ledger.

Proxied responses always keep the local `private, no-store` cache policy.
Peers cannot replace it with a public or long-lived policy. ETag validators
continue to pass through for client-managed revalidation.
