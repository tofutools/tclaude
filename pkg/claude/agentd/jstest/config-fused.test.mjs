// Tests for the Config tab in a fused multi-node view: the node picker
// (fleet-island.js ScopeBanner) lists the ticked nodes and the tab edits one
// of them, this node by default. A peer is read and saved through this node's
// /api/peer/{node} proxy in the remote mode of its own page, the header and
// the save confirmation name the node, and switching with unsaved edits asks
// first. Parts the form reads from this node itself point to the peer's page.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const KEYS = [
  { path: 'log_level', class: 'general' },
  { path: 'notifications.enabled', class: 'general' },
  { path: 'notifications.cooldown_seconds', class: 'general' },
];
const LOCAL = { level: 'local', classes: { general: 'write', agents: 'write', network: 'write', credentials: 'write', installation: 'write', local: 'write' }, slugs: {}, keys: KEYS };
const PEER = { ...LOCAL, level: 'restricted', classes: { ...LOCAL.classes, agents: 'read', network: 'read', credentials: 'read', installation: 'read', local: 'read' } };
const NODES = [
  { id: 'inst_self', name: 'desk', color: '#58a6ff', local: true },
  { id: 'inst_forge', name: 'forge', color: '#f0883e', local: false },
];

async function mountFused(t, { gate = null } = {}) {
  const harness = await createPreactHarness(t);
  const [{ createConfigState }, { ConfigApp }, adapter, { setConfigPanel }, access] = await Promise.all([
    harness.importDashboardModule('js/config-state.js'),
    harness.importDashboardModule('js/config-island.js'),
    harness.importDashboardModule('js/config-form-adapter.js'),
    harness.importDashboardModule('js/config-panel.js'),
    harness.importDashboardModule('js/config-access.js'),
  ]);
  t.after(() => access.setConfigNodes([]));
  setConfigPanel('notifications');
  const calls = [];
  const fetchImpl = async (path, init = {}) => {
    calls.push(`${init.method || 'GET'} ${path}`);
    const peer = path.startsWith('/api/peer/inst_forge/config');
    if (peer && gate && init.method !== 'POST') await gate;
    if (peer || path.startsWith('/api/config')) {
      const raw = JSON.stringify({ log_level: peer ? 'debug' : 'info', notifications: { enabled: true, cooldown_seconds: 5 } }, null, 2);
      const body = { raw, version: peer ? 'p1' : 'l1', access: peer ? PEER : LOCAL, redacted: [], ...(peer ? {} : { path: '/home/me/.tclaude/config.json' }) };
      if (init.method === 'POST') return { ok: true, status: 200, json: async () => ({ ...body, raw: JSON.stringify(JSON.parse(init.body).config, null, 2), changed: true }) };
      return { ok: true, status: 200, json: async () => body };
    }
    return { ok: false, status: 404, json: async () => ({}) };
  };
  const state = createConfigState({ activeTab: harness.signals.signal('config') });
  const asked = [];
  let answer = true;
  const confirms = [];
  const mounted = await harness.mount(harness.html`<${ConfigApp} state=${state} dependencies=${{
    fetchImpl,
    confirm: async (opts) => { asked.push(opts.title); return answer; },
    confirmDiff: async (...args) => { confirms.push(args); return false; },
  }} />`);
  await adapter.loadConfigTab();
  const settle = () => harness.act(async () => { for (let i = 0; i < 5; i++) await new Promise((r) => setTimeout(r)); });
  await settle();
  const root = mounted.container;
  return {
    harness, adapter, access, state, root, calls, asked, confirms, settle,
    header: () => root.querySelector('#cfg-path').textContent,
    setAnswer: (v) => { answer = v; },
  };
}

test('the fused view edits this node first, then a picked peer through the proxy', async (t) => {
  const f = await mountFused(t);
  assert.equal(f.header(), '/home/me/.tclaude/config.json', 'no picker, no node name');
  await f.harness.act(() => f.access.setConfigNodes(NODES));
  await f.settle();
  assert.equal(f.header(), 'desk’s config (this node) · /home/me/.tclaude/config.json', 'the picker names this node without a reload');
  assert.equal(f.calls.filter((c) => c.startsWith('GET')).length, 1, 'ticking a node keeps this node’s form');
  assert.equal(await f.access.pickConfigNode('inst_forge'), true);
  await f.settle();
  assert.ok(f.calls.includes('GET /api/peer/inst_forge/config'), f.calls.join('\n'));
  assert.equal(f.header(), 'forge’s config (remote)');
  const perms = f.root.querySelector('[data-cfg-key="notifications.enabled"]');
  assert.equal(perms.dataset.cfgAccess, 'write', 'the peer’s access block gates the rows');
  // A save goes to the peer and the confirmation names it.
  const cooldown = f.root.querySelector('[data-cfg-key="notifications.cooldown_seconds"] input');
  cooldown.value = '9';
  await f.harness.act(() => f.harness.fireEvent(cooldown, 'input'));
  await f.adapter.saveConfig();
  await f.settle();
  assert.ok(f.calls.some((c) => c.startsWith('POST /api/peer/inst_forge/config')), 'the save’s dry run goes to the peer');
  assert.ok(!f.calls.some((c) => c.startsWith('POST /api/config')), 'nothing is saved to this node');
  assert.equal(f.confirms[0]?.[3], 'forge’s config (remote)');
});

test('switching nodes with unsaved edits asks first, and staying keeps the edits', async (t) => {
  const f = await mountFused(t);
  await f.harness.act(() => f.access.setConfigNodes(NODES));
  await f.settle();
  assert.equal(await f.access.pickConfigNode('inst_forge'), true);
  await f.settle();
  assert.deepEqual(f.asked, [], 'a clean form switches without asking');
  const cooldown = f.root.querySelector('[data-cfg-key="notifications.cooldown_seconds"] input');
  cooldown.value = '9';
  await f.harness.act(() => f.harness.fireEvent(cooldown, 'input'));
  assert.equal(f.state.dirty.value, true);
  f.setAnswer(false);
  assert.equal(await f.access.pickConfigNode('inst_self'), false);
  assert.deepEqual(f.asked, ['Switch to desk\'s config?']);
  assert.equal(f.access.configTarget.value.id, 'inst_forge', 'refused: the tab stays on forge');
  assert.equal(f.root.querySelector('[data-cfg-key="notifications.cooldown_seconds"] input').value, '9');
  f.setAnswer(true);
  assert.equal(await f.access.pickConfigNode('inst_self'), true);
  await f.settle();
  assert.equal(f.header(), 'desk’s config (this node) · /home/me/.tclaude/config.json');
});

test('a peer’s installation panel points to its own page', async (t) => {
  const f = await mountFused(t);
  await f.harness.act(() => f.access.setConfigNodes(NODES));
  await f.access.pickConfigNode('inst_forge');
  await f.settle();
  const link = [...f.root.querySelectorAll('.cfg-on-peer-page a')].find((a) => a.getAttribute('href').includes('/config/installation'));
  assert.ok(link, 'the installation panel links to the peer’s page');
  assert.match(link.getAttribute('href'), /node=inst_forge/);
});

test('a save while the picked node is still loading is refused: one node’s form never goes to another', async (t) => {
  let open;
  const f = await mountFused(t, { gate: new Promise((r) => { open = r; }) });
  await f.harness.act(() => f.access.setConfigNodes(NODES));
  await f.settle();
  await f.access.pickConfigNode('inst_forge');
  await f.adapter.saveConfig();
  assert.ok(!f.calls.some((c) => c.startsWith('POST')), f.calls.join('\n'));
  open();
  await f.settle();
  assert.equal(f.header(), 'forge’s config (remote)');
});
