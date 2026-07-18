import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('usage lookahead is independent of fetched history span', async (t) => {
  const harness = await createPreactHarness(t);
  const { createUsageHistoryState } = await harness.importDashboardModule('js/usage-history-state.js');
  const values = new Map([
    ['tclaude.dash.usage.historyHours', '720'],
    ['tclaude.dash.usage.lookaheadHours', '24'],
  ]);
  const prefs = {
    getItem: (key) => values.get(key) ?? null,
    setItem: (key, value) => values.set(key, value),
  };
  const state = createUsageHistoryState({
    snapshot: harness.signals.signal({ usage_tab_visible: true }),
    activeTab: harness.signals.signal('usage'),
    prefs,
  });

  assert.equal(state.view.value.hours, 168);
  assert.equal(state.view.value.lookaheadHours, 168);
  assert.equal(state.initialize(), true);
  assert.equal(state.initialize(), false);
  assert.equal(state.view.value.hours, 720);
  assert.equal(state.view.value.lookaheadHours, 24);
  assert.equal(state.setLookaheadHours(5), true);
  assert.equal(state.view.value.lookaheadHours, 5);
  assert.equal(state.view.value.hours, 720, 'changing lookahead does not refetch or change history');
  assert.equal(values.get('tclaude.dash.usage.lookaheadHours'), '5');
  assert.equal(state.setHours(2160), true);
  assert.equal(values.get('tclaude.dash.usage.historyHours'), '2160');
  assert.equal(state.setLookaheadHours(12), false);
  assert.equal(state.view.value.lookaheadHours, 5);
});
