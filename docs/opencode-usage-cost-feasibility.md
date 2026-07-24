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

## Cost — partial (native, but honest-zero on subscription)

OpenCode computes cost itself and exposes it as a plain `number` (USD) on
`Session.cost`, `AssistantMessage.cost`, `StepFinishPart.cost`, and the
`session.next.step.ended` SSE event. It also ships a **per-model price table**
at `GET /config/providers` (the `Model.cost` schema), so — unlike Codex —
tclaude would **not** need to maintain its own price table (contrast
`codexModelPrices` in `pkg/claude/harness/codex_cost.go`):

```jsonc
// Model.cost — USD per token (not per 1M)
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
counts but `cost` = 0, because there is no per-token bill. This matches
TCL-673's own guidance that reporting tokens/context while reporting cost as
not-applicable is the honest answer. An **API-key provider** instead reports a
real non-zero `cost`.

Recommended handling for a TCL-673 cost implementation:

- **API-key providers:** trust OpenCode's `cost` → `sessions.cost_usd`.
- **Subscription providers:** treat `cost == 0` as *not-applicable*, not free;
  report tokens/context only. Optionally derive a *virtual* what-if cost from
  `Model.cost` × token counts and land it in `sessions.virtual_cost_usd`
  (Codex's never-billed estimate lives in that column too), keeping `cost_usd`
  at 0 — mirroring the plan-type branch the Claude statusline already makes in
  `pkg/claude/statusbar/statusbar.go` (real bill → `cost_usd`; subscription
  what-if → `virtual_cost_usd`).

Distinguishing the two is a provider/auth-mode question; the safe default is
honest-zero (tokens + context, cost N/A) unless a real non-zero `cost` arrives.
The cost source is the same SSE stream TCL-701 already consumes, so cost can be
folded into the existing `opencode_context.go` projection when implemented.

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
- **Cost:** implement in TCL-673 with the honest-zero / virtual-cost split
  above; prefer OpenCode's own `cost` for API-key providers and a
  `Model.cost`-derived virtual cost for subscriptions. No hard-coded price
  table needed.
- **Provider account rate limits:** **descope** from TCL-673 as *not feasible
  via OpenCode*. The 429-reactive signal is the only available hint. If
  proactive limits are ever wanted, spin a separate provider-usage-API ticket —
  it is a different, provider-specific facility, not a harness-usage feature.

No dedicated new implementation ticket is needed beyond TCL-673.
