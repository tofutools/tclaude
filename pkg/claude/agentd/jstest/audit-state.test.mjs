import test from 'node:test'; import assert from 'node:assert/strict'; import { createPreactHarness } from './preact-harness.mjs';
test('Audit state owns filters, sorting, paging, refresh gate, and stale requests', async (t) => {
  const harness = await createPreactHarness(t); const { createAuditState } = await harness.importDashboardModule('js/audit-state.js');
  let now = 1000; const activeTab = harness.signals.signal('audit'); const state = createAuditState({ activeTab, now: () => now });
  state.setFilter('query', 'deny'); state.setFilter('source', 'popup'); state.cycleSort('actor');
  assert.equal(state.view.value.sort, 'actor'); assert.equal(state.view.value.dir, 'asc'); assert.equal(state.view.value.page, 1);
  const old = state.beginRequest(); now = 2000; const fresh = state.beginRequest();
  assert.equal(state.commitRequest(old, { entries: [{ id: 1 }], total: 1, total_unfiltered: 1 }), false);
  assert.equal(state.commitRequest(fresh, { entries: [{ id: 2 }], total: 1, total_unfiltered: 2, page: 1, page_size: 50, sort: 'actor', dir: 'asc' }), true);
  assert.equal(state.view.value.rows[0].id, 2); assert.equal(state.refreshDue(30000, 31000), false); assert.equal(state.refreshDue(30000, 32001), true);
  const failure = state.beginRequest(); state.failRequest(failure, Object.assign(new Error('HTTP 500'), { body: 'boom' }));
  assert.match(state.view.value.request.error, /boom/); assert.equal(state.view.value.response, null);
});
