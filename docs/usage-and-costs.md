# Querying usage and costs

Operators and agents can query account quotas and recorded costs through the
agentd daemon. The commands do not access the private database or provider
credentials, and do not trigger provider API requests.

```bash
tclaude usage
tclaude usage --json
tclaude costs
tclaude costs --from 2026-10-01 --to 2026-10-03 --json
tclaude costs --self --json
```

Agents need `usage.read` and `costs.read`, respectively. Both are installed by
`tclaude setup --install-default-agent-permissions` (or `--install-all`).
Existing installations need to rerun that setup option to add them. Explicit
per-agent denies still take precedence. Human operators have implicit access.
`--self` requires an identified agent and includes costs attributed to its
stable agent ID across linked conversation generations.

## Usage

`tclaude usage` replaces the former Anthropic-only direct API command. Its
`--json` format is now the daemon's multi-provider response, not raw Anthropic
JSON. `GET /v1/usage/summary` serves the same response over the agent socket.

The response contains `generated_at`, account `scope`, `windows`, and
`coverage_warnings`. Each observed provider/window includes:

- `pct`: last observed percentage consumed, with native `used_units` and
  `limit_units` when known.
- `observed_at`, `age_seconds`, and `source`: the reading's freshness and origin.
- `resets_at`: reset time, when reported.
- `status`: `current`, `stale`, or `reset`; `available` is true only for a
  current reading. A reset reading retains its old percentage as historical
  information; it does not assert current usage is zero.
- `forecasts`: the Usage tab's `span`, `recent`, and `fit` predictions,
  including status, percentage points per hour, predicted limit time, and
  forecast sample window. The span estimator uses a seven-day history view.

Missing windows are omitted; an empty array means no quota observations are
available. Do not interpret an absent quota as unlimited capacity. Forecast
freshness is independent of the current reading: a cache can be newer than
sampled history. Check the forecast's status before using its rate or ETA.
Forecasts need at least three samples over thirty minutes and pause when stale.
These quotas and forecasts describe shared account usage, not just the caller.
Coverage warnings identify OpenCode activity lacking recent native readings.

## Costs

`GET /v1/costs` serves the cost response. `from` and `to` are inclusive dates in
the daemon's local timezone, defaulting to month start and today. Requests must
have ordered dates and span at most 366 days. `self=true` selects the caller's
stable agent ID.

JSON includes the range, `generated_at`, `timezone`, `scope`, `total_usd`,
`real_total_usd`, `what_if_total_usd`, `cost_kind`, daily `days`, and per-day
conversation `agents` with provider, model, and stable agent attribution when
known. Zero split totals may be omitted; treat them as zero. `today_real_usd`
and `today_what_if_usd` count today only when it is in the requested range.
`agent_id` is present for self queries.

Recorded API costs and hypothetical subscription equivalents remain separate.
`total_usd` can contain both: use `real_total_usd` when comparing recorded API
spend to a monetary budget. `what_if_enabled` follows the operator's
`cost.show_on_subscription` setting. WHAT-IF values are omitted from spend
when disabled. Dashboard display multipliers never change these raw amounts.

Cost data covers recorded tclaude sessions, not the provider's entire invoice.
Model attribution follows the Costs tab: each conversation/day slice uses the
last observed model. `generated_at` is the response time, not proof that all
sessions have recently reported costs. No configured monetary budget or hard
spending enforcement is provided by these queries.
