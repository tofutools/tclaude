import test from 'node:test'; import assert from 'node:assert/strict'; import { createPreactHarness } from './preact-harness.mjs';
test('Audit actions preserve API parameters and reject stale overlapping responses', async (t) => {
  const harness = await createPreactHarness(t); const [{ createAuditState }, { createAuditActions }] = await Promise.all([harness.importDashboardModule('js/audit-state.js'), harness.importDashboardModule('js/audit-actions.js')]);
  const state = createAuditState({ activeTab: harness.signals.signal('audit') }); state.setFilter('outcome', 'failure'); state.cycleSort('verb');
  const pending = []; const actions = createAuditActions({ state, fetchImpl: (url, options) => new Promise((resolve) => pending.push({ url, options, resolve })) });
  const old = actions.load(); const fresh = actions.load(); assert.match(pending[0].url, /^\/api\/audit\?page=1&page_size=100&sort=verb&dir=asc&outcome=failure$/); assert.deepEqual(pending[0].options, { credentials: 'same-origin' });
  pending[1].resolve({ ok: true, json: async () => ({ entries: [{ id: 2 }], total: 1, total_unfiltered: 1 }) }); assert.equal(await fresh, true);
  pending[0].resolve({ ok: true, json: async () => ({ entries: [{ id: 1 }], total: 1, total_unfiltered: 1 }) }); assert.equal(await old, false); assert.equal(state.view.value.rows[0].id, 2);
});
test('Audit actions expose HTTP failures', async (t) => {
  const harness = await createPreactHarness(t); const [{ createAuditState }, { createAuditActions }] = await Promise.all([harness.importDashboardModule('js/audit-state.js'), harness.importDashboardModule('js/audit-actions.js')]);
  const state = createAuditState({ activeTab: harness.signals.signal('audit') }); const actions = createAuditActions({ state, fetchImpl: async () => ({ ok: false, status: 503, text: async () => 'offline' }) });
  assert.equal(await actions.load(), false); assert.match(state.view.value.request.error, /offline/);
});
test('a fused audit reads one peer through its routes, or merges every node newest first', async (t) => {
  const harness = await createPreactHarness(t); const [{ createAuditState }, { createAuditActions }] = await Promise.all([harness.importDashboardModule('js/audit-state.js'), harness.importDashboardModule('js/audit-actions.js')]);
  const self = { id: 'inst_self', name: 'desk', local: true, color: '#58a6ff' }; const forge = { id: 'inst_forge', name: 'forge', color: '#d29922' }; const mac = { id: 'inst_mac0', name: 'mac', color: '#a371f7' };
  const page = (entries) => ({ ok: true, json: async () => ({ entries, total: entries.length, total_unfiltered: entries.length }) });
  const calls = [];
  const fetchFor = (id) => async (url) => { calls.push([id, url]); return id === 'inst_mac0' ? { ok: false, status: 403, text: async () => '' } : page([{ id: 7, at: '2026-10-18T10:02:00Z' }]); };
  const state = createAuditState({ activeTab: harness.signals.signal('audit') });
  const actions = createAuditActions({ state, nodes: () => [self, forge, mac], fetchFor, fetchImpl: async (url) => { calls.push(['self', url]); return page([{ id: 1, at: '2026-10-18T10:05:00Z' }, { id: 2, at: '2026-10-18T09:00:00Z' }]); } });
  state.setNode('inst_forge'); assert.equal(await actions.load(), true);
  assert.deepEqual(calls.map(([id]) => id), ['inst_forge'], "one peer's log, through its routes");
  state.setNode('inst_mac0'); assert.equal(await actions.load(), false);
  assert.match(state.view.value.request.error, /mac does not share its audit log with you \(needs federation\.audit\.read\)/);
  calls.length = 0; state.setNode('all'); assert.equal(await actions.load(), true);
  const view = state.view.value;
  assert.deepEqual(view.rows.map((e) => `${e.node.name}#${e.id}`), ['desk#1', 'forge#7', 'desk#2'], 'merged newest first, each tagged with its node');
  assert.equal(view.pages, 1, 'the merged view does not page');
  assert.deepEqual(view.response.refused.map((r) => r.node.name), ['mac']);
});
