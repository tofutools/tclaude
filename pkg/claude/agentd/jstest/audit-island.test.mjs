import test from 'node:test'; import assert from 'node:assert/strict'; import { createPreactHarness, getByRole } from './preact-harness.mjs';
const entry = (id, detail = 'copy me') => ({ id, at: '2026-07-12T00:00:00Z', actor_kind: 'human', verb: 'agent.stop', group_name: 'team', target_label: 'Worker', detail, status: 200, source: 'dashboard' });
const payload = (entries) => ({ entries, total: entries.length, total_unfiltered: entries.length, page: 1, page_size: 100, sort: 'time', dir: 'desc', pruning_on: true, retention_days: 30 });
test('Audit island filters/sorts and preserves keyed row focus across refreshes', async (t) => {
  const harness = await createPreactHarness(t); const [{ createAuditState }, { AuditApp }] = await Promise.all([harness.importDashboardModule('js/audit-state.js'), harness.importDashboardModule('js/audit-island.js')]);
  const state = createAuditState({ activeTab: harness.signals.signal('audit') }); let token = state.beginRequest(); state.commitRequest(token, payload([entry(1)])); const calls = []; const actions = { load: async () => calls.push('load') };
  const mounted = await harness.mount(harness.html`<${AuditApp} state=${state} actions=${actions} />`); const row = mounted.container.querySelector('tr[data-key="audit-1"]'); row.tabIndex = 0; row.focus();
  await harness.act(() => { token = state.beginRequest(); state.commitRequest(token, payload([entry(2), entry(1)])); }); assert.equal(mounted.container.querySelector('tr[data-key="audit-1"]'), row); assert.equal(harness.document.activeElement, row);
  const search = getByRole(mounted.container, 'textbox', { name: 'Search audit events' }); await harness.input(search, 'deny'); assert.equal(state.view.value.query, 'deny');
  await harness.act(() => harness.fireEvent(getByRole(mounted.container, 'button', { name: 'Clear audit search' }), 'click')); assert.equal(state.view.value.query, ''); assert.equal(harness.document.activeElement, search); assert.equal(mounted.container.querySelector('#filter-audit-count').getAttribute('aria-live'), 'polite');
  const actor = [...mounted.container.querySelectorAll('th')].find((th) => th.textContent.includes('Actor')); await harness.act(() => harness.fireEvent(actor, 'click')); assert.equal(state.view.value.sort, 'actor');
  assert.match(mounted.container.querySelector('#audit-retention').textContent, /keeping 30 days/); await mounted.unmount();
});
test('Audit snapshot listener exists only while active and obeys the 30s gate', async (t) => {
  const harness = await createPreactHarness(t); const [{ createAuditState }, { AuditApp }] = await Promise.all([harness.importDashboardModule('js/audit-state.js'), harness.importDashboardModule('js/audit-island.js')]);
  let now = 0; const activeTab = harness.signals.signal('audit'); const state = createAuditState({ activeTab, now: () => now }); const calls = []; const mounted = await harness.mount(harness.html`<${AuditApp} state=${state} actions=${{ load: () => { calls.push(now); state.beginRequest(); } }} />`);
  await harness.act(() => harness.document.dispatchEvent(new harness.window.CustomEvent('tclaude:tab-reselected', { detail: { tab: 'audit' } }))); assert.equal(calls.length, 2, 'reselecting active Audit forces a refresh');
  now = 10000; await harness.act(() => harness.document.dispatchEvent(new harness.window.CustomEvent('tclaude:snapshot'))); assert.equal(calls.length, 2);
  now = 31001; await harness.act(() => harness.document.dispatchEvent(new harness.window.CustomEvent('tclaude:snapshot'))); assert.equal(calls.length, 3);
  await harness.act(() => { activeTab.value = 'logs'; }); now = 70000; await harness.act(() => harness.document.dispatchEvent(new harness.window.CustomEvent('tclaude:snapshot'))); assert.equal(calls.length, 3);
  await mounted.unmount();
});
test('Audit island exposes errors and production cleanup', async (t) => {
  const harness = await createPreactHarness(t); const [{ createAuditState }, { AuditApp }] = await Promise.all([harness.importDashboardModule('js/audit-state.js'), harness.importDashboardModule('js/audit-island.js')]);
  const state = createAuditState({ activeTab: harness.signals.signal('logs') }); const token = state.beginRequest(); state.failRequest(token, new Error('offline')); const mounted = await harness.mount(harness.html`<${AuditApp} state=${state} actions=${{ load() {} }} />`); assert.match(getByRole(mounted.container, 'alert').textContent, /offline/); await mounted.unmount();
  const host = harness.document.body.appendChild(harness.document.createElement('div')); host.id = 'audit-root'; const { mountAuditFeature } = await harness.importDashboardModule('js/preact-loader.js'); const cleanup = await mountAuditFeature({ fetchImpl: async () => { throw new Error('inactive must not fetch'); } }); assert.ok(host.querySelector('#filter-audit')); cleanup(); assert.equal(host.childElementCount, 0);
});
