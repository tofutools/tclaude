# Models and usage

Let agents on one node use another node's models and subscriptions, and cap what that costs.

## Model gateways

Local dashboard administration mirrors the CLI's `/v1/models` handlers at
`/api/federation/models`: GET/POST `/control` for policy and switches,
GET/POST `/leases` to list or revoke a lease (`{"id":"LEASE_ID"}`), and GET
`/usage?day=YYYY-MM-DD` for daily usage. Control reads return `{disabled,
gateways}` without provider URLs, headers or credentials; writes accept
`{name?, peer?, disabled}`. Lease and usage reads return arrays. These dashboard
routes require the local operator cookie and are refused through peer views.

⚙ Fleet config › Model gateways can also set a gateway up without editing config:
**Add gateway…** picks a provider preset (Anthropic API, OpenAI API, or a custom
Anthropic- or OpenAI-compatible endpoint), takes the provider API key, the model
allowlist and the limits (Light, Standard or Heavy presets, or custom numbers),
and **Test the key** lists the provider's models without spending tokens. A pasted
key is written to a 0600 file under `~/.tclaude/data/model-gateways/` and
referenced by `header_value_file`; no dashboard read returns it, and removing the
gateway deletes it. A key file you name yourself is referenced as is and left in
place. A Claude subscription token (from `claude setup-token`) is refused with an
explanation: a gateway needs a provider API key. **Edit…** keeps the stored key unless you give a new one; **Remove…** revokes
the gateway's leases and deletes its entry. The routes behind these are GET, PUT and
DELETE `/api/federation/models/gateways/{name}` and POST
`/api/federation/models/gateway-test`.

A trusted machine can provide a named gateway to Claude Code or Codex workers
on another machine. Claude Code uses Anthropic Messages; Codex uses OpenAI Responses. The provider key stays on the gateway machine;
the worker receives a random, per-launch loopback bearer. Gateway traffic uses
sealed federation control messages and an encrypted, flow-controlled hub stream.
The relay cannot read prompts or responses. Ordinary Claude Code workers and Codex
workers (TUI or app-server drive) are supported; non-interactive one-shot runs
refuse a proxy choice. This does not relay ChatGPT or Claude subscription tokens.

Configure a named entry under `agent.http_proxies` on the gateway machine:

```json
{
  "agent": {
    "http_proxies": {
      "anthropic": {
        "url": "https://api.anthropic.com",
        "header": "x-api-key",
        "header_value_file": "/private/path/anthropic-key",
        "model_policy": {
          "enabled": true,
          "models": ["claude-sonnet-*", "claude-haiku-*"],
          "precount_input": false,
          "daily_requests": 1000,
          "daily_tokens": 10000000,
          "peer_daily_requests": 300,
          "peer_daily_tokens": 3000000,
          "session_daily_requests": 100,
          "session_daily_tokens": 1000000,
          "max_input_tokens": 100000,
          "max_output_tokens": 16000,
          "max_concurrent": 4,
          "requests_per_minute": 30
        }
      }
    }
  }
}
```

Set `model_policy.dialect` to `anthropic` (the default when omitted) or `openai`.
An OpenAI gateway uses a URL such as `https://api.openai.com`, `header` set to
`Authorization`, and a credential file containing `Bearer <provider API key>`.
Configure its model allowlist for the exact Responses model IDs or prefixes.
Its accepted endpoints are `POST /v1/responses` and filtered `GET /v1/models`;
Messages, WebSockets, standalone search and `/responses/compact` are refused.
OpenAI `precount_input: true` is unsupported and refuses configuration. Omitted
`max_output_tokens` is filled with the configured output maximum; an explicit
larger or invalid bound is refused. Terminal `response.completed` usage settles
stream reservations; cached input is already included in `input_tokens` and is
not charged twice. Nonstream completed Responses objects are also accounted.

Codex uses a launch-unique custom provider, `wire_api=responses`, a loopback
`base_url`, and `env_key=TCLAUDE_MODEL_PROXY_TOKEN`. tclaude passes provider config
as command overrides to both TUI and app-server; it never edits the user's
`config.toml`. OpenAI authentication and WebSockets are disabled on this provider.
The effective-config probe uses the same overrides and refuses a differing or
uninspectable route. Provider-changing pass-through config/profile arguments
are refused, and inherited routing/auth/proxy environment variables are cleared.
The launch selects Codex's ephemeral credential store and verifies that setting,
so a gateway 401 cannot trigger refresh of a saved ChatGPT login. A saved login
or `OPENAI_API_KEY` is not used for gateway model requests;
the upstream is an API/provider-credential endpoint, not subscription billing.
Copilot CLI 1.0.91 or newer can use an `openai` gateway too. The binding sets
`COPILOT_PROVIDER_BASE_URL` to the session bridge, selects Responses over HTTP,
and supplies only the launch bearer. `COPILOT_OFFLINE=true` disables GitHub
login/routing and telemetry, as well as GitHub MCP and web tools. Pass an explicit
Copilot model (for example `--model gpt-5.4`); `auto` and an absent model are
refused. Both the TUI and embedded API drive run with the same binding.
Competing provider key commands, headers, registry files and credential/routing
variables are stripped before applying the binding. This follows the native
[Copilot BYOK contract](https://docs.github.com/en/copilot/how-tos/copilot-cli/customize-copilot/use-byok-models).

This also applies to remote spawn and teleport `--credentials proxy:<name>@<peer>`.
There is no automatic fallback to local credentials on refusal or disconnect.
Filtered sandbox IP rules still govern tools; the separately authorized model
bridge runs inside the workload's namespace and reaches agentd through its socket.

This follows the [Codex gateway contract](https://learn.chatgpt.com/docs/enterprise/connect-to-a-gateway)
and [provider configuration reference](https://learn.chatgpt.com/docs/config-file/config-reference).
The opt-in native smoke uses an isolated saved login and conflicting ambient key,
then verifies the launch bearer at a fake Responses endpoint, tool execution,
follow-up turns, and app-server drive:

```bash
TCLAUDE_CODEX_PROXY_SMOKE=1 scripts/test.sh ./pkg/claude/session -run TestNativeCodexModelProxySavedLoginPrecedence -v -count=1
TCLAUDE_COPILOT_PROXY_SMOKE=1 scripts/test.sh ./pkg/claude/session -run TestNativeCopilotModelProxyNoFallback -v -count=1
```

All six daily limits and both token bounds must be positive. Model entries match
exact IDs or a trailing `*` prefix pattern, such as `claude-sonnet-*`.
`precount_input` defaults to false: input bounds are checked from provider usage,
with the request byte cap providing the hard pre-flight bound. Set it to true to
count input through the provider before generation; this adds a round trip and
requires the provider token-counting endpoint. Every request reserves the maximum input plus
requested output tokens atomically against gateway, peer, and session budgets.
Complete terminal usage replaces that reservation, including Anthropic cache tokens (OpenAI cached input is included in input tokens).
Interrupted requests retain their reservation until the next UTC day, including
across daemon restarts. Provider usage is checked against the token bounds.
Independent defaults cap requests at 4 MiB, responses at 64 MiB, individual SSE
events at 1 MiB and stream duration at 30 minutes; `max_request_bytes`,
`max_response_bytes`, `max_event_bytes`, and `max_duration_seconds` can narrow or
raise these within the enforced safety ceilings.

Grant the consumer peer access on the gateway, then grant its worker access on
the consumer machine. Both grants are needed for restricted peers:

```bash
# Gateway machine: laptop is a trusted consumer peer.
tclaude federation grant laptop models.proxy --scope http_proxy=anthropic
# Consumer machine: master is the trusted gateway peer.
tclaude agent permissions grant <worker> models.proxy --scope peer=master --scope http_proxy=anthropic
tclaude agent spawn <group> --harness claude --model-proxy anthropic@master --initial-message 'Continue the task'
```

For a new worker, put its `models.proxy` permission in the receiving group or
worker defaults before launch. Permissions are checked before binding and on
every request. A deny or revoked grant interrupts active requests. The bearer
is stored only as a SHA-256 hash, pinned to the launch generation; daemon
upgrades preserve running bindings, and exit, retirement or replacement makes
them unusable. `--model-proxy off` explicitly overrides a default profile.

The bridge sets `ANTHROPIC_BASE_URL` and `ANTHROPIC_AUTH_TOKEN`, which takes
precedence over saved Claude logins according to the
[Claude Code gateway documentation](https://code.claude.com/docs/en/llm-gateway-connect).
It clears inherited provider/authentication selectors and refuses conflicting
Claude settings or `apiKeyHelper` rather than falling back to local billing.
This is an explicit model route, not an OS sandbox: tools still have the network
access granted by the worker's sandbox, and operators must avoid changing
provider settings while it runs.

Inspect metadata and stop access without exposing keys:

```bash
tclaude federation models status
tclaude federation models usage --day 2026-10-08
tclaude federation models disable                         # all gateways
tclaude federation models disable --name anthropic        # one gateway
tclaude federation models disable --name anthropic --peer laptop
# Use enable with the same flags to restore access.
```

The daily usage view and audit log contain request identifiers, peer/session,
model, status, token counts, byte counts and duration. They do not store prompts,
responses, bearer tokens or provider credentials. Provider error bodies are
replaced by readable Anthropic-shaped errors.

Teleport's frozen `--credentials proxy:anthropic@master` mode selects this
binding, subject to the receiver landing policy's exact `credentials_allowed`
list and worker permissions. The gateway peer is pinned to its immutable
identity before admission. `--credentials local` explicitly overrides any proxy
in source or receiver profiles. Proxy mode never falls back to a saved local
login, and requires Claude Code history and an available gateway.

Use provider credentials authorized for the machines, people and locations in
your fleet. A gateway does not turn a Claude subscription into API credentials
or change the provider's account and service terms; cross-organization or
cross-location sharing needs the gateway operator's separate authorization.

## Running as yourself on another node (Claude subscription)

A gateway relays model requests and needs a provider API key. To run Claude
Code on another node on **your own** Claude subscription instead (for example
a helper on a family member's machine), store a long-lived token once:
run `claude setup-token` where you are logged in, then paste it under ⚙ Fleet config ›
Model gateways → **My Claude subscription**. It is written to a 0600
file under `~/.tclaude/data/` and never shown again; **Forget…** deletes it.
The token lasts about a year; revoke it at claude.ai (Settings → Claude Code).

When you spawn on another node (an unrestricted peer, or a restricted one under
a spawn grant), the spawn dialog's **Claude login** row offers **Your Claude
subscription**. This node's peer proxy then adds the token to that spawn's
encrypted request, and only for this node's own operator. The receiving node:

- keeps the token in daemon memory only, keyed by the agent. A durable marker
  records only whose subscription the agent runs on;
- hands it to the agent's pane wrapper once per launch. The wrapper passes it
  to Claude Code through an inherited pipe
  (`CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR`), never through the environment,
  argv or disk, and strips competing auth variables;
- requires Claude Code 2.1.303 or newer, and refuses another harness, a model
  gateway, and `apiKeyHelper` or `env` settings for that agent;
- refuses to relaunch the agent once it no longer holds the token (after its
  tclaude restarts, or when the agent retires), rather than falling back to its
  own login.

The member row shows a **◆ \<name\>'s subscription** badge. When the token is
gone, the badge says so, and a resume, restart or reincarnate does not stop or
start anything. Instead, a dialog offers:
- **the subscription's owner:** send the token again. Their own node adds it to
  an encrypted `POST /api/agents/{id}/claude-subscription`.
- **anyone else who may relaunch the agent:** switch it to the node's own login
  (`DELETE` on the same route), billed there from then on. Over the peer view
  this means the owner or an unrestricted peer.

The agent's ⚙ menu offers both explicitly: "send my subscription again" and
"use the node's own login". A restart checks the token before it stops the
agent. Clones, moves,
teleports and manual `conv resume` on the receiving node do not carry the
subscription: a moved agent runs on its destination's own login. A receiver
whose requester-pays policy is `required` accepts a subscription spawn without
a gateway lease, since the requester pays through their own subscription. The routes
are GET, PUT and DELETE `/api/federation/claude-subscription`, and these routes
are never served to peers.

## Usage sharing: a dollar cap on a peer's gateway grant

A `models.proxy` grant can carry a dollar cap. The peer's agents use this
node's gateway, on this node's provider account, until they have spent that
much. Their model calls are the only thing shared; they keep running on
their own node, in their own environment.

```bash
tclaude federation grant alice models.proxy --scope http_proxy=team-key --max-usd 20 --for 7d
tclaude federation grants alice   # … [max $20.00] [expires …]
```

The gateway prices each request from the usage the provider reports, at the
model's standard API rate (global routing, no batch or fast-mode surcharge),
so the cap works the same for every harness. Built-in rates cover the Claude
and OpenAI API models tclaude knows. `model_policy.prices` adds or overrides
rates in USD per million tokens, by model ID or trailing-`*` prefix:

```json
"prices": {"my-local-*": {"input": 1, "output": 4, "cache_read": 0.1}}
```

- **What counts:** every request the peer makes through the gateway since
  the grant was created. Raising the cap with a new `grant` keeps what was
  spent; revoking and granting again starts over. An unscoped `models.proxy`
  grant caps the peer's spend through every gateway together. A peer holding
  both a gateway's capped grant and a capped unscoped one is held to both.
- **When it stops:** a request starts only while the peer's spend, including
  requests still in flight, is below the cap. The requests in flight when it
  is reached can overrun it by their own cost. After that, requests get 429
  `usage share used up`.
- **What a request costs:**
  - Free: a request the provider never received, one it refused with an
    error status, and token counting.
  - A finished response: its reported usage.
  - A response that broke off: the input it reported plus every output token
    it was allowed.
  - A request sent with no answer (a timeout, a dropped connection): its worst
    case, since the provider may have run it to the end.
- **Unpriced models:** with a cap set, a model without a known price is
  refused, since its spend could not be counted.
- **Where it is recorded:** this node's database records the charge for each
  request. **⚙ Fleet config › Model gateways › Usage** and
  `tclaude federation models usage` show it per gateway, peer and model.

**Seeing what is shared.** `tclaude federation shares` asks every trusted
peer which of its gateways this node may use, and lists for each one the
models, the cap, what has been spent so far, when it ends, and the
`--model-proxy` reference to spawn with. `--given` lists what this node shares
with its peers. A peer answers only about its own grants; one sharing nothing
answers with an empty list. A gateway the peer is blocked on, or that is
switched off, is not listed. The dashboard reads the same lists from
`/api/federation/models/shares` and `…/shares/received`.

**Asking for usage.** An operator access request (see
[Operator access requests](federation-work.md#operator-access-requests)) for `models.proxy` asks a peer to share usage:
`tclaude federation access request --node forge --permission models.proxy
--max-usd 20 --ttl 168h --reason '…'`, optionally with `--http-proxy NAME` for
one gateway. In the dashboard, open the peer's view and use **ask for usage…**
in its peer-view pill. The peer's operator sees the amount, the time and the
reason under Messages → Access requests. They pick the gateway (unless one was
asked for), and may lower the amount or shorten the time, never raise them:
`tclaude federation access approve --id ID --http-proxy team-key --max-usd 10`.
Approval writes a `models.proxy` grant scoped to that gateway with the amount
as its cap. Asking again for a gateway that already has a live capped share
adds the amount to its cap. The share keeps counting from its start, so
"$5 more" means $5 more than is left. An expired share starts over. A
capped approval is refused when it would limit access that has no cap, or
when a capped share on every gateway would still bind the chosen gateway
(approve for any gateway to add to that share). Unrestricted trust does not
lift a share's cap. Requests time out after five minutes, like every
operator access request.

**The Usage sharing panel.** ⚙ Fleet config › **Usage sharing** lists both
directions. **Shared with you** shows each peer's gateway that your agents may
bill, with a bar of what is spent against the cap and when it ends, and **Ask
for more…**, which sends that peer a usage request. **You share** shows what
your gateways serve each peer, with **+7 days…** for a timed share, **Raise…**
to add dollars to a cap (the share keeps counting from its start), and
**Revoke…**.

**Alerts.** The daemon checks shares every two minutes and notifies the
operator when a share reaches 80% and 100% of its cap, and when it ends. The
node that gives a share is notified about what it gives. The node that
receives one is notified about what it receives, while one of its agents is
bound to a peer's gateway. A share revoked by its own grantor does not alert
that grantor. A daemon restart picks up where each share stands without
alerting again.

Use an API key for a shared gateway, not a consumer subscription's login:
letting other people use a Claude Pro/Max or ChatGPT subscription is likely
against the provider's terms.

## Requester-paid workers

A requesting machine can pay for its remote Claude Code worker through its own
named gateway. On the requester, grant the receiving peer lease-only access:

```bash
tclaude federation grant colleague models.proxy.leased --scope http_proxy=anthropic
tclaude federation spawn-request builders@colleague --credentials proxy:anthropic@self --brief 'Check the change'
# Teleport uses the same credential choice:
tclaude agent teleport colleague --group builders --credentials proxy:anthropic@self
```

The receiver must consent through `requester_pays` in its applied node profile,
or `--requester-pays` on a `groups.members.spawn` peer grant:

- `off` (default) rejects requester-issued leases and keeps existing launch behavior.
- `allowed` accepts an explicit requester-paid offer; it does not choose one implicitly.
- `required` refuses requests without a requester gateway lease before launch.

A concrete spawn grant's policy overrides the node profile default, using the
existing group-scope and direct-peer precedence. Conflicting equal-priority pool
policies refuse. Teleport uses the node profile default, with an optional
`teleport_landing.requester_pays` override, and still requires its ordinary
`credentials_allowed` entry. Allowed credential references may name the requesting
peer's alias or its pinned instance ID. Manual acceptance rechecks the current
policy and preserves the offered account. Missing gateway support, permission,
connectivity or harness support refuses; it never falls back to receiver credentials.

Agent callers creating a paid request need ordinary `models.proxy` covering the
receiving peer and their own named proxy. Receiving workers also need ordinary
`models.proxy` covering the requesting gateway and proxy name, through receiving
group permissions or node-profile worker defaults. The lease-only peer grant
allows only requests issued by that gateway for an exact remote worker; it does
not allow arbitrary sessions to use the provider account. Existing ordinary
`models.proxy` peer grants remain available for broader fleet gateway access.

Leases bind the signed request or teleport offer, receiving instance, reserved
worker, session and launch generation before the harness starts. They survive
daemon restart and end on worker exit, retirement, generation replacement,
revocation or a gateway kill switch. Disconnected close notices retry after
reconnect. There is no fixed lifetime: requests slide the idle timeout, configured
as `model_policy.lease_idle_hours` (default 24). An idle-expired or revoked lease
cannot be reactivated; submit a new request. A failed launch that already bound its
lease likewise requires a new request before changing worker identity.

`tclaude federation models leases` shows the paying proxy, receiving peer,
request, worker, generation and last activity. Use `--revoke <id>` to stop one.
Both instances audit account attribution; prompts, responses and bearer values
remain outside audit records. The existing budgets and switches apply to leased
traffic. Remote argv jobs and other harness bindings are outside this release.
