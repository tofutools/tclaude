# Copilot CLI `--ui-server` exploration

This note records a hands-on exploration of GitHub Copilot CLI **1.0.78** on Linux as
an API-backed tclaude harness. It is the Copilot counterpart to
[OpenCode harness exploration](opencode-exploration.md), and it focuses on the
contracts in [Adding a harness](adding-a-harness.md).

Everything below marked *verified* was reproduced against a real `copilot` process.
Everything marked *unverified* is exactly that; do not treat it as settled.

## Verdict

Copilot CLI can serve a machine API and an interactive TUI **from the same process at
the same time**, through a hidden `--ui-server` flag:

```
--ui-server            Enable TUI with embedded JSON-RPC server
--port <port>          (default: random available port)
--host <host>          (default: 127.0.0.1)
```

The bundled `@github/copilot-sdk` typings name this mode explicitly — several methods
are documented as "Only available when connecting to a server running in TUI+server
mode (`--ui-server`)".

The resulting topology is simpler than OpenCode's, because there is no second process:

```text
tmux pane: copilot --ui-server --port <managed-port>
                   |                    |
                 TUI                JSON-RPC/TCP
             (the human)                |
                             tclaude status, ask, and conversation adapters
```

This matters for tclaude: liveness is still anchored to a single harness process under
the pane, so the existing `Spawner` contract holds. OpenCode needed explicit server
supervision because its TUI and its server are different processes; Copilot does not.

## `--acp` is not this

The [documented ACP server](https://docs.github.com/en/copilot/reference/copilot-cli-reference/acp-server)
is a *headless* mode and cannot be combined with the TUI. In the shipped bundle `--acp`
sits in the same mutual-exclusion set as `--server`, `--headless` and `--embedded-host`,
and taking that branch disposes the stdin raw-mode capture and never mounts the
terminal UI. Starting `copilot --acp` therefore looks like it "accepts the command and
shows nothing" — that is the intended behaviour, not a misconfiguration.

`--acp` is also a lowest-common-denominator protocol next to Copilot's own JSON-RPC
surface, which exposes roughly 250 methods.

## Wire protocol

Verified:

- **LSP-style `Content-Length:`-framed JSON-RPC 2.0 over TCP.** Not HTTP.
- `connect` is the handshake; it returns `{ok, protocolVersion, version}`
  (`protocolVersion: 3`, `version: "1.0.78"`).
- `ping` echoes `{message, timestamp, protocolVersion}`.
- Two server-pushed notification methods: `session.event` (wrapping a typed session
  event) and `session.lifecycle`.

The contract ships inside the npm package rather than being something we reverse
engineer:

| File | Contents |
|---|---|
| `schemas/api.schema.json` | ~1.3 MB. Self-describes as the input SDK codegen consumes. |
| `schemas/session-events.schema.json` | Session event shapes. |
| `copilot-sdk/*.d.ts`, `copilot-sdk/docs/` | Typed SDK surface and prose docs. |
| `copilot-sdk/index.js` | The reference client. `grep 'sendRequest("'` yields the full method list. |

**`api.schema.json` is not complete.** It documents `sessions.open`, but the method the
SDK actually uses to create a session is `session.create`, which the schema does not
contain. Treat `copilot-sdk/index.js` as authoritative for what a real client sends.

## Session bootstrap

The one genuinely non-obvious part.

The session the TUI opens for itself is **not drivable over RPC**. `session.getForeground`
reports its ID, but every `session.*` call against that ID fails with
`Session not found`: foreground registration backs the get/set-foreground and
remote-control paths only, not the RPC session registry.

The sequence that does work (verified end to end):

1. `connect`
2. `session.create` — the caller chooses the session UUID and passes at least
   `workingDirectory`, `clientName`, `streaming: true`
3. `session.setForeground` with that ID — **the TUI switches to display it**, and the
   pane's original startup session drops to background
4. `session.send` — the prompt and the model's reply render in the TUI

`--session-id` appeared to have no effect in `--ui-server` mode; drive the ID through
`session.create` instead. *(Not investigated further.)*

The folder-trust prompt still blocks the TUI at startup, even though the RPC port is
already listening by then. `session.permissions.folderTrust.addTrusted` exists and
config-level trust persistence exists, but *neither was verified* as a way to satisfy
the prompt for an unattended agent.

`COPILOT_HOME` relocates config and state; `--log-dir` relocates logs. Both are useful
for keeping a managed agent out of the operator's own Copilot profile.

## Bidirectional behaviour

Verified in both directions:

- An RPC `session.send` renders in the attached TUI.
- A prompt typed **into the TUI** streams back over RPC to a client that did not send
  it: `user.message`, `assistant.turn_start`, `model.call_start`,
  `assistant.message_start`, `assistant.streaming_delta`, `assistant.message_delta`,
  `assistant.reasoning`, `assistant.usage`, `assistant.message`, `assistant.turn_end`,
  `assistant.idle`, `session.idle`, `session.usage_checkpoint`, plus
  `session.lifecycle` events (`session.created`, `session.foreground`,
  `session.background`).
- `session.name.set` changes the title live in the TUI.

**Multiple simultaneous clients are supported.** With four sockets held open at once,
all four completed `connect`, all four received the *same* event stream for a single
turn, and a connection could freely drive a session that a *different* connection had
created. There is no per-connection scoping of any kind.

## Capability notes

Typed RPC equivalents exist for effectively every lifecycle command tclaude currently
delivers as tmux keystrokes:

| In-pane today | RPC |
|---|---|
| prompt text + Enter | `session.send`, `session.sendMessages` |
| `/rename` | `session.name.set` |
| `/compact` | `session.history.compact` |
| interrupt | `session.abort`, `session.interruptMainTurn` |
| `/model` | `session.model.switchTo`, `session.model.setReasoningEffort` |
| `/exit` | `session.shutdown`, `sessions.close` |
| mode switch | `session.mode.set` |

With no keystroke equivalent at all: `session.queue.*` (inspect, reorder, clear the
pending queue), `session.plan.*`, `session.tasks.*`, `session.permissions.*`,
`session.mcp.*`, `session.eventLog.tail`, `sessions.startRemoteControl`.

Structured state that removes the need to scrape the pane:

- `session.usage.getMetrics` — per-model input, output, cache-read, cache-write and
  reasoning token counts, AIU cost, request counts, and code-change stats.
- `session.metadata.contextInfo` — `totalTokens`, `limit`, compaction threshold, split
  into system, tool-definition, MCP, custom-instruction and conversation tokens.

## Security posture

**`--ui-server` has no authentication.** This is the significant caveat, and it is a
step down from what the OpenCode integration provides.

- `COPILOT_CONNECTION_TOKEN` is read only on the headless `--server` startup path. The
  options object built for `--ui-server` is `{enabled, port, host}`; no
  `connectionToken` is threaded through. Verified: with the variable exported into the
  pane, a client sending `connect` with no token is accepted.
- The underlying listener signature *is*
  `startTcpListener(host, port, connectionToken, onAccepted)`, and headless mode does
  pass a token, so the capability exists upstream and is simply unwired here.
- Combined with the lack of per-connection scoping, an unauthenticated client is not a
  passive observer: it can read the whole conversation stream and issue commands
  against every session on that server.

A unix socket is not an option. `startTcpListener` is the only listener; the SDK offers
stdio, TCP, URI and in-process transports and nothing else; `--stdio` is explicitly
rejected in combination with `--ui-server`; and the `socketPath` references in the
bundle belong to the IDE-MCP *client*, not the server.

For contrast, the OpenCode integration mints a per-runtime 256-bit password
(`randomOpenCodePassword`), sends it as HTTP Basic on every request, binds
`127.0.0.1`, and on Linux can run over a control socket whose device and inode identity
agentd proves before sending the credential.

The operator decision recorded for the initial work is to accept this: the mode is
opt-in and off by default, and all tclaude agents share one operator trust domain. It
should be revisited before the mode becomes a default or runs unattended anywhere less
trusted.

## Port discovery

Omitting `--port` binds an OS-assigned port. That port is **not published anywhere
machine-readable**: the only record is an `Embedded server started on port <N>` line in
the log directory. Copilot does have an attach-discovery registry, but only
`--managed-server` publishes to it.

For a multi-agent host this needs an explicit mechanism — either tclaude assigns the
port, or it reads the chosen port back from a per-agent `--log-dir`, or it inspects the
pane process's listening sockets.

## Stability risk

`--ui-server` is hidden from `--help`. It is named in the shipped SDK typings, which
suggests it is intended rather than accidental, but it is absent from the public
documentation and could change without notice. Every finding here is against **1.0.78**
specifically.
