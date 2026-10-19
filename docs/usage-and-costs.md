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
tclaude costs --json --days --agents
tclaude costs --models --harnesses
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
  Observation time and age are omitted when a legacy Claude cache is the only
  source: its overall refresh timestamp cannot establish each bucket's age.
- `resets_at`: reset time, when reported.
- `status`: `current`, `stale`, `reset`, or `unknown_age`; `available` is true only for a
  current reading. A reset reading retains its old percentage as historical
  information; it does not assert current usage is zero.
- `forecasts`: the Usage tab's `span`, `recent`, and `fit` predictions,
  including status, percentage points per hour, predicted limit time, and
  forecast sample window. The span estimator uses a seven-day history view.

Missing windows are omitted; an empty array means no quota observations are
available. Do not interpret an absent quota as unlimited capacity. Forecast
freshness is independent of the current reading: Codex cache readings can be newer than
sampled history. Check the forecast's status before using its rate or ETA.
Forecasts need at least three samples over thirty minutes and pause when stale.
These quotas and forecasts describe shared account usage, not just the caller.
Coverage warnings identify OpenCode activity lacking recent native readings.
Gemini session logs expose tokens and context usage, but no account-wide quota
windows are captured by tclaude. Gemini spawns therefore have no quota gate;
check the CLI’s `/model` quota display. Gemini costs are grouped under
`google`, using recorded model token counts and list pricing; they count as real
API spend or what-if per `cost.gemini_billing` (see `docs/harnesses.md`).

## Costs

`GET /v1/costs` serves the cost response. `from` and `to` are inclusive dates in
the daemon's local timezone, defaulting to month start and today. Requests must
have ordered dates and span at most 366 days. `self=true` selects the caller's
stable agent ID.

JSON defaults to a compact summary: the range, `generated_at`, `timezone`,
`scope`, `total_usd`, `real_total_usd`, `what_if_total_usd`, `cost_kind`, and
`providers` rollups. Each rollup has `name`, `total_usd`, `real_total_usd`,
`what_if_total_usd`, and `cost_kind` when there is spend. Native Copilot credits
appear as `virtual_cost_credits` when recorded.
`today_real_usd` and `today_what_if_usd` count today only when it is in the
requested range. `agent_id` is present for self queries.

Details are opt-in and can be combined, in both text and JSON output:

- `--days`: daily totals, including zero-spend dates in the requested range.
- `--agents`: detailed per-day conversation costs, with provider, model, and
  stable agent attribution when known. This can be a large response.
- `--models`: totals grouped by the last observed model for each agent/day.
- `--harnesses`: totals grouped by coding harness.

The daemon accepts matching boolean query parameters (`days=true`,
`agents=true`, `models=true`, `harnesses=true`). Unrequested categories are
omitted entirely; requested empty breakdowns are empty arrays. All summaries
and breakdowns use the same costs as the dashboard, and all respect `--self`
and the requested date range. Unknown provider, model, or harness attribution
is grouped under `unknown`.

Recorded API costs and hypothetical subscription equivalents remain separate.
`total_usd` can contain both: use `real_total_usd` when comparing recorded API
spend to a monetary budget. `what_if_enabled` follows the operator's
`cost.show_on_subscription` setting. WHAT-IF values are omitted from spend
when disabled. Dashboard display multipliers never change these raw amounts.

A conversation created from copied history (a clone, a cross-node teleport or
move, a bundle or `.paf` import) records only its own spend; the copied
history's spend stays with its source. Claude Code transcripts drop their
restored cost meter when copied, and Codex, Gemini and OpenCode cost readers
skip usage recorded before the copy landed.

Cost data covers recorded tclaude sessions, not the provider's entire invoice.
Model attribution follows the dashboard's Costs view: each conversation/day slice uses the
last observed model. `generated_at` is the response time, not proof that all
sessions have recently reported costs. These queries do not enforce spending limits. Configurable alert thresholds
are described below.

## Estimated spend limits and alerts

Set **Config → Costs → Spend limits** in the dashboard, or edit the `cost`
block in `~/.tclaude/config.json`:

```json
{"cost":{"daily_limit_usd":20,"monthly_limit_usd":300}}
```

Limits are global across harnesses on this instance. Zero or an absent setting
turns off that period. Daily and monthly periods follow the daemon's local
calendar. The dashboard shows a banner at 80% and at 100%; each threshold also
sends one notification through the existing notify-human path to Messages and,
if enabled, desktop notifications. Threshold records survive daemon restarts,
message deletion, and edits to limits. A jump past both thresholds records both.
The daemon checks every three minutes on its usage timer even when the dashboard is closed; checks
use the latest recorded ledger, so newly reported costs can arrive later.

Amounts are **estimates from the API cost ledger**, before display multipliers.
Subscription WHAT-IF equivalents are excluded; subscription plans may not map
to actual charges. These are per-instance estimates, not a provider invoice or
model-gateway budgets. Alerts do not stop or confirm spawns; a soft spawn gate
is a possible follow-up.
