import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { createPreactHarness } from './preact-harness.mjs';

const fleet = {
  self: { id: 'inst_self', name: 'desk', color: '#58a6ff', local: true },
  peers: [{ id: 'inst_forge', name: 'forge', color: '#f0883e', local: false }],
};

async function mountTabs(t, nodeFleet = fleet) {
  const harness = await createPreactHarness(t);
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const [{ createTerminalShellState }, { createTerminalShellActions }, { TerminalTabs }, { skynetState }] = await Promise.all([
    harness.importDashboardModule('js/terminal-shell-state.js'),
    harness.importDashboardModule('js/terminal-shell-actions.js'),
    harness.importDashboardModule('js/terminal-shell-island.js'),
    harness.importDashboardModule('js/skynet-state.js'),
  ]);
  skynetState.fleet.value = nodeFleet;
  const state = createTerminalShellState({ persistPresentation: false });
  const actions = createTerminalShellActions({
    state, fetchImpl: async () => ({ ok: true }), windowRef: harness.window, documentRef: harness.document,
  });
  const snapshot = harness.signals.signal({ agents: [{ agent_id: 'agt_one', conv_id: 'conv-one', title: 'builder-1', online: true, state: { status: 'idle', cwd: '/w' } }] });
  const widgetFactory = (options) => ({
    connect() { options.onStatus('connected'); return Promise.resolve(true); },
    copy() { return Promise.resolve(); }, fit() {}, focus() {}, setActive() {}, status() { return 'connected'; }, dispose() {},
  });
  const mounted = await harness.mount(harness.html`<${TerminalTabs} state=${state} actions=${actions} widgetFactory=${widgetFactory} snapshot=${snapshot} />`, host);
  await harness.act(async () => {
    actions.openPane({ ws: '/one', key: 'one', label: 'builder-1', agent: 'agt_one' });
    actions.openPane({ ws: '/peer', key: 'peer', label: 'scout @ forge', remote: { peer: 'inst_forge', peerLabel: 'forge', agent: 'agt_scout' } });
    actions.openPane({ ws: '/shell', key: 'shell', label: 'shell' });
    await Promise.resolve();
  });
  t.after(async () => { await mounted.unmount(); actions.dispose(); skynetState.fleet.value = null; });
  return { harness, host, state, snapshot, tab: (key) => host.querySelector(`[data-pane-key="${key}"]`) };
}

test('an agent tab carries the Groups ⚙ menu next to its status light; a plain shell does not', async (t) => {
  const s = await mountTabs(t);
  const tab = s.tab('one');
  const gear = tab.querySelector('.mux-tab-gear');
  assert.ok(gear, 'agent tab has a ⚙');
  assert.ok(gear.classList.contains('row-actions'), 'the menu takes the Groups row menu styling (and its wizard theme)');
  assert.equal(gear.previousElementSibling.classList.contains('mux-tab-status'), true, 'it sits next to the status light');
  assert.equal(s.tab('shell').querySelector('.mux-tab-gear'), null, 'a plain shell has no ⚙');
  const menu = gear.querySelector('.action-menu');
  assert.ok(menu.querySelector('[data-act="term"][data-agent="agt_one"]'), 'the same row menu items');
  assert.deepEqual([...menu.querySelectorAll('[data-act="send-elsewhere"]')].map((b) => b.dataset.mode), ['move', 'teleport', 'clone', 'morph']);
  assert.equal(menu.querySelector('[data-act="remove-member"]'), null, 'agent level: no group to remove it from');

  const active = s.state.view.value.activeKey;
  assert.equal(active, 'shell');
  await s.harness.act(() => gear.querySelector('.cog-btn').click());
  assert.equal(menu.classList.contains('open'), true);
  assert.match(menu.style.top, /^\d+px$/, 'placed as it opens, not on the next snapshot');
  assert.equal(s.state.view.value.activeKey, 'shell', 'opening ⚙ does not switch tabs');
  await s.harness.act(() => s.harness.fireEvent(menu.querySelector('.action-menu-filter'), 'keydown', { key: 'Enter' }));
  assert.equal(s.state.view.value.activeKey, 'shell', 'keys in the menu do not reach the tab');
  await s.harness.act(() => menu.querySelector('[data-mode="morph"]').click());
  assert.equal(s.state.view.value.activeKey, 'shell', 'picking an item does not switch tabs');
  assert.ok(s.tab('one'), 'nor close the tab');
  assert.equal(menu.classList.contains('open'), false, 'the menu closes on a pick');
});

test('a peer agent tab offers moving it here, naming its node; a peer view shows no ⚙', async (t) => {
  const s = await mountTabs(t);
  const items = [...s.tab('peer').querySelectorAll('.mux-tab-gear [role="menuitem"]')];
  assert.equal(items.length, 1);
  assert.match(items[0].textContent, /^move here…/);
  assert.equal(items[0].dataset.agent, 'agt_scout');
  assert.equal(items[0].dataset.node, 'inst_forge');
  assert.equal(items[0].dataset.nodeName, 'forge');

  await s.harness.act(() => { s.snapshot.value = { ...s.snapshot.value, peer_view: true }; });
  assert.equal(s.host.querySelector('.mux-tab-gear'), null);
});


test('without a hub, local terminal gear keeps clone/morph and omits move/teleport', async (t) => {
  const s = await mountTabs(t, null);
  const menu = s.tab('one').querySelector('.mux-tab-gear .action-menu');
  assert.ok(menu);
  assert.deepEqual([...menu.querySelectorAll('[data-act="send-elsewhere"]')].map((b) => b.dataset.mode), ['clone', 'morph']);
  assert.ok(menu.querySelector('[data-act="term"]'));
});

test('the launch dialog styles its fields with the dashboard controls, in both themes', async () => {
  const css = await readFile(new URL('../dashboard/dashboard.css', import.meta.url), 'utf8');
  const shared = css.split('\n').filter((l) => l.startsWith(':where(.fleet-admin, [id^="fleet-"], #launch-target) :where(select, textarea'));
  assert.ok(shared.length >= 1, 'selects, inputs and textareas share the Fleet form controls');
  assert.match(css, /#launch-target input\[type="checkbox"\] \{[^}]*color-scheme: dark/, 'checkboxes follow the dark scheme');
  assert.match(css, /body\.wizard :is\(#clone-agent-modal, #reincarnate-agent-modal, #group-nest-modal, #launch-target\) :is\(\.cron-create-row, \.lt-row\) :is\(input\[type=text\], input:not\(\[type\]\), select, textarea\)/,
    'wizard mode gives its fields the Clone dialog family skin');
});
