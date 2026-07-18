import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('usage lookahead is independent of fetched history span', async (t) => {
  const harness = await createPreactHarness(t);
  const { createUsageHistoryState } = await harness.importDashboardModule('js/usage-history-state.js');
  const state = createUsageHistoryState({
    snapshot: harness.signals.signal({ usage_tab_visible: true }),
    activeTab: harness.signals.signal('usage'),
  });

  assert.equal(state.view.value.hours, 168);
  assert.equal(state.view.value.lookaheadHours, 168);
  assert.equal(state.setLookaheadHours(5), true);
  assert.equal(state.view.value.lookaheadHours, 5);
  assert.equal(state.view.value.hours, 168, 'changing lookahead does not refetch or change history');
  assert.equal(state.setLookaheadHours(12), false);
  assert.equal(state.view.value.lookaheadHours, 5);
});
