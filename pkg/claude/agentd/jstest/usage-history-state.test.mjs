import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

function fakePrefs(entries = []) {
  const values = new Map(entries);
  return {
    values,
    getItem: (key) => values.get(key) ?? null,
    setItem: (key, value) => values.set(key, value),
  };
}

test('usage spans are stored per series with legacy globals as the default', async (t) => {
  const harness = await createPreactHarness(t);
  const { createUsageHistoryState } = await harness.importDashboardModule('js/usage-history-state.js');
  const prefs = fakePrefs([
    ['tclaude.dash.usage.historyHours', '720'],
    ['tclaude.dash.usage.lookaheadHours', '24'],
    ['tclaude.dash.usage.seriesSpans', JSON.stringify({ 'anthropic:seven_day': { hours: 24, lookaheadHours: 5 } })],
  ]);
  const state = createUsageHistoryState({
    snapshot: harness.signals.signal({ usage_tab_visible: true }),
    activeTab: harness.signals.signal('usage'),
    prefs,
  });

  assert.deepEqual(state.view.value.spanFor('anthropic:seven_day'), { hours: 168, lookaheadHours: 168 });
  assert.equal(state.initialize(), true);
  assert.equal(state.initialize(), false);
  assert.deepEqual(state.view.value.spanFor('anthropic:seven_day'), { hours: 24, lookaheadHours: 5 },
    'a stored per-series entry wins');
  assert.deepEqual(state.view.value.spanFor('openai:five_hour'), { hours: 720, lookaheadHours: 24 },
    'series without an entry fall back to the legacy global spans');
  assert.equal(state.view.value.defaultHours, 720);
  assert.deepEqual(state.view.value.spanOverrides, { 'anthropic:seven_day': 24 },
    'only non-default history spans are sent as request overrides');

  assert.equal(state.setSeriesLookaheadHours('openai:five_hour', 5), true);
  assert.deepEqual(state.view.value.spanFor('openai:five_hour'), { hours: 720, lookaheadHours: 5 });
  assert.deepEqual(state.view.value.spanOverrides, { 'anthropic:seven_day': 24 },
    'changing a lookahead never adds a history override');
  assert.equal(state.setSeriesHours('openai:five_hour', 2160), true);
  assert.deepEqual(state.view.value.spanOverrides, { 'anthropic:seven_day': 24, 'openai:five_hour': 2160 });
  assert.deepEqual(JSON.parse(prefs.values.get('tclaude.dash.usage.seriesSpans')), {
    'anthropic:seven_day': { hours: 24, lookaheadHours: 5 },
    'openai:five_hour': { hours: 2160, lookaheadHours: 5 },
  });

  assert.equal(state.setSeriesHours('openai:five_hour', 12), false, 'unknown history span rejected');
  assert.equal(state.setSeriesLookaheadHours('openai:five_hour', 12), false, 'unknown lookahead rejected');
  assert.equal(state.setSeriesHours('', 24), false, 'empty series key rejected');
  assert.equal(state.setSeriesHours('a:b:c', 24), false, 'key breaking the server spans grammar rejected');
  assert.equal(state.setSeriesHours('a,b:c', 24), false, 'key with comma rejected');
});

test('usage span store drops entries that would break the request grammar', async (t) => {
  const harness = await createPreactHarness(t);
  const { createUsageHistoryState } = await harness.importDashboardModule('js/usage-history-state.js');
  const stored = {
    'bad:extra:colon': { hours: 24 },
    'bad,comma:window': { hours: 24 },
    'openai:five_hour': { hours: 24 },
  };
  for (let i = 0; i < 150; i++) stored[`provider${i}:window`] = { hours: 24 };
  const prefs = fakePrefs([['tclaude.dash.usage.seriesSpans', JSON.stringify(stored)]]);
  const state = createUsageHistoryState({
    snapshot: harness.signals.signal({ usage_tab_visible: true }),
    activeTab: harness.signals.signal('usage'),
    prefs,
  });
  state.initialize();
  const overrides = state.view.value.spanOverrides;
  assert.equal(overrides['bad:extra:colon'], undefined, 'grammar-breaking key dropped on load');
  assert.equal(overrides['bad,comma:window'], undefined, 'comma key dropped on load');
  assert.equal(overrides['openai:five_hour'], 24, 'valid entry survives');
  assert.ok(Object.keys(overrides).length <= 100, 'entry count capped below the server override limit');
});

test('usage span store tolerates corrupt persisted JSON', async (t) => {
  const harness = await createPreactHarness(t);
  const { createUsageHistoryState } = await harness.importDashboardModule('js/usage-history-state.js');
  const prefs = fakePrefs([
    ['tclaude.dash.usage.seriesSpans', '{not json'],
  ]);
  const state = createUsageHistoryState({
    snapshot: harness.signals.signal({ usage_tab_visible: true }),
    activeTab: harness.signals.signal('usage'),
    prefs,
  });
  state.initialize();
  assert.deepEqual(state.view.value.spanFor('anthropic:seven_day'), { hours: 168, lookaheadHours: 168 });
  assert.deepEqual(state.view.value.spanOverrides, {});
});
