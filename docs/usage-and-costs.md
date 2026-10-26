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

Agents ignored from the dashboard's Costs view (the ⊘ row button) are left
out of every summary and breakdown here, as they are from the dashboard
totals and usage limits. Restore them from the Costs view's **Ignored**
list.

An agent whose model calls went through a peer's model gateway (usage
sharing, or a requester-paid worker) is ignored on its own, with the reason
"billed to <gateway>@<peer>". Its spend stays recorded here, but the peer
paid for it and counts it, so it cannot be restored. The gateway's node shows
that spend in its Costs view under **Served to peers**: what its gateways paid
for each peer in the span, priced by the gateway from the provider's token
counts. A request still running, or one the provider never answered, counts
its worst case. That spend is kept apart from the node's own agents' totals.
The mark is per conversation: a conversation that ever ran through a peer's
gateway stays billed to it, including when it is resumed in a session on
your own account.

Cost data covers recorded tclaude sessions, not the provider's entire invoice.
Model attribution follows the dashboard's Costs view: each conversation/day slice uses the
last observed model. `generated_at` is the response time, not proof that all
sessions have recently reported costs. These queries do not enforce budgets; usage limits and budgets
are described below.

## Usage limits and budgets

tclaude can watch spend and quota usage against budgets you set, and act as
they fill up. There are two kinds of rule, which work differently:

- **Limits** measure a total with **one shared counter**: the whole instance
  (a global limit, e.g. "$500 a month"), one group's members (a group limit),
  or a provider's account quota window (global only, e.g. Anthropic's 5-hour
  window).
- **Usage profiles** are named sets of **per-agent** rules. Each agent a
  profile applies to gets **its own counter**, e.g. "any agent: tell it at
  $5, ask it to wrap up at $15, stop it at $18".

Every rule is a metric, a period, a budget and a ladder of steps:

| Part | Values |
| --- | --- |
| Metric | `total_usd` (the Costs view's figure: recorded API spend, else the subscription-equivalent estimate), `real_usd`, `whatif_usd`, `window_pct` (global limits only) |
| Period | `day`, `week`, `month` (daemon-local calendar), `lifetime` (usage profiles only: the agent's whole life, across reincarnations) |
| Step | a threshold in percent of the budget plus an action |

Step actions:

- `notify` messages the agent (its inbox) and/or the operator (Messages,
  plus a desktop notification when enabled). It can repeat every N percent
  or every N minutes while the value stays above the threshold.
- `wrap-up` asks the agent to stop starting new work, write a handoff
  summary, send it to whoever assigned the work, and stay idle.
- `hard-stop` kills the agent's process.
- `block-spawn` (limits only) refuses every new spawn the limit covers,
  including the operator's, until the period rolls over or the limit is
  raised, disabled or removed.

On a limit, agent-directed steps reach every running agent in its scope.
Every step other than a plain agent notify also tells the operator, so
nothing drastic happens silently. A rule in **dry run** only tells the
operator what each step would have done.

### Which usage profiles apply to an agent

Profiles are assigned at layers: the **global default**, a **group**, and an
**agent**. The nearest non-empty layer wins as a whole: the agent's own
profiles, else the profiles of every group it belongs to (all of them apply,
each with its own counter), else the global default. A spawn profile can
carry usage profiles too. Spawning through it copies them onto the new agent
as its own assignment, so they outrank the group and global defaults. A
human can also pick a spawned agent's profiles explicitly
(`tclaude agent spawn --usage-profile …`).

### Readings, timing and edge cases

- Rules are evaluated every three minutes on the daemon's usage timer, even
  with the dashboard closed. Costs can be reported late, so leave some room
  between a wrap-up and a hard stop.
- A step fires once per period and budget. Raising a budget re-arms its
  ladder at the new amount; a new period starts it over.
- A stopped agent's steps wait until it runs again, so a hard stop crossed
  while it was offline still applies on resume.
- Quota-window limits act only on a current reading.
- Ignored and peer-billed conversations do not count, as in the Costs view.

### Managing rules

```bash
tclaude usage rules                       # limits, profiles, assignments, readings
tclaude usage limit add --name monthly --period month --budget 500 \
  --step 50:notify:operator --step 90:wrap-up --step 100:block-spawn
tclaude usage limit add --scope group --group backend --name weekly \
  --period week --budget 100 --step 80:notify:agent+operator:every=5%
tclaude usage limit add --name 5h --metric window_pct --window anthropic:five_hour \
  --step 90:notify:operator --step 98:wrap-up
tclaude usage profile set default --name budget --period lifetime --budget 20 \
  --step 50:notify:agent --step 80:notify:agent+operator \
  --step 100:wrap-up --step 120:hard-stop
tclaude usage assign --scope global --profile default
tclaude usage assign --scope group --target backend --profile strict
tclaude usage assign --scope spawn_profile --target reviewer --profile cheap
tclaude usage adjust worker-1 --rule profile:1:budget --extra 10
tclaude usage adjust worker-1 --snooze 60
tclaude usage self                        # an agent's own budgets
```

A step is `AT:ACTION[:TARGETS][:every=N%|every=Nm]`. `--file` reads a rule
(or, for `profile set`, an array of rules) as JSON instead. `adjust` changes
one agent: `--extra` adds budget to one of its rules, `--snooze` holds its
steps for a while, and `--exempt` takes it out of usage profiles (shared
limits still count it).

Reading rules needs `usage.read`. Changing them needs `usage-rules.manage`.
That permission is not default-granted and group ownership does not confer it,
because an agent holding it could lift its own budget.

The former **Spend limits** settings (`cost.daily_limit_usd`,
`cost.monthly_limit_usd`) are converted on the next usage tick into global
limits on recorded API spend with operator alerts at 80% and 100%, and are
removed from `config.json`.
