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

## 7. Share a group

Nothing is shared until you grant it. On the **desktop**, grant access to a group to
the laptop:

```bash
tclaude federation grant laptop message.direct --scope group=builders
tclaude federation grant laptop groups.roster.read --scope group=builders
tclaude federation grant laptop groups.presence.read --scope group=builders
```

On the **laptop**, see what the desktop offers and grant an agent reach to
the remote group:

```bash
tclaude federation remote
tclaude agent permissions grant lead message.direct --scope peer=desktop/builders
tclaude agent ls --remote
```

## 8. Send a message

As the operator, from the laptop:

```bash
tclaude federation send some-agent@desktop "hello from the laptop"
```

For an agent to send on its own, it needs `message.direct` scoped to the
desktop peer and its builders group:

```bash
tclaude agent permissions grant lead message.direct --scope peer=desktop/builders
```

The agent then uses its ordinary messaging command with the remote address,
and replies come back the same way:

```bash
tclaude agent message some-agent@desktop "can you review PR 42?"
```

## Where to go next

- More capabilities to export: `attachments`, `spawn` (ask for a worker on
  the other side) and `routes` (open a TCP service across instances). See
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
| `remote` lists nothing | the other side has not exported a group to you, or has not trusted you yet |
| an agent's send is refused | the peer does not grant mail access, or the agent lacks `message.direct` scoped to the peer/group |
