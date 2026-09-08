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
  assert.equal(state.stackByProvider.value, true, 'provider grouping is the default');
  assert.equal(state.stackByModel.value, false, 'model grouping starts opt-in');
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
  state.toggleProvider('codex');
  assert.deepEqual([...state.view.value.selectedProviders], ['claude']);
  assert.deepEqual([...state.view.value.selectedModels], ['opus'],
    'turning off a provider also removes its provider-only models');
  assert.equal(state.view.value.modelStats.find((entry) => entry.model === 'gpt').available, false);
  assert.equal(state.view.value.narrowed.total_usd, 3);
  assert.ok(storage.values.has('tclaude.dash.costs.providers'));
  state.toggleProvider('codex');
  assert.deepEqual([...state.view.value.selectedModels], ['gpt', 'opus'],
    're-enabling a provider restores its models');
  assert.equal(state.view.value.narrowed.total_usd, 5);
  assert.equal(storage.values.has('tclaude.dash.costs.models'), false,
    'restoring every model uses the compact default preference');
  state.toggleModel('gpt');
  assert.deepEqual([...state.view.value.selectedModels], ['opus']);
  assert.equal(state.view.value.narrowed.total_usd, 3);
  assert.ok(storage.values.has('tclaude.dash.costs.models'));
  assert.equal(state.view.value.accumulatedChart.points.filter((point) => !point.projected).at(-1).cost, 3,
    'recorded accumulated spend follows the selected model');
  assert.equal(state.view.value.accumulatedChart.points.at(-1).projected, true,
    'the accumulated series continues through the month projection');
  state.setStackByProvider(false);
  state.setStackByModel(true);
  assert.equal(state.view.value.chart.stackByProvider, false);
  assert.equal(state.view.value.chart.stackByModel, true);
  assert.equal(storage.values.get('tclaude.dash.costs.stackProvider'), '0');
  assert.equal(storage.values.get('tclaude.dash.costs.stackModel'), '1');
  state.cycleSort('cost');
  assert.equal(state.sort.value.key, 'cost');
  state.activateMonth(2);
  assert.equal(state.span.value, 'calmonth');
  state.setSpan('month');
  assert.equal(state.monthOffset.value, 0);
});

test('Provider filtering retains shared models while dropping exclusive models', async (t) => {
  const harness = await createPreactHarness(t);
  const { createCostsState } = await harness.importDashboardModule('js/costs-state.js');
  const state = createCostsState({
    snapshot: harness.signals.signal({ cost_tab_visible: true, cost_tab_whatif: false }),
    activeTab: harness.signals.signal('costs'), prefs: prefs(),
  });
  state.initialize();
  state.beginRequest(1);
  state.commitRequest(1, {
    from: '2026-07-10', to: '2026-07-10', total_usd: 9,
    days: [{ day: '2026-07-10', cost_usd: 9 }],
    agents: [
      { conv_id: 'a', day: '2026-07-10', harness: 'claude', model: 'shared', cost_usd: 4 },
      { conv_id: 'b', day: '2026-07-10', harness: 'codex', model: 'shared', cost_usd: 3 },
      { conv_id: 'c', day: '2026-07-10', harness: 'codex', model: 'codex-only', cost_usd: 2 },
    ],
  });

  state.toggleProvider('codex');
  assert.deepEqual([...state.view.value.selectedModels], ['shared']);
  assert.equal(state.view.value.modelStats.find((entry) => entry.model === 'shared').available, true);
  assert.equal(state.view.value.modelStats.find((entry) => entry.model === 'codex-only').available, false);
  assert.equal(state.view.value.narrowed.total_usd, 4);
  state.toggleProvider('codex');
  assert.deepEqual([...state.view.value.selectedModels], ['codex-only', 'shared'],
    're-enabling a provider restores its exclusive model without duplicating the shared model');
  assert.equal(state.view.value.narrowed.total_usd, 9);
});

test('Saved model selection is normalized against the saved provider scope', async (t) => {
  const harness = await createPreactHarness(t);
  const { createCostsState } = await harness.importDashboardModule('js/costs-state.js');
  const storage = prefs();
  storage.values.set('tclaude.dash.costs.providers', JSON.stringify(['claude']));
  storage.values.set('tclaude.dash.costs.models', JSON.stringify(['codex-only']));
  const state = createCostsState({
    snapshot: harness.signals.signal({ cost_tab_visible: true, cost_tab_whatif: false }),
    activeTab: harness.signals.signal('costs'), prefs: storage,
  });
  state.initialize();
  state.beginRequest(1);
  state.commitRequest(1, {
    from: '2026-07-10', to: '2026-07-10', total_usd: 6,
    days: [{ day: '2026-07-10', cost_usd: 6 }],
    agents: [
      { conv_id: 'a', day: '2026-07-10', harness: 'claude', model: 'opus', cost_usd: 4 },
      { conv_id: 'b', day: '2026-07-10', harness: 'codex', model: 'codex-only', cost_usd: 2 },
    ],
  });

  assert.deepEqual([...state.view.value.selectedModels], ['opus'],
    'an unavailable legacy preference cannot leave the selected provider with no models');
  assert.equal(state.view.value.narrowed.total_usd, 4);
  state.toggleProvider('codex');
  assert.deepEqual([...state.view.value.selectedModels], ['codex-only', 'opus'],
    're-enabling the provider combines its models with the effective visible selection');
  assert.equal(state.view.value.narrowed.total_usd, 6);
  assert.equal(storage.values.has('tclaude.dash.costs.models'), false,
    'the fully restored model set clears the stale explicit preference');
});
