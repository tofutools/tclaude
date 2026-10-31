# Federation setup walkthrough

This walks through linking two tclaude instances (say, a laptop and a
desktop) from nothing to an agent on one mailing an agent on the other.
[Federation](federation.md) maps the rest of what you can do with linked nodes.

You need three things:

- a **hub** (`tclaude-hub`) running somewhere both machines can reach;
- **tclaude** on each machine, with `tclaude agentd` running;
- a group with at least one agent on each side.

Steps 5 to 7 can also be done in each machine's dashboard: **⚙ Fleet config →
Setup guide** (**Fleet** at the right of the tab bar, then ⚙ Fleet config) joins the hub
from a pasted join code (or the address and invite), trusts the other machine after you compare
fingerprints, and shares a group. It shows the CLI command for every step,
and step 6 of the guide lists what each peer shares and the agent grants
that use it. The hub itself (steps 1 to 4) is set up on the command line; the guide's
optional first two steps explain it and claim hub admin from the dashboard.

## 1. Install the hub

`tclaude-hub` is a separate binary. It is in the release archives, or:

```bash
go install github.com/tofutools/tclaude/cmd/tclaude-hub@latest
```

Run it on any machine both instances can reach: one of the two machines
themselves, a VM, or a team server. It only relays and keeps a small
admission database, plus encrypted board content if you use boards; it never
stores messages.

## 2. Give the hub a TLS certificate

Clients only accept a plain `ws://` hub on loopback, so a hub on the network
needs TLS. Either front it with a TLS proxy (Caddy, nginx) that has a real
certificate, or make a self-signed one.

List every name and address clients will use in the `wss://` URL in the
certificate's `subjectAltName`, comma-separated. Clients check the host in
the URL against it.

```bash
openssl req -x509 -newkey ed25519 -nodes -days 365 -subj "/CN=hub" \
  -addext "subjectAltName=DNS:myhub.example,DNS:myhub,IP:192.168.1.10,IP:127.0.0.1" \
  -keyout hub.key -out hub.crt

openssl x509 -in hub.crt -noout -ext subjectAltName   # check what it covers
```

Wildcards (`DNS:*.example.com`) cover one subdomain level. If the names
change, regenerate the certificate, restart the hub and copy the new
`hub.crt` to the clients.

## 3. Start the hub

```bash
tclaude-hub serve --listen 0.0.0.0:8470 --tls-cert hub.crt --tls-key hub.key
```

The hub's state lives in `$TCLAUDE_HUB_DIR` (default `~/.tclaude/data/hub/`
for the user running the hub). Existing `~/.tclaude-hub/` directories stay in
use with a migration hint; no state is moved automatically. To
keep it running, use the systemd unit in
[Running a hub](federation-hub.md#running-a-hub).

## 4. Admit both machines

The hub admits nobody by default. Create one single-use invite per machine.
Instances only see each other when they share a **space**, so put both in
the same one. With `--url` (and `--ca-file` for a self-signed certificate)
the hub prints a **join code** instead: one `tchj_…` string holding the hub
address, the invite and the certificate to trust, so a machine needs nothing
else to join:

```bash
tclaude-hub invite --space home --ttl 24h --url wss://myhub.example:8470 --ca-file hub.crt   # tchj_…, once per machine
tclaude-hub invite --space home --ttl 24h   # or just the invite, tchi_…
tclaude-hub ls                              # later: who is admitted, last seen
```

A join code carries a bearer invite and decides which certificate the
machine trusts for its hub: send it like a password, and only accept one from
your hub admin. Once a node is a hub admin, its dashboard (**⚙ Fleet config → Hub →
Invites**) and `tclaude federation hub invites create` mint invites as join
codes too, using the hub address and CA file that node connects with.

## 5. Connect each machine

On each machine, with its own join code:

```bash
tclaude federation join --code-stdin --name laptop   # paste the tchj_… code, then Ctrl-D
tclaude federation status
```

`join` first prints what the code holds (hub, space, invite expiry and the
SHA-256 of any certificate it trusts; compare that with the hub admin), then
stores the certificate under `~/.tclaude/data/federation/hub-ca/` and
connects. `--preview` only prints. The code can also be the argument or come
from `--code-file`. In the dashboard, paste it into **⚙ Fleet config → Setup guide**.

Or with a bare invite token:

```bash
tclaude federation connect wss://myhub.example:8470 --invite tchi_… --name laptop --ca-file hub.crt
```

`--ca-file` is only needed for a self-signed certificate: copy `hub.crt` to
the machine first and keep it there, since `agentd` reads it from that path
on every connect. `--name` is how this instance shows up to others. The
settings are saved under `federation` in `~/.tclaude/data/config.json`, so
the connection comes back after a restart.

## 6. Pair the two instances

The hub shows each instance the others in its spaces. Trusting one is your
decision, made on each side:

```bash
tclaude federation peers                       # the other instance + its fingerprint
tclaude federation trust inst_… --label desktop
```

Compare the fingerprint with what `tclaude federation identity` prints on
the other machine. The label is the short name used in addresses
(`agent@desktop`).

For your own machines, you can instead opt into unrestricted trust on each
side with `tclaude federation trust <label> --level unrestricted`. Confirm the
fingerprint and permissions when prompted (`--yes` for scripts). This grants
all peer permissions on all live groups and auto-approves spawns using group
defaults, with a default cap of 8 automatic workers per peer (configure
`federation.unrestricted_max_live`). Local unscoped agent grants and defaults
then work towards that peer. Group scopes and ownership still do not, and
**interactive attach permits answering harness approvals and prompts**;
`--ask-human` still uses the local operator workflow. The hub cannot
change this level. `peers` and `status` show it; `trust <label> --level restricted`
downgrades immediately. The remaining steps use restricted trust.

## 7. Share a group

These are two separate grants: the desktop’s operator grants the laptop
**instance** access; the laptop’s operator grants a local **agent** permission
to use it. With the default restricted level, local defaults do not authorize remote
actions. Group ownership never does.

On the **desktop**, where the laptop is trusted with label `laptop`, grant
mail access to the local group `builders`. Roster and presence grants also
share roles and online/offline status:

```bash
tclaude federation grant laptop message.direct --scope group=builders
tclaude federation grant laptop groups.roster.read --scope group=builders
tclaude federation grant laptop groups.presence.read --scope group=builders
tclaude federation grants laptop
```

On the **laptop**, where the desktop is trusted with label `desktop`, see
what it offers and grant the local agent `lead` permission to mail its
`builders` group. Run the first two commands as the operator:

```bash
tclaude federation remote
tclaude agent permissions peer-reach add lead --peer desktop/builders --slug message.direct
```

`peer-reach add` keeps any other peer groups `lead` can already mail. Use
`--group <group>` instead of `lead` to give every member of a local group
the reach. In the dashboard, step 6 of **⚙ Fleet config → Setup guide** does the
same in place.

The agent can then discover the shared members:

```bash
tclaude agent ls --remote
```

## 8. Send a message

As the operator, from the laptop:

```bash
tclaude federation send some-agent@desktop "hello from the laptop"
```

The operator send uses the desktop’s peer mail grant and needs no local
agent grant. With the peer-scoped grant from step 7, `lead` can send using
its ordinary messaging command:

```bash
tclaude agent message some-agent@desktop "can you review PR 42?"
```

The recipient can answer with `tclaude agent reply <id> "done"`; a reply to
received remote agent mail needs no standing grant in the reverse direction.

## Where to go next

- Other peer grants: `message.attachments` (files with mail),
  `groups.members.spawn` (automatically launch workers under local settings)
  and `routes.consume` (open a TCP service across instances). Agents requesting
  workers or opening routes also need the corresponding `peer=` grants. See
  [Peer grants](federation-peers.md#peer-grants-what-a-peer-may-see-and-do).
- Mailing a whole remote group, `--cc` to remote members, operator to
  operator: [Sending](federation-work.md#sending).
- Trying it on one machine first: `scripts/federation-smoke.sh` starts a hub
  and two instances locally ([Trying it locally](federation.md#trying-it-locally)).

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `connect` fails with a certificate error | the host in the URL is not in the certificate's `subjectAltName`, or `--ca-file` is missing |
| `connect` refuses a `ws://` URL | plain `ws://` is accepted only for a loopback hub |
| `status` shows connected but `peers` is empty | the instances are in different spaces (`tclaude-hub ls`), or the other one is offline |
| `remote` lists nothing | the other side has not granted your instance access to a group, or has not trusted you yet |
| `agent ls --remote` lists nothing | the agent lacks a matching `peer=` grant, or the peer's catalog contains no shared members |
| an agent's send is refused | the peer lacks a `message.direct` grant for your instance on the target group, or the agent lacks `message.direct` scoped to the peer/group |
| a local grant does not permit a remote send | remote actions require `peer=desktop/builders`; unscoped and local grants never authorize them |
| a spawn request stays `pending` | the receiving peer has no `groups.members.spawn` grant, or an automatic launch failed or hit a worker/rate cap; its operator can inspect and approve it |
| a worker stays `launching` | startup is unconfirmed; inspect `tclaude federation requests --all` before explicitly abandoning the launch |

## Watch or attach to a remote agent

The target operator grants `sessions.read` for discovery and `sessions.watch`
for viewing, or `sessions.attach` for viewing and full keyboard input:

```bash
tclaude federation grant laptop sessions.read --scope group=builders
tclaude federation grant laptop sessions.watch --scope group=builders
# Optional full keyboard authority, including harness approval answers:
tclaude federation grant laptop sessions.attach --scope group=builders
# Optional: let the laptop disconnect every other viewer (--take-over):
tclaude federation grant laptop sessions.takeover --scope group=builders

# On the laptop, use the stable address returned by sessions:
tclaude federation sessions desktop
tclaude federation attach agt_…@desktop --read-only
# Omit --read-only for interactive access. Ctrl-] detaches.
```

A local agent needs the corresponding `sessions.watch` or `sessions.attach`
grant scoped to `peer=desktop/builders`, plus `sessions.read` to discover the
address. Unrestricted trust includes these permissions.

The target requires tmux 3.2+, one window and one pane. A visible pane border
indicator identifies remote viewers. Viewers and the target's own terminals
share the window size: the smallest attached terminal sets it.
Its operator can run `tclaude federation viewers` and
`tclaude federation kick <viewer-id>` to remove a viewer immediately.
See [remote terminals](federation-work.md#remote-terminals) for revocation and audit
behavior.
