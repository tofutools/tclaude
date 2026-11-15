import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const prefs = { getItem: () => null, setItem: () => {}, removeItem: () => {} };

function quotaPayload() {
  const point = (at, pct) => ({ at, pct, resets_at: '2026-07-24T03:00:00Z' });
  return {
    from: '2026-07-23T00:00:00Z', generated_at: '2026-07-24T00:00:00Z', coverage_warnings: [],
    series: [{
      provider: 'anthropic', window_name: 'five_hour', duration_seconds: 18000,
      points: [point('2026-07-23T22:00:00Z', 10), point('2026-07-23T23:00:00Z', 40), point('2026-07-23T23:45:00Z', 70)],
      forecast: { status: 'before_reset', hits_limit_at: '2026-07-24T01:00:00Z', reset_at: '2026-07-24T03:00:00Z', rate_pct_per_hour: 30 },
    }, {
      provider: 'openai', window_name: 'seven_day', duration_seconds: 604800,
      points: [point('2026-07-23T12:00:00Z', 5), point('2026-07-23T23:00:00Z', 6)],
      forecast: { status: 'after_reset', hits_limit_at: '2026-08-10T00:00:00Z', reset_at: '2026-07-26T00:00:00Z', rate_pct_per_hour: 0.1 },
    }],
  };
}

function costsPayload() {
  return {
    from: '2026-07-01', to: '2026-07-31', first_day: '2026-07-01',
    days: [{ day: '2026-07-09', cost_usd: 1, real_cost_usd: 1 }, { day: '2026-07-10', cost_usd: 3, real_cost_usd: 3 }],
    agents: [
      { agent_id: 'agt_a', conv_id: 'c1', day: '2026-07-09', title: 'alpha', harness: 'claude', model: 'opus', cost_usd: 1, real_cost_usd: 1, cost_kind: 'real' },
      { agent_id: 'agt_a', conv_id: 'c1', day: '2026-07-10', title: 'alpha', harness: 'claude', model: 'opus', cost_usd: 2, real_cost_usd: 2, cost_kind: 'real' },
      { agent_id: 'agt_b', conv_id: 'c2', day: '2026-07-10', title: 'beta', harness: 'codex', model: 'gpt', cost_usd: 1, real_cost_usd: 1, cost_kind: 'real' },
    ],
    total_usd: 4, real_total_usd: 4, what_if_total_usd: 0,
  };
}

async function setup(t, snap) {
  const harness = await createPreactHarness(t);
  const [hubState, hubIsland, quotaMod, costsMod] = await Promise.all([
    harness.importDashboardModule('js/usage-hub-state.js'),
    harness.importDashboardModule('js/usage-hub-island.js'),
    harness.importDashboardModule('js/usage-history-state.js'),
    harness.importDashboardModule('js/costs-state.js'),
  ]);
  const snapshot = harness.signals.signal(snap);
  const activeTab = harness.signals.signal('usage');
  const state = hubState.createUsageHubState({ snapshot, activeTab });
  const quotaState = quotaMod.createUsageHistoryState({ snapshot, activeTab, subview: state.subview, prefs });
  const costsState = costsMod.createCostsState({ snapshot, activeTab, subview: state.subview, prefs, now: () => new Date(2026, 6, 10, 12) });
  quotaState.initialize();
  costsState.initialize();
  quotaState.beginRequest(1);
  quotaState.commitRequest(1, quotaPayload());
  costsState.beginRequest(1);
  costsState.commitRequest(1, costsPayload());
  const section = harness.document.createElement('section');
  section.id = 'tab-usage';
  harness.document.body.append(section);
  return { harness, hubState, hubIsland, snapshot, activeTab, state, quotaState, costsState, section };
}


// The phone hub: Overview as before, Quota and Costs as phone summaries.
async function mountPhone(t, pickSpan = () => {}) {
  const ctx = await setup(t, { usage_tab_visible: true, cost_tab_visible: true });
  const mounted = await ctx.harness.mount(ctx.harness.html`<${ctx.hubIsland.UsageHubApp} state=${ctx.state}
    quotaState=${ctx.quotaState} costsState=${ctx.costsState} phone=${true} pickSpan=${pickSpan} />`);
  const chip = (name) => [...mounted.container.querySelectorAll('.phone-usage-chips .ui-tab')]
    .find((tab) => tab.textContent.startsWith(name));
  return { ...ctx, mounted, chip };
}

const squash = (text) => text.replace(/\s+/g, ' ').trim();

test('phone Usage: the subnav is a chip row with the desktop subtab counts, and the Overview stays', async (t) => {
  const { mounted } = await mountPhone(t);
  assert.equal(mounted.container.querySelector('.usage-subnav'), null, 'no boxed segmented control');
  const chips = [...mounted.container.querySelectorAll('.phone-usage-chips .ui-tab')].map((tab) => squash(tab.textContent));
  assert.deepEqual(chips, ['Overview', 'Quota 2 windows', 'Costs $4.00 mtd']);
  assert.ok(mounted.container.querySelector('.usage-overview'), 'the Overview is the hub one');
  assert.equal(mounted.container.querySelector('.phone-usage-quota'), null);
});

test('phone Usage Quota: one card per window, the one at risk first', async (t) => {
  const { harness, mounted, chip, state } = await mountPhone(t);
  await harness.act(() => harness.fireEvent(chip('Quota'), 'click', { button: 0 }));
  assert.equal(state.subview.value, 'quota');
  assert.equal(mounted.container.querySelector('.usage-overview'), null);
  const cards = [...mounted.container.querySelectorAll('.phone-quota-card')];
  assert.equal(cards.length, 2);
  assert.deepEqual(cards.map((card) => card.getAttribute('aria-label')), ['Claude 5 hour window', 'Codex 7 day window']);
  const [risk, calm] = cards;
  assert.ok(risk.classList.contains('at-risk'));
  assert.equal(risk.querySelector('.phone-quota-pct').textContent, '70%');
  assert.equal(risk.querySelector('[role="meter"]').getAttribute('aria-valuenow'), '70');
  assert.ok(risk.querySelector('.phone-quota-bar b').classList.contains('bad'), 'a window at risk has a red bar');
  assert.equal(risk.querySelector('.phone-quota-meta').textContent, 'resets in 3h · sampled 15m ago');
  assert.equal(risk.querySelector('.phone-quota-status').textContent, '⚠ limit in 1h · 2h without quota');
  assert.ok(risk.querySelector('.usage-ov-spark'), 'a sparkline');
  assert.equal(calm.querySelector('.phone-quota-pct').textContent, '6%');
  assert.ok(calm.querySelector('.phone-quota-bar b').classList.contains('ok'));
  assert.equal(calm.querySelector('.phone-quota-status').textContent, '✓ resets before the limit');
  assert.equal(mounted.container.querySelector('.usage-forecast-controls, .usage-series-card'), null, 'no desktop forecast controls');
});

test('phone Usage Costs: the total, per provider, per model and the top agents; a span chip picks the span', async (t) => {
  const picked = [];
  const { harness, mounted, chip } = await mountPhone(t, (key) => picked.push(key));
  await harness.act(() => harness.fireEvent(chip('Costs'), 'click', { button: 0 }));
  const root = mounted.container.querySelector('.phone-usage-costs');
  assert.ok(root);
  assert.equal(root.querySelector('.phone-cost-big').textContent, '$4.00');
  const lists = [...root.querySelectorAll('.phone-section')].map((section) => ({
    title: section.getAttribute('aria-label'),
    rows: [...section.querySelectorAll('.phone-cost-row')].map((row) => squash(row.textContent)),
  }));
  assert.deepEqual(lists, [
    { title: 'By provider', rows: ['claude1 agent$3.0075%', 'codex1 agent$1.0025%'] },
    { title: 'By model', rows: ['opusclaude$3.0075%', 'gptcodex$1.0025%'] },
    { title: 'Top agents', rows: ['alphaagt_a$3.00', 'betaagt_b$1.00'] },
  ]);
  const spans = [...root.querySelectorAll('.phone-cost-spans .ui-tab')];
  assert.deepEqual(spans.map((tab) => tab.textContent), ['This month', 'Last 7d', 'Last 30d', 'Last 90d']);
  await harness.act(() => harness.fireEvent(spans[0], 'click', { button: 0 }));
  assert.deepEqual(picked, [], 'the current span is not picked again');
  await harness.act(() => harness.fireEvent(spans[1], 'click', { button: 0 }));
  assert.deepEqual(picked, ['7d']);
});

test('phone Usage helpers', async (t) => {
  const harness = await createPreactHarness(t);
  const { quotaBarTone, spanChips, costTotals } = await harness.importDashboardModule('js/phone-usage.js');
  assert.equal(quotaBarTone({ latest: { pct: 20 } }), 'ok');
  assert.equal(quotaBarTone({ latest: { pct: 70 } }), 'warn');
  assert.equal(quotaBarTone({ latest: { pct: 95 } }), 'bad');
  assert.equal(quotaBarTone({ latest: { pct: 10 }, atRisk: true }), 'bad');
  assert.equal(quotaBarTone({ latest: null }), 'ok');
  assert.deepEqual(spanChips({ span: 'calmonth', monthLabel: 'August 2026' }).map((chip) => chip.key),
    ['month', '7d', '30d', '90d', 'calmonth'], 'a browsed month shows as its own chip');
  // Only the models narrowed (opus kept): codex has no kept model, so it is
  // not a provider row, and the rows add up to the total.
  const totals = costTotals({
    narrowed: {
      total_usd: 3,
      agents: [
        { agent_id: 'a', provider: 'anthropic', model: 'opus', cost_usd: 1 },
        { agent_id: 'a', provider: 'anthropic', model: 'opus', cost_usd: 1 },
        { agent_id: 'b', provider: 'anthropic', model: 'opus', cost_usd: 1 },
      ],
    },
    providers: ['anthropic', 'codex'], models: ['gpt', 'opus'],
    selectedProviders: new Set(['anthropic', 'codex']), selectedModels: new Set(['opus']),
    rows: [],
  });
  assert.deepEqual(totals.providers.map((p) => [p.key, p.cost, p.agentCount, p.share]), [['anthropic', 3, 2, 1]]);
  assert.deepEqual(totals.models.map((m) => [m.key, m.subs]), [['opus', ['anthropic']]]);
  assert.equal(totals.narrowed, true);
});
