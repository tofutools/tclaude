# Federation

Federation links separate tclaude instances so agents on different machines
can find and mail each other. Two cases motivate it:

- **Colleagues**, each running their own tclaude, whose agents need to
  coordinate across machines.
- **One operator with several instances** (a laptop and a build server, say)
  who wants them to talk.

Instances never connect to each other directly. Each `agentd` dials *out* to
a shared relay, **`tclaude-hub`**, so no machine needs an inbound listener and
laptops behind NAT or a corporate network work as-is.

!!! note "Status"
    CLI only. Built: discovery, mail with attachments, mail to remote
    groups, operator mail, automatic or operator-approved remote spawn, and
    cross-instance group routes, remote session state, and remote terminal
    watch and interactive attach. The dashboard shows and manages linked
    nodes too; see [Skynet UI](dashboard.md#skynet-ui-linked-nodes).

For a step-by-step first setup, including the hub's TLS certificate, see
the [setup walkthrough](federation-setup.md).

## Trust model

The hub routes; it is never the authority over what an agent may do.

- **Identity.** Every `agentd` has an ed25519 keypair under
  `~/.tclaude/data/federation/instance.key` (0600). Its **instance id**
  (`inst_…`) is derived from the public key, so nobody — the hub included —
  can swap the key behind an id you have checked.
- **Signed envelopes.** Everything one instance sends another (catalogs,
  mail, acknowledgements) is a signed envelope. The receiver verifies it
  against the key it pinned when it trusted the sender. The hub cannot forge
  or alter envelopes.
- **End-to-end encrypted payloads.** Each payload (mail bodies, catalogs,
  acknowledgements) is encrypted to the recipient instance's key with an
  ephemeral X25519 key and ChaCha20-Poly1305, so the hub routes ciphertext
  only. The ciphertext is bound to the envelope header, so no other peer can
  lift it into an envelope of its own. The encryption key is derived from the identity key, so trusting a
  peer's fingerprint covers both. The hub still sees routing metadata: who
  talks to whom, when, envelope kinds and sizes.
- **A remote instance speaks only for its own agents**, and only reaches
  what your operator granted it.
- **Remote content is untrusted.** Every inbound remote body starts with a
  banner naming its origin, and the sender shows as `name@peer (remote)`.
- **Approvals stay local.** Permission prompts, `--ask-human`, sudo and
  grants are never actionable by a remote party.

Three layers decide what is allowed, and each one can only narrow:

| Layer | Decides |
|---|---|
| Hub | which instances may connect, who can see whom (spaces), rate limits |
| Receiving operator’s peer grants | what your instance may see and do on that peer’s local groups |
| Sending operator’s agent grants | which of your agents may act remotely, scoped to that peer and its groups |

For example, Bob’s operator grants Alice’s **instance** mail access to
`builders`. Alice’s operator separately grants her **agent** `lead`
`message.direct` with `peer=bob/builders`. Both grants are needed for a
spontaneous agent send. Trust alone grants no group access.

## Running a hub

`tclaude-hub` is a separate binary, included in the release archives, or
built with `go install github.com/tofutools/tclaude/cmd/tclaude-hub@latest`.
Run it anywhere every instance can reach: a team server, a cloud VM, or one
of your own machines.

```bash
tclaude-hub serve --listen 0.0.0.0:8470 --tls-cert hub.crt --tls-key hub.key
```

Without `--tls-cert` it serves plain HTTP, which clients accept only over
loopback. Otherwise front it with a TLS-terminating proxy or use the flags
above. Its state is a SQLite file under `$TCLAUDE_HUB_DIR` (default
`~/.tclaude-hub/`). It stores admitted instances, spaces and invites, never
messages.

As a systemd service:

```ini
# /etc/systemd/system/tclaude-hub.service
[Unit]
Description=tclaude federation hub
After=network-online.target

[Service]
User=tclaude-hub
Environment=TCLAUDE_HUB_DIR=/var/lib/tclaude-hub
ExecStart=/usr/local/bin/tclaude-hub serve --listen 0.0.0.0:8470 \
  --tls-cert /etc/tclaude-hub/hub.crt --tls-key /etc/tclaude-hub/hub.key
Restart=on-failure
StateDirectory=tclaude-hub

[Install]
WantedBy=multi-user.target
```

Admitting instances:

```bash
tclaude-hub admit inst_… --space team-a     # by instance id
tclaude-hub invite --space team-a --ttl 24h # or a single-use invite token
tclaude-hub ls                              # instances, spaces, last seen
tclaude-hub spaces inst_… team-a,team-b     # replace an instance's spaces
tclaude-hub revoke inst_…                   # drop it (live within ~15s)
```

Instances see each other only if they share a **space**. Admin commands edit
the database directly. A running hub picks up changes at its next policy
refresh (`--policy-refresh`, default 15s). Per-instance send limits are
`--frames-per-minute` (default 120) and `--bytes-per-minute` (default 8 MiB).
`--open` admits anyone who proves key possession and is for development only.

## Joining

Connection, trust and peer-grant commands are human-only.
`spawn-request` is also callable by agents with a peer-scoped spawn grant.

```bash
tclaude federation identity                  # your instance id: give it to the hub admin
tclaude federation connect wss://hub.example:8470 [--invite tchi_…] [--name alice]
tclaude federation status
```

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
from untrusted instances are dropped unanswered. Untrusting a peer also
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
operator workflow; remote keyboard access does not grant its daemon API. Each side chooses its own level;
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
| `routes.consume` | lists ready group routes and permits opening them |
| `sessions.read` | live agent sessions, harness, state and waiting reason |
| `agents.status.read` | dashboard-aligned agent activity, model, task and numeric context summaries |
| `sessions.watch` | read-only terminal view of a group member agent |
| `sessions.attach` | terminal view and full keyboard input, including harness approvals |
| `node.read` | platform, harness versions, labels and numeric node resources (unscoped only) |
| `costs.read` | complete node-wide cost collection in peer UI requests (unscoped only) |
| `federation.audit.read` | complete node-wide dashboard audit collection in peer UI requests (unscoped only) |
| `approvals.answer` | one-shot access-request answer while selected as away cover (unscoped only) |

Prefer `--scope group=<local group>` to limit access. For the same slug, a
group-scoped peer grant takes precedence over an unscoped grant, including
its spawn launch policy. Revoking the scoped row leaves any unscoped grant
in force.

Nothing is granted by default; agent defaults and group ownership never grant
peer authority. Any grant covering a live group makes it visible in the peer's
signed **catalog**. Archived groups are hidden. Group scopes follow the group's
identity through renames; deleting the group does not authorize a replacement.
Omitting `--scope` grants authority on **every active group, including future
groups**; the CLI warns explicitly. There is no grant to all peers at once.

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

Peer grants for roster, presence and attachments control what the receiving
instance shares. Agents do not need separate roster, presence or attachment
grants for these remote operations.

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

Unscoped grants, local `group=` scopes, defaults and group ownership never
authorize remote actions. Peer-scoped grants never authorize local actions.
Denies are unscoped and block the slug on both local and remote actions.
Operator actions and replies keep their own authority, and one-shot
`--ask-human` approval remains available.

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
mail yet. The local dashboard can compose or reply to the sending operator
through `POST /api/federation/notify` with `{peer, subject?, body}`; replies use
the inbox row's stable `instance` and a reply subject. The same operation is
`tclaude federation notify` in the CLI. `POST /api/federation/send` mirrors
`federation send` with `{to, role?, subject?, body, attachments?}` for remote
agent/group mail. `GET /api/federation/inbox` and `/outbox` return the CLI inbox
and delivery rows. These dashboard routes require the local operator cookie
and cannot be called through a peer view.

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
Even unrestricted peers need an explicit selectable-profile allowlist.
`--credentials local|proxy:<name>@self` overrides the chosen profile's gateway,
as it does for the receiving teleport landing profile. Placement considers
only groups advertising the selected profile.

A visible group without a spawn grant queues the request for human approval.

The local dashboard mirrors the CLI handlers at `/api/federation/spawn-requests`:
GET returns the incoming request array; POST queues an outgoing request with the
same `{peer, group, brief, name?, role?, profile?, credentials?, node?, require?,
prefer?}` body as `/v1/federation/spawn-requests`. POST `/{id}/approve` accepts
optional name, profile, cwd, harness and model overrides; POST `/{id}/deny`
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
for a clean end. The hub limits each instance to 16 concurrent streams and
1 MiB/s by default (`tclaude-hub serve --max-streams`,
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
separate discovery permission. Watch never authorizes typing. Unrestricted
trust includes both modes on all live groups, including future groups.

A target pane displays `REMOTE WATCH` or `REMOTE INPUT` with the peer label
while viewers are attached. The original pane border options are restored
when the last viewer leaves, or after a daemon restart. Local option edits
are preserved; hiding the indicator disconnects viewers. The target operator's
`viewers` and `kick` commands are human-only. Opening and closing an attachment
are audited on both instances; kicks are audited at the target.

The renderer requires tmux 3.2 or newer and a session with exactly one window
and one pane. The renderer has `ignore-size`, and its PTY has no input path. The window
size stays pinned while viewers are attached, then its original sizing policy
is restored. Resizing a viewer never resizes the target. Interactive input is
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

## Upgrading

The envelope format is versioned. Instances on different envelope versions
cannot exchange mail (envelopes are dropped and outbox rows eventually
expire), so upgrade linked instances together. This branch uses version 2.

## Trying it locally

`scripts/federation-smoke.sh` builds the binaries and starts a hub plus two
real `agentd` instances, each in its own temporary HOME and tmux directory.
It pairs them, grants access to a group holding a model-free `shell` agent, grants requester reach
on the other side, and checks that a remote mail is accepted. Run it from a
plain terminal, not from inside an agent: `agentd` decides who the operator
is by inspecting the caller's process tree.

## Limitations

- The hub sees routing metadata (sender, recipient, kind, size, timing),
  though not payloads.
- Remote group mail carries no attachments, and remote cc recipients do
  not see each other.
- Remote routes relay through the hub, so their throughput is bounded by
  the hub's stream limits, and each connection moves at most one window
  (`routes.window_kib`) per round trip through the hub. Connections whose
  local end runs without flow control (an older Linux helper, or
  `routes.flow_control` off) fall back to about 4 MiB of buffering, and a
  sender that outruns the hub's bandwidth for longer resets its
  connection.
- Each remote route connection costs one sealed control frame from each
  instance against the hub's per-instance frame budget (120 a minute by
  default, shared with mail). A peer accepts at most 240 opens a minute.
- Mirror routes cannot be shared onward to another peer.
- Attachments ride inline and are capped at 512 KiB per message; operator
  mail and replies cannot carry them.
- No remote stop, restart, or transcript access. Remote spawn runs under
  a receiving peer grant or waits for that operator’s approval.
- Operator mail is sent from the CLI only; agents cannot reach a remote
  operator.
- One hub per instance. Hub-to-hub federation is a later step.
- Some capabilities are still CLI-only in the dashboard (away cover,
  sending operator mail, spawn requests, remote jobs, offers, moves); see
  [Skynet UI](dashboard.md#skynet-ui-linked-nodes) for what it covers.

## Portable local setup bundles

Setup transfer also works without federation:

```bash
tclaude config export --file setup.json
tclaude config import --file setup.json                 # preview and diff
tclaude config import --file setup.json --apply --replace
```

The versioned JSON envelope records creation time and the creating tclaude
version. Its independently selectable sections are `profiles`, `roles`,
`templates`, `sandbox-profiles`, `default-permissions`, `process-templates`
(current YAML source, without runtime history), and `config` (an allowlist of
portable preferences). Existing component export formats remain embedded in
the bundle. Local identities, group assignments, federation credentials and
trust, remote-access settings, keychain references, and credential environment
entries are excluded. Templates reference the separate roles and profiles
sections; include those sections when bringing dependencies to a new machine.

Both commands accept repeatable `--only section[/name]` and
`--skip section[/name]`. Import previews before/after values and marks changes
to permissions, sandbox policy, roles, templates and launch profiles with a
`security` tag. Nothing is written without `--apply`; conflicting values need
`--replace` or exclusion. Identical items are left alone. Components are applied
in dependency order; an apply failure reports items already written, and does
not roll back earlier components. Sandbox include graphs are imported together.

Structured paths under the exporting user's home use `${HOME}`. Other absolute
paths become named placeholders with their original values in metadata. Preview
shows those originals and lists missing bindings and the exact
`--set name=value` flag to resolve each one (repeatable). Use `--keep-paths` to
retain all exported original paths; explicit `--set` values override individual
originals. Applying with unresolved
placeholders is refused. Prompts, scripts and process sources remain verbatim;
review their machine assumptions before using them on another host.

Export detects common credential patterns in free text, reports the item and
field without printing the suspected value, and refuses unless you exclude
those items or explicitly pass `--allow-flagged`. This is a detection aid, not a
guarantee that arbitrary prose or scripts contain no secrets; inspect the file
before sharing it. Structured credential fields are always omitted.

Agents need explicit `config.export` / `config.import` grants; neither is granted
by default or through group ownership. `config.import` permits changes to
agent authority, including default permissions and sandbox access. Human CLI
callers use the same daemon APIs without those agent grants. Bundles currently
transfer as local files; these commands do not send anything to a peer.

## Portable local agent bundles

Agent transfer uses a ZIP archive, separate from the existing export jobs:

```bash
tclaude agent bundle export reviewer --file reviewer.zip --history
tclaude agent bundle import --file reviewer.zip --cwd "$PWD" --group team
tclaude agent bundle import --file reviewer.zip --cwd "$PWD" --group team --apply
```

Preview is the default. `--apply` creates a fresh local agent through the normal
spawn permission checks and launch path; it requires an existing receiving
`--group`. `--name` overrides the source name. Source group names, roles and
permission provenance are advisory: ownership, permission overrides and role
references never become grants on the receiver. The inline profile includes
launch posture (including sandbox and approval settings), shown in the preview
alongside an explicit security summary and checked by normal receiver policy.
Role guidance, startup context, initial task text and task-reference links travel
as configuration. Source profile names are informational; no receiver profile is
looked up by that name.

`--cwd` remaps the working directory. `--keep-paths` reuses recorded originals
that exist locally; missing directories refuse apply with a remapping hint.
Optional worktree hints can be remapped with `--worktree`. Profile path
placeholders use the same `--set name=value` and `--keep-paths` conventions as
setup bundles. Working directories, branches and paths inside prompts or tool
output are hints, not filesystem contents; no repository or worktree is copied.

`--history` includes Claude Code JSONL or a Codex rollout through the harness's
`HistoryTransfer` capability. Import remints native conversation identity and
remaps known cwd metadata before resuming it as a new local agent. Message and
tool-output text is preserved. OpenCode, Copilot and Gemini currently export
configuration only with a warning; `--skip-history` also requests configuration
only on import. Sidecar databases, credentials, hooks and artifacts are excluded.
Credential detection uses the setup-bundle boundary, reporting counts per kind
and the first three locations. Export refuses flagged text unless
`--allow-flagged` is explicit; history is never sanitized or silently rewritten.
Inspect archives before sharing them.

The version 1 `manifest.json` records format/version, creation time, tclaude
version, configuration and optional transcript length/checksum. Unknown fields
within version 1 are ignored; unknown versions refuse. Archives accept only
`manifest.json` and optional `history/transcript.jsonl`, with bounded compressed
and uncompressed sizes, no symlinks, duplicate entries or arbitrary extraction.
Agents need explicit `agent.bundle.export` / `agent.bundle.import` grants;
neither is default-granted or implied by ownership. Import also requires normal
spawn authority. These commands operate on local files and do not contact peers.

## Offering configuration to a peer

Config offers go to the remote operator, and are never applied automatically,
including between unrestricted peers. On the receiving machine, grant the
sender the peer-only, unscoped `config.offer` permission:

```bash
tclaude federation grant alice config.offer
```

Send either an existing config bundle or a selection of current configuration:

```bash
tclaude federation offer-config bob setup.json
tclaude federation offer-config bob --only roles --only profiles
```

Sending and managing offers are operator-only. The sender rechecks supplied
files for credential patterns and omits structured credentials, using the local
config export boundary described above. Flagged free text refuses sending
unless `--allow-flagged` is explicit; content is never silently rewritten.

The receiver sees offers in `federation inbox` and manages them with:

```bash
tclaude federation offers
tclaude federation offers import <id>                  # fetch if needed, then preview
tclaude federation offers import <id> --only roles --apply
tclaude federation offers decline <id>
```

Preview includes the source peer, offer ID, digest, expiry, before/after diffs
and security tags. Import uses the existing config import path: `--only`,
`--skip`, repeatable `--set name=value`, `--keep-paths` and `--replace` retain
those meanings. Applying selected items finishes the offer and discards the
rest. A partial apply failure reports items already written and leaves the
offer available for review and retry. Use `--peer` to disambiguate duplicate
IDs from different peers. Revoking trust or `config.offer` blocks further
fetch/import, while local decline remains available.

`federation offers --outgoing` shows sent offer outcomes; `federation outbox`
shows transport delivery and receipts. Offers expire after 72 hours. Each peer
may have ten active config offers, totaling at most 64 MiB in each direction;
a single bundle is limited to 16 MiB. Small payloads (up to 256 KiB) travel in
sealed envelopes. Larger payloads use the encrypted hub stream relay when the
operator fetches or previews; the sender must be online then. Explicit
`federation offers fetch <id>` downloads without importing. Exact length,
SHA-256 and authenticated stream completion are checked before a private
spool file becomes ready. Failed transfers remain retryable. Expiry and
decline remove payload files, including after daemon restart; terminal receipt
metadata remains visible. No remote bundle becomes an agent prompt.

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

## Sharing an agent with a peer

Agent offers use the same inbox, spool and transfer protocol as config offers.
On the receiving machine, allow a peer to offer agents into a particular group:

```bash
tclaude federation grant alice agents.receive --scope group=reviewers
```

`agents.receive` requires an existing group scope. It advertises that receiving
group without exposing its members or transcripts. An offer is pinned to the
group's identity: renaming preserves it, while deleting and recreating the same
name does not retarget it. Unrestricted peers may offer into active groups, but
receipt never launches an agent automatically.

On the sending machine:

```bash
tclaude federation share-agent reviewer bob --group reviewers
tclaude federation share-agent reviewer bob --group reviewers --history
```

The default shares configuration only. `--history` includes the native
conversation when the harness supports `HistoryTransfer` (currently Claude
Code and Codex); other harnesses warn and share configuration only. The original
agent and transcript remain in place. Credential findings report counts and at
most three locations per kind; sharing refuses unless `--allow-flagged` is
explicit. Structured credential fields are omitted and history stays verbatim.

Agent callers need `agent.share` with `peer=bob` or
`peer=bob/reviewers` scope. They may share themselves using `self`; sharing
another agent additionally requires `agent.bundle.export`. An unscoped grant
confers no remote authority. Receiving, previewing and applying offers remain
operator actions.

On the receiver:

```bash
tclaude federation offers
tclaude federation offers import <id> --cwd "$PWD"
tclaude federation offers import <id> --cwd "$PWD" --name received-reviewer --apply
```

Import goes through `agent bundle import` and normal spawn checks, creating a
fresh local agent. The offered group is the default; `--group` can choose
another active group that grants this sender `agents.receive`. Trust and the
original receiving grant must remain valid. Preview shows provenance, the
chosen group, paths, history capability, credential findings and launch
security posture. Source permissions, ownership, roles and profile names are
advisory and never become grants on the receiver.

Use `--skip-history` for configuration only. `--cwd`, `--worktree`, `--keep-paths`
and repeatable `--set name=value` retain the local agent-bundle import meanings.
Configuration selectors (`--only`, `--skip`, `--replace`) do not apply to agent
offers. Missing paths or unresolved placeholders refuse apply. Group identity
and admission are rechecked after transfer and immediately before import.

Agent archives are limited to 256 MiB, with ten active offers and at most
512 MiB of archived payload per peer per direction. The shared 72-hour expiry,
inline threshold, verification and decline behavior apply. If an apply is
interrupted after dispatching a launch, its reserved agent ID stays visible;
inspect that identity before requesting another offer. Pre-launch failures can
be corrected and retried. Declining or expiring an offer discards transfer data;
it does not stop an already dispatched agent.

### Moving an agent

A move clones native conversation history to a new destination agent, then
retires the source after the destination confirms that its reserved launch is
running. Claude and Codex history are supported. A peer must advertise move
support; an older peer cannot complete a move with an ordinary import receipt.

```bash
tclaude federation move-agent worker peer-name --group receiving-group
# On the receiving instance, preview and explicitly import as usual:
tclaude federation offers import OFFER_ID --cwd /local/project
tclaude federation offers import OFFER_ID --cwd /local/project --apply
# On the source instance:
tclaude federation moves ls
tclaude federation moves show OFFER_ID
tclaude federation moves abandon OFFER_ID
```

The local dashboard uses the same responses at `GET /api/federation/moves`,
`GET /api/federation/moves/{id}` and `POST /api/federation/moves/{id}/abandon`.
These cookie-authenticated routes are local operator administration and are
refused through peer views.

Moves require history; `--skip-history` is refused. Paths, group membership and
permissions follow the agent-offer import rules. The receiving agent has a new
agent ID and conversation ID, with a durable `moved_from` link containing the
source instance, agent and offer. The source retains a `moved_to` tombstone.
Both links are visible in operator-only move status.

An offer captures the history at the time it is sent. The source remains active
while the receiver reviews and launches the clone; later source history and
pending mail are retained locally. No continuous synchronization takes place.
Agent callers need `agent.move` scoped to the destination peer and ordinary
retire authority for the source. Exporting another agent also needs
`agent.bundle.export`. Move and retire authority are checked again before
retirement; a revoked grant leaves the move blocked and the source intact.

Outgoing state advances from `awaiting_confirmation` to `confirmed`, `retiring`
and `moved`. An import's ordinary `applied` receipt does not retire the source.
The dedicated confirmation binds the archive digest and both identities to a
verified live destination launch; selected Codex app-server launches must also
be ready. A confirmation older than five minutes cannot authorize retirement.
Restart recovery retains launch and retirement progress without creating another
clone. If the source rotates to a new conversation while waiting, retirement is
blocked; abandon the old move and offer the current generation.

Declined, expired and abandoned moves leave the source intact. Abandon is
idempotent and ignores later confirmations. Once retirement starts it cannot be
abandoned. An independently created destination clone remains running after
abandon. Successful retirement stops the source and removes its normal agent
authority while retaining its conversation and worktree.

Mail to the retired source's old stable address is refused with `agent_moved`
for senders still authorized through its former group's current peer mail grant.
The refusal does not disclose the new address. Other senders receive the usual
unknown-agent or authorization refusal. Mail is never forwarded automatically;
an operator can share the new destination address and configure its mail grants.

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

Zero (the default) means unlimited. The maximum is advertised capacity, not
an admission limit or resource reservation. Existing peers without a node block
continue to work and do not appear in `nodes`.

### Local node pools

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

### Node profiles and trust defaults

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

### Enroll a node with a token

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

### Remote one-shot jobs

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

### Teleport yourself to another node

An agent with `self.teleport` scoped to a trusted peer can run:

```bash
tclaude agent teleport laptop --group helpers --note "Continue investigating the failing test"
tclaude agent teleport group:debuggers --group helpers --clone --credentials local
tclaude agent teleport --node auto --require 'os=linux' --group helpers
tclaude agent teleport status
tclaude agent teleport --home --group helpers
```

Teleport transfers native Claude Code or Codex conversation history using the
existing agent-offer transport. Move mode keeps the source running until the
receiver confirms that the imported generation is running. `--clone` keeps the
source running. There is no destination retry after an offer might have been
delivered. Selection failures include candidate reasons; missing or stale node
metrics exclude a candidate. Automatic selection also needs peer-scoped
`node.read` authority. Agent-visible candidate information follows that scope.

The destination gets a fresh agent identity and inbox. Its continuation briefing
includes the origin, predecessor, current instance, hop count and note. `agent ls`
and `agent whoami` show its predecessor. The old inbox remains at the source;
after a completed teleport, mail to the old address bounces with the destination
address. Teleport does not forward mail or transfer permissions, credentials or
uncommitted working-tree changes.

Automatic landing requires a group-scoped peer grant `agents.teleport.receive`
and an applied node profile with `teleport_landing`. Its `group` and
`spawn_profile` resolve to immutable local IDs; `cwd` is an existing receiver
path and `max_live` bounds live plus reserved teleport workers from that peer.
The profile's existing `worker_permissions` are installed before launch.
Receiver-owned launch settings must match the history's harness. Changes or
revocation before dispatch stop automatic launch. Without automatic landing,
`agents.receive` can admit the usual pending offer for an operator to preview and
accept. Neither permission is granted by default. Unrestricted peers hold peer
slugs implicitly, but still need a landing policy for automatic teleport.

An example landing block in the receiver's node profile is:

```json
{
  "teleport_landing": {
    "group": "helpers",
    "cwd": "/work/project",
    "spawn_profile": "claude-worker",
    "max_live": 2,
    "repo": "project",
    "credentials_default": "local",
    "credentials_allowed": ["local"]
  }
}
```

`--git-ref main` uses this policy's optional `repo`, an entry in
`federation repos`, to prepare a private, detached checkout through the same
allowlist and Git isolation as remote jobs. Its immutable ID, revision, enabled
state, clone identity and receiving groups are rechecked before launch. The
preview names the repo/ref and the result records the resolved commit. A pinned
full SHA can use the shared checkout's verified local-object fallback; branch
refs never fall back to stale objects. The operator clone is not modified.
Successful checkouts are retained because an agent's native history can resume
there; clean them only after confirming that no running or resumable agent needs
the directory. Without `--git-ref`, landing uses the policy's `cwd`.

`--credentials local|proxy:<name>@<peer>` chooses a credential mode. Resolution
uses an explicit flag, then `teleport_landing.credentials_default`, then `local`.
`credentials_allowed` is a simple exact allowed list (default `["local"]`). Local
mode uses provider credentials already on the receiver. Proxy mode uses the named gateway through the Claude Code binding described below. Modes never
silently fall back to each other. The chosen mode appears in the import preview,
continuation briefing and audit.

Both sides durably charge attempts, including clones and failed launches. Under
`federation.teleport.limits`, `per_hour`, `per_day`, `per_chain` and
`revisit_minutes` default to 4, 12, 16 and 10. Landing policies can tighten these
limits. `allow_return` on both the node and landing policy permits an explicit
`--home` return within the revisit window; all other current grants and limits still apply. Older provenance hops
are explanatory, not authority to act as another principal.

An operator can run `tclaude federation teleport off` (and `on` to restore it).
`tclaude federation teleport status` reads the current switch as
`{"disabled": true|false}`. The local dashboard has the same read and write
contract at `GET` and `PUT /api/federation/teleport`; the CLI uses
`GET` and `PUT /v1/federation/teleport`. The dashboard routes require the local
operator cookie and are refused through peer views.
This freezes new incoming/outgoing teleports and prevents uncommitted launch or
retirement on that instance. Apply it on each node to freeze a fleet; disconnected
nodes cannot be changed by a local switch. Existing agents and backup leases keep running.

#### Keep a paused backup

```bash
tclaude agent teleport server --group helpers --keep-paused-backup
tclaude agent teleport report "Fixed the index; committed abc123 and ran the tests"
tclaude agent teleport --home --note "Findings and next steps"
# On the origin, the operator or an agent with covering agent.resume authority:
tclaude agent teleport recover agt_<source-id>
```

`--keep-paused-backup` stops the source after confirmed landing, retaining its
identity, inbox, permissions, native history and working directory. It is not
retired. `agent ls` shows `paused (teleported to <instance>/<agent>)`, and
`teleport status` includes its lease, epoch and any recovery error. Humans can
inspect all teleport records; an agent sees only its own records. Mail to the
paused identity remains in its inbox. Dormant slots include pending landings
and recovering backups, separately from live node capacity. Clone plus backup
is refused; a leased roaming copy must report or return home before another
teleport. Ordinary resume/power-on cannot bypass the lease.

The origin owns the durable lease epoch. Renewals use the existing federation
channel, every 30 seconds by default. After five minutes without renewal plus
two minutes of grace **while the origin is online**, auto recovery advances the
epoch and resumes the original backup. Disconnects, daemon runtime restarts and
laptop suspend restart the origin's full observation window; offline time does
not count. Manual recovery uses the same wait, with no force bypass.

Availability takes precedence during an origin outage: the remote keeps working
when the origin sleeps, disconnects, or restarts, and a remote daemon restart
resumes renewals without stopping its agent. A true partition may leave both
copies running temporarily. The recovery inbox briefing and operator notification
explicitly warn that the remote may still be alive: check current state before
repeating destructive or one-time work. On reconnect the origin's newer epoch
supersedes the remote, which stops by default. The optional `clone` policy keeps
it as an independent clone and queues a coordination briefing instead.

Configure these defaults under `federation.teleport.backup` in local config:

```json
{
  "renew_seconds": 30,
  "lease_seconds": 300,
  "grace_seconds": 120,
  "dormant_max": 4,
  "recovery": "auto",
  "superseded": "stop"
}
```

`recovery` accepts `auto|manual`; `superseded` accepts `stop|clone`. Lease duration
must cover at least two renewal intervals. Timings are pinned when the backup is
reserved, and the origin's required cadence travels with the offer so different
receiver defaults cannot trigger false failover. Recovery mode and superseded
policy are local and re-read live.
Retiring the backup or replacing its generation releases its dormant slot and
supersedes the old remote on its next contact. Revoking `self.teleport` prevents
further accepted renewals; recovery still observes the full online wait.

`report` and a leased copy's `--home` commit bounded findings (up to 16 KiB for
report). Without explicit findings, `--home` captures a bounded native transcript
tail; if it cannot read history it refuses with guidance to supply `--note`.
The roaming copy then stops. Only after its node verifies the stop does
the origin deliver the findings and resume the original identity. Retries use
the same durable return ID and never launch a second identity. Findings arriving
after lease-loss recovery still reach the original inbox once, without another
resume. Shutdown process identity survives daemon restarts, so a missing tmux
pane alone cannot certify exit. Lost connectivity
leaves a return pending; use `teleport status` to inspect it. An operator can
retire the stopped roaming identity after the origin acknowledges recovery.
An ordinary `--home` without a paused backup still uses the normal teleport
policy, group selection and revisit limits above.

### Model gateways (Claude Code)

Local dashboard administration mirrors the CLI's `/v1/models` handlers at
`/api/federation/models`: GET/POST `/control` for policy and switches,
GET/POST `/leases` to list or revoke a lease (`{"id":"LEASE_ID"}`), and GET
`/usage?day=YYYY-MM-DD` for daily usage. Control reads return `{disabled,
gateways}` without provider URLs, headers or credentials; writes accept
`{name?, peer?, disabled}`. Lease and usage reads return arrays. These dashboard
routes require the local operator cookie and are refused through peer views.


A trusted machine can provide a named gateway to Claude Code or Codex workers
on another machine. Claude Code uses Anthropic Messages; Codex uses OpenAI Responses. The provider key stays on the gateway machine;
the worker receives a random, per-launch loopback bearer. Gateway traffic uses
sealed federation control messages and an encrypted, flow-controlled hub stream.
The relay cannot read prompts or responses. Ordinary Claude Code workers and Codex
workers (TUI or app-server drive) are supported; non-interactive one-shot runs
refuse a proxy choice. This does not relay ChatGPT or Claude subscription tokens.

Configure a named entry under `agent.http_proxies` on the gateway machine:

```json
{
  "agent": {
    "http_proxies": {
      "anthropic": {
        "url": "https://api.anthropic.com",
        "header": "x-api-key",
        "header_value_file": "/private/path/anthropic-key",
        "model_policy": {
          "enabled": true,
          "models": ["claude-sonnet-*", "claude-haiku-*"],
          "precount_input": false,
          "daily_requests": 1000,
          "daily_tokens": 10000000,
          "peer_daily_requests": 300,
          "peer_daily_tokens": 3000000,
          "session_daily_requests": 100,
          "session_daily_tokens": 1000000,
          "max_input_tokens": 100000,
          "max_output_tokens": 16000,
          "max_concurrent": 4,
          "requests_per_minute": 30
        }
      }
    }
  }
}
```

Set `model_policy.dialect` to `anthropic` (the default when omitted) or `openai`.
An OpenAI gateway uses a URL such as `https://api.openai.com`, `header` set to
`Authorization`, and a credential file containing `Bearer <provider API key>`.
Configure its model allowlist for the exact Responses model IDs or prefixes.
Its accepted endpoints are `POST /v1/responses` and filtered `GET /v1/models`;
Messages, WebSockets, standalone search and `/responses/compact` are refused.
OpenAI `precount_input: true` is unsupported and refuses configuration. Omitted
`max_output_tokens` is filled with the configured output maximum; an explicit
larger or invalid bound is refused. Terminal `response.completed` usage settles
stream reservations; cached input is already included in `input_tokens` and is
not charged twice. Nonstream completed Responses objects are also accounted.

Codex uses a launch-unique custom provider, `wire_api=responses`, a loopback
`base_url`, and `env_key=TCLAUDE_MODEL_PROXY_TOKEN`. tclaude passes provider config
as command overrides to both TUI and app-server; it never edits the user's
`config.toml`. OpenAI authentication and WebSockets are disabled on this provider.
The effective-config probe uses the same overrides and refuses a differing or
uninspectable route. Provider-changing pass-through config/profile arguments
are refused, and inherited routing/auth/proxy environment variables are cleared.
The launch selects Codex's ephemeral credential store and verifies that setting,
so a gateway 401 cannot trigger refresh of a saved ChatGPT login. A saved login
or `OPENAI_API_KEY` is not used for gateway model requests;
the upstream is an API/provider-credential endpoint, not subscription billing.
Copilot CLI 1.0.91 or newer can use an `openai` gateway too. The binding sets
`COPILOT_PROVIDER_BASE_URL` to the session bridge, selects Responses over HTTP,
and supplies only the launch bearer. `COPILOT_OFFLINE=true` disables GitHub
login/routing and telemetry, as well as GitHub MCP and web tools. Pass an explicit
Copilot model (for example `--model gpt-5.4`); `auto` and an absent model are
refused. Both the TUI and embedded API drive run with the same binding.
Competing provider key commands, headers, registry files and credential/routing
variables are stripped before applying the binding. This follows the native
[Copilot BYOK contract](https://docs.github.com/en/copilot/how-tos/copilot-cli/customize-copilot/use-byok-models).

This also applies to remote spawn and teleport `--credentials proxy:<name>@<peer>`.
There is no automatic fallback to local credentials on refusal or disconnect.
Filtered sandbox IP rules still govern tools; the separately authorized model
bridge runs inside the workload's namespace and reaches agentd through its socket.

This follows the [Codex gateway contract](https://learn.chatgpt.com/docs/enterprise/connect-to-a-gateway)
and [provider configuration reference](https://learn.chatgpt.com/docs/config-file/config-reference).
The opt-in native smoke uses an isolated saved login and conflicting ambient key,
then verifies the launch bearer at a fake Responses endpoint, tool execution,
follow-up turns, and app-server drive:

```bash
TCLAUDE_CODEX_PROXY_SMOKE=1 scripts/test.sh ./pkg/claude/session -run TestNativeCodexModelProxySavedLoginPrecedence -v -count=1
TCLAUDE_COPILOT_PROXY_SMOKE=1 scripts/test.sh ./pkg/claude/session -run TestNativeCopilotModelProxyNoFallback -v -count=1
```

All six daily limits and both token bounds must be positive. Model entries match
exact IDs or a trailing `*` prefix pattern, such as `claude-sonnet-*`.
`precount_input` defaults to false: input bounds are checked from provider usage,
with the request byte cap providing the hard pre-flight bound. Set it to true to
count input through the provider before generation; this adds a round trip and
requires the provider token-counting endpoint. Every request reserves the maximum input plus
requested output tokens atomically against gateway, peer, and session budgets.
Complete terminal usage replaces that reservation, including Anthropic cache tokens (OpenAI cached input is included in input tokens).
Interrupted requests retain their reservation until the next UTC day, including
across daemon restarts. Provider usage is checked against the token bounds.
Independent defaults cap requests at 4 MiB, responses at 64 MiB, individual SSE
events at 1 MiB and stream duration at 30 minutes; `max_request_bytes`,
`max_response_bytes`, `max_event_bytes`, and `max_duration_seconds` can narrow or
raise these within the enforced safety ceilings.

Grant the consumer peer access on the gateway, then grant its worker access on
the consumer machine. Both grants are needed for restricted peers:

```bash
# Gateway machine: laptop is a trusted consumer peer.
tclaude federation grant laptop models.proxy --scope http_proxy=anthropic
# Consumer machine: master is the trusted gateway peer.
tclaude agent permissions grant <worker> models.proxy --scope peer=master --scope http_proxy=anthropic
tclaude agent spawn <group> --harness claude --model-proxy anthropic@master --brief 'Continue the task'
```

For a new worker, put its `models.proxy` permission in the receiving group or
worker defaults before launch. Permissions are checked before binding and on
every request. A deny or revoked grant interrupts active requests. The bearer
is stored only as a SHA-256 hash, pinned to the launch generation; daemon
upgrades preserve running bindings, and exit, retirement or replacement makes
them unusable. `--model-proxy off` explicitly overrides a default profile.

The bridge sets `ANTHROPIC_BASE_URL` and `ANTHROPIC_AUTH_TOKEN`, which takes
precedence over saved Claude logins according to the
[Claude Code gateway documentation](https://code.claude.com/docs/en/llm-gateway-connect).
It clears inherited provider/authentication selectors and refuses conflicting
Claude settings or `apiKeyHelper` rather than falling back to local billing.
This is an explicit model route, not an OS sandbox: tools still have the network
access granted by the worker's sandbox, and operators must avoid changing
provider settings while it runs.

Inspect metadata and stop access without exposing keys:

```bash
tclaude federation models status
tclaude federation models usage --day 2026-10-08
tclaude federation models disable                         # all gateways
tclaude federation models disable --name anthropic        # one gateway
tclaude federation models disable --name anthropic --peer laptop
# Use enable with the same flags to restore access.
```

The daily usage view and audit log contain request identifiers, peer/session,
model, status, token counts, byte counts and duration. They do not store prompts,
responses, bearer tokens or provider credentials. Provider error bodies are
replaced by readable Anthropic-shaped errors.

Teleport's frozen `--credentials proxy:anthropic@master` mode selects this
binding, subject to the receiver landing policy's exact `credentials_allowed`
list and worker permissions. The gateway peer is pinned to its immutable
identity before admission. `--credentials local` explicitly overrides any proxy
in source or receiver profiles. Proxy mode never falls back to a saved local
login, and requires Claude Code history and an available gateway.

Use provider credentials authorized for the machines, people and locations in
your fleet. A gateway does not turn a Claude subscription into API credentials
or change the provider's account and service terms; cross-organization or
cross-location sharing needs the gateway operator's separate authorization.

### Requester-paid workers

A requesting machine can pay for its remote Claude Code worker through its own
named gateway. On the requester, grant the receiving peer lease-only access:

```bash
tclaude federation grant colleague models.proxy.leased --scope http_proxy=anthropic
tclaude federation spawn-request builders@colleague --credentials proxy:anthropic@self --brief 'Check the change'
# Teleport uses the same credential choice:
tclaude agent teleport builders@colleague --credentials proxy:anthropic@self
```

The receiver must consent through `requester_pays` in its applied node profile,
or `--requester-pays` on a `groups.members.spawn` peer grant:

- `off` (default) rejects requester-issued leases and keeps existing launch behavior.
- `allowed` accepts an explicit requester-paid offer; it does not choose one implicitly.
- `required` refuses requests without a requester gateway lease before launch.

A concrete spawn grant's policy overrides the node profile default, using the
existing group-scope and direct-peer precedence. Conflicting equal-priority pool
policies refuse. Teleport uses the node profile default, with an optional
`teleport_landing.requester_pays` override, and still requires its ordinary
`credentials_allowed` entry. Allowed credential references may name the requesting
peer's alias or its pinned instance ID. Manual acceptance rechecks the current
policy and preserves the offered account. Missing gateway support, permission,
connectivity or harness support refuses; it never falls back to receiver credentials.

Agent callers creating a paid request need ordinary `models.proxy` covering the
receiving peer and their own named proxy. Receiving workers also need ordinary
`models.proxy` covering the requesting gateway and proxy name, through receiving
group permissions or node-profile worker defaults. The lease-only peer grant
allows only requests issued by that gateway for an exact remote worker; it does
not allow arbitrary sessions to use the provider account. Existing ordinary
`models.proxy` peer grants remain available for broader fleet gateway access.

Leases bind the signed request or teleport offer, receiving instance, reserved
worker, session and launch generation before the harness starts. They survive
daemon restart and end on worker exit, retirement, generation replacement,
revocation or a gateway kill switch. Disconnected close notices retry after
reconnect. There is no fixed lifetime: requests slide the idle timeout, configured
as `model_policy.lease_idle_hours` (default 24). An idle-expired or revoked lease
cannot be reactivated; submit a new request. A failed launch that already bound its
lease likewise requires a new request before changing worker identity.

`tclaude federation models leases` shows the paying proxy, receiving peer,
request, worker, generation and last activity. Use `--revoke <id>` to stop one.
Both instances audit account attribution; prompts, responses and bearer values
remain outside audit records. The existing budgets and switches apply to leased
traffic. Remote argv jobs and other harness bindings are outside this release.

### Identity rotation and key-loss recovery

Instance IDs derive from public keys. Rotation therefore creates a linked
successor ID, rather than changing the key behind an existing ID:

```bash
tclaude federation identity rotate             # preview consequences
tclaude federation identity rotate --apply     # stage and publish successor
tclaude federation identity rotations          # local and peer transition state
```

The public transition contains both keys and IDs and signatures from both keys.
Peers verify it from their own pinned predecessor. The hub independently verifies
it before transferring admission and spaces. Each authority starts a detection
window at first observation (default ten minutes); replay does not shorten it.
Set `federation.identity_rotation_seconds` in local configuration or hub
`serve --identity-rotation-window 10m` to change the window. A competing signed
successor freezes automatic acceptance until explicit recovery. The original
key remains current during the window. An operator notification shows both
fingerprints and the statement issue date when a change is observed and accepted.

The local daemon stages the replacement key privately and journals activation.
Activation closes federation streams and re-establishes handshakes. Pending
sealed mail is refused with an instruction to resend; ciphertext and history
are never rewritten. Existing model credentials and requester-paid leases are
revoked immediately, with an audit reason. Live execution reservations remain
charged until their existing lifecycle proves the worker ended. Paused teleport
backup leases and their linked routing records follow the successor without
changing epochs, sequence numbers, deadlines or historical origin provenance.
Normal teleport `--home` resolves accepted successors for routing.

Four public transition hops are retained. A fifth rotation refuses with an
instruction to re-pair offline peers. Offline peers can verify a retained chain
from their pinned key and start their own detection window when they return.
Older peers and hubs lacking rotation support need explicit re-pairing.

A missing previously recorded local key refuses ordinary federation startup;
it does not silently create another identity. To create an unlinked replacement
explicitly after key loss or suspected compromise:

```bash
tclaude federation identity recover-local              # preview
tclaude federation identity recover-local --apply      # new ID and fingerprint
```

Recovery journals its replacement before retiring capabilities or changing the key.
Startup resumes an interrupted recovery with that same replacement. Explicit
recovery also abandons a pending signed rotation and archives its public journal,
so lost staged keys do not block recovery.

Local peer grants and historical records remain. The replacement needs explicit
hub admission recovery and explicit confirmation at every peer that trusted the
old identity. Verify the replacement fingerprint out of band, then preview and
apply on each trust authority:

```bash
tclaude-hub identity recover OLD_ID NEW_ID --db /path/hub.sqlite
tclaude-hub identity recover OLD_ID NEW_ID --db /path/hub.sqlite \
  --fingerprint NEW_FINGERPRINT --apply

tclaude federation identity recover-peer OLD_ID NEW_ID
tclaude federation identity recover-peer OLD_ID NEW_ID \
  --fingerprint NEW_FINGERPRINT --apply
```

Peer recovery displays the old label and trust level, direct grants, pool
memberships, profile assignment and rebind rules. Apply preserves these and
updates exact peer scopes; it refuses merging two already trusted identities.
Unrestricted authority stays unrestricted, as explicitly displayed in the
preview. No default node profile or config offer is applied. Historical activity
and consumed enrollment records stay under the old identity; enrollment tokens
cannot resurrect the predecessor. Hub recovery replaces the replacement's
spaces with the predecessor's spaces and revokes the old admission.

If a transition is unexpected, revoke its predecessor on the relevant peer and
hub (both commands preview unless `--apply` is present):

```bash
tclaude federation identity revoke-old OLD_ID --apply
tclaude-hub identity revoke-old OLD_ID --db /path/hub.sqlite --apply
```

Revocation blocks pending rotation and old-key replay. An already accepted
successor remains current; revocation does not roll it back. If the old signing
key was stolen, the attacker can also sign a valid rotation. The detection window
helps expose competing successors; it cannot prove which signer is the owner.
An undetected winning successor inherits authority. For immediate containment,
revoke that accepted successor itself on every trusting peer and on the hub:

```bash
tclaude federation identity revoke-old SUCCESSOR_ID --apply
tclaude-hub identity revoke-old SUCCESSOR_ID --db /path/hub.sqlite --apply
```

Despite the command name, `revoke-old` can revoke the currently trusted successor.
It removes that identity's trust and closes its remaining capabilities. Pair a
separately verified replacement and explicitly restore the intended authority;
revocation does not preserve an authority assignment for later automatic recovery.
Identity management commands are operator-only.

### Fleet health notices and watching

```bash
tclaude federation nodes --watch
tclaude federation nodes --watch --json
tclaude federation nodes health
tclaude federation nodes health --peer bob --set '{"resources":true,"failures":true}'
tclaude federation nodes health --peer bob --set '{"presence":false}'
```

These commands are operator-only. `--watch` prints the current node list, then
live transitions; `--json` emits one initial `{"nodes":[…]}` object followed by
one event object per line. Watch cannot be combined with `--match`. It adds no
polling, resource probes or status-snapshot gathers. Presence comes from hub
directory changes; resource checks consume the existing shared node snapshots;
job and spawn outcomes feed failure counters. The existing federation tick
flushes debounces. A hub connection outage alone is not proof that every peer
went offline. The initial directory establishes a baseline without notices.

Trusted-peer offline/back notices are enabled by default, with a 15-second
debounce that suppresses short flaps. Resource and repeated-failure signals are
off by default. Enabled notices use the same operator Messages inbox and desktop
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
| `debounce_seconds` | 15 | Delay before a stable transition is emitted |
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

### Peer UI request contract

The receiving daemon's `agentd.PeerViewHandler(instanceID)` accepts requests
from an authenticated federation transport. The transport supplies the pinned
calling instance ID; browser parameters and HTTP headers cannot select it.
This handler is separate from the local dashboard, whose cookie authentication
and response shape remain unchanged. Proxy transport and remote actions are
separate features.

Peer reads carry a `peer_view` object with the peer label, `included` concept
names and `omitted` entries (`feature`, `requires`). These describe capabilities,
never hidden object names or counts. A restricted `/api/snapshot` contains only
visible groups and agents, using roster, presence and status grants to select
fields. Status comes from the same shared cached gather as the local dashboard.
The peer projection uses the normal dashboard snapshot and row types, including
`conv_id`, `state.status`, `state.model`, `state.effort_level`, numeric context
fields and `task_ref_url` / `task_ref_label`. Lists and maps for withheld
concepts are empty, unsupported scalars are blank/zero, and optional private
fields are omitted. Paths, worktrees, permissions, spawn configuration,
account usage and local notification/approval content are not shared through
roster/presence/status grants. The harness spawn catalog is empty for restricted
peers: local provider model suggestions and readiness diagnostics are private
configuration. Group roles require roster access; online fields
require presence or status access. Status granted on one visible group does
not populate an agent's row in another group without that grant. The deduped
`agents` list combines only authorized data and lists only visible groups.
Task URLs have query strings and fragments stripped. A field classification
guard and final deny filter cover snapshot, group, member, agent, task and
state fields; newly added fields default to withheld until classified.

Single-group and single-agent reads use `/api/groups/{name}` and
`/api/agents/{id}` in the same row schema; invisible objects return 404.
Restricted snapshots always send their small registry fields, with
`static_unchanged` false, regardless of a supplied `static_version`. A client
must not reuse registry blobs from a local, different-peer or older-authority
snapshot. Unrestricted peers get the complete dashboard snapshot plus metadata.

`GET /api/harnesses/availability` exposes detailed installed harness paths,
versions and boolean credential presence under the instance-wide
`node.harnesses.read` peer grant. This permission defaults off and is implied
only by unrestricted trust. `node.read`, group grants, and unrelated node
permissions do not imply it. Node profiles and node pools may carry this
unscoped grant; group scopes are rejected.

`GET /api/instance` exposes node metadata under `node.read`. Costs and audit
collections require their own instance grants above. Those grants default off,
are never implied by group access, and can also be supplied by node profiles
or pools. Only unrestricted trust implies them. Without a grant, a collection
read succeeds with no rows and reports the omitted concept in metadata. An
audit grant exposes the entire node's audit collection, including local actions.

`POST /api/operator-message` accepts `{to, subject, body}`, where `to` is a
visible stable agent ID. It requires `message.direct` in a live group containing
the recipient. A visible recipient without that grant yields 403 naming the
missing permission and visible group. No attachment, agent impersonation or
all-live broadcast is accepted. Mail is persisted with federation sender
identity and the attempt is recorded in `federation audit` as the calling peer's
operator.

Every dashboard route is classified in the peer mapping. Other routes and
methods are local-only and return the default-deny 403, including for
unrestricted peers. Local-only features cannot be enabled by a grant through
this handler; their `omitted.requires` identifies the related federation grant
where one exists, or `local_only`. A route guard test requires an explicit
classification when a dashboard route is added.

### Node summaries and polling

`GET /api/node-summary` is the lightweight map-card endpoint on both the local
dashboard and the authenticated peer dispatcher. It returns:

```json
{
  "presence": "online",
  "shared_groups": 2,
  "shared_agents": 4,
  "online_agents": 3,
  "waiting_for_input": 1,
  "peer_view": {"peer": "laptop", "included": [], "omitted": []}
}
```

Counts describe only identifiable shared agents, deduplicated across visible
groups. Online counts need presence or status permission; attention counts
need status permission and count online `awaiting_input` and
`awaiting_permission` agents (idle agents are not attention). An unrestricted
peer, or the local operator, also counts loose active agents. `resources` and
`health` are optional and require `node.read` for peers. Health is the cached
resource observation status (`current`, `stale`, or `warming`), not an inferred
all-clear for all agents. Host-wide agent totals are removed from resources;
the card uses the authorized shared counts. A successful response means the
receiving daemon is online; transport failure/offline/staleness is marked by
the frontend using its own receive time. Local responses have no `peer_view`.

Summaries reuse the shared status gather and cached host readings; they do not
build full dashboard snapshots or launch resource probes. They return a strong
`ETag` over the filtered response plus `Cache-Control: private, no-cache`.
Clients can send `If-None-Match` to get a bodyless 304 for an unchanged summary.
Trust and grants are reevaluated before the validator, so a scope/permission
change changes the authorized response/ETag rather than retaining old data.
A 304 keeps the previously received body and its metadata. Transport caches
must key representations by receiving node and authenticated calling peer,
never share them across callers, and forward validators through to agentd.
Full snapshots continue to return `Cache-Control: no-store`.

Polling contract for the node-switching, map and merged-view frontends:

- A per-node view polls full `/api/snapshot` data only for the displayed node,
  using the existing dashboard cadence. Stop/abort its poll when switching
  away, and discard snapshots from an earlier selected-node generation.
- The map polls `/api/node-summary` only. Start with a relaxed 10-second
  interval per node, stagger initial offsets across nodes and add jitter;
  never trigger a synchronized fleet-wide full snapshot poll.
- The merged regular view polls full snapshots only for nodes whose groups
  are on screen. Suspend full polls for hidden/collapsed-out nodes and use
  summary data when only a map card is being shown.
- Avoid overlapping polls to a node. Back off on failures, stop polling when
  the view is hidden, and distinguish cached/stale data from an offline node.
  Recheck omitted features after every new authorized response.

### Local dashboard Fleet status

`GET /api/federation/status` is the cookie-authenticated local dashboard wrapper
for `GET /v1/federation/status`. Federation administration is local-only;
trusted peers, including unrestricted peers, cannot call this route through
the peer-view dispatcher.

For the Fleet chip row, use `GET /api/federation/status?summary=1`. The same
query also works on the CLI API. It returns the local identity (`instance_id`,
`name`, `fingerprint`), configuration (`enabled`, optional `hub_url`), optional
cached `hub` connection state, and `peers`. Summary peers include only trusted
linked instances, with their stable `instance_id`, `label`, `name`, trust
`level`, `trusted`, `online`, and `last_seen` fields (plus fingerprint, version,
and trust time). An empty peer list is `[]`. Presence comes from the cached
hub directory; no remote requests or status probes run during this read.

Summary responses omit `peer_grants`, `outbox`, and `remote`, and avoid loading
those records or remote catalogs. The full status response still includes
hub-visible untrusted instances and the administration fields. Responses are
private and uncached. Poll this local list at a relaxed interval, pause when
the dashboard is hidden, and poll each visible remote map card separately.

The rest of the local Fleet administration API mirrors the CLI API: replace
`/v1/federation/` with `/api/federation/`, retaining the HTTP method, JSON
request/response, query parameters, and path variables. Every wrapper checks
the local dashboard session before calling the shared handler as the local
human. Peer-view transport refuses all of these administration routes,
including for unrestricted peers. `HEAD` reads on enrollment-token and profile
GET routes preserve read-only semantics on both API surfaces.

| Method | Tail under `/api/federation/` | Existing `tclaude federation` CLI |
| --- | --- | --- |
| GET | `status`, `audit` | `status`, `audit` |
| POST | `config` | `connect`, `disconnect` |
| GET / POST | `enroll-tokens` | `enroll-token ls`, `enroll-token create` |
| POST | `enroll-tokens/{id}/revoke` | `enroll-token revoke` |
| GET | `enrollments` | `enrollments` |
| POST | `enroll/preview`, `enroll` | `enroll --preview`, `enroll` |
| POST | `peers/trust`, `peers/untrust` | `trust`, `untrust` |
| GET / POST / DELETE | `grants` | `grants`, `grant`, `revoke` |
| GET / POST | `profiles` | `profile ls`, `profile create` |
| GET / PUT / DELETE | `profiles/{name}` | `profile show`, `profile update`, `profile rm` |
| POST | `profiles/{name}/apply` | `profile apply` |
| PUT | `default-peer-profile` | `profile default` |
| GET / POST | `nodes/groups` | `nodes groups ls`, `nodes groups create` |
| DELETE | `nodes/groups/{name}` | `nodes groups rm` |
| POST / DELETE | `nodes/groups/{name}/members` | `nodes groups add`, `nodes groups rm` |

Important shared request shapes for dashboard clients:

- Trust uses `instance`, optional `label`, `level`, `profile`,
  `no_default_profile`, `preview`, `preview_token`, and
  `confirm_fingerprint`. Changing to unrestricted requires the exact displayed
  fingerprint; selecting a profile preserves the CLI preview/apply contract.
  Untrust uses `instance`.
- Grants use `peer`, `slug`, optional `scope` and `spawn_policy`. Read a peer's
  grants with `?peer=<instance_id>`; a local pool selector is `group:<name>`.
- Profiles are `{name, revision, definition}` with the existing definition
  schema. Updates must send the current revision. Applying a profile uses
  `{peer, apply, preview_token, confirm_fingerprint}`: first preview with
  `apply: false`, then commit using the returned token. The default-profile
  request is `{profile}` (an empty string clears it).
- Pool creation uses `{name}`; adding or removing a member uses `{peer}`.
- Enrollment token creation uses `{profile, uses, ttl_seconds, trust_level}`.
  The bearer is returned only at creation; token listings expose public
  metadata. Revoke by public token ID. Joining uses `{master, token}` for
  preview, then adds `preview_token` for enrollment. Responses preserve the
  existing consent and fingerprint fields.

Full status exposes hub-visible untrusted instances for the trust screen.
There is no separate incoming trust-request queue: trusting one of these
instances pins its identity through the existing trust operation. Summary
status intentionally lists only linked trusted peers for the chip row.

### Dashboard peer proxy

The browser uses its local dashboard session for
`/api/peer/{instance_id}/<same tail>`: for example,
`GET /api/peer/inst_.../snapshot` or
`GET /api/peer/inst_.../node-summary`. Select peers by their stable pinned
instance ID from the local Fleet status response; labels are display names.
The local daemon opens a single encrypted federation stream to the selected
instance, which serves the request through its peer-view dispatcher using
the authenticated sender's instance ID. Browser cookies, authorization tokens,
and caller identity headers are removed before transport. No local-human
wrapper runs on behalf of a peer.

Supported JSON reads are `snapshot`, `groups/{group}`, `agents/{agent}`,
`instance`, `costs`, `audit`, and `node-summary`; `POST operator-message`
uses the same scoped messaging permission as a direct peer-view request.
Other routes are refused by the receiving dispatcher. Nested peer proxies and
Fleet administration remain local-only. `If-None-Match` and `ETag` pass through, including bodyless 304 responses.
The local proxy keeps its private/no-store cache policy. Errors such as a
receiving node's 403 or a hidden agent's 404 retain their status and JSON body.
Responses are validated as JSON and served with a fixed JSON content type,
`nosniff`, and a sandbox CSP. A peer cannot publish executable content under
the local dashboard origin.

An unknown or no longer trusted selector returns 403 with `code: not_trusted`.
Unavailable peers return 502 with `code: peer_unreachable` and
`reason: peer_offline`; deadlines return 504 with the same code and
`reason: peer_timeout`. These errors include `last_seen` when the cached hub
directory has an observation. Concurrency saturation returns 503 with
`code: peer_busy`. Requests have a 15-second deadline, bounded JSON bodies,
and bounded concurrent streams. The proxy does not retry or fan out requests.
Clients should keep their last successful view, mark it stale, and back off.

The stable `peer_view.included` / `peer_view.omitted[].feature` concept keys are
`agents.status`, `groups`, `groups.roster`, `groups.presence`, `messaging`,
`node.summary`, `health`, `costs`, `audit`, `terminals`, `spawn`, and
`local_dashboard`, `node.harnesses`, and `node.exec`. `local_dashboard` covers local administration, registries,
and lifecycle controls. An omitted concept carries a `requires` permission or
`local_only`. Clients must tolerate additive concept keys; absence of an
omission is not a grant for an unknown endpoint.

Poll full snapshots only for nodes whose regular dashboard views are visible.
Map cards use relaxed, staggered summary polls. Merged views poll the nodes
that contribute visible groups. Pause hidden dashboards, avoid overlapping
requests to one node, and back off on failure.

Terminal websocket attach continues to use the existing federation sessions
watch/attach API. This JSON proxy does not yet adapt it under the per-node
prefix. Remote terminal image uploads remain a separate feature; they need
staging at the owning instance with the same interactive attach authorization.


### Harness availability

Inspect the daemon's registered harnesses, including missing binaries and the
shell harness, with:

```bash
tclaude harness ls
tclaude harness ls --refresh --json
tclaude harness ls --node bob --json
tclaude federation grant bob node.harnesses.read
```

The local dashboard reads `GET /api/harnesses/availability`; the operator CLI
reads `GET /v1/harnesses/availability`. Remote reads use the same peer-view
proxy with tail `harnesses/availability`. These are operator reads locally;
remote peers need `node.harnesses.read` or unrestricted trust. Existing
`node.read` catalog harness versions remain a coarse capability summary and
do not reveal binary paths or credential presence.

Responses contain `schema: 1`, `observed_at`, `refresh_after`, and `harnesses`.
Each harness row includes `name`, `display_name`, `binary`, `installed`,
optional `path` and `version`, `version_status`, nullable `credential_present`,
and nullable `usable`. Version status is `known`, `unknown`, `not_installed`,
`not_spawnable`, or `path_unavailable`. Remote responses also include the
normal `peer_view` metadata with concept `node.harnesses`.

Paths resolve against agentd's PATH. Version subprocesses use fixed
`--version` arguments, bounded output, and deadlines. Credential presence is
only a boolean check for descriptor-declared ambient credential variables;
credential values are never returned. Presence does not prove that a key is
valid. Missing ambient credentials mean unknown, since native login and
provider configuration may still work. The probe does not read credential
files, run login commands, or contact authentication services. Missing
binaries are unusable; installed credential-free shell is usable; other
installed harnesses report unknown usability.

Results cache for five minutes. `--refresh` or `?refresh=1` requests a fresh
probe, with a ten-second minimum between probes and a single shared probe at
a time. The first uncached read can take up to eight seconds. Do not put this
probe on the fast snapshot or map-summary poll; request it when opening a
node's harness details or explicitly refreshing them.

### Live peer views from the CLI

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
operator reads. The local daemon APIs are `GET /v1/federation/node-summary`
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
"Groups · all nodes" view: this node's groups and every trusted peer's shared
groups, named `group@node`. Peers are read like a summary listing (at most four
at a time, no retries). JSON carries `nodes` (an `error` on any unreachable
peer) and `groups` rows with `group`, `node`, `node_id`, `members` and
`online`. An unreachable peer keeps the other rows and makes the exit status
nonzero.

For a how-to tour of the dashboard side, see
[Skynet UI](dashboard.md#skynet-ui-linked-nodes).

The dashboard's **⚙ Fleet** view (beside Map and Groups · all nodes, or
"Fleet administration" in the command palette before any peer is trusted)
covers `federation status`, `peers`, `trust`, `untrust` and `disconnect`:
this node's identity and hub connection, the trusted peers with their level,
grants and pools, and the hub-visible instances waiting to be trusted. Trust
previews first and shows the fingerprint the daemon will pin in full; the
operator confirms the out-of-band comparison before it applies. Granting
unrestricted trust repeats what it implies and sends the fingerprint as the
daemon's confirmation. Its Peer grants page covers `federation grants`,
`grant` and `revoke` for a trusted peer or a node pool: each grant shows where
it applies (an unscoped group grant is flagged as covering future groups too),
and granting confirms with that consequence spelled out. Its Invites & joining
page covers `enroll-token create/ls/revoke`, `enroll` and `enrollments`: an
invite confirms its terms (uses, lifetime, the profile's trust here and the
trust the joining node grants back) and shows the bearer once; joining
previews both fingerprints, the profile and the trust level before Enroll.
Its Profiles & pools page covers pools (`nodes/groups`: create, delete, add
and remove members — adding names the pool grants the member gains) and node
profiles: making one the default for newly trusted peers, applying one to a
trusted peer after previewing its plan (unrestricted confirms the peer's
fingerprint), and deleting. Its Audit page reads `federation audit` (filtered
by peer and time window, newest first, up to 1000 rows). Its Harnesses page
shows every harness on this node and each trusted peer (through the peer
proxy, so a peer needs `node.harnesses.read`): version, available update and
whether a login is present. A cell installs or updates that harness (busy
workers ask whether to run now or when idle), and for login-file harnesses
pushes your login to a peer, backs up or restores the node's login files —
the same operations as `tclaude harness install|update|credentials`. Its Run
scripts page is `federation run` and `federation scripts`: this node's
local-only accept-remote-scripts switch (turning it on confirms the full
remote-code-execution consequence) and script limits; a node picker (this
node plus peers that granted `node.exec` and accept remote scripts, with an
all-online shortcut; offline peers are skipped); and one result pane per node
with state, exit code, duration, output tails and the full logs on demand.
Failed nodes can be re-run alone. Profile definitions,
receiver launch settings beyond the live cap and model gateway scopes are
managed with their `tclaude federation` commands.

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

### Updating local and linked nodes

`tclaude update` checks the latest stable release. Use `--apply` to install it,
`--version vX.Y.Z` to pin a release, or `--rollback` to restore the preceding
binaries. `--node <label-or-instance-id>` performs the same operation on a
trusted node; that node downloads its own update. Installed `tclaude`,
`tclaude-agentd`, and `tclaude-hub` binaries are included. Agentd gracefully
restarts and reconnects; existing tmux sessions continue running. A running
standalone hub needs its own service restart after its binary is replaced.

Official release binaries report their real release version and the `release`
install marker. Release archives must match the official SHA-256 checksums.
Go-installed binaries use `go install` at the selected version. Source/dev or
unmarked builds report their method explicitly and warn that updating replaces
local modifications; their default is `go install @latest`. A staged binary
must report the expected release version and the same federation protocol before
any installed binary is replaced. Unknown current versions have a nullable
update-available hint rather than a guessed ordering. A new update creates a
verified backup; rollback refuses to overwrite files changed outside the updater.

`node.update` is an **instance-wide**, default-off peer permission. Neither
`node.read` nor `node.harnesses.read` implies it; only unrestricted trust implies
it automatically. It allows reading updater status/jobs as well as checking,
applying and rolling back updates. Permission is checked again before each
replacement. Update requests and results appear in the federation audit.

The local dashboard API is `GET /api/node/update` (status), `POST
/api/node/update` with `{ "action": "check" | "apply" | "rollback", "version":
"vX.Y.Z" }` (optional version, never with rollback), and `GET
/api/node/update/jobs/{id}`. Starting a job returns HTTP 202 and its durable ID;
concurrent jobs return HTTP 409 `update_busy`. The corresponding operator-only
CLI API uses `/v1/node/update`; remote dashboard calls use
`/api/peer/{instance_id}/node/update` and its `/jobs/{id}` suffix.

Poll a started job about once per second. Its states are `running`, `restarting`,
`succeeded` and `failed`; `phase`, `warnings`, and optional `error` explain its
progress. During `restarting`, tolerate the normal daemon/peer disconnect and
resume polling the same ID after reconnecting. `tclaude update --no-wait` returns
that ID immediately; `--job <id>` reads it later, including after a restart.
Node summaries expose cached `version`, optional `latest_version`, nullable
`update_available`, and optional `update_checked_at` without release requests
in the polling path. Release metadata is refreshed in the background hourly.

In the dashboard, each Skynet map card shows the node's tclaude version and ↑
when a newer release is known; its update…/manage… link opens that node's
update dialog (the Fleet page's identity bar opens it for this node). The
dialog shows the version, install method, latest release, binaries and
warnings, and offers Check now, Update to the latest and Roll back; update and
rollback confirm that the binaries are replaced and the daemon restarts. It
follows the job through the status read, so the restart does not lose it. A
peer that has not granted you `node.update` says so.

### Installing and updating harnesses

`tclaude harness install <name> [--node <peer>]` runs a fixed official npm
package recipe as the daemon user, with no sudo. New installs live under
`~/.local/share/tclaude/harnesses/npm`; the daemon includes its `bin` directory
in subsequent probes and launches. The supported names are `claude`, `codex`,
`opencode`, `copilot`, and `gemini`, using their vendors' latest stable npm
packages. The recipes link to the official vendor instructions.

`tclaude harness update <name|--all> [--node <peer>|--all-nodes]` updates
recognized npm installations in their existing prefix, or uses the native
Claude/OpenCode updater for user-owned installations. Other installation methods,
missing npm, root execution, and unwritable prefixes require a manual command;
the daemon never elevates privileges. `--all-nodes` includes this node and trusted
linked nodes, and reports failures per node. Cached npm metadata adds optional
`latest_version` and nullable `update_available` fields to harness availability.

Active sessions require an explicit choice: `--now` acknowledges the warning,
or `--when-idle` waits until the selected harness has no busy sessions. With
neither option, a busy harness returns HTTP 409 `harness_workers_busy`. A node
runs one harness job at a time. `--no-wait` returns the job ID; `--job <id>` reads
it later. Jobs contain stored phase/result logs, never vendor stdout, environment
values or credential contents. An interrupted job is marked failed after daemon
restart; inspect availability before retrying it.

Remote installs and updates require the instance-wide `node.harnesses.install`
permission, default off and implied only by unrestricted trust. This grant does
not follow from `node.harnesses.read`, and it does not authorize credential drops.
Both grants can be assigned through node profiles/pools. Operations are audited.

For a **single remote install**, `--copy-credentials` explicitly pushes the
operator's own file credentials to the installing node. Its agents can then act
as the operator with that provider. Copy is never enabled by default, never pulls
from a peer, and additionally requires `node.credentials.receive` on the receiver.
That receiving grant is also instance-wide and default off except unrestricted
trust. Existing credentials require `--overwrite-credentials`; every write first
creates a private, owner-only backup and reports its ID/location. Symlink targets
are refused and new auth files have mode 0600.

File copy supports Claude's `.credentials.json`, Codex's `auth.json`, OpenCode's
`auth.json`, and Gemini's OAuth/account files in their standard directories.
Configured Codex/Gemini/XDG directories on each node are respected. Claude capture
honors the sender configuration; receiving uses tclaude's pinned `~/.claude` state root. Keychain-only,
Copilot, and environment-only credentials need the target's own login flow;
secret stores and ambient environment variables are never exported. Bundles are
bounded to 18 KiB. The local daemon captures files only after explicit operator
opt-in and sends them over the existing end-to-end encrypted peer transport.
The browser, CLI output, job records, and audit never receive credential contents.
Availability checks report file presence without reading those contents;
provider usability remains unknown until the harness authenticates.

The dashboard API uses `GET /api/harnesses/operations` for recipes/options,
`POST /api/harnesses/operations` with `action`, either `harness` or update `all`,
optional `mode` (`now` or `when_idle`), and opt-in `copy_credentials` /
`overwrite_credentials`. POST returns HTTP 202 with a job; poll `GET
/api/harnesses/operations/jobs/{id}` about once per second until `succeeded` or
`failed` (intermediate states: `running`, `waiting_idle`). A job includes
`results`, safe `log`, `warnings`, and final `availability`. Concurrent requests
return HTTP 409 `harness_operation_busy`. Operator CLI routes mirror these under
`/v1`; remote dashboard calls use `/api/peer/{instance_id}/harnesses/operations`.

Credential copy during install is a third way to prepare a teleport target,
alongside the target's own login and a model gateway. It does not change teleport's
credential choice or defaults.

### Standalone credential push and restore

An operator can refresh an existing node's file-based harness login without
installing a harness:

```sh
tclaude harness credentials push codex --node laptop --confirm-share
tclaude harness credentials push claude,gemini --node laptop --confirm-share
tclaude harness credentials ls codex --node laptop
tclaude harness credentials restore codex --node laptop --backup BACKUP_ID
tclaude harness credentials backup codex
tclaude harness credentials restore codex
```

`--confirm-share` explicitly acknowledges that the remote node's agents will act
as you with those providers. The authenticated local daemon captures only your
selected standard auth files and sends them over the encrypted federation
channel. Clients cannot supply credential contents or arbitrary paths. Locally configured
HOME and harness-directory symlinks are resolved once before opening a pinned
canonical directory; credential-file symlinks are refused. There is
no API for pulling a peer's credentials. Copilot, keychain logins, and environment
secrets require the target harness's own login flow.

Standalone operations require only `node.credentials.receive` on the receiving
node, independently of `node.harnesses.install`. This node-wide grant defaults
off and is implied only by unrestricted trust. Each push writes a timestamped,
owner-only backup before replacement; a failed backup refuses the replacement.
Restore also backs up the current files first. Omitting `--backup` selects the
newest matching backup, including backups created by install-copy. Restore
removes a copied file if that file was absent in the selected backup. Backup and
restore work locally when `--node` is omitted; push requires a remote node.
Chosen-set pushes run separately per harness and report individual failures.

The dashboard API uses `GET /api/harnesses/credentials/backups?harness=codex`,
`POST /api/harnesses/credentials/push`, `POST /api/harnesses/credentials/backup`,
and `POST /api/harnesses/credentials/restore`. Use
`/api/peer/{instance_id}/harnesses/credentials/{action}` for a remote node.
Local CLI routes have the same tails under `/v1`; remote CLI routes use
`/v1/federation/peer/{node}/harnesses/credentials/{action}`.

POST bodies contain `harness`; push also requires `confirm_share: true`, and
restore optionally accepts `backup` (the opaque 32-character backup ID).
Success returns `receipt` with `backup_id`, `backup_location`, and `copied`;
restore adds `restored_from` and `restored`. It also returns fresh `availability`.
Backup listings return `backups` containing `id`, `harness`, `created_at`, and
`location`, plus `credential_share_warning`. Failed file operations return a
409 `credential_operation_failed` and any safety-backup receipt. Credential
contents never appear in responses, logs, or audits. Audits record the harness,
operator/peer, backup ID, action, and result. File presence can update after a
push; usability remains unknown until the harness actually authenticates.

### Stable group grant identities

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

### Operator scripts on nodes

`node.exec` grants full remote code execution as the agentd user. It is an
unscoped node permission, default off and implied by unrestricted trust.
Remote execution additionally requires the receiving node's local
`accept_remote_scripts` setting, **including for unrestricted peers**. Enable
that switch locally; peers cannot change it. Local operator runs require the
usual dashboard/CLI authority and neither remote lock.

```sh
tclaude federation scripts --accept-remote on
tclaude federation scripts --memory 1GiB --pids 256
tclaude federation run --node laptop --node desktop --file maintenance.sh --timeout 2m
tclaude federation run --all -- echo 'hello from the fleet'
tclaude federation run -- printf '%s\n' 'local only'
tclaude federation run --node laptop --job JOB_ID
tclaude federation run --node laptop --job JOB_ID --log stdout
```

Scripts are bounded to 16 KiB and their encoded request to 32 KiB. They run as
private script files passed as argv to `/bin/sh` in a detached one-shot tmux
session on Linux (a managed one-shot subprocess on macOS), with the existing
process-group cleanup, timeout (exit 124), output
limit (4 MiB per stream, exit 125 on overflow), and Linux cgroup resource-limit
runner. This is code execution, not a filesystem/network sandbox. Root daemon
execution is refused. Default Linux limits are 1 GiB memory and 256 processes;
configured limits require working cgroup delegation and fail closed if it is
unavailable. macOS uses process-group and deadline cleanup, with no cgroups.
The local operator can change receiver-owned limits in settings. Jobs execute
in the daemon user's canonical HOME, never a peer-selected working directory.

`--all` selects this node and trusted linked nodes, reporting offline nodes as
skipped; explicit nodes that are offline are also skipped. No offline queue is
created. `--no-wait` returns durable per-node job IDs; later `--job` reads one.
The CLI collects result summaries and bounded output tails. Failed, canceled,
interrupted, or skipped outcomes return a nonzero command status. Re-run only
failed nodes by selecting their `--node` values again.

Dashboard/API routes (same tails under `/v1` for local CLI) are:

- `GET /api/node/run`: settings/availability including the receiving switch.
- `POST /api/node/run`: `{script, timeout_seconds?}`, returns 202 job metadata.
- `GET /api/node/run/jobs/{id}`: durable result with state, exit code,
  duration_ms, stdout_tail and stderr_tail (8 KiB each), hash and size.
- `GET /api/node/run/jobs/{id}/logs?stream=stdout&offset=0`: bounded 64 KiB
  chunks, `{data, next_offset, eof}` (`data` is base64); stderr is analogous.
- Local-only `GET/PUT /api/node/run/settings`: `{accept_remote_scripts?,
  resource_limits?: {memory?, cpu?, pids?}}`, with the full-code-execution warning.

Remote data routes use `/api/peer/{instance_id}/node/run...`, or
`/v1/federation/peer/{node}/node/run...` for CLI. The UI can fan out the same
per-node requests for checked nodes and poll their jobs independently. Active
jobs are canceled when either receiving lock is withdrawn. Peer jobs/logs are
readable only by their initiating peer and the receiving local operator.
Scripts and bounded logs remain private on the receiver; audits on each side
record operator, peer/node, script hash and size, job ID and result. The sender
records results when they are observed through job polling. After daemon
restart, unfinished jobs are marked interrupted and never replayed.

### Per-group federation links

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

Additional stable action concept keys are `spawn.inline`, `lifecycle.stop`, `lifecycle.retire`, `lifecycle.clone`, `lifecycle.move`, `lifecycle.teleport`.

### Remote operator actions

Remote actions use the peer-view dispatcher and the existing dashboard proxy.
The receiver authorizes every request and rechecks durable move authority before
retiring a source. It never gives the requester local human identity or the
source agent's own grants. Whole-agent stop/retire/clone/move actions need their
peer grant on every affected active group; clone/retire/move also cover owned groups.

- `POST /api/groups/{name}/spawn`: `{brief, name?, role?, profile?}` under
  `groups.members.spawn` and receiver-owned launch policy. Arbitrary cwd,
  harness, model, permissions and launch overrides are refused. Returns a
  durable spawn-request row (202); `GET /api/spawn-requests/{id}` reads only
  that initiating peer's request while its grant remains valid.
- `POST /api/agents/{id}/stop[?force=1]`: `groups.members.stop`.
- `POST /api/agents/{id}/retire`: `groups.members.retire`; worktree deletion
  and its options are not exposed remotely.
- `POST /api/agents/{id}/clone`: `groups.members.clone`, with optional
  `{follow_up, no_copy_conv}` and receiver-owned source defaults.
- `POST /api/agents/{id}/move`: `{group}`; `agent.move` plus source
  `groups.members.retire`. The destination is always the requesting peer.
- `POST /api/agents/{id}/teleport`: `{group, note?, clone?}` with the same
  source authority and existing teleport landing/confirmation policy.
- `POST /api/operator-message` retains the existing scoped messaging contract.

IDs are stable remote agent IDs, and `group` on move/teleport names the
receiving group on the requesting node. No third-node delegation is accepted.
Use `/api/peer/{instance_id}/<tail>` or the CLI's
`/v1/federation/peer/{node}/<tail>` proxy. The CLI equivalent is
`tclaude federation action ACTION --node PEER --agent ID`, adding
`--group GROUP --brief TEXT` for spawn, `--group GROUP` for move/teleport,
or `spawn-status --job ID` to inspect a remote launch. Message uses
`--body TEXT [--subject TEXT]`; clone uses `--follow-up`/`--no-copy-conv`.
The action command never retries mutations automatically.
### Operator access requests

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

### Local dashboard away administration

`GET /api/federation/away` reads `{away: null|{cover,since,until}}`;
POST the same path with `{cover,until?}` selects a trusted covering operator and
returns `{away,warnings}`. `POST /api/federation/return` ends forwarding.
`POST /api/federation/answer` sends `{ticket,decision:"approve"|"deny"}` using
the exact ticket supplied in the covering notice, returning `{envelope_id,state}`.
These local cookie-authenticated routes share the CLI's `/v1/federation` handlers.
A delegated answer is one-shot and never grants persistent permission. Peer
access requests remain decidable only by the receiving local operator and are
never forwarded to an away cover. None of these administration routes is peer-viewable.
