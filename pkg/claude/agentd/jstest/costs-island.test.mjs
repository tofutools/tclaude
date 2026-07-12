import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness, getByRole } from './preact-harness.mjs';

const storage = { getItem: () => null, setItem: () => {}, removeItem: () => {} };
function payload(title = 'Alpha') {
  return {
    from: '2026-07-01', to: '2026-07-10', first_day: '2026-07-01', total_usd: 5,
    days: [{ day: '2026-07-10', cost_usd: 5 }],
    agents: [
      { agent_id: 'agt_alpha', conv_id: 'conv-a', day: '2026-07-10', title, harness: 'claude', model: 'opus', cost_usd: 3 },
      { agent_id: 'agt_beta', conv_id: 'conv-b', day: '2026-07-10', title: 'Beta', harness: 'codex', model: 'gpt', cost_usd: 2 },
    ],
  };
}

test('Costs island renders controls and preserves keyed table focus/selection across refreshes', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createCostsState }, { CostsApp }] = await Promise.all([
    harness.importDashboardModule('js/costs-state.js'), harness.importDashboardModule('js/costs-island.js'),
  ]);
  const snapshot = harness.signals.signal({ cost_tab_visible: true, cost_tab_whatif: false });
  const activeTab = harness.signals.signal('groups');
  const state = createCostsState({ snapshot, activeTab, prefs: storage, now: () => new Date(2026, 6, 10, 12) });
  state.initialize();
  state.beginRequest(1);
  state.commitRequest(1, payload());
  const calls = [];
  const actions = { load: async () => calls.push('load'), loadFactor: async () => calls.push('factor'), saveFactor: async () => calls.push('save') };
  const mounted = await harness.mount(harness.html`<${CostsApp} state=${state} actions=${actions} />`);
  const chartColumn = mounted.container.querySelector('.cost-col[data-tip]');
  harness.fireEvent(chartColumn, 'mousemove', { clientX: 20, clientY: 30 });
  const tooltip = harness.document.body.querySelector('.cost-tip');
  assert.ok(tooltip, 'chart hover opens its tooltip');
  await harness.act(() => {
    snapshot.value = { cost_tab_visible: true, cost_tab_whatif: false, generated_at: '2026-07-10T12:00:02Z' };
  });
  assert.equal(mounted.container.querySelector('.cost-col[data-tip]'), chartColumn, 'snapshot refresh preserves the imperative chart');
  assert.equal(harness.document.body.querySelector('.cost-tip'), tooltip, 'snapshot refresh preserves the open chart tooltip');

  const row = mounted.container.querySelector('tr[data-key="cost-conv-a-2026-07-10"]');
  const id = row.querySelector('.id');
  id.tabIndex = 0;
  id.focus();
  const text = row.querySelector('.rowname').firstChild;
  await harness.act(() => { state.beginRequest(2); state.commitRequest(2, payload()); });
  assert.equal(mounted.container.querySelector('tr[data-key="cost-conv-a-2026-07-10"]'), row);
  assert.equal(row.querySelector('.rowname').firstChild, text);
  assert.equal(harness.document.activeElement, id);

  const filter = getByRole(mounted.container, 'textbox', { name: 'Filter cost agents' });
  await harness.input(filter, 'gpt');
  assert.equal(mounted.container.querySelectorAll('tbody tr[data-key]').length, 1);
  assert.equal(mounted.container.querySelector('#filter-costs-count').textContent, '1 / 2');
  const last7 = [...mounted.container.querySelectorAll('#costs-spans button')].find((button) => button.textContent === 'Last 7d');
  await harness.act(() => harness.fireEvent(last7, 'click'));
  assert.equal(state.span.value, '7d');
  assert.ok(calls.includes('load'));
  await mounted.unmount();
});

test('Costs island exposes loading/error/what-if visibility and production cleanup', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createCostsState }, { CostsApp }] = await Promise.all([
    harness.importDashboardModule('js/costs-state.js'), harness.importDashboardModule('js/costs-island.js'),
  ]);
  const snapshot = harness.signals.signal({ cost_tab_visible: true, cost_tab_whatif: true });
  const activeTab = harness.signals.signal('groups');
  const state = createCostsState({ snapshot, activeTab, prefs: storage });
  state.initialize();
  state.beginRequest(1);
  const actions = { load: async () => {}, loadFactor: async () => {}, saveFactor: async () => {} };
  const mounted = await harness.mount(harness.html`<${CostsApp} state=${state} actions=${actions} />`);
  assert.match(mounted.container.textContent, /Loading costs/);
  assert.match(mounted.container.textContent, /WHAT-IF/);
  await harness.act(() => state.failRequest(1, new Error('offline')));
  assert.match(getByRole(mounted.container, 'alert').textContent, /offline/);
  await mounted.unmount();

  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  host.id = 'costs-root';
  const { mountCostsFeature } = await harness.importDashboardModule('js/preact-loader.js');
  const cleanup = await mountCostsFeature({ fetchImpl: async () => { throw new Error('should remain inactive'); } });
  assert.equal(typeof cleanup, 'function');
  assert.ok(host.querySelector('#costs-spans'));
  cleanup();
  assert.equal(host.childElementCount, 0);
});
