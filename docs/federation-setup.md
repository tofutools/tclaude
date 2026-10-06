# Federation setup walkthrough

This walks through linking two tclaude instances (say, a laptop and a
desktop) from nothing to an agent on one mailing an agent on the other.
[Federation](federation.md) is the reference for everything shown here.

You need three things:

- a **hub** (`tclaude-hub`) running somewhere both machines can reach;
- **tclaude** on each machine, with `tclaude agentd` running;
- a group with at least one agent on each side.

## 1. Install the hub

`tclaude-hub` is a separate binary. It is in the release archives, or:

```bash
go install github.com/tofutools/tclaude/cmd/tclaude-hub@latest
```

Run it on any machine both instances can reach: one of the two machines
themselves, a VM, or a team server. It only relays and keeps a small
admission database; it never stores messages.

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

The hub's state lives in `$TCLAUDE_HUB_DIR` (default `~/.tclaude-hub/`). To
keep it running, use the systemd unit in
[Running a hub](federation.md#running-a-hub).

## 4. Admit both machines

The hub admits nobody by default. Create one single-use invite per machine.
Instances only see each other when they share a **space**, so put both in
the same one:

```bash
tclaude-hub invite --space home --ttl 24h   # prints tchi_…, once per machine
tclaude-hub ls                              # later: who is admitted, last seen
```

## 5. Connect each machine

On each machine, with its own invite token:

```bash
tclaude federation connect wss://myhub.example:8470 --invite tchi_… --name laptop --ca-file hub.crt
tclaude federation status
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
**approvals remain local and cannot be answered remotely**. The hub cannot
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
tclaude agent permissions grant lead message.direct --scope peer=desktop/builders
```

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
  [Peer grants](federation.md#peer-grants-what-a-peer-may-see-and-do).
- Mailing a whole remote group, `--cc` to remote members, operator to
  operator: [Sending](federation.md#sending).
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
