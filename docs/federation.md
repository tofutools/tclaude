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
    watch and interactive attach. A federation dashboard view is not built yet.

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
mail yet, and replying from the dashboard is not wired up: answer with
`tclaude federation notify`.

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

The peer sends only the name, role and brief. Launch profile, directory,
harness and model come from the receiving peer grant; unset fields inherit
the group's normal operator spawn defaults. The positive live auto-worker
cap defaults to two and counts that peer's live automatically spawned workers
across groups. The receiving spawn rate limit also applies, keyed by peer.
Ordinary group member caps and launch guardrails remain in force.

A visible group without a spawn grant queues the request for human approval.
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
- CLI only; the dashboard does not show federation yet.

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
must cover at least two renewal intervals. Policies are local and re-read live.
Retiring the backup or replacing its generation releases its dormant slot and
supersedes the old remote on its next contact. Revoking `self.teleport` prevents
further accepted renewals; recovery still observes the full online wait.

`report` and a leased copy's `--home` commit bounded findings (up to 16 KiB for
report), then stop the roaming copy. Only after its node verifies the stop does
the origin deliver the findings and resume the original identity. Retries use
the same durable return ID and never launch a second identity. Lost connectivity
leaves a return pending; use `teleport status` to inspect it. An operator can
retire the stopped roaming identity after the origin acknowledges recovery.
An ordinary `--home` without a paused backup still uses the normal teleport
policy, group selection and revisit limits above.

### Model gateways (Claude Code)

A trusted machine can provide a named Anthropic Messages gateway to Claude
Code workers on another machine. The provider key stays on the gateway machine;
the worker receives a random, per-launch loopback bearer. Gateway traffic uses
sealed federation control messages and an encrypted, flow-controlled hub stream.
The relay cannot read prompts or responses. This first binding supports ordinary Claude
Code workers only; non-interactive one-shot runs refuse a proxy choice; it does not transfer subscriptions or implement requester billing.

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

All six daily limits and both token bounds must be positive. Model entries match
exact IDs or a trailing `*` prefix pattern, such as `claude-sonnet-*`.
`precount_input` defaults to false: input bounds are checked from provider usage,
with the request byte cap providing the hard pre-flight bound. Set it to true to
count input through the provider before generation; this adds a round trip and
requires the provider token-counting endpoint. Every request reserves the maximum input plus
requested output tokens atomically against gateway, peer, and session budgets.
Complete terminal usage replaces that reservation, including cache tokens.
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
