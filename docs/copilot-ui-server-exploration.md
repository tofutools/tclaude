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
--port <port>          Port to listen on when in server mode (default: random available port)
--host <host>          Host address to bind server to (default: 127.0.0.1)
```

All three are `hideHelp()` in the bundle, so that block is reconstructed from the option
definitions rather than quoted from `--help` output.

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
is a *headless* mode. Taking the `--acp` branch disposes the stdin raw-mode capture and
never mounts the terminal UI, so `copilot --acp` looks like it "accepts the command and
shows nothing" — intended behaviour, not a misconfiguration.

**`--acp` and `--ui-server` are not mutually exclusive, and that is worse than if they
were.** There is no exclusion check between them: `copilot --acp --ui-server --port N`
is accepted, ACP silently wins, no TUI mounts, and the port listens. A combination that
looks like it should give both modes gives you one, with nothing to say so. (The
`Set(["--server","--headless","--acp","--embedded-host"])` in the bundle is a
non-interactive argv sniff gating the raw-mode stdin capture — it is not an exclusion
set, and should not be cited as one.)

`--acp` is also a lowest-common-denominator protocol next to Copilot's own JSON-RPC
surface, which exposes 299 methods in the SDK and 338 paths in the schema.

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

**`api.schema.json` is not complete.** Its session section is missing `session.create`,
`session.setForeground` and `session.getForeground`, all three of which work against a
live server. Treat `copilot-sdk/index.js` as authoritative for what a real client sends.

### The `sessions.open` trap

The schema does document `sessions.open`, and this is where an implementer will lose
time, because **`sessions.open` looks like it works and then does not**:

- `sessions.open {kind: "create"}` genuinely succeeds — it returns
  `{"status": "created", "sessionId": ...}` and creates a session-state directory on
  disk. But **that session is not in the RPC session registry**: every `session.*` call
  against it fails with `Session not found`, and `setForeground` returns
  `{"success": false}`. The danger is not a missing create path; it is a create path
  that exists and produces a session you cannot drive.
- `sessions.open {kind: "attach"}` against the pane's own **startup** session returns
  `{"status": "resumed"}` — and the session is *still* undrivable. So attach is not a
  rescue path either, which is the first thing most implementers try.
- Against a session the server does not have, `sessions.open` reports
  `{"status": "not_found"}` as a **successful result** rather than a JSON-RPC error.
  This is documented behaviour (`SessionsOpenStatus`, and `SessionsOpenAttach` says so
  explicitly) — but a caller that only checks for transport errors will sail past it and
  see the failure much later as `Session not found`.

Use `session.create`. It is absent from the schema and it is the only path that yields a
drivable session.

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
- `session.name.set` changes the **terminal title** (OSC escape, visible as tmux's
  `pane_title`) — not in-pane text. Anything scraping pane contents will not see it.

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
| any other `/` command | `session.commands.*` (list, invoke, enqueue, execute) |

With no keystroke equivalent at all: `session.queue.*` (inspect, reorder, clear the
pending queue), `session.plan.*`, `session.tasks.*`, `session.permissions.*`,
`session.mcp.*`, `session.eventLog.tail`, `sessions.startRemoteControl`.

Structured state that removes the need to scrape the pane:

- `session.usage.getMetrics` — per-model input, output, cache-read, cache-write and
  reasoning token counts, AIU cost, request counts, and code-change stats.
- `session.metadata.contextInfo` — exactly `modelName`, `systemTokens`,
  `conversationTokens`, `toolDefinitionsTokens`, `mcpToolsTokens`, `totalTokens`,
  `promptTokenLimit`, `compactionThreshold`, `limit`, `bufferTokens`
  (`additionalProperties: false`).

Four traps, all of which fail quietly rather than loudly:

- `modelMetrics` entries are **nested**, not flat:
  `{requests: {count, cost}, usage: {inputTokens, outputTokens, cacheReadTokens,
  cacheWriteTokens, reasoningTokens}, totalNanoAiu, tokenDetails}`. A flattened struct
  still decodes without error and reports zero for everything.
- `session.metadata.contextInfo` returns `{contextInfo: null}` until the first turn
  completes. Normal state, not an error.
- **`mcpToolsTokens` is a documented subset of `toolDefinitionsTokens`.** Adding the
  breakdown fields together double-counts it.
- **`contextInfo.modelName` is not the model that ran the turn.** Observed reporting
  `claude-sonnet-4.5` for a turn that actually ran on `gpt-5-mini`. Read the model from
  `session.usage.getMetrics` instead.

## Requires a real terminal

The embedded server only starts once the TUI mounts, and the TUI only mounts on a
genuine PTY. Running `copilot --ui-server` with redirected stdio produces no TUI and
**no listening port**. tmux supplies a PTY so tclaude's pane is fine, but any test
harness driving Copilot directly has to allocate one.

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

A unix socket is not an option. `startTcpListener` is the only listener; the SDK's
transports are stdio, TCP, URI, in-process and child-process — none of them a socket
path; `--stdio` is explicitly rejected in combination with `--ui-server`; and all eight
`socketPath` references in the bundle belong to the IDE-MCP *client*, not the server.

For contrast, the OpenCode integration mints a per-runtime 256-bit password
(`randomOpenCodePassword`), sends it as HTTP Basic on every request, binds
`127.0.0.1`, and on Linux can run over a control socket whose device and inode identity
agentd proves before sending the credential.

The operator decision recorded for the initial work is to accept this: the mode is
opt-in and off by default, and all tclaude agents share one operator trust domain. It
should be revisited before the mode becomes a default or runs unattended anywhere less
trusted.

## Port discovery

Omitting `--port` binds an OS-assigned port, on `127.0.0.1` by default. That port is
**not published anywhere machine-readable**: the records are an
`Embedded server started on port <N>` log line (INFO), a
`CLI server listening on port <N>` line logged at ERROR level, and the number printed
in-pane by the TUI itself. Copilot does have an attach-discovery registry, but only
`--managed-server` publishes to it.

For a multi-agent host this needs an explicit mechanism. tclaude's decision is that
**agentd assigns the port** rather than discovering it — the consumer then holds the
number before the process exists, which no discovery option can offer. Because
`--ui-server` has no authentication, the launched process must be positively observed to
own the listening socket before the first RPC; that ownership check is the only thing
distinguishing our agent from whatever else won the bind race.

## Stability risk

`--ui-server` is hidden from `--help`. It is named in the shipped SDK typings, which
suggests it is intended rather than accidental, but it is absent from the public
documentation and could change without notice. Every finding here is against **1.0.78**
specifically.

One ambiguous signal worth tracking: the attach-discovery registry type still carries a
`"ui-server"` kind, commented as "legacy/normal CLI process", and the attach picker
still consumes it — even though `--ui-server` does not publish to the registry today.
That reads like a capability that was removed, or one that is being reintroduced, rather
than one that never existed. Either way it is a reason to re-check this mode on each
Copilot CLI upgrade rather than assuming it is stable.
