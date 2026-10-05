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
    This is the first slice: discovery and mail, CLI only. Remote control
    (spawning or stopping a colleague's agents, reading their transcripts),
    attachments, cross-instance group multicast, and a dashboard view are not
    built yet.

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
  only. The encryption key is derived from the identity key, so trusting a
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
| Your imports + slugs | which local groups may address which remote groups, and which agents may send |

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
`--frames-per-minute` (default 120) and `--bytes-per-minute` (default 2 MiB).
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
keeps peers, exports and imports.

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
removes your exports to it, your imports from it, and its cached catalog.

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

Nothing is exported by default. A peer receives a signed **catalog** listing
exactly what you export to it. Catalogs go straight to the peer and are never
published to the hub. They are re-sent when exports change, when the peer
comes online, and every couple of minutes so presence stays fresh.

## Discovery and imports: what your agents may address

```bash
tclaude federation remote                         # what each peer exports to you
tclaude federation import bob/builders --into team
tclaude federation unimport bob/builders --into team
```

An import is a directed link, like
[`groups link add`](agents-and-groups.md#inter-group-links): members of the
local group may address members of the remote group.

Agents (and you) see the imported members next to local ones with
`tclaude agent ls --remote`. The remote section shows each member's address,
harness, role, presence, remote group and the importing local group. An agent
sees only what its own groups import. Presence is marked stale when the peer
is offline or its catalog has not been refreshed for several minutes. With
`--json` the output becomes `{"local": [...], "remote": [...]}`.

## Sending

Agents use the ordinary messaging command with a `member@peer` address:

```bash
tclaude agent message bob-agent@bob "can you review PR 42?"
tclaude agent reply <id> "done"     # replies to remote mail go back over federation
```

A spontaneous remote send requires all of the following:

- the remote group exports `mail` to you;
- it is imported into a local group the sender belongs to;
- the sender holds **`federation.message`**, which is not default-granted and
  not conferred by group ownership. Narrow it to the importing group with
  `--scope group=<local-group>`, and to one remote instance with
  `--scope peer=<instance id>`:

```bash
tclaude agent permissions grant lead federation.message --scope group=team
tclaude agent permissions grant lead federation.message --scope group=team --scope peer=inst_…
```

The `peer` scope takes the full instance id from `tclaude federation peers`,
not a label: labels are local nicknames you can move, and a grant must not
follow one to a different instance.

Replies to received remote mail need neither an import nor the slug. The
other side accepts them because they answer mail it sent from that agent.

The operator can send as the human:

```bash
tclaude federation send bob-agent@bob "hello" --subject intro
```

`member` may be the member's name, its agent id, or an 8+ character id
prefix. `peer` may be the label, the hub-reported name, or an instance-id
prefix. An address whose `@…` part does not name a trusted peer is resolved
locally as before, so local titles containing `@` keep working.

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

Bodies are text only, up to 16 KiB. The receiver applies a per-peer rate
limit (30 mails a minute) and the usual unprocessed-message cap per recipient.
A full inbox is retried rather than refused. Inbound remote mail is recorded
in the [audit trail](permissions-and-audit.md) as `federation.mail.in`, and
every operator change under `/v1/federation/*` is audited too.

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
- No attachments, no `group:` multicast across instances, no `--cc` to
  remote recipients.
- Mail only: no remote spawn, stop, or transcript access.
- Operator mail is sent from the CLI only; agents cannot reach a remote
  operator.
- One hub per instance. Hub-to-hub federation is a later step.
- CLI only; the dashboard does not show federation yet.
