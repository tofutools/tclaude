# External stewards

If you already run an assistant of your own, it can act as a steward on your tclaude
instance, alone or beside the built-in [Steward](companion.md). It gets a name, a
place on the roster, an inbox and a credential you can end at any time. It reads the
fleet and messages agents, and you decide what else it may do.

This page covers the access mechanism. The assistant connects on the same machine;
remote access is not supported.

## Turning it on

External stewards are off by default. They need [Collab](companion.md#turning-collab-on)
on, plus **Allow external stewards** under **Config** › **Steward**. That page lists what
an assistant can do, so you can read it first. In `~/.tclaude/config.json` the setting is:

```json
{ "steward": { "external_enabled": true } }
```

While it is off, no credential can be created and every credential presented is
refused. You can still revoke credentials. A running daemon picks up a change to this
file within about 5 seconds.

## Creating one

Only you can do this. Agents are refused, with no permission or sudo path, and so is
another instance viewing your dashboard. Use **Your external stewards** under **Config** ›
**Steward**, or the command line:

```bash
tclaude agent steward external add my-assistant --days 30
```

The credential is printed once and is stored only as a hash, so it can't be shown
again. It lasts 30 days by default and at most 365. On the Config page it stays on screen
until you press **Done**, and it's left out of anything a Steward captures from the page.

Give it to the assistant through the environment or a file only you can read
(`chmod 600`). Don't put it on a command line, where other processes can read it.

| Command | What it does |
|---|---|
| `tclaude agent steward external ls` | Lists external stewards and their credentials, never the secret |
| `tclaude agent steward external renew <name>` | Makes a new credential; old ones keep working until they expire |
| `tclaude agent steward external renew <name> --revoke-old` | Makes a new credential and ends the old ones at once |
| `tclaude agent steward external revoke <name>` | Ends all its credentials on the next request |
| `tclaude agent steward external revoke <name> --token <id>` | Ends one credential |

The Config page can list, add, renew and revoke too. Its **Revoke** ends all of a
steward's credentials, and to end old ones while keeping a new one, use the command line.

Retiring the agent also ends its credentials, and reinstating it doesn't bring them
back.

## Connecting your assistant

Add `tclaude mcp serve` to your assistant's MCP settings. It talks to your assistant over
standard input and output, so the assistant starts it. The tclaude daemon has to be running
on the same machine, and the assistant has to run outside tclaude's terminals.

For example, in a Claude Code `.mcp.json`:

```json
{
  "mcpServers": {
    "tclaude": {
      "command": "tclaude",
      "args": ["mcp", "serve", "--credential-file", "/home/you/.config/tclaude-steward"]
    }
  }
}
```

Create that file with `chmod 600`. `tclaude mcp serve` refuses a credential file anyone else
can read, and it has no option that takes the credential itself. You can set
`TCLAUDE_EXTERNAL_STEWARD_CREDENTIAL` in its environment instead, but not both.

It reads the credential once when it starts, so restart it after a renewal.

| Tool | What it does | What decides it |
|---|---|---|
| `whoami` | Shows its name and identity | Always available |
| `fleet_brief` | Reads the fleet: who is working, idle, offline or needs you | Needs `fleet.read` |
| `list_agents` | Lists the agents and groups it can reach | Always shows its own groups and agents in no group; `fleet.read` adds every group |
| `inbox` | Lists its messages, with previews | Always available |
| `read_message` | Reads one message and marks it read | Always available |
| `send_message` | Sends a message to an agent or group, including `member@peer` on a linked instance | An agent needs `message.direct`, a shared or owned group, or a group link. A group needs it to be a member or owner. Another instance needs a group link or `message.direct` |
| `reply` | Replies to a message it received | Always available: having received a message is enough |
| `notify_human` | Sends you a message in the dashboard | Needs `human.notify`, or owning a group |
| `wait_for_activity` | Waits up to 50 seconds for new mail or a change in what needs attention | Always watches mail; `fleet.read` adds what needs attention |

So taking `message.direct` away doesn't stop it messaging agents it can reach through a group.

**Config** › **Steward** shows this list with Collab on, whether or not the feature is on.

Nothing wakes an idle assistant: it only sees new mail when it asks. Call `wait_for_activity`
in a loop to stay current, passing back the `cursor` from the last call. Past 10 unread
messages, senders are told to retry until it reads some, so it has to keep up.

Each tool calls the ordinary daemon routes with the credential, so the same permissions and
audit log apply. `tclaude mcp serve` makes no decisions of its own, and it never sends your
operator token, even if the token is in its environment.

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
on the roster as an offline agent labelled **External steward**, with no resume.

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
because tclaude looks for itself or one of its terminals above the caller. This covers
one-shot agents, OpenCode tools, and a background process inside a sandbox that has its own process namespace. It
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
