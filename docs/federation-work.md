# Work across nodes

Message agents on other nodes, start workers there, run jobs, reach services, and open terminals.

## Remote sessions

```bash
# Bob shares sessions of agents belonging to builders:
tclaude federation grant alice sessions.read --scope group=builders
# Alice lists them as the operator:
tclaude federation sessions bob
tclaude federation sessions                 # all trusted peers
tclaude federation sessions bob --notify    # print/bell on new waits until Ctrl-C
# To let Alice's lead agent list them too:
tclaude agent permissions grant lead sessions.read --scope peer=bob/builders
```

The listing shows stable `agt_…@peer` session targets, names, shared groups,
harness, state, waiting reason and an observed waiting duration. `--json`
also includes the current runtime session ID. Only group-member agents with
live panes are shared; prompt text, working directories and pane handles are
not. Session access is independent of roster and presence grants. Agents
need `sessions.read` with a covering `peer=` scope.

Waiting reasons are `permission` (permission prompt), `question` (harness
question, such as AskUserQuestion), and `prompt` (idle). Detection follows the
harness's reported state; an unobservable wait is not inferred from terminal
text. `waiting ≥3m` means the observer has continuously seen that state for
at least three minutes, not the precise prompt start time. Observation resets
when the daemon restarts or observation stops during disconnection. Brief
transitions between observations can be missed.

The full session list travels in regular catalogs. While connected, a local
observer checks shared sessions every two seconds and pushes only changed
session snapshots to authorized peers. Instances with no online peer holding
`sessions.read` do no session observation. `--notify` polls the local cache
and reports new waits after its initial listing; it never alerts from stale
data. Disconnected peers and old snapshots are marked **stale**, and stale
waiting durations stop at the last snapshot. Unrestricted peers include
`sessions.read` automatically. This grant shares state only; terminal access
requires its own capability.

## Sending

Agents use the ordinary messaging command with a `member@peer` address:

```bash
tclaude agent message bob-agent@bob "can you review PR 42?"
tclaude agent reply <id> "done"     # replies to remote mail go back over federation
```

A spontaneous remote send requires the peer to share the target group with
`message.direct` and the sender to hold `message.direct` scoped to that peer
and group (or to the whole peer):

```bash
# Bob’s operator: let Alice’s instance mail builders
tclaude federation grant alice message.direct --scope group=builders

# Alice’s operator: let lead mail Bob’s builders
tclaude agent permissions grant lead message.direct --scope peer=bob/builders

# Alice’s lead agent
tclaude agent message bob-agent@bob "can you review PR 42?"
```

Use `--scope peer=bob` instead to authorize that agent on all Bob’s
mail-capable groups, including future groups.

A missing standing grant can use the usual `--ask-human` approval for one send.
Replies to received remote mail need no standing grant. The other side accepts
them because they answer mail it sent from that agent.

The operator can send as the human:

```bash
tclaude federation send bob-agent@bob "hello" --subject intro
```

`member` may be the member's name, its agent id, or an 8+ character id
prefix. `peer` may be the operator-chosen label, the full instance id, or
an 8+ character instance-id prefix. Hub-reported names are not accepted in
mail addresses. An address whose `@…` part does not name a trusted peer is resolved
locally as before, so local titles containing `@` keep working.

`--cc` may name remote members too:

```bash
tclaude agent message carol "release notes attached below" --cc bob-agent@bob --cc dan@bob
```

Each remote cc gets its own copy. It needs the same peer-scoped
`message.direct` grant as a direct send, and all of them are checked before
anything is sent: one refused cc aborts the whole send. A remote message
can cc other remote members, but not local agents. Recipients on another
instance do not see who else received the message.

### Remote groups

An agent can mail every member of a remote group at once:

```bash
tclaude agent message group:builders@bob "release at 5" --role reviewer
tclaude federation send group:builders@bob "maintenance at 6" # as the operator
```

The same rules apply as for a single member: the peer must share the group
with `message.direct`, and you need `message.direct` scoped to its
peer/group. One envelope crosses the hub. The receiving instance
delivers it to the group's members as of arrival, not to the roster in your
catalog. `--role` narrows the recipients there, case-insensitively. It needs
a peer grant of `groups.roster.read` on the group, since the catalog
otherwise hides roles. Members can reply, and their replies come back to you.

Group mail is text only: no `--cc`, `--attach` or member subsets. Each
recipient counts against the peer's inbound mail budget. Delivery is
reported like this:

- When nobody matches, the mail is refused (`no_recipients`).
- When every recipient's backlog is full, it is retried later.
- Otherwise `tclaude federation outbox` shows it as accepted, with
  "delivered to N members" when the roster is shared.

### Attachments

```bash
# Bob’s operator (in addition to the mail grant)
tclaude federation grant alice message.attachments --scope group=builders

# Alice’s lead agent (with its peer-scoped message.direct grant)
tclaude agent message bob-agent@bob "build log attached" --attach build.log --attach shot.png

# Alice’s operator can also attach files when mailing an agent
tclaude federation send bob-agent@bob "see attached" --attach diff.patch
```

The CLI reads the files as the caller, so an agent can attach only what it
can read itself. Files travel inside the encrypted envelope: at most 4 per
message and 512 KiB in total. The receiver accepts them only for a recipient
in a group granting the sending peer both `message.direct` and
`message.attachments`. No separate agent attachment grant is required. It
re-derives each file's name and type itself and accepts only images, text,
Markdown, CSV, JSON, YAML, diffs/patches and PDF; HTML, SVG, archives and
executables are refused. Each peer may keep at most 64 MiB of files on the
receiving side. Received files appear on the message like any other inbox
attachment. Local recipients do not take attachments: send them a path.

## Operator to operator

Operators of two trusted instances can message each other directly; trusting
a peer is the consent. No group peer grant or agent grant is required.

```bash
tclaude federation notify bob "are your agents done with the release?" --subject release
tclaude federation inbox [--unread]          # messages remote operators sent you
```

Inbound operator mail lands in the local operator's inbox: the dashboard
Messages tab (filed under the group `federation:<instance id>`) and
`tclaude federation inbox`. It carries the same untrusted-content banner as
remote agent mail and raises a desktop notification if those are enabled. It
is a plain inbox entry: it cannot answer a permission prompt, an
`--ask-human` request or anything else that needs local approval. At most
100 unread messages per peer are kept; beyond that the sender's outbox
retries. Untrust the peer to stop it entirely. Agents cannot send operator
mail yet. Reply with `tclaude federation notify`, or with **reply** on the
message in the dashboard's Messages tab. The local dashboard can compose or reply to the sending operator
through `POST /api/federation/notify` with `{peer, subject?, body}`; replies use
the inbox row's stable `instance` and a reply subject. The same operation is
`tclaude federation notify` in the CLI. `POST /api/federation/send` mirrors
`federation send` with `{to, role?, subject?, body, attachments?}` for remote
agent/group mail. `GET /api/federation/inbox` and `/outbox` return the CLI inbox
and delivery rows. These dashboard routes require the local operator cookie
and cannot be called through a peer view.

## Human inbox across your own nodes

An operator running several nodes of their own can read and answer each node's
human inbox from one dashboard. A node serves `GET /api/human-inbox` (the
newest 50 unread human notifications and the pending ask-human requests of its own
agents) and the answers `POST /api/human-inbox/reply {id, body}` and
`POST /api/human-inbox/access/{id} {decision: approve|deny}` only to a peer it
trusts unrestricted (`human.inbox.read` / `human.inbox.answer`); these slugs
cannot be granted or requested. Read state,
`POST /api/human-inbox/read {id[, read:false]} | {all:true}`, has its own
grantable instance-wide slug `node.messages.mark-read` (off by default;
unrestricted trust holds it): with it and `node.messages.read`, opening a
notification on the peer's page or in fused Messages marks it read there, as
it does locally. It carries no reply, delete or approval authority. Decisions are one-shot: "always allow" and
extending a deadline stay with the node's own operator, as do peer access
requests, away-cover forwards, process obligations and attachments. An
approval lets the blocked call run with all its effects (a `sudo.*` request
grants what it names for the time it asked); it writes no standing rule. A
reply reaches the agent with the remote-operator banner, and the approval
history names the deciding node (`operator@INSTANCE`). The requesting node
reaches the routes only through its dashboard peer proxy or the operator-only
CLI, and both nodes audit each answer.

The dashboard's Messages tab lists the other nodes' unread notifications and
pending requests under **Other nodes**, read every minute (every five while
hidden). CLI: `tclaude federation human-inbox PEER`,
`tclaude federation human-inbox reply PEER ID --body TEXT` and
`tclaude federation human-inbox decide PEER ID approve|deny`.

## Operator access requests

A trusted peer with an existing active grant can request additional dispatcher
permissions through `POST /api/peer-access-requests`, using the ordinary local
proxy `/api/peer/{instance_id}/peer-access-requests`. Trust alone and public node
summaries do not permit requests. The `permissions.requests` omission names
`peer_access` when this admission condition is missing. Local-only endpoints
cannot be opened by a request.

The JSON body is `{permission, group_id?, reason?, grant_ttl_seconds?}`. A
positive `group_id` identifies an already shared receiving group; zero requests
an unscoped grant. TTL defaults to one hour; zero means permanent and the maximum
is 30 days. The receiving operator can narrow an unscoped request to one shared
group and choose the lifetime. Approval never replaces an existing broader or
permanent grant or its launch policy. Expired grants stop authorizing every peer
read and write through the common grant reader.

A request returns HTTP 202 with `{id, origin_peer, perm, group_id,
grant_group_id, grant_ttl_seconds, status}`. Poll
`GET /api/peer-access-requests/{id}` through the same proxy for its owned status;
other peers cannot read it. Requests time out after five minutes unless the
operator extends them. Pending requests interrupted by a daemon restart are not
approvable; status reads report `interrupted`. Each peer can have eight pending
requests. Approval is a grant, so retry the original action explicitly afterwards.

Receiving operators see peer requests in the existing Access requests folder,
with decisions reserved to the local operator; away-cover peers cannot answer
these trust-administration requests. Rows add `origin_peer`,
`group_id`, `grant_ttl_seconds`, and `grant_expires_at` when applicable. The local
`GET /api/federation/access-requests` lists peer requests and history. Decide via
`POST /api/federation/access-requests/{id}/decision` (or the existing local
`/api/access-requests/{id}/decision`) with `{decision:"approve"|"deny"|"extend",
grant_ttl_seconds?, group_id?, secs?}`. Peer requests do not support agent
"always allow" decisions. These administration routes remain local-only.

CLI parity uses `tclaude federation access request --node bob
--permission message.direct --group-id 7 --reason 'Coordinate the task' --ttl 1h`,
`access status --node bob --id ID`, `access list`, and
`access approve --id ID --group-id 7 --ttl 1h` (or `deny` / `extend`). The CLI
administration routes mirror `/v1/federation/access-requests`. Both nodes audit
requests and terminal decisions without copying the reason into audit logs.
Agents continue to request missing local cross-node permissions from their own
operator using the existing `--ask-human` path.

Peer snapshot and group reads include each visible group's stable numeric `id`,
which is the `group_id` used in permission requests. Omitted feature entries
include `requestable: true|false`, derived from the dispatcher's permission and
transport mapping plus existing-access admission. Local-only features and the
request mechanism itself are never requestable. Features the peer view never
serves itself (the dashboard shell, terminals and their file links, inline
spawn, access requests) carry `transport: true` (others `false`): their own
channel decides whether they work, so the dashboard does not list them as
"not shared". An approval may shorten the
requested lifetime, but cannot lengthen it or convert a finite request to a
permanent grant. A permanent request can be approved with a finite lifetime.

## Away mode and a covering operator

Choose a trusted peer operator to cover while you are away:

```bash
tclaude federation grant bob sessions.read --scope group=builders
tclaude federation grant bob sessions.attach --scope group=builders
tclaude federation grant bob approvals.answer
tclaude federation away --cover bob --until 2h
tclaude federation away                    # show current coverage
tclaude federation return                  # stop forwarding and delegation
```

`--until` accepts a positive duration or a future RFC3339 timestamp. Omit it
for coverage until you return. Away configuration survives a daemon restart;
live delegated approval tickets do not. These commands are human-only.
Setting away warns if the cover is offline or lacks answering grants, but
still enables notification forwarding.

Choosing a cover shares local agents' `notify-human` text and daemon access
request previews with that peer's operator, through signed, encrypted away
notices in their Messages inbox. Group-member sessions entering a question
or permission wait also produce a notice. Notices include the stable
`agt_…@instance` address and a `tclaude federation attach` command to go answer
harness prompts. Session listing and attach enforce their existing grants;
away mode does not grant terminal access. For a notification reply, send mail
to the agent using the usual `message.direct` grant. Attachments stay on the
originating instance; previews are bounded and mark truncation.

Daemon `--ask-human` access requests also carry an exact request ticket:

```bash
tclaude federation answer <request>.<epoch>@<origin-instance> --decision approve
tclaude federation answer <request>.<epoch>@<origin-instance> --decision deny
```

The peer-only `approvals.answer` slug is an **unscoped instance grant**;
`--scope group=…` is rejected because an access request can affect instance
permissions or several groups. It authorizes one-shot approve/deny only, for
an exact live ticket, while that peer is the currently selected cover. It
cannot create an always-allow grant or extend a deadline. The original
request deadline remains binding even if the local operator extends the
local countdown. Return, expiry, revoke, changing cover, untrust, trust-level
changes, and federation/daemon restart invalidate previously issued tickets.
An unrestricted peer holds `approvals.answer` implicitly, like other peer
slugs; choosing it as cover while away is the additional authority gate.

Both instances audit delegated answers, naming the covering operator's
instance as the decider. Submission queues an answer; inspect
`tclaude federation outbox` for the receiving instance's accepted/refused
receipt. The origin also attributes its ordinary approval audit to that
remote operator. Queued notices retry while offline, for at most 24 hours
and never past the request deadline or away expiry. Return or changing
coverage cancels unsent notices from the earlier coverage. Already delivered
notices remain in the covering operator's inbox, but their old tickets lose
authority. Wait observation reuses the session observer only while federation
is connected and away coverage is active; idle instances do no extra session
polling for this feature.

## Remote spawn requests

An agent can request a worker in any group visible to its peer. The receiving
operator controls whether the request runs automatically:

```bash
# Bob’s operator: auto-approve Alice’s requests under a local worker profile
tclaude federation grant alice groups.members.spawn --scope group=builders --profile worker --cwd /work/builders --max-live 2

# Alice’s operator: let lead request workers in Bob’s builders
tclaude agent permissions grant lead groups.members.spawn --scope peer=bob/builders
# Alternatively: tclaude agent permissions grant lead agent.spawn --scope peer=bob

# Alice’s lead agent
tclaude federation spawn-request builders@bob --brief "port the parser to Go" --name parser-port --role worker
```

The requester needs groups.members.spawn scoped to the peer/group, or
agent.spawn scoped to the peer without a group suffix. The group must be
visible in the peer's catalog.

The peer sends a name, role, brief and optional profile choice. Directory,
harness and model overrides come from the receiving peer grant; unset fields inherit
the group's normal operator spawn defaults. The positive live auto-worker
cap defaults to two and counts that peer's live automatically spawned workers
across groups. The receiving spawn rate limit also applies, keyed by peer.
Ordinary group member caps and launch guardrails remain in force.

The receiving operator can expose selected profiles separately from its default:

```bash
# Bob: allow Alice to select these local profiles
tclaude federation grant alice groups.members.spawn --scope group=builders --allow-profile reviewer --allow-profile builder --max-live 2
# Alice: inspect the advertised name, harness, model and effort
tclaude federation remote
# Alice: select one for this request
tclaude federation spawn-request builders@bob --profile reviewer --brief "review the parser"
```

Only allowlisted profiles are advertised, per group; profile prompts, permissions,
environment and gateway settings are omitted. Disabled profiles disappear.
The receiver checks the live allowlist when receiving the request and again
before launch, including manual approval after a failed automatic launch.
Without `--profile`, the existing receiver/group default applies. A grant's
`--profile` pins that default; `--allow-profile` permits named alternatives.
Pinned grant harness/model overrides still apply to those alternatives.
An unrestricted peer, or one granted `groups.members.spawn.profile` (or
`.all`), may also select any enabled profile that is not operator-only,
without an allowlist; operator-only profiles still need `--allow-profile`.
`--credentials local|proxy:<name>@self` overrides the chosen profile's gateway,
as it does for the receiving teleport landing profile. Placement considers
only groups advertising the selected profile.

A visible group without a spawn grant queues the request for human approval.

#### Launch settings a peer may choose

A peer operator spawning from the dashboard (the regular spawn dialog aimed at
your group) may also choose the directory, harness, model, effort, profile,
roles and sandbox profile — but only where you allow it. Each is its own
group-scoped grant, closed by default, and applies together with an automatic
spawn grant:

```bash
# Bob: let Alice pick the model and effort for workers in builders
tclaude federation grant alice groups.members.spawn --scope group=builders
tclaude federation grant alice groups.members.spawn.model --scope group=builders
tclaude federation grant alice groups.members.spawn.effort --scope group=builders
# or every launch setting at once
tclaude federation grant alice groups.members.spawn.all --scope group=builders
```

- An unrestricted peer may choose all of them; its spawn is not a request at
  all but the regular local dialog run against your node (see below).
- `groups.members.spawn.cwd`, `.harness`, `.model`, `.effort`, `.profile`,
  `.roles`, `.sandbox`, `.permissions`, `.environment`, `.owner`,
  `.features`, `.prompts` and `.worktree` each open one;
  `groups.members.spawn.all` opens them all. A setting the peer may not choose is refused (`override_not_allowed`),
  not silently dropped. A requested value wins over the grant's pinned
  `--cwd/--harness/--model`; leaving it blank keeps them. Requested roles
  replace the profile and group roles, and must be roles on this node; a
  requested sandbox profile must exist here and adds an explicit tier (it
  cannot omit your global and group tiers; omitting them is
  `.permissions`).
- The last six ride on the request as `launch`: keys of the ordinary spawn
  request (`approval`, `tools`, `auto_review`, `ask_user_question_timeout`,
  `trust_dir`, `permission_overrides`, `sandbox`, `sandbox_implementation`,
  `allow_unenforced_sandbox`, `omit_sandbox_profiles`; `environment`;
  `is_owner`; `include_group_context`, `context_features`, `remote_control`,
  `auto_memory`, `peer_messaging`, `copilot_api`, `codex_app_server`,
  `fast_mode`, `ssh_workaround`, `auto_compact_window`, `context_window_max`,
  `descr`, `task_ref_url`; `prompt_refs`; `worktree_path`,
  `worktree_branch`). They are laid over the launch this node builds, so the
  group spawn path validates them exactly as a local spawn's; any other key
  is refused. A requested description goes ahead of this node's own
  "spawned for" line. Attachments, focus and the break-glass acknowledgement
  are never carried. An approval that launches another harness than the
  requested one drops the harness-specific keys too (the modes, harness
  features and context settings), as it drops the model and effort.
- A field grant extends the spawn grant in force for the group and counts
  only when it is at least as specific (a group scope above an unscoped
  grant, a direct grant above a pool's): a group-scoped spawn grant without
  field grants of its own narrows broader ones, as its policy narrows a
  broader spawn grant's. So a direct spawn grant added beside a pool's (say,
  to change the live cap) needs its own field grants too. On its own a field
  grant shows the peer nothing, and revoking a spawn grant (or removing it
  from a node profile) revokes the field grants with its scope.
- `groups.members.spawn.profile` lets the peer pick profiles that bring
  their own roles, sandbox and approval modes and environment; mark a profile
  operator-only to keep it out.
- These grants replace the earlier `allowed_overrides` list on the spawn grant
  (`--allow-override`); upgrading turns a stored list, and one in a saved node
  profile, into the matching grants.
- Without a spawn grant the request waits for your approval and carries none;
  `tclaude federation requests approve ID --cwd … --harness … --model … --effort …`
  decides them. A request that failed its automatic launch and returned to you
  lists what it asked for (in `tclaude federation requests`, the request JSON's
  `overrides`, and the needs-approval notice). Approve applies only those the
  peer may still choose under the current grant or trust, unless you pass your
  own; revoking or narrowing the grant stops the rest. The requested model and
  effort were chosen for the requested harness, so they are dropped too when
  the launch runs another one (you pass a different `--harness`, or the
  requested harness is dropped) unless you pass your own. The listing
  previews drops as `would_drop` (without your values; `overrides_allowed`
  lets the dashboard's approve dialog recompute as you type), and the approve
  response (success or failure) names them in `dropped_overrides`. Only the values that will
  actually launch are validated, so a stale stored value you replace or that
  gets dropped does not block the approval.

**Allowing `cwd` is broad.** The peer may then start a worker in any existing
directory on this machine, and the worker's sandbox usually grants its launch
directory write access. A requested directory also overrides the grant's pinned
`--cwd`. Allow it only to peers you would let work anywhere on this node.
(Constraining it to a root or prefix is a possible follow-up, not implemented.)

The directory must be a clean absolute path on this node. The harness must be
known here and able to take a brief, so `shell` is never offered. A model may
not start with `-`. A requested harness that conflicts with the harness of the
requested profile, or of the grant's pinned `--profile`, is refused
(`harness_profile_conflict`) rather than silently skipping that profile's
harness-specific settings; ambient group or global default profiles are not
consulted, matching how an explicit harness already skips them on a local
spawn. The grant is checked when the request arrives and again at an automatic
launch; a request that fails that recheck stays pending with the reason and
notifies you, and the ordinary group spawn path applies every launch
guardrail to the result. Peers learn what they may set per group from the
peer view (`GET /api/spawn-options/{group}`), which names your grant's pinned
settings and the group's default directory only to a peer holding
`node.spawn.read` (unrestricted trust holds it).

#### Managing a peer's node

A peer's per-node page (or a fused view whose primary is that peer) keeps the
Groups toolbar's 🧠 global default spawn profile, 🛡 global sandbox profile and
**+ new group** controls; they act on the peer through its own dashboard
handlers when it shares them, and stay greyed with the reason otherwise.

- `node.spawn.defaults.manage` sets or clears both global defaults, choosing
  among the peer's existing profiles (an operator-only one takes unrestricted
  trust); the pickers also need `node.spawn.read`
  to list them, and stay greyed without it. With either grant the peer's
  snapshot carries the two current defaults.
- `node.spawn.profiles.create` saves a new spawn profile from the peer's spawn
  dialog (**Save as profile…**, choosing the node under **Save on**). A
  restricted peer's profile is saved operator-only; editing and deleting
  profiles stay with unrestricted trust.
- `node.groups.create` creates a plain group. A restricted peer sets only its
  name, a parent it can see, description, context, attachment and member cap;
  a directory, environment, profile or repository clone is refused. It sees
  the new group only through an unscoped grant.
- Unrestricted trust holds all three, and also creates groups from a template
  (whose team spawns here) or as a clone of a group.

#### The remote spawn dialog

Under unrestricted trust a spawn into a peer's group (from its per-node page or
the fused Groups view) is the local spawn dialog run against that peer: the
peer's groups, profiles, saved prompts (with **Manage…** editing the peer's
library), worktrees, queue and spawn route, as on its own dashboard. Only
what works on this machine stays here: **Browse…** for directories (type the
path), and saving a profile from the fused view. Auto focus opens the new
agent in the browser terminal instead of a window on the peer's desktop.

Otherwise spawning into a peer's group sends a spawn request: it opens the
regular spawn dialog with every control in place. Where the peer has not
granted you `groups.members.spawn` on the group, its spawn button is outlined
and labelled "request", and clicking it first asks whether to request an agent
from the peer's operator; the dialog then reads "Request an agent" and sends
with **Send request**. The peer's operator reviews it on their Spawn requests
page (below), and the answer arrives in your Messages. Controls the
request cannot carry are disabled, and hovering one says why; the spawn still
works, the peer applying its own value there.

With `node.spawn.read` on the peer (unrestricted trust holds it), the dialog
reads the peer's catalogs and pre-fills what the peer would apply: its default
or pinned profile, the grant's pinned directory, harness and model, its
sandbox and launch defaults, its roles, and its profile and group saved
prompts. Disabled controls show those values. Without it the dialog cannot
show them and says so; the peer's defaults apply all the same.

Profiles and saved prompts come from two libraries, the peer's and this
node's (never a third node's), and a "Show from" filter narrows both pickers:

- A peer profile rides by name when you may select it (see above); one you
  may not select is listed but cannot be picked. A pre-filled default is not
  sent: the peer applies it itself.
- A local profile fills only what the request carries: the overrides the
  grant opens, the name, display role and initial message, and its saved
  prompts.
- Saved prompts the peer applies itself (its profile and group defaults) are
  locked. Any other prompt, the peer's or this node's, is written into the
  brief ahead of the initial message, within the 8 KiB remote brief limit.
- With `groups.members.spawn.prompts` the peer's prompts ride by name
  instead, so its own defaults can be dropped like any other; this node's
  prompts are still written into the brief.
- Roles and sandbox profiles are the peer's; choosing them needs
  `groups.members.spawn.roles` and `.sandbox`, plus `node.spawn.read` for
  the lists to choose from. A remote spawn cannot remove every role (the
  peer would apply its profile and group roles instead).
- The permission and sandbox modes, environment, ownership, harness
  features, context and description unlock under their grants. The harness
  controls show the peer's harnesses, models and modes (from
  `GET /api/spawn-launch-catalog`), not this node's.
- With `groups.members.spawn.worktree` (and `.cwd`) the worktree controls
  list the peer's repository worktrees and branches, and a new worktree is
  created on the peer just before the request is sent, as a local spawn
  creates one. The folder browser reads this machine, so paths are typed.
- Focus, attachments and queueing only mean something on this node and stay
  disabled.

The local dashboard mirrors the CLI handlers at `/api/federation/spawn-requests`:
GET returns the incoming request array; POST queues an outgoing request with the
same `{peer, group, brief, name?, role?, profile?, credentials?, node?, require?,
prefer?}` body as `/v1/federation/spawn-requests`. POST `/{id}/approve` accepts
optional name, profile, cwd, harness, model and effort overrides; POST `/{id}/deny`
accepts an optional reason. POST `/{id}/abandon` requires
`{"acknowledge_late_worker":true}` before returning an unconfirmed incoming
launch to pending. A late worker may still appear.
`GET /api/federation/outbox?limit=100` returns the same outgoing delivery rows
as `/v1/federation/outbox`, including spawn requests and operator mail. This is
delivery status; the remote operator's spawn result arrives in the local inbox.
All these dashboard routes require the local operator cookie and are refused
through peer views.
An automatic spawn with a definite failure, including a worker cap or rate limit, also
leaves the request pending and notifies the operator; it is not retried
without a human decision. Every automatic spawn notifies the operator inbox.

Slow launches remain durably `launching` until their reserved worker identity
has enrolled and its pane is ready. They occupy a cap slot across daemon
restarts, and cannot be approved again. If startup stays unconfirmed, the
operator gets a notice. Explicit abandonment returns it to pending:

```bash
tclaude federation requests abandon 7 --acknowledge-late-worker
```

Abandonment requires acknowledging that a late worker may still appear.
Inspect the original launch before approving another attempt.

Request states are `pending`, `launching`, `approved`, `denied` and
`expired`. The default list shows only pending requests; use `--all` to see
launching and decided requests.

```bash
tclaude federation requests --all
tclaude federation requests approve 7 [--profile p] [--cwd dir] [--harness h] [--model m] [--name n]
tclaude federation requests deny 7 --reason "no capacity this week"
```

The worker joins the requested group with the remote brief bannered as an
outside request. The decision travels back to the requester's inbox. A peer
may have at most ten undecided requests, expiring after 72 hours. Hidden groups
are refused like missing groups. Untrusting the peer makes pending requests
unapprovable.

## Automatic worker placement

Use node metadata to choose a suitable trusted peer before requesting a worker:

```bash
tclaude federation spawn-request --node auto --group builders --require 'os=darwin,harness=codex,label=test-rig' --prefer least-loaded --brief "run the native tests"
tclaude federation spawn-request --node group:gpu-pool --prefer most-free-ram --brief "build the model" --json
```

`--node auto` considers trusted peers; `group:<pool>` considers the current
members of a local node pool. `--require` accepts comma-separated `os`, `arch`,
`harness` and `label` matches. `--prefer` defaults to `least-loaded` (one-minute
load divided by logical cores); `most-free-ram` ranks available RAM. Instance
IDs break ties consistently. Omit `--group` only when a candidate exposes
exactly one group the caller may request workers in.

An agent needs `node.read` scoped to each candidate peer, plus the usual
`groups.members.spawn` or `agent.spawn` permission. The explanation lists
visible candidates, ranking readings, rejection reasons and attempted sends;
`--json` preserves structured rows even when no candidate qualifies. Peers
outside the caller's `node.read` scope are omitted. Grants and pool membership
are checked again before each send, and deleting/recreating a pool cannot
redirect a request already in progress.

Offline peers, missing or warming metadata, observations older than 90 seconds,
and missing readings needed for the chosen ranking are excluded. Receivers
must advertise placement admission support. Placement does not grant automatic
launch: the receiving operator's policy or human approval still decides.
Requirements travel with the request and are checked against the receiver's
actual launch harness and node settings. They never override its profile,
harness, directory or model. An incompatible automatic policy refuses the
request; a human approval must choose a compatible harness.

The receiver authoritatively enforces `federation.max_live_agents` across live
agents and reserved launches, including pending remote requests, all peers and
both manual and automatic approvals. Local managed launches use the same gate.
Zero means unlimited. Pending requests hold capacity until decided or expired;
launching reservations survive restart. Unconfirmed capped local launches stay
in the Pending list until their pane is ready or the launch wrapper reports failure.
Deleting one whose pane is unconfirmed requires inspecting the launch and
explicitly acknowledging a possible late worker through the dashboard API
(`POST /api/pending/delete/<label>?acknowledge_late_worker=1`). Direct peer requests also obey this
node cap.

Only a definitive `node_busy` refusal, before the receiver creates a request or
launch, permits trying the next candidate. Other refusals stop selection. A
missing receipt or timeout leaves delivery uncertain and also stops selection;
inspect the outgoing request rather than retrying on another node.

## Remote one-shot jobs

A peer can run a shell command or a supported coding harness against a Git ref
only after the receiving operator allows both the repository and the receiving
group. The requester supplies a repository alias and ref, never a filesystem
path, Git configuration, environment or launch profile.

On the receiving machine:

```sh
tclaude federation repos add project --url ssh://git@example.com/team/project.git \
  --clone /work/project --group builders
tclaude federation grant master jobs.run --scope group=builders --max-live 2
```

The repo registry records an immutable ID, revision, canonical clone and Git
filesystem identity, and active receiving group IDs. `repos ls` displays them;
`repos update ... --revision N` replaces an entry after an explicit revision
check, and `repos remove project` disables it. A change invalidates queued jobs.
The registry is also the authority seam for future repository transfers.

Each job fetches only the configured URL into a new private Git directory and
checks out the exact resolved commit, detached from the operator's clone.
Hooks, submodules, templates, global Git configuration and executable filters
are disabled. SSH agent authentication can be used; inline HTTPS credentials
and arbitrary Git transport helpers are refused. Private HTTPS repositories
require SSH or a pre-fetched operator clone with a pinned full SHA; credential
helpers are intentionally disabled. Jobs borrow verified clone objects during
fetch, then repack to make the worker checkout independent of that clone. A
failed fetch can use a locally available full SHA, reported as "resolved from
local clone (fetch failed)"; branches never silently fall back to stale refs.

On the requesting machine:

```sh
tclaude federation job run --node linux-box --repo project --ref main \
  --group builders --harness shell --command 'go test ./...' --timeout 1800
```

The command prints the immutable job ID, waits for verified completed output,
and returns the worker's exit status. A task prompt can select a supported
coding harness instead of `shell`. Add `--follow` to print live stdout and
stderr while it runs. A disconnected follower reconnects by per-channel offset;
it never resubmits or cancels execution. The final verified artifact supplies
any missing tail. `--follow` and `--json` are separate display modes.

For one automatically chosen node, use `--node auto` or `--node group:<pool>`,
optionally with `--require os=darwin,harness=codex` and `--prefer least-loaded`
or `--prefer most-free-ram`. This reuses normal placement freshness and ranking.
It selects one node and makes one admission attempt: a busy receiver fails the
job clearly, without queueing or retrying elsewhere. Agent placement also needs
`node.read` for each visible candidate.

For explicit fan-out, repeat `--node`. Multi-node runs require a full commit SHA
in `--ref` so every node checks out the same content. Resolve branches locally
first, for example:

```sh
commit=$(git rev-parse origin/main)
tclaude federation job run --node linux-box --node mac-box --repo project \
  --ref "$commit" --group builders --harness shell --command 'go test ./...' --follow
```

Each node has an independent job ID. The command waits for all nodes and prints
a per-node summary including commit, state and exit status; any failed node
makes the fan-out return nonzero. It does not provide an atomic transaction
across nodes. Live fan-out chunks carry node/channel labels.
Agent callers need `jobs.run` scoped to the concrete peer (including a live
`peer=group:<pool>` scope). An agent can inspect, retry, cancel or read output
only for its own submitted jobs and while that permission still covers the peer.

The receiver applies its grant's profile, harness and model plus the ordinary
receiving-group launch policy. A requested harness cannot override a harness
pinned by the grant. The checkout supplies cwd; a grant's interactive-spawn cwd
is never used to redirect a job. Worker permission defaults are resolved through
the node profile, frozen and installed on a temporary registered worker before
the broker permits execution. A scoped per-agent worker grant narrows a broader
receiving-group grant because the existing per-agent permission tier takes
precedence. Explicit worker denies also retain their normal precedence.

`job ls`, `job status ID`, `job retry ID`, and `job cancel ID` provide recovery
and control. Retrying resends the same immutable request and cannot execute it
twice. A delivery failure never silently chooses another peer. Jobs reserve the
same node-wide admission slots as interactive workers, including preparation,
and count against peer limits. Timeout covers checkout and execution.

Completed logs use a digest and length descriptor, inline for small results and
the encrypted hub stream for larger results. A terminal worker exit is exposed
only after complete matching logs and authenticated stream FIN are verified.
Raw logs stay in private spool storage; CLI text output strips terminal controls.
Output is bounded to 4 MiB per channel; hitting the bound returns exit 125.
Artifacts expire after 72 hours, and the sender can discard its copy once the
requester confirms receipt. Durable job receipts prevent later retries from
executing completed jobs again.

A daemon interruption or unconfirmed broker teardown leaves the job `unknown`
and retains its capacity reservation and checkout. It is never automatically
rerun. After checking that the workload has stopped, the receiving operator can
release that reservation explicitly:

```sh
tclaude federation job acknowledge-stopped ID --acknowledge-stopped
```

This refuses while a recorded worker pane is still live. Untrust cancels
unfinished jobs and prevents further delivery. An uncertain checkout is retained
for inspection after acknowledgement.

For a peer whose individual jobs need operator consent, add
`--job-approval manual` to its `jobs.run` grant. Requests remain `pending` until
`job approve ID`; approval rechecks trust, the repository revision and the
receiving group's current job grant. The default approval policy is `auto`.

## Remote group routes

A [group route](group-routes.md) can be opened from another instance. Grant
the consumer peer `routes.consume` on the publisher's group. On the consumer
instance, an agent needs `routes.consume` scoped to the publisher peer/group
and membership in the local group used for its private mirror:

```bash
# Bob’s operator: let Alice’s instance consume routes in svc
tclaude federation grant alice routes.consume --scope group=svc

# Alice’s operator: let consumer open Bob’s routes
tclaude agent permissions grant consumer routes.consume --scope peer=bob/svc

# Alice’s consumer agent, a member of local group team
tclaude agent routes open api-server/api@bob -g team
```

The consumer gets an ordinary lease and local endpoint, exactly as for a
local route. Neither sandbox changes. On the consumer's side, `agentd`
creates a private mirror route that only the opening agent can see or use,
and serves it itself. On the publisher's side, `agentd` connects to the real
route like any other consumer. `tclaude federation remote` lists each
shared route as `route <publisher>/<name>@<peer>`.

Each TCP connection becomes one **hub stream**. The route id, stream id and
an ephemeral X25519 key travel in sealed control envelopes. The two
instances derive per-direction ChaCha20-Poly1305 keys, so the hub relays
ciphertext it cannot read, and a truncated stream is detected, not mistaken
for a clean end. The hub limits each instance to 128 concurrent streams and
10 MiB/s by default (`tclaude-hub serve --max-streams`,
`--stream-bytes-per-second`).

Remote route connections are [flow-controlled](group-routes.md#flow-control)
end to end: `agentd` reads a hub stream only as fast as the local reader
takes the bytes, so a slow reader on either instance holds back the sender
on the other, through the hub, instead of overflowing a buffer. A stream
whose receiver accepts nothing for 90 seconds is closed; adjust that with
`tclaude-hub serve --stream-idle`.

Authority is checked on both sides, continuously. Within a few seconds of
any of these changes, open connections close and the consumer's lease ends:

- the route is withdrawn or its publisher exits;
- the peer loses its `routes.consume` grant;
- the consumer agent loses its peer-scoped `routes.consume` grant;
- the peer is untrusted;
- the group's membership changes.

A route the peer cannot consume is refused with the same answer as one that
does not exist.

## Remote terminals

Use the stable `agt_…@peer` address from `federation sessions`:

```bash
# Target operator: share discovery and the chosen terminal mode.
tclaude federation grant bob sessions.read --scope group=builders
tclaude federation grant bob sessions.watch --scope group=builders
# Interactive mode permits all keyboard input, including approval answers.
tclaude federation grant bob sessions.attach --scope group=builders

# Viewer operator, or a local agent with the matching peer-scoped grant:
tclaude federation sessions alice
tclaude federation attach agt_…@alice --read-only
tclaude federation attach agt_…@alice
# Ctrl-] detaches this viewer without stopping the agent.

# Target operator: inspect viewers and disconnect one immediately.
tclaude federation viewers
tclaude federation viewers agt_…
tclaude federation kick <viewer-id>
```

For restricted peers, a local agent also needs `sessions.watch` or
`sessions.attach` with `--scope peer=alice/builders`; `sessions.read` is
separate discovery permission. Watch never authorizes typing. Take-over
(below) needs `sessions.takeover`, likewise scoped for a local agent.
Unrestricted trust includes both modes and take-over on all live groups,
including future groups.

A target pane displays `REMOTE WATCH` or `REMOTE INPUT` with the peer label
while viewers are attached. The original pane border options are restored
when the last viewer leaves, or after a daemon restart. Local option edits
are preserved; hiding the indicator disconnects viewers. The target operator's
`viewers` and `kick` commands are human-only. Opening and closing an attachment
are audited on both instances; kicks are audited at the target.

The renderer requires tmux 3.2 or newer and a session with exactly one window
and one pane. The renderer has `ignore-size`, and its PTY has no input path. The window
size stays manual while viewers are attached, then its original sizing policy
is restored.

Attached clients share the window size, like tmux's `window-size smallest`.
Each remote viewer, watch or interactive, reports the size it could show (a
CLI its terminal, a browser its panel), and the target sizes the window to the
per-axis minimum across those viewers and its own local tmux terminals showing
that window, less the status line. A remote viewer's report counts as at least
20 columns by 5 rows, so a peer cannot shrink the window to a sliver for
everyone else; local terminals count at their real size. It recomputes, coalesced, when a viewer joins, leaves or
resizes, and once a second for local terminals. Alone, a viewer simply sizes
the window. Every viewer renders the shared size, never stretched: a browser
whose panel is larger shows the grid at the normal font and says the size is
limited by another viewer. Viewers are told, without naming peers, what else
is attached.

With the `sessions.takeover` grant, `tclaude federation attach --take-over`,
or **Take over** in the browser terminal, disconnects the other clients first,
local tmux terminals included, so the viewer has the window to itself. The
target checks the grant; the viewing node shows **Take over** only when its
cached catalog has it, and refuses `--take-over` otherwise. Displaced remote
viewers close with reason `taken_over`. A remote viewer displaced by a local
web terminal, which force-attaches and detaches every other client, closes the
same way. Without the grant a peer never disconnects anyone. An older peer
ignores sharing and keeps every viewer at the target's size.

Interactive input is
delivered separately to the resolved pane: special keys use a fixed tmux key
table, text is literal, and unknown escape sequences are discarded. It can answer prompts, send
Ctrl-C, or perform any other action available at that keyboard.

Discovery includes an opaque launch incarnation token, so a reused session ID
cannot turn stale discovery into access to a resumed launch. The attachment
pins the live session and pane incarnation. Exit, pane
replacement, reincarnation, loss of group membership, peer untrust, permission
revocation or disconnect closes it; it never follows a new pane automatically.
Permission checks run before input/output and once a second while idle.

Terminal traffic uses the same encrypted hub stream transport as routes.
Explicit credits bound outstanding terminal data to 256 KiB in each direction;
credits return after terminal output or keyboard input is delivered. Each daemon
limits attachments to 32 total, 8 per peer, and 30 opens per peer per minute.
The hub's stream limits and idle timeout also apply.

### Remote terminal file downloads

Grant `sessions.files.read` explicitly on the agent's group to enable browser
file links. Watch and attach do not imply file access; unrestricted trust does.
The browser hello reports `viewer_id` and `files`. Clicking a visible output
path makes a HEAD preflight followed by GET
`/api/federation/terminal-file?terminal=<remote-socket-path>&viewer=<viewer_id>&path=<path>`.
OSC 8 links are not used for remote file access. The route is local and human-only.

The target requires the same live terminal viewer and pinned session throughout
transfer. Reads stay under the working directory recorded when that viewer
attached. Absolute paths must be inside it; parent traversal and symlinks below
the root are refused. Home and system directory roots return `root_too_broad`:
run the agent in a project directory instead. Credential paths (including
configured harness authentication files), `.env` files and `.git/config` are
blocked at every depth. Only regular files up to 32 MiB are accepted. Both nodes
audit reads, and encrypted transfers verify size and SHA-256 before the browser
receives an attachment with `nosniff` and a CSP sandbox. Transfer time is bounded
to two minutes and viewer closure or revoked authority cancels it.

CLI parity uses the same pin and target enforcement:

```bash
tclaude federation file get agt_…@bob reports/build.txt --output build.txt
```

This opens a temporary watch viewer (or interactive viewer when only attach is
shared), without sending keyboard input, and refuses existing local output files.
It needs both terminal authority and the separate file-read grant. Neither API
accepts a replacement session after the original viewer closes.

## Delivery

Mail is store-and-forward. The sender writes a durable outbox row before
anything leaves the machine, then retries with backoff until the peer
acknowledges or the envelope expires (7 days). Receivers deduplicate by
envelope id, so resends are safe.

```bash
tclaude federation outbox     # queued → sent → accepted | refused | expired
```

| State | Meaning |
|---|---|
| `queued` | waiting for the hub or the peer to come online, or retrying |
| `sent` | the hub handed it to the peer; waiting for its acknowledgement |
| `accepted` | stored in the recipient's inbox |
| `refused` | rejected by the peer (for example, a required peer grant is missing); final |
| `expired` | never acknowledged in time |

The wire refusal code `not_exported` means the peer has not granted your
instance that capability for the target group.

Bodies are text, up to 16 KiB; control characters are stripped on receipt. The receiver applies a per-peer rate
limit (30 mails a minute) and the usual unprocessed-message cap per recipient.
A full inbox is retried rather than refused. Inbound remote mail is recorded
in the [audit trail](permissions-and-audit.md) as `federation.mail.in`, and
every operator change under `/v1/federation/*` is audited too.

## Activity audit

```bash
tclaude federation audit
tclaude federation audit --peer bob --since 24h
tclaude federation audit --since 2026-10-08T12:00:00Z --json
# Let a local lead read the same metadata:
tclaude agent permissions grant lead federation.audit.read
```

This is a local, read-only view across both directions: mail and operator inbox
notices, spawns, jobs, routes, attaches, config/agent offers, teleports, model
leases and model requests. Agents need the unscoped `federation.audit.read`
permission; it does not grant access to payloads or to a peer's audit log.
`--peer` accepts a current label or immutable instance ID. `--since` accepts an
RFC3339 timestamp or a positive duration ago. The default is the latest 200
rows; `--limit` permits 1–1000.

Rows from the existing audit trail are marked `event`. Other sources show their
current durable state, ordered by creation time (model leases use their last
activity time). These are status snapshots alongside events, not a new history
of every state transition. Existing retention and cleanup still apply.
Only IDs, actors/targets, groups, operation names, states, timestamps and HTTP
status are returned. Message bodies/subjects, prompts, command arguments,
logs, error details, endpoint paths and credential material are omitted.

Older audit rows recorded display labels rather than immutable peer IDs.
The view associates them only when current labels identify one peer; rows
that cannot be associated remain visible without `--peer`. Durable federation
tables retain exact peer IDs, including after untrust; use that ID to filter
those records after a label disappears.
