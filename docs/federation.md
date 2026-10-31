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

For a step-by-step first setup, including the hub's TLS certificate, see
the [setup walkthrough](federation-setup.md).

## Find your way around

| I want to | Go to |
|---|---|
| Link my first two machines | [Setup walkthrough](federation-setup.md) |
| Run the relay | [Running a hub](federation-hub.md) |
| Join a hub, trust peers, share groups | [Connect and share](federation-peers.md) |
| Message, spawn, run jobs or open terminals on a peer | [Work across nodes](federation-work.md) |
| Move, share or teleport an agent | [Move agents and config](federation-moving.md) |
| Share models or a subscription, and cap spend | [Models and usage](federation-models.md) |
| Update nodes, manage harnesses, run scripts, recover keys | [Run the fleet](federation-admin.md) |
| Share skills and config with people outside my peers | [Content boards](federation-boards.md) |
| Do all this in the browser | [Fleet UI](fleet-ui.md) |
| Look up an API contract | [Federation reference](federation-reference.md) |

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

## Upgrading

The envelope format is versioned. Instances on different envelope versions
cannot exchange mail (envelopes are dropped and outbox rows eventually
expire), so upgrade linked instances together. The current version is 2.

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
  instance against the hub's per-instance frame budget (600 a minute by
  default, shared with mail). A peer accepts at most 240 opens a minute.
- Mirror routes cannot be shared onward to another peer.
- Attachments ride inline and are capped at 512 KiB per message; operator
  mail and replies cannot carry them.
- No remote transcript access. Remote spawn runs under a receiving peer grant or
  waits for that operator’s approval.
- Agents cannot reach a remote operator; operator mail is for operators.
- One hub per instance.
