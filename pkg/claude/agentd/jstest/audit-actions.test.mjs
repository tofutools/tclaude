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
