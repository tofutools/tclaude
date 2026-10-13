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

test('effectiveSubview keeps the request with both halves and collapses to the only one otherwise', async (t) => {
  const harness = await createPreactHarness(t);
  const { effectiveSubview, usageLocation } = await harness.importDashboardModule('js/usage-hub-state.js');
  const both = { loaded: true, quota: true, costs: true };
  assert.equal(effectiveSubview('overview', both), 'overview');
  assert.equal(effectiveSubview('costs', both), 'costs');
  assert.equal(effectiveSubview('overview', { loaded: true, quota: false, costs: true }), 'costs');
  assert.equal(effectiveSubview('costs', { loaded: true, quota: true, costs: false }), 'quota');
  assert.equal(effectiveSubview('costs', { loaded: false, quota: false, costs: false }), 'costs',
    'a deep link holds its view until the first snapshot says otherwise');
  assert.deepEqual(usageLocation('overview'), { tab: 'usage' });
  assert.deepEqual(usageLocation('quota'), { tab: 'usage', subtab: 'quota' });
});

test('Usage hub Overview summarises both halves and subtab clicks announce one navigation', async (t) => {
  const ctx = await setup(t, { usage_tab_visible: true, cost_tab_visible: true });
  const { harness, state, quotaState, costsState, section } = ctx;
  assert.equal(quotaState.view.value.polling, true, 'the Overview keeps quota data fresh');
  assert.equal(costsState.view.value.polling, true, 'the Overview keeps costs data fresh');
  assert.equal(costsState.view.value.active, false, 'but the Costs view itself is not on screen');

  const mounted = await harness.mount(harness.html`<${ctx.hubIsland.UsageHubApp} state=${state}
    quotaState=${quotaState} costsState=${costsState} />`);
  const tabs = [...mounted.container.querySelectorAll('.usage-subtab')];
  assert.deepEqual(tabs.map((tab) => tab.textContent), ['Overview', 'Quota2 windows', 'Costs$4.00 mtd'],
    'Quota and Costs carry their at-a-glance badges');
  assert.deepEqual(tabs.map((tab) => tab.getAttribute('href')), ['/usage', '/usage/quota', '/usage/costs']);
  assert.equal(section.getAttribute('data-subview'), 'overview');

  const text = mounted.container.textContent;
  assert.match(text, /\$4\.00/, 'spend KPI shows the span total');
  assert.match(text, /1 \/ 2/, 'one of two windows is predicted to run out');
  assert.match(text, /Claude 5 hour window/, 'next quota wall names the window');
  assert.equal(mounted.container.querySelectorAll('.usage-ov-quota').length, 2);
  const agents = [...mounted.container.querySelectorAll('.usage-ov-agents tbody tr')].map((row) => row.textContent);
  assert.deepEqual(agents.map((row) => row.replace(/\s+/g, ' ').trim()), ['alphaagt_a$3.00', 'betaagt_b$1.00'],
    'top agents fold their day slices together and keep their ids');
  assert.match(text, /⚠ limit in 1h · 2h without quota/, 'compact quota status names the gap without quota');
  assert.match(text, /✓ resets before the limit/);
  assert.match(text, /in 1h.*Claude 5 hour window · 2h without quota/, 'next wall says how long the window goes dry');
  const bars = mounted.container.querySelectorAll('.usage-ov-chart rect.bar');
  assert.equal(bars.length, 3, 'one bar segment per provider and day');
  assert.ok([...bars].every((bar) => /cost-seg-h\d/.test(bar.getAttribute('class'))), 'bars use the Costs provider palette');
  assert.match(mounted.container.querySelector('.usage-ov-legend').textContent, /claude.*codex/);

  const navigated = [];
  const onNav = (event) => navigated.push(event.detail.location);
  harness.document.addEventListener('tclaude:navigated', onNav);
  t.after(() => harness.document.removeEventListener('tclaude:navigated', onNav));
  await harness.act(() => harness.fireEvent(tabs[2], 'click', { button: 0 }));
  assert.deepEqual(navigated, [{ tab: 'usage', subtab: 'costs' }]);
  assert.equal(state.subview.value, 'costs');
  assert.equal(section.getAttribute('data-subview'), 'costs');
  assert.equal(mounted.container.querySelector('.usage-overview'), null, 'the Overview steps aside');
  assert.equal(costsState.view.value.active, true);
  assert.equal(quotaState.view.value.polling, false, 'quota stops polling off its view and the Overview');

  await harness.act(() => harness.fireEvent(mounted.container.querySelector('.usage-subtab.active'), 'click', { button: 0 }));
  assert.equal(navigated.length, 1, 're-selecting the current view is not a navigation');

  // A router restore (Back) moves the view silently.
  assert.equal(state.applyLocation({ tab: 'usage' }), true);
  assert.equal(state.subview.value, 'overview');
  assert.equal(navigated.length, 1);
  await mounted.unmount();
});

test('Usage hub with one half shows it directly and gates the other on a body class', async (t) => {
  const ctx = await setup(t, { usage_tab_visible: false, cost_tab_visible: true });
  const { harness, state, quotaState, costsState, section } = ctx;
  const mounted = await harness.mount(harness.html`<${ctx.hubIsland.UsageHubApp} state=${state}
    quotaState=${quotaState} costsState=${costsState} />`);
  assert.equal(mounted.container.querySelector('.usage-subnav'), null, 'no subnav for a single view');
  assert.equal(state.subview.value, 'costs');
  assert.deepEqual(state.location.value, { tab: 'usage', subtab: 'costs' });
  assert.equal(section.getAttribute('data-subview'), 'costs');
  const body = harness.document.body.classList;
  assert.equal(body.contains('hide-usage-quota'), true);
  assert.equal(body.contains('hide-usage-costs'), false);
  assert.equal(body.contains('hide-usage-tab'), false);

  await harness.act(() => { ctx.snapshot.value = { usage_tab_visible: false, cost_tab_visible: false }; });
  assert.equal(body.contains('hide-usage-tab'), true, 'the tab hides only when both halves are off');
  await mounted.unmount();
});

test('Usage hub open() is one navigation from outside or inside the tab', async (t) => {
  const ctx = await setup(t, { usage_tab_visible: true, cost_tab_visible: true });
  const { harness, state } = ctx;
  harness.document.body.insertAdjacentHTML('beforeend', '<nav><a data-tab="usage"></a></nav>');
  const nav = harness.document.querySelector('nav [data-tab="usage"]');
  let clicks = 0;
  nav.addEventListener('click', () => { clicks += 1; nav.classList.add('active'); });
  const navigated = [];
  const onNav = (event) => navigated.push(event.detail.location);
  harness.document.addEventListener('tclaude:navigated', onNav);
  t.after(() => harness.document.removeEventListener('tclaude:navigated', onNav));
  assert.equal(state.open('costs', harness.document), true);
  assert.equal(clicks, 1, 'off the tab: the nav click records the location');
  assert.deepEqual(navigated, []);
  assert.equal(state.subview.value, 'costs');
  state.open('quota', harness.document);
  assert.equal(clicks, 1);
  assert.deepEqual(navigated, [{ tab: 'usage', subtab: 'quota' }], 'on the tab: announced like a subnav click');
  state.open('quota', harness.document);
  assert.equal(navigated.length, 1, 'already there: nothing to record');
  assert.equal(state.open('nope', harness.document), false);
});

test('pure Overview helpers', async (t) => {
  const harness = await createPreactHarness(t);
  const { nextQuotaWall, topCostAgents, spanLabel } = await harness.importDashboardModule('js/usage-hub-island.js');
  const item = (status, at) => ({ atRisk: ['before_reset', 'projected', 'limit'].includes(status), status, forecast: { hits_limit_at: at } });
  const soon = item('before_reset', '2026-07-24T01:00:00Z');
  const later = item('projected', '2026-07-25T01:00:00Z');
  assert.equal(nextQuotaWall([later, soon, item('after_reset', '2026-07-23T00:00:00Z')]), soon);
  const spent = item('limit', '');
  assert.equal(nextQuotaWall([soon, spent]), spent, 'a window already at its limit wins');
  assert.equal(nextQuotaWall([item('flat', '')]), null);
  assert.deepEqual(topCostAgents([{ conv_id: 'c', title: 'z', cost_usd: 0 }]), [], 'zero-cost agents are dropped');
  assert.equal(spanLabel({ span: '7d' }), 'Last 7d');
  assert.equal(spanLabel({ span: 'calmonth', monthLabel: 'June 2026' }), 'June 2026');
});
