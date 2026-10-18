import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const storage = { getItem: () => null, setItem: () => {}, removeItem: () => {} };
const peer = (id, label) => ({ instance_id: id, label, trusted: true, online: true, level: 'restricted' });

async function fusedFleet(harness) {
  const { createFleetState } = await harness.importDashboardModule('js/fleet-state.js');
  const fleet = createFleetState({ search: '?nodes=all' });
  fleet.setStatus({ enabled: true, instance_id: 'inst_self', name: 'desk', hub: { state: 'connected' }, peers: [peer('inst_forge', 'forge'), peer('inst_mac0', 'mac')] });
  return fleet;
}

// fetchFor answers forge's reads with body and refuses mac (it trusts this node
// less than unrestricted); every call is recorded.
function peerFetch(calls, body) {
  return (id) => async (path, init = {}) => {
    calls.push(`${id} ${init.method || 'GET'} ${path}`);
    if (id === 'inst_mac0') return { ok: false, status: 403, json: async () => ({}), text: async () => '' };
    if (!init.method || init.method === 'GET') return { ok: true, json: async () => body };
    return { ok: true, status: 200, text: async () => '{}' };
  };
}

const heads = (c) => [...c.querySelectorAll('.fused-section-head')].map((h) => [...h.children].map((n) => n.textContent.trim()).filter(Boolean).join(' '));
const settle = (harness) => harness.act(() => new Promise((r) => setTimeout(r, 0)));

test('a fused Plugins tab adds a section per peer; its actions run on that node', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createPluginsState }, { PluginsApp }] = await Promise.all([
    harness.importDashboardModule('js/plugins-state.js'), harness.importDashboardModule('js/plugins-island.js'),
  ]);
  const snapshot = harness.signals.signal({ plugins: [], plugins_catalog: [], plugins_tab_visible: false });
  const poll = harness.signals.signal({ phase: 'ready', requestId: 1, error: null });
  const state = createPluginsState({ snapshot, poll, activeTab: harness.signals.signal('plugins'), prefs: storage });
  state.initialize();
  const fleet = await fusedFleet(harness);
  const calls = [];
  const fetchFor = peerFetch(calls, {
    plugins: [{ name: 'canvas', status: 'ok', steps: [{ name: 'server', check: 'true', run: 'docker run canvas' }] }],
    catalog: [{ name: 'canvas', steps: [] }, { name: 'github', descr: 'source control', steps: [{ name: 'server', run: 'gh serve' }] }],
  });
  const mounted = await harness.mount(harness.html`<${PluginsApp} state=${state} actions=${{ refresh: async () => {} }} fleet=${fleet}
    confirm=${async () => true} notify=${() => {}} fetchFor=${fetchFor} />`);
  await settle(harness);
  const c = mounted.container;
  assert.equal(document.body.classList.contains('hide-plugins'), false, 'a fused view keeps the tab though this node has none');
  assert.match(c.querySelector('.fused-self-head').textContent, /⌂ desk/);
  assert.deepEqual(heads(c), ['forge 1 plugin open on forge ↗', 'mac not shared 🔒']);
  const [forge, mac] = c.querySelectorAll('.fused-section');
  assert.equal(mac.hasAttribute('open'), false, 'a refused section stays collapsed');
  assert.equal(forge.hasAttribute('open'), true);
  assert.match(mac.querySelector('.lock-why').dataset.why, /needs unrestricted trust from mac/);
  assert.deepEqual([...forge.querySelectorAll('.plugin-catalog-card .plugin-head .rowname')].map((n) => n.textContent), ['github'], 'the peer\'s catalog less what it has installed');
  calls.length = 0;
  const check = [...forge.querySelectorAll('.plugin-card button')].find((b) => b.textContent.trim() === 'check');
  await harness.act(() => check.click());
  await settle(harness);
  assert.deepEqual(calls.slice(0, 2), ['inst_forge POST /api/plugins/canvas/check', 'inst_forge GET /api/plugins']);
  await mounted.unmount(); fleet.dispose();
});

test('a fused Processes tab lists each peer\'s templates; delete acts on that node', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createProcessesState }, { ProcessesApp }] = await Promise.all([
    harness.importDashboardModule('js/processes-state.js'), harness.importDashboardModule('js/processes-island.js'),
  ]);
  const state = createProcessesState({ activeTab: harness.signals.signal('processes'), prefs: storage });
  state.templatesRequest.commitRequest(state.templatesRequest.beginRequest(), { templates: [] });
  const fleet = await fusedFleet(harness);
  const calls = [];
  const fetchFor = peerFetch(calls, { templates: [{ id: 'tpl_rel', name: 'Release train', versionCount: 2, latestVersion: { semanticHash: 'abcdef0123456' } }] });
  const actions = { refreshActive() {}, load() {}, observeTemplateHeads() {}, activateSubtab() {}, openEditor() {}, closeCanvas() {}, describeActor: () => null };
  const mounted = await harness.mount(harness.html`<${ProcessesApp} state=${state} actions=${actions} fleet=${fleet}
    confirm=${async () => true} notify=${() => {}} fetchFor=${fetchFor} />`);
  await settle(harness);
  const c = mounted.container;
  assert.deepEqual(heads(c), ['forge 1 template open on forge ↗', 'mac not shared 🔒']);
  const row = c.querySelector('[data-process-peer-template="tpl_rel"]');
  assert.match(row.textContent, /Release train/);
  calls.length = 0;
  await harness.act(() => row.querySelector('.process-delete-btn').click());
  await settle(harness);
  assert.deepEqual(calls.slice(0, 2), ['inst_forge DELETE /v1/process/templates/tpl_rel', 'inst_forge GET /v1/process/templates']);
  await mounted.unmount(); fleet.dispose();
});

test('outside a fused view neither tab adds sections', async (t) => {
  const harness = await createPreactHarness(t);
  const { fusedPeers } = await harness.importDashboardModule('js/fused-sections.js');
  const { createFleetState } = await harness.importDashboardModule('js/fleet-state.js');
  const fleet = createFleetState({ search: '' });
  fleet.setStatus({ enabled: true, instance_id: 'inst_self', name: 'desk', hub: { state: 'connected' }, peers: [peer('inst_forge', 'forge')] });
  assert.deepEqual(fusedPeers(fleet), []);
  const fused = await fusedFleet(harness);
  assert.deepEqual(fusedPeers(fused, { id: 'inst_forge' }), [], 'a peer\'s own view never fuses');
  assert.deepEqual(fusedPeers(fused).map((n) => n.name), ['forge', 'mac']);
  fleet.dispose(); fused.dispose();
});
