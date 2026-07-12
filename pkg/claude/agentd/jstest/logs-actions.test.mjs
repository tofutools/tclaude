import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('Logs actions preserve the API and reject overlapping stale responses', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createLogsState }, { createLogsActions }] = await Promise.all([
    harness.importDashboardModule('js/logs-state.js'), harness.importDashboardModule('js/logs-actions.js'),
  ]);
  const state = createLogsState({ activeTab: harness.signals.signal('logs') });
  state.setFilter('query', 'panic'); state.setFilter('includeRotated', true);
  const pending = [];
  const fetchImpl = (url, options) => new Promise((resolve) => pending.push({ url, options, resolve }));
  const actions = createLogsActions({ state, fetchImpl, now: () => 10000 });
  const old = actions.load(); const fresh = actions.load();
  assert.match(pending[0].url, /^\/api\/logs\?page=1&page_size=100&q=panic&include_rotated=1$/);
  assert.deepEqual(pending[0].options, { credentials: 'same-origin' });
  pending[1].resolve({ ok: true, json: async () => ({ entries: [{ msg: 'fresh' }], total: 1, total_unfiltered: 1 }) });
  assert.equal(await fresh, true);
  pending[0].resolve({ ok: true, json: async () => ({ entries: [{ msg: 'old' }], total: 1, total_unfiltered: 1 }) });
  assert.equal(await old, false);
  assert.equal(state.view.value.rows[0].row.msg, 'fresh');
});

test('Logs actions expose HTTP failures', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createLogsState }, { createLogsActions }] = await Promise.all([
    harness.importDashboardModule('js/logs-state.js'), harness.importDashboardModule('js/logs-actions.js'),
  ]);
  const state = createLogsState({ activeTab: harness.signals.signal('logs') });
  const actions = createLogsActions({ state, fetchImpl: async () => ({ ok: false, status: 503, text: async () => 'offline' }) });
  assert.equal(await actions.load(), false);
  assert.match(state.view.value.request.error, /offline/);
});
