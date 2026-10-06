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
    groups, operator mail, request-and-approve remote spawn, and
    cross-instance group routes. Stopping or reading a colleague's agents
    and a dashboard view are not built yet.

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
  what you exported to it.
- **Remote content is untrusted.** Every inbound remote body starts with a
  banner naming its origin, and the sender shows as `name@peer (remote)`.
- **Approvals stay local.** Permission prompts, `--ask-human`, sudo and
  grants are never actionable by a remote party.

Three layers decide what is allowed, and each one can only narrow:

| Layer | Decides |
|---|---|
| Hub | which instances may connect, who can see whom (spaces), rate limits |
| Your exports | which local groups a peer can see and mail, with which capabilities |
| Your peer-scoped grants | which agents and local group members may act on which peer groups |

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

All `tclaude federation` commands are human-only.

```bash
tclaude federation identity                  # your instance id: give it to the hub admin
tclaude federation connect wss://hub.example:8470 [--invite tchi_…] [--name alice]
tclaude federation status
```

Connection settings live under `federation` in
`~/.tclaude/data/config.json` (`enabled`, `hub_url`, `name`, `invite`,
`hub_ca_file`). Use `--ca-file` when the hub's certificate is signed by a
private CA. `tclaude federation disconnect` turns the connection off but
keeps peers and exports.

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
removes your exports to it, its cached catalog.

## Exports: what a peer may see and mail

```bash
tclaude federation export builders --to bob --cap roster,presence,mail
tclaude federation export builders --to '*' --cap mail   # every trusted peer
tclaude federation unexport builders --to bob
```

| Capability | Grants |
|---|---|
| `roster` | member names and roles |
| `presence` | online/offline per member |
| `mail` | members may receive mail from the peer (names and ids are shared so they are addressable) |
| `attachments` | with `mail`: that mail may carry files |
| `spawn` | the peer may *ask* for a worker to be spawned into the group; you approve or deny each request |
| `routes` | the group's ready [group routes](group-routes.md) are listed, and the peer's agents may open them |

Nothing is exported by default. A peer receives a signed **catalog** listing
exactly what you export to it. Catalogs go straight to the peer and are never
published to the hub. They are re-sent when exports change, when the peer
comes online, and every couple of minutes so presence stays fresh.

## Discovery: what your agents may address

```bash
tclaude federation remote                         # what peers share with you
tclaude agent permissions grant lead message.direct --scope peer=bob/builders
```

Agents see remote members next to local ones with `tclaude agent ls --remote`.
The remote section shows each member's address, harness, role, presence and
remote group. An agent sees catalog groups covered by its effective peer-scoped
mail, spawn or route grants; the operator sees all received catalogs. Grants on
local groups give their current members the same reach. Presence is marked
stale when the peer is offline or its catalog has not refreshed for several
minutes. With `--json` the output is `{"local": [...], "remote": [...]}`.

Federation scopes use `peer=<label-or-instance-id>[/<remote-group>]`.
The daemon resolves labels at grant time and stores the full trusted instance
id, so renaming or reassigning a label cannot redirect the grant. Unknown peers
are refused. Remote group names need not appear in the current catalog when
granting: a catalog may be stale. A peer-only scope covers all that peer's
groups, including future groups. Git and GitHub scopes continue to use `remote=`.

Unscoped grants, local `group=` scopes, defaults and group ownership never
authorize remote actions. Peer-scoped grants never authorize local actions.

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
tclaude agent permissions grant lead message.direct --scope peer=bob/builders
tclaude agent permissions grant lead message.direct --scope peer=bob
```

A missing standing grant can use the usual `--ask-human` approval for one send.
Replies to received remote mail need no standing grant. The other side accepts
them because they answer mail it sent from that agent.

The operator can send as the human:

```bash
tclaude federation send bob-agent@bob "hello" --subject intro
```

`member` may be the member's name, its agent id, or an 8+ character id
prefix. `peer` may be the label, the hub-reported name, or an instance-id
prefix. An address whose `@…` part does not name a trusted peer is resolved
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
with mail capability, and you need `message.direct` scoped to its peer/group. One envelope crosses the hub. The receiving instance
delivers it to the group's members as of arrival, not to the roster in your
catalog. `--role` narrows the recipients there, case-insensitively. It needs
the group's `roster` export, since otherwise it would reveal roles that the
export hides. Members can reply, and their replies come back to you.

Group mail is text only: no `--cc`, `--attach` or member subsets. Each
recipient counts against the peer's inbound mail budget. Delivery is
reported like this:

- When nobody matches, the mail is refused (`no_recipients`).
- When every recipient's backlog is full, it is retried later.
- Otherwise `tclaude federation outbox` shows it as accepted, with
  "delivered to N members" when the roster is shared.

### Attachments

```bash
tclaude agent message bob-agent@bob "build log attached" --attach build.log --attach shot.png
tclaude federation send bob-agent@bob "see attached" --attach diff.patch
```

The CLI reads the files as the caller, so an agent can attach only what it
can read itself. Files travel inside the encrypted envelope: at most 4 per
message and 512 KiB in total. The receiver accepts them only for a recipient
in a group exported to the sender with both `mail` and `attachments`. It
re-derives each file's name and type itself and accepts only images, text,
Markdown, CSV, JSON, YAML, diffs/patches and PDF; HTML, SVG, archives and
executables are refused. Each peer may keep at most 64 MiB of files on the
receiving side. Received files appear on the message like any other inbox
attachment. Local recipients do not take attachments: send them a path.

## Operator to operator

Operators of two trusted instances can message each other directly, with no
export or import involved; trusting a peer is the consent.

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

An agent can ask a peer for a worker in one of the peer's groups. It is
always a request; nothing runs until that instance's operator approves it.

```bash
# requester side (agent-callable)
tclaude agent permissions grant lead groups.members.spawn --scope peer=bob/builders
# alternatively: agent.spawn --scope peer=bob covers all groups offered by bob
tclaude federation spawn-request builders@bob --brief "port the parser to Go" --name parser-port --role worker
```

The remote group must appear in the peer's catalog. The requester needs
`groups.members.spawn` scoped to that peer/group, or `agent.spawn` scoped to
the peer without a group suffix. On the receiving side:

```bash
tclaude federation requests [--all]           # pending requests, with their briefs
tclaude federation requests approve 7 [--profile p] [--cwd dir] [--harness h] [--model m] [--name n]
tclaude federation requests deny 7 --reason "no capacity this week"
```

You choose how the worker launches. Approval goes through the ordinary
group spawn path, so group guardrails, member caps and spawn rate limits
apply. The worker joins the exported group with the brief as its first
message, bannered as an outside request, so it becomes reachable to the
requester through the existing export. If the spawn fails, the request stays
pending and you can retry with other options. The decision travels back
and lands in the requester's inbox (the operator's inbox when the operator
asked).

You are notified of each new request in your inbox. A peer may have at most
10 undecided requests here, and requests expire after 72 hours. Untrusting
the peer makes its pending requests unapprovable.

## Remote group routes

A [group route](group-routes.md) can be opened from another instance. Export
the publisher's group with `routes`. The peer then imports it into a local
group whose members hold `routes.consume`, and those members open the route
by naming the peer:

```bash
# publisher side (operator)
tclaude federation export svc --to bob --cap roster,routes

# consumer side (operator, then agent)
tclaude federation import alice/svc --into team
tclaude agent routes open api-server/api@alice -g team
```

The consumer gets an ordinary lease and local endpoint, exactly as for a
local route. Neither sandbox changes. On the consumer's side, `agentd`
creates a private mirror route that only the opening agent can see or use,
and serves it itself. On the publisher's side, `agentd` connects to the real
route like any other consumer. `tclaude federation status` lists each
exported route as `route <publisher>/<name>@<peer>`.

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
- the export loses `routes`;
- the import is removed;
- the peer is untrusted;
- the group's membership changes.

A route that is not exported is refused with the same answer as one that
does not exist.

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
| `refused` | rejected by the peer (for example `not_exported`); final |
| `expired` | never acknowledged in time |

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
It pairs them, exports a group holding a model-free `shell` agent, imports it
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
- Routes are not re-exported: a mirror cannot be exported onward.
- Attachments ride inline and are capped at 512 KiB per message; operator
  mail and replies cannot carry them.
- No remote stop, restart, or transcript access; remote spawn is
  request-and-approve only.
- Operator mail is sent from the CLI only; agents cannot reach a remote
  operator.
- One hub per instance. Hub-to-hub federation is a later step.
- CLI only; the dashboard does not show federation yet.
