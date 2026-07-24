# OpenCode harness exploration

This note records a hands-on exploration of OpenCode 1.18.4 as a possible
third tclaude harness. It focuses on the contracts in
[Adding a harness](adding-a-harness.md), not on an implementation.

The target provider was OpenAI through an existing ChatGPT subscription. The
tests reused that authentication read-only and ran with isolated XDG data,
cache, and state roots so they did not change the operator's OpenCode
configuration or conversation database.

Useful upstream references:

- [CLI](https://opencode.ai/docs/cli/)
- [Server and HTTP API](https://opencode.ai/docs/server/)
- [Permissions](https://opencode.ai/docs/permissions/)
- [Web client](https://opencode.ai/docs/web/)
- [MCP servers](https://opencode.ai/docs/mcp-servers/)

## Verdict

OpenCode is a strong fit for an **API-backed harness**. Its TUI is already an
HTTP client, the headless server publishes an OpenAPI 3.1 document, and its SSE
streams expose message deltas and session/tool lifecycle events. The clean
tclaude runtime topology is:

```text
tmux pane: opencode attach http://127.0.0.1:<managed-port>
                              |
                              v
             tclaude-managed opencode serve
                    |                    |
                 HTTP/SSE             SQLite
                    |
          tclaude status, ask, and conversation adapters
```

Do not start a normal `opencode` TUI and an independent `opencode serve` and
expect them to share live state. A normal TUI starts its own server; a separate
`serve` command starts another. Instead, start one authoritative server and
attach the pane TUI to it.

This is not a descriptor-only integration. The current `Spawner` contract
builds one pane command, process liveness is anchored to the harness process
under that pane, and `StreamAsker` consumes a foreground child's stdout. A
server-backed adapter therefore also needs explicit server supervision,
endpoint/authentication discovery, and a long-lived SSE consumer. Those
requirements are detailed below.

That topology was tested bidirectionally:

- An asynchronous HTTP prompt appeared and streamed in an already attached
  TUI.
- A prompt submitted in the TUI emitted user, assistant, token-delta, status,
  and completion events on the server's SSE stream.
- An HTTP title update immediately changed the attached TUI's title.
- HTTP compaction streamed a summary into the TUI and reduced its displayed
  context from about 10.3K tokens to 539 tokens.
- Deleting the selected session over HTTP notified the TUI and returned it to
  its home screen.
- Exiting the attached TUI left the headless server running.

This is a stronger upstream coexistence contract than either polling storage or
typing control text into an unrelated pane, but tclaude needs new orchestration
around it before it is a safe harness integration.

## Capability matrix

The ratings describe how an OpenCode adapter maps onto tclaude's current
harness contracts:

- **Native**: OpenCode exposes the capability in a form the current contract can
  use directly.
- **Degradable**: tclaude can provide the behavior through a supported
  alternative or a contained contract extension, but OpenCode does not expose
  the exact foreground-process, launch-time, or in-pane contract.
- **Blocked**: no reliable OpenCode 1.18.4 surface was found.

| Capability | Rating | Evidence and integration implication |
|---|---|---|
| **Spawn** | **Native** | `opencode [project]` starts the TUI. For the recommended topology, start `opencode serve` and put `opencode attach <url> --dir <cwd>` in the pane. |
| **Resume** | **Native** | The TUI, `attach`, and `run` accept `--session <id>`, `--continue`, and `--fork`. |
| **Model and effort selection** | **Native** | Models use `provider/model`; reasoning effort uses `--variant`. The connected OpenAI catalog exposed `none`, `low`, `medium`, `high`, `xhigh`, and, on the GPT-5.6 families, `max`. |
| **Ad-hoc ask** | **Native** | `opencode run [message]` supports a session, model, variant, directory, JSON event output, and `--attach` to an existing server. The HTTP API offers blocking `message` and non-blocking `prompt_async` requests. |
| **Live-streamed ask output** | **Degradable** | The current `StreamAsker` reads a foreground child's stdout, but direct `opencode run`, including JSON mode, emits a text part only after it is complete. `/event` and `/global/event` do emit SSE `message.part.delta` events token-by-token, so streaming requires the managed-server extension rather than a direct implementation of the existing exec-based contract. |
| **Conversation list and search** | **Native** | `opencode session list --format json`, `GET /session` (including search/path/limit filters), and `opencode export <id>` provide supported read surfaces. |
| **Rename** | **Native** | The TUI has `/rename`; `PATCH /session/{id}` updates the title out of band and was observed updating a live attached TUI. The API is safer than interpolating a title into pane keystrokes. |
| **Compact** | **Native** | The TUI has `/compact` (alias `/summarize`), `POST /session/{id}/summarize` compacted a live session, and the managed `/tui/publish` endpoint can dispatch `session.compact` through the attached TUI's command registry without prompt keystrokes. The newer advertised `POST /api/session/{id}/compact` returned `503 "Session compact is not available yet"` in 1.18.4. |
| **Graceful stop** | **Native** | The TUI command is `/exit`, with `/quit` and `/q` aliases. The managed `/tui/publish` endpoint can dispatch `app.exit` through the attached TUI's command registry; it exits the attach client with status 0 without stopping the server. |
| **Remote control** | **Blocked; different capability** | tclaude's contract is an in-pane toggle for a hosted relay such as Claude Remote Access. OpenCode has no equivalent. |
| **Self-hosted browser / remote TUI** | **Native** | `opencode web` supplies a browser UI and `opencode attach` connects another TUI to the same server. This is useful remote access, but it is not the hosted-relay capability above. The [server documentation](https://opencode.ai/docs/server/#authentication) specifies `OPENCODE_SERVER_PASSWORD` and HTTP Basic authentication. |
| **Reincarnate / clone** | **Native** | Resume, fork, title update, and graceful exit are all available. tclaude can create the successor with the same session or a fork without storage surgery. |
| **Hooks / live status** | **Degradable through SSE** | SSE includes busy/idle status, session changes, message and text deltas, tool-part state, permission requests, questions, compaction, and deletion. A managed-server adapter can consume these, but the current `HookInstaller` callback contract does not model a long-lived side-server event consumer. |
| **OS sandbox at spawn** | **Blocked** | OpenCode permissions govern tools, not an operating-system filesystem/process sandbox. No OpenCode launch flag or config equivalent to Codex's sandbox modes was found. An external wrapper would be a separate tclaude facility, not a native harness capability. This absence makes an unattended auto-approval posture unsafe. |
| **Approval posture at spawn** | **Degradable; no safe autonomous default** | Permission rules support `allow`, `ask`, and `deny`, including per-tool and pattern-specific rules, and permission requests have SSE and HTTP response surfaces. However, `--auto` approves anything not explicitly denied and OpenCode has no OS sandbox backstop. A detached adapter must not use unrestricted `--auto`; it needs an isolated, curated deny ruleset that blocks dangerous tools, and remains materially weaker than Codex's sandbox-confined autonomous default. |
| **AskUserQuestion timeout at spawn** | **Degradable** | Questions have list, reply, reject, and SSE event APIs, but no launch-time timeout option was found. A managed-server adapter could apply a tclaude timer and reject or answer a pending question. |
| **Auto-approve review** | **Blocked** | `--auto` is blanket approval for non-denied actions, not an independent supervisor/guardian review. No per-action reviewer equivalent was found. |
| **Auto memory at spawn** | **Blocked** | OpenCode loads project instruction and skill files, but no automatic cross-session memory store or launch control equivalent was found. |
| **Status bar** | **Degradable** | The TUI footer shows the model/provider, variant, context, and working directory. It is not a command-backed custom status line, but SSE and session APIs provide enough data for the tclaude dashboard. |
| **Background shell tracking** | **Blocked** | The `bash` tool schema has command, timeout, and working-directory fields but no background/task-id mode. The PTY API is a separate user terminal facility, not agent background-shell lifecycle. `/experimental/capabilities` reported `backgroundSubagents: false`. |
| **Usage, cost, and context** | **Native (tokens/context/cost); provider account limits blocked** | `opencode stats`, session/export APIs, and message records expose input, output, reasoning, and cache token counts plus cost and model limits. Subscription-backed turns report exact tokens but zero monetary cost; tclaude derives a labeled WHAT-IF estimate from OpenCode's native model catalog while preserving non-zero reported cost as real spend. The newer `/api/session/{id}/context` response was empty, so tclaude uses the established session/message surfaces. The underlying provider account's usage/rate limits are **not** exposed; the Usage tab warns when OpenCode activity lacks matching native Claude/Codex quota history. See [OpenCode usage, cost, and provider-limit feasibility](opencode-usage-cost-feasibility.md) for the cost/limit findings. |
| **MCP** | **Native** | `opencode mcp` manages local and remote servers and OAuth. HTTP endpoints expose status, add, connect/disconnect, and authentication lifecycle. |
| **Dashboard** | **Native** | tclaude owns its dashboard and can populate it from SSE/session data. OpenCode also supplies its own browser client over the same server. |

## Integration requirements beyond the descriptor

### Unattended safety

tclaude couples a non-blocking approval posture with an OS sandbox: autonomous
approval is safe only when the resulting writes and processes are confined.
OpenCode 1.18.4 provides granular tool permissions but no equivalent OS
sandbox. Consequently, the two desirable properties cannot both be achieved
natively:

- An `ask` posture can deadlock a detached agent waiting for a human.
- An unrestricted `--auto` posture avoids that deadlock but permits arbitrary
  shell execution without a sandbox boundary.

An implementation must not select unrestricted `--auto` as the daemon-spawn
default. At minimum it needs a per-session, fail-closed permission configuration
that denies dangerous tools and exposes only a deliberately curated set.
External OS containment would be needed for parity with Codex's unattended
safety model; tool-deny rules alone are defense in depth, not a sandbox.

### Server ownership and liveness

In the recommended topology, `opencode attach` in the pane is only a client.
The server owns the conversation and can outlive that pane. The adapter must
therefore define:

- who starts the server, chooses its port and credentials, and waits for health;
- how endpoint metadata reaches conversation, ask, status, and lifecycle code;
- how pane exit, hard kill, clone, reincarnation, and daemon restart reap or
  recover the server without leaking processes or ports;
- how a server crash becomes a visible session failure even if the attached TUI
  has not exited yet; and
- how the long-lived SSE consumer reconnects and reconciles missed state.

This could be a pane-owned supervisor wrapper that treats the server and
attached TUI as one process group, or an agentd-managed service with its own
lifecycle contract. It cannot be left as an incidental background process
started by `BuildCommand`.

### Control-plane authentication

Loopback is not a trust boundary: any local process that can reach the port can
submit prompts, trigger shell work, drive the TUI, or delete sessions. This is
broader than tclaude's in-pane command-injection sink, and filesystem sandboxing
normally does not prevent a peer agent from reaching localhost.

The managed server should therefore require a unique credential even while
bound to loopback, using the upstream
[`OPENCODE_SERVER_PASSWORD` HTTP Basic authentication
support](https://opencode.ai/docs/server/#authentication). tclaude must keep the
credential out of command-line arguments and logs and supply it to both the
attached TUI and API client. A future authenticated Unix-domain transport could
provide a narrower local boundary, but OpenCode 1.18.4's documented server
surface is TCP.

## Server and API findings

`opencode serve --hostname 127.0.0.1 --port <port>` exposed:

- `GET /global/health` for version and liveness.
- `GET /doc` for the OpenAPI 3.1 JSON document.
- `GET /event` and `GET /global/event` for SSE.
- Session CRUD, search, fork, abort, compact/summarize, revert, share, message,
  command, shell, permission, question, and todo surfaces.
- TUI control endpoints for selecting a session, editing/submitting the prompt,
  and opening dialogs.
- Provider/model, agent, tool-schema, MCP, file, VCS, LSP, formatter, and PTY
  endpoints.

The document in 1.18.4 is large and includes both established routes and newer
`/api/...` routes. Presence in OpenAPI is not proof that a route is usable:
the v2 compact route was documented but returned 503, while the established
summarize route worked. Integration tests should cover the exact routes tclaude
depends on, and the adapter should retain version/capability checks.

The server listens on loopback by default, but the browser UI and API are
unprotected when no server password is set. Loopback limits network exposure;
it does not authenticate local callers. A tclaude-managed server should require
the documented Basic authentication on loopback as well as on an intentionally
remote listener.

## Conversation storage

OpenCode 1.18.4 did **not** use the older
`storage/session`, `storage/message`, and `storage/part` JSON trees described in
some prior integrations. Its data root contained:

```text
auth.json
opencode.db
opencode.db-shm
opencode.db-wal
log/
repos/
```

The SQLite database is the source of truth. Relevant normalized tables include
`project`, `workspace`, `session`, `session_message`, `message`, `part`,
`permission`, `todo`, `event`, and `event_sequence`. The `session` row includes
directory, title, model/agent metadata, timestamps, cumulative cost, and
input/output/reasoning/cache token counts. Message and part rows retain the
detailed typed JSON payloads.

A `ConvStore` should prefer supported interfaces:

1. List and search through `opencode session list --format json` or
   `GET /session`.
2. Resolve and read details through session/message APIs or
   `opencode export`.
3. Rename through `PATCH /session/{id}` when the managed server is available.
4. Treat direct SQLite access as a version-sensitive fallback, not as a stable
   public schema.

This avoids racing the live SQLite WAL and avoids baking another harness's
private database schema into tclaude.

## OpenAI model observations

With the existing OpenAI subscription, `opencode models openai` exposed these
families during the test:

```text
openai/gpt-5.3-codex-spark
openai/gpt-5.4
openai/gpt-5.4-fast
openai/gpt-5.4-mini
openai/gpt-5.4-mini-fast
openai/gpt-5.5
openai/gpt-5.5-fast
openai/gpt-5.6-luna
openai/gpt-5.6-luna-fast
openai/gpt-5.6-sol
openai/gpt-5.6-sol-fast
openai/gpt-5.6-terra
openai/gpt-5.6-terra-fast
```

A test turn with `openai/gpt-5.6-terra --variant high` succeeded through the
ChatGPT subscription. Model catalogs are provider- and time-dependent, so an
implementation should query the installed OpenCode rather than hard-code this
observed list.

## Commands exercised

The exploration exercised the following surfaces without changing global
authentication or configuration:

```text
opencode --help
opencode models openai
opencode run --pure --format json --model ... --variant ...
opencode run --attach http://127.0.0.1:<port> --format json ...
opencode session list --format json
opencode stats
opencode export <session-id>
opencode db
opencode mcp
opencode serve --pure --hostname 127.0.0.1 --port <port>
opencode attach http://127.0.0.1:<port> --dir <cwd>
```

HTTP tests covered health, OpenAPI, sessions, messages, asynchronous prompts,
SSE, TUI session selection, rename, compaction, deletion, provider/model
metadata, tool schemas, usage/context, permissions/questions, MCP status, and
experimental capabilities.

One packaging detail is worth handling during implementation: the tested binary
was installed at `~/.opencode/bin/opencode`, but that directory was not on the
agent sandbox's `PATH`. Setup should either require a discoverable binary or
recognize OpenCode's standard install location without persisting a
machine-specific absolute path.
