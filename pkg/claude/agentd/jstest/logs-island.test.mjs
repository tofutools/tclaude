import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness, getByRole } from './preact-harness.mjs';

const payload = (entries) => ({ entries, page: 1, page_size: 100, total: entries.length, total_unfiltered: entries.length, path: '/tmp/output.log', sources: [{ path: '/tmp/output.log', name: 'output.log', lines: entries.length, bytes: 2048 }] });

test('Logs island renders controls and preserves duplicate row focus across tail updates', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createLogsState }, { LogsApp }] = await Promise.all([
    harness.importDashboardModule('js/logs-state.js'), harness.importDashboardModule('js/logs-island.js'),
  ]);
  const activeTab = harness.signals.signal('logs'); const state = createLogsState({ activeTab });
  const row = { time: '2026-07-12T00:00:00Z', level: 'INFO', msg: 'same', fields: { n: 1 } };
  const token = state.beginRequest(); state.commitRequest(token, payload([row]));
  const calls = []; const actions = { load: async () => calls.push('load') };
  const mounted = await harness.mount(harness.html`<${LogsApp} state=${state} actions=${actions} />`);
  const oldRow = mounted.container.querySelector('tbody tr'); oldRow.tabIndex = 0; oldRow.focus();
  await harness.act(() => { const next = state.beginRequest(); state.commitRequest(next, payload([row, row])); });
  const rows = mounted.container.querySelectorAll('tbody tr');
  assert.equal(rows[1], oldRow);
  assert.equal(harness.document.activeElement, oldRow);
  assert.equal(rows.length, 2);
  const search = getByRole(mounted.container, 'textbox', { name: 'Search logs' });
  await harness.input(search, 'panic');
  assert.equal(state.view.value.query, 'panic');
  await harness.act(() => harness.fireEvent(getByRole(mounted.container, 'button', { name: 'Clear log search' }), 'click'));
  assert.equal(state.view.value.query, '');
  assert.equal(harness.document.activeElement, search);
  assert.equal(mounted.container.querySelector('#filter-logs-count').getAttribute('aria-live'), 'polite');
  assert.match(mounted.container.querySelector('#logs-status').textContent, /2 lines/);
  await mounted.unmount();
});

test('Logs streaming timer exists only while active and cleans up on unmount', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createLogsState }, { LogsApp }] = await Promise.all([
    harness.importDashboardModule('js/logs-state.js'), harness.importDashboardModule('js/logs-island.js'),
  ]);
  const realSet = globalThis.setInterval; const realClear = globalThis.clearInterval;
  const active = new Set(); let next = 1;
  globalThis.setInterval = () => { const id = next++; active.add(id); return id; };
  globalThis.clearInterval = (id) => active.delete(id);
  t.after(() => { globalThis.setInterval = realSet; globalThis.clearInterval = realClear; });
  const activeTab = harness.signals.signal('logs'); const state = createLogsState({ activeTab }); state.setStream(true);
  const mounted = await harness.mount(harness.html`<${LogsApp} state=${state} actions=${{ load: async () => {} }} />`);
  assert.equal(active.size, 1);
  await harness.act(() => { activeTab.value = 'audit'; });
  assert.equal(active.size, 0, 'inactive Logs tab owns no streaming timer');
  await harness.act(() => { activeTab.value = 'logs'; });
  assert.equal(active.size, 1);
  await mounted.unmount();
  assert.equal(active.size, 0, 'unmount clears streaming timer');
});

test('Logs island exposes errors, empty results, and production cleanup', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createLogsState }, { LogsApp }] = await Promise.all([
    harness.importDashboardModule('js/logs-state.js'), harness.importDashboardModule('js/logs-island.js'),
  ]);
  const state = createLogsState({ activeTab: harness.signals.signal('audit') });
  const failure = state.beginRequest(); state.failRequest(failure, new Error('offline'));
  const mounted = await harness.mount(harness.html`<${LogsApp} state=${state} actions=${{ load: async () => {} }} />`);
  assert.match(getByRole(mounted.container, 'alert').textContent, /offline/);
  await mounted.unmount();
  const host = harness.document.body.appendChild(harness.document.createElement('div')); host.id = 'logs-root';
  const { mountLogsFeature } = await harness.importDashboardModule('js/preact-loader.js');
  const cleanup = await mountLogsFeature({ fetchImpl: async () => { throw new Error('inactive feature must not fetch'); } });
  assert.ok(host.querySelector('#filter-logs'));
  cleanup(); assert.equal(host.childElementCount, 0);
});
