# OpenCode usage, cost, and provider-limit feasibility (TCL-696)

This note answers one question: **can tclaude track cost, token usage, and
provider usage/rate limits when it runs the OpenCode harness?** It is the spike
deliverable for TCL-696 and feeds the implementation ticket
[TCL-673](https://linear.app/johan-kjolhede/issue/TCL-673) (OpenCode
usage/cost/context reporting). It is grounded in a live inspection of OpenCode
**1.18.4** (a tclaude-managed `opencode serve` with isolated XDG roots; its
472-schema OpenAPI 3.1 document at `GET /doc`) and a read of the existing
Codex/Claude usage pipelines on `main`.

## Verdict at a glance

| Dimension | Verdict | Status |
|---|---|---|
| **Token counts** (input/output/reasoning/cache) | **Yes — native** | **Implemented on `main`** by TCL-701 / [#1492](https://github.com/tofutools/tclaude/pull/1492). |
| **Context-window occupancy** | **Yes — native** | **Implemented on `main`** by TCL-701 / #1492. |
| **Cost (USD)** | **Partial — native, but N/A on subscription** | Not yet implemented; belongs to TCL-673. Details below. |
| **Model / provider identity** | **Yes — native** | `providerID`/`modelID`/`variant` per message; consumed by the TCL-701 context path. |
| **Provider account usage / rate limits** | **No** | Not exposed by OpenCode; only a reactive 429 is observable. Details below. |

Net: **feasible for the parts that matter.** Token counts and context-window
occupancy are done. The remaining live question for TCL-673 is **cost**, which
is feasible with one caveat (subscription = honest zero). The only genuine gap
is proactive **provider-account rate limits**, which OpenCode does not surface
at all.

## Already implemented: token counts + context window (TCL-701 / #1492)

Live token/context projection is **implemented on `main`**. `pkg/claude/agentd/opencode_context.go`
parses per-turn token usage from the `/event` SSE `message.updated` payload
(`tokens: {input, output, reasoning, cache: {read, write}}`), resolves the
active model's context-window limit from `/config/providers`
(`provider.models[modelID].limit.context`), and writes the harness-agnostic
dashboard columns `context_pct`, `tokens_input`, `tokens_output`, and
`context_window_size` via `db.UpdateContextSnapshot` — the same columns the
Claude statusline and the Codex rollout projector populate. When the model
limit is unavailable it records the token counts with a blank meter, matching
Codex's graceful-degrade behaviour. See the "Context-window reporting" row in
[`harnesses.md`](harnesses.md).

This note therefore no longer needs to argue that the data is *reachable* or to
prescribe wiring for it: it is on the wire and now projected. The two
dimensions below are what remain unique to this feasibility finding.

## Cost — implemented as real-or-WHAT-IF

OpenCode computes cost itself and exposes it as a plain `number` (USD) on
`Session.cost`, `AssistantMessage.cost`, `StepFinishPart.cost`, and the
`session.next.step.ended` SSE event. It also ships a **per-model price table**
at `GET /config/providers` (the `Model.cost` schema), so — unlike Codex —
tclaude would **not** need to maintain its own price table (contrast
`codexModelPrices` in `pkg/claude/harness/codex_cost.go`):

```jsonc
// Model.cost — USD per million tokens
"cost": {
  "input":  number,
  "output": number,
  "cache": { "read": number, "write": number },
  "tiers": [ … ],            // e.g. long-context tiers
  "experimentalOver200K": …
}
```

The caveat, confirmed by the earlier hands-on exploration and consistent with
the API shape: on a **ChatGPT/Codex subscription** OpenCode reports exact token
counts but `cost` = 0, because there is no per-token bill. An **API-key
provider** instead reports a real non-zero `cost`.

The implemented handling is:

- **API-key providers:** trust OpenCode's `cost` → `sessions.cost_usd`.
- **Subscription providers:** treat `cost == 0` as *not-applicable*, not free;
  derive a virtual WHAT-IF cost from the native `Model.cost` catalog and land it
  in `sessions.virtual_cost_usd`
  (Codex's never-billed estimate lives in that column too), keeping `cost_usd`
  at 0. Input, output, reasoning, cache read/write, and model pricing tiers are
  included. Message identity makes repeated SSE updates, reconnect history,
  and recovery backfills replacement-safe instead of double-counting.

The Costs tab selects real spend per session/day slice when present and
otherwise selects its WHAT-IF estimate. Mixed Claude, Codex, and OpenCode
history therefore remains visible in one chart and table, with hypothetical
values explicitly marked.

## Provider account usage / rate limits — no

This is the dimension the operator flagged as maybe-impossible, and the honest
answer is **no**. A full scan of the 472-schema OpenAPI document found **no**
rate-limit, quota, remaining-spend, or `x-ratelimit`-style fields anywhere, and
no `/usage`, `/billing`, `/account`, or `/quota` route. OpenCode brokers the
request to the provider but does not re-surface the provider account's
rate-limit headers, remaining spend, or reset windows.

The only rate-limit signal observable through OpenCode is **reactive**: a
`session.error` event carrying an `APIError` whose `data.statusCode == 429`
(already classified `"rate_limit"` by `openCodeHookErrorType` in
`pkg/claude/agentd/opencode_events.go`). That says a limit was *hit*, not how
much headroom remains.

This is a real gap versus Codex, whose rollout carries a proactive
`rate_limits` block (5-hour / weekly windows with `used_percent` / `resets_at`)
that tclaude lifts in `pkg/claude/harness/codex_usage.go` with no network call.
OpenCode exposes no equivalent, so the account-usage dashboard windows
(`collectUsageSnapshot` in `pkg/claude/agentd/usage.go`) should stay empty for
OpenCode sessions.

Proactive provider-limit tracking for OpenCode would have to come from **outside**
OpenCode — tclaude calling the provider's own usage API directly, as the Claude
path does via `pkg/claude/common/usageapi/usageapi.go`. That is provider-specific,
needs separate credentials, and is a different facility from harness usage.

## Recommendation

- **Token counts + context window:** done on `main` (TCL-701 / #1492). No
  further action.
- **Cost:** implemented with the real/virtual split above. OpenCode's native
  model catalog remains the price source; tclaude does not hard-code an
  OpenCode price table.
- **Provider account rate limits:** **descope** from TCL-673 as *not feasible
  via OpenCode*. The 429-reactive signal is the only available hint. If
  proactive limits are ever wanted, spin a separate provider-usage-API ticket —
  it is a different, provider-specific facility, not a harness-usage feature.

Provider-account limit history remains a separate provider-specific facility.
