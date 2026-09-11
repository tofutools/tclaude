import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('terminal costs follow roster updates and share the persisted Groups toggle', async (t) => {
  const harness = await createPreactHarness(t);
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const groupToggle = harness.document.body.appendChild(harness.document.createElement('button'));
  groupToggle.id = 'groups-cost-toggle';
  const [{ createTerminalShellState }, { createTerminalShellActions }, { TerminalTabs }, prefs, costs] =
    await Promise.all([
      harness.importDashboardModule('js/terminal-shell-state.js'),
      harness.importDashboardModule('js/terminal-shell-actions.js'),
      harness.importDashboardModule('js/terminal-shell-island.js'),
      harness.importDashboardModule('js/prefs.js'),
      harness.importDashboardModule('js/cost-display-toggle.js'),
    ]);
  const prefKey = 'tclaude.dash.agentCost.hidden';
  prefs.dashPrefs.syncItem(prefKey, '1');
  const unbind = costs.bindCostDisplayToggle();
  const state = createTerminalShellState({ persistPresentation: false });
  const actions = createTerminalShellActions({
    state, fetchImpl: async () => ({ ok: true }),
    windowRef: harness.window, documentRef: harness.document,
  });
  const snapshot = harness.signals.signal(null);
  const widgetFactory = (options) => ({
    connect() { options.onStatus('connected'); return Promise.resolve(true); },
    copy() { return Promise.resolve(); },
    fit() {}, focus() {}, setActive() {}, status() { return 'connected'; }, dispose() {},
  });
  const mounted = await harness.mount(harness.html`
    <${TerminalTabs} state=${state} actions=${actions} widgetFactory=${widgetFactory}
      snapshot=${snapshot} />
  `, host);
  await harness.act(async () => {
    actions.openPane({ ws: '/one', key: 'one', label: 'one', agent: 'agt_one' });
    actions.openPane({ ws: '/shell', key: 'shell', label: 'shell' });
    await Promise.resolve();
  });
  const tab = host.querySelector('[data-pane-key="one"]');
  const toggle = host.querySelector('.mux-cost-toggle');
  assert.equal(toggle.getAttribute('aria-pressed'), 'false');
  assert.equal(tab.querySelectorAll('.mux-tab-cost').length, 0);

  const publish = (state) => harness.act(() => {
    snapshot.value = { agents: [{ agent_id: 'agt_one', conv_id: 'conv-new', online: true, state }] };
  });
  await publish({ cost_usd: 2.41, virtual_cost_usd: 8.73 });
  assert.deepEqual([...tab.querySelectorAll('.mux-tab-cost')].map((node) => node.textContent), ['$2.41', '≈$8.73']);
  assert.match(tab.querySelector('.mux-tab-cost-whatif').title, /hypothetical, not a real charge/);
  assert.equal(host.querySelector('[data-pane-key="shell"]').querySelectorAll('.mux-tab-cost').length, 0);
  assert.equal(toggle.previousElementSibling.dataset.paneKey, 'shell', 'toggle follows final tab');

  await harness.act(() => harness.fireEvent(toggle, 'click'));
  assert.equal(toggle.getAttribute('aria-pressed'), 'true');
  assert.equal(groupToggle.getAttribute('aria-pressed'), 'true');
  assert.equal(harness.document.body.classList.contains('agent-cost-hidden'), false);
  assert.equal(prefs.dashPrefs.getItem(prefKey), '0');
  await harness.act(() => harness.fireEvent(groupToggle, 'click'));
  assert.equal(toggle.getAttribute('aria-pressed'), 'false');
  assert.equal(toggle.classList.contains('off'), true);
  assert.equal(harness.document.body.classList.contains('agent-cost-hidden'), true);
  assert.equal(prefs.dashPrefs.getItem(prefKey), '1');

  await publish({ harness: 'copilot', virtual_cost_usd: 3.25, virtual_cost_credits: 325 });
  assert.equal(tab.querySelectorAll('.mux-tab-cost').length, 1);
  assert.equal(tab.querySelector('.mux-tab-cost').textContent, '≈$3.25');
  assert.match(tab.querySelector('.mux-tab-cost').title, /325.*subscription value/);
  await publish({});
  assert.equal(tab.querySelectorAll('.mux-tab-cost').length, 0);
  await mounted.unmount();
  unbind();
  actions.dispose();
});
