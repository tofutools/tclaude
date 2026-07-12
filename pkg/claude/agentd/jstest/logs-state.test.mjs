import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('Logs state owns filters, paging, stream state, and stale request rejection', async (t) => {
  const harness = await createPreactHarness(t);
  const { createLogsState } = await harness.importDashboardModule('js/logs-state.js');
  const activeTab = harness.signals.signal('logs');
  const state = createLogsState({ activeTab });
  state.setFilter('query', 'error'); state.setFilter('level', 'warn'); state.setPageSize(50); state.setStream(true);
  assert.equal(state.view.value.page, 1);
  assert.equal(state.view.value.query, 'error');
  const old = state.beginRequest(); const fresh = state.beginRequest();
  assert.equal(state.commitRequest(old, { entries: [{ msg: 'old' }], total: 1, total_unfiltered: 1 }), false);
  assert.equal(state.commitRequest(fresh, { entries: [{ msg: 'new' }], total: 1, total_unfiltered: 2, page: 1, page_size: 50 }), true);
  assert.equal(state.view.value.rows[0].row.msg, 'new');
  const failed = state.beginRequest();
  assert.equal(state.failRequest(failed, Object.assign(new Error('HTTP 500'), { body: 'boom' })), true);
  assert.match(state.view.value.request.error, /boom/);
  activeTab.value = 'audit';
  assert.equal(state.view.value.active, false);
});
