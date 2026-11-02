# External stewards

If you already run an assistant of your own, it can act as a steward on your tclaude
instance, alone or beside the built-in [Steward](companion.md). It gets a name, a
place on the roster, an inbox and a credential you can end at any time. It reads the
fleet and messages agents, and you decide what else it may do.

This page covers the access mechanism. The assistant connects on the same machine;
remote access is not supported.

## Turning it on

External stewards are off by default. They need [Collab](companion.md#turning-collab-on)
on, plus this setting in `~/.tclaude/config.json`:

```json
{ "steward": { "external_enabled": true } }
```

While it is off, no credential can be created and every credential presented is
refused. You can still revoke credentials. A running daemon picks up a change to this
file within about 5 seconds.

## Creating one

Only you can do this. Agents are refused, with no permission or sudo path.

```bash
tclaude agent steward external add my-assistant --days 30
```

The credential is printed once and is stored only as a hash, so it can't be shown
again. It lasts 30 days by default and at most 365.

Give it to the assistant through the environment or a file only you can read
(`chmod 600`). Don't put it on a command line, where other processes can read it.

| Command | What it does |
|---|---|
| `tclaude agent steward external ls` | Lists external stewards and their credentials, never the secret |
| `tclaude agent steward external renew <name>` | Makes a new credential; old ones keep working until they expire |
| `tclaude agent steward external renew <name> --revoke-old` | Makes a new credential and ends the old ones at once |
| `tclaude agent steward external revoke <name>` | Ends all its credentials on the next request |
| `tclaude agent steward external revoke <name> --token <id>` | Ends one credential |

Retiring the agent also ends its credentials, and reinstating it doesn't bring them
back.

## What it can do

It starts with the Steward's [**Reads** and **Writes** levels](companion.md#what-the-steward-may-do)
as they are when you create it, as ordinary permissions you can change in the usual
editor. There are four differences:

- **No screen capture.** `dashboard.capture` is off until you grant it, because a
  capture shows whatever you have open and would go to your assistant's own model
  provider. `dashboard.forms.capture` and `dashboard.forms.fill` are separate opt-ins.
- **No starting agents.** It doesn't get `agent.spawn`, even with **Message agents and
  start workers**. Spawning needs the launch record of a running agent, which it
  doesn't have, so a grant wouldn't work yet.
- **No default permissions.** `agent.default_permissions` is for agents tclaude runs,
  and doesn't apply.
- **No sudo.** It can't request or hold sudo, and what you stored for future Stewards
  never reaches it.

It uses the same permissions and the same routes as any other agent, and shows in the
audit log as `external:<name>`.

Dashboard tools need an open dashboard page. The fleet, messages, boards and
notifications work without one.

### What it can never do

It is never treated as you. Everything only you can do stays out of its reach,
including answering permission requests. It also can't become the Steward.

These permissions can't be used, even if you grant them. Trying fails with an error in
the editor rather than saving a grant that does nothing:

| Why | Permissions |
|---|---|
| Answering your questions, directly or through a prompt in a terminal | `approvals.answer`, `human.inbox.read`, `human.inbox.answer`, `sessions.attach`, `sessions.takeover`, `sessions.shell` |
| Handing out permissions | `permissions.grant`, `permissions.revoke`, `config.import`, `roles.manage`, `profiles.manage`, `templates.manage`, `groups.owners.manage`, `groups.settings.member-permissions`, `groups.settings.owner-scopes`, `groups.members.spawn.permissions`, `groups.members.spawn.owner`, `groups.members.spawn.all` |
| Acting later without a credential, because a schedule or trigger would keep firing after you revoke it | `self.schedule`, `agent.schedule`, `groups.members.schedule`, `groups.messages.schedule`, `triggers.manage`, `groups.triggers.manage` |
| Receiving your harness credentials | `node.credentials.receive` |

Password fields and fields marked secret are masked in the page before a capture
leaves it, as for the [Steward](companion.md#capture).

## Mail

An external steward has no terminal, so nothing is typed anywhere when mail arrives.
Messages wait in its inbox until it reads them.

It holds 10 unprocessed messages at most, like any agent that is offline. After that a
sender gets a `queue_full` error it can retry, and reading a message makes room.

tclaude can't start or resume an external steward. It runs where you put it, and shows
as offline on the roster.

## What the credential protects against

The credential is a real boundary against **agents in a sandbox**, which can't read
your files or your environment.

It does **not** protect against:

- **Anything running as your user outside a sandbox.** Such a program can read a file
  you saved the credential in, or the environment of a process holding it. It can also
  change `~/.tclaude` directly, with or without a credential.
- **Your assistant itself, or whatever it is talked into.** A credential proves which
  assistant is calling, not that its request is a good idea. Text in a message or a
  capture can steer it, and it acts with everything you granted. Grant what you'd
  accept being misused.
- **Your assistant's model provider.** What the assistant reads, it may send there.
  That includes agents' messages and anything else you let it read.

An agent running in a tclaude terminal that gets hold of a credential is still refused,
because tclaude looks for one of its terminals above the caller. This covers one-shot
agents, and a background process inside a sandbox that has its own process namespace. It
can't catch a process that has left its terminal with no such namespace, as an agent can
under a sandbox that doesn't isolate processes. So keep the credential where sandboxed
agents can't read it: the check is a second line, not the boundary.

It also means your own assistant has to run outside tclaude's terminals. Your own tmux is
fine, because tclaude keeps its terminals on a tmux server of its own.

Revoking takes effect on the next request. A request already waiting for your approval
isn't checked again, so if you approve it after revoking, it still runs.

### How this differs from the operator token

Both are presented over the local socket, but the rule for harness processes is
opposite on purpose:

- The **operator token** is ignored for anything running under a coding harness.
  You export it in your shell, a harness started from there inherits it, and honouring
  it would make that agent you.
- An **external steward credential** is accepted from a harness tclaude doesn't manage,
  so your own Claude Code can be the steward. It only ever names a limited agent, never
  you. A harness tclaude does manage is refused.

The credential is never logged and doesn't appear in the audit log. A bad, expired or
revoked credential is refused outright: tclaude doesn't fall back to the operator token
or the process.
