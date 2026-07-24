import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

function prefs() {
  const values = new Map();
  return { values, getItem: (key) => values.get(key) ?? null, setItem: (key, value) => values.set(key, String(value)), removeItem: (key) => values.delete(key) };
}

test('Costs state owns controls, derived rows, selection, requests, and preferences', async (t) => {
  const harness = await createPreactHarness(t);
  const { createCostsState } = await harness.importDashboardModule('js/costs-state.js');
  const storage = prefs();
  storage.values.set('tclaude.dash.costs.fillEmptyWeekdays', '1');
  const state = createCostsState({
    snapshot: harness.signals.signal({ cost_tab_visible: true, cost_tab_whatif: false }),
    activeTab: harness.signals.signal('costs'), prefs: storage,
    now: () => new Date(2026, 6, 10, 12),
  });
  state.initialize();
  assert.equal(state.fillEmpty.value, true);
  state.beginRequest(1);
  state.commitRequest(1, {
    from: '2026-07-01', to: '2026-07-10', total_usd: 5,
    real_total_usd: 3, what_if_total_usd: 2, cost_kind: 'mixed',
    days: [{ day: '2026-07-10', cost_usd: 5, real_cost_usd: 3, what_if_cost_usd: 2, cost_kind: 'mixed' }],
    agents: [
      { conv_id: 'a', day: '2026-07-10', title: 'Alpha', harness: 'claude', model: 'opus', cost_usd: 3, real_cost_usd: 3, cost_kind: 'real' },
      { conv_id: 'b', day: '2026-07-10', title: 'Beta', harness: 'codex', model: 'gpt', cost_usd: 2, what_if_cost_usd: 2, cost_kind: 'what_if' },
    ],
  });
  assert.equal(state.view.value.request.hasLoaded, true);
  state.setQuery('gpt');
  assert.deepEqual(state.view.value.rows.map((row) => row.conv_id), ['b']);
  state.setQuery('');
  state.toggleHarness('codex');
  assert.deepEqual([...state.view.value.selectedHarnesses], ['claude']);
  assert.equal(state.view.value.narrowed.total_usd, 3);
  assert.ok(storage.values.has('tclaude.dash.costs.harnesses'));
  state.cycleSort('cost');
  assert.equal(state.sort.value.key, 'cost');
  state.activateMonth(2);
  assert.equal(state.span.value, 'calmonth');
  state.setSpan('month');
  assert.equal(state.monthOffset.value, 0);
});
