// js/phone-diagnostics.js: Diagnostics › Audit and Logs on a phone, as
// two-line rows over the desktop's state, with one search and one filter.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const audit = (id, extra = {}) => ({
  id, at: '2026-07-12T09:41:00Z', actor_kind: 'agent', actor_label: 'lead', verb: 'spawn', group_name: 'tclaude',
  target_label: 'reviewer-3', detail: 'cli', status: 201, source: 'cli', ...extra,
});
const auditPage = (entries, total = entries.length) => ({ entries, total, total_unfiltered: 40, page: 1, page_size: 100, sort: 'time', dir: 'desc' });

test('audit: two-line rows, the outcome on the right, a tap opens the rest', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createAuditState }, { PhoneAudit, clock }] = await Promise.all([
    harness.importDashboardModule('js/audit-state.js'),
    harness.importDashboardModule('js/phone-diagnostics.js'),
  ]);
  const state = createAuditState({ activeTab: harness.signals.signal('diagnostics'), activitySubtab: harness.signals.signal('audit') });
  const token = state.beginRequest();
  state.commitRequest(token, auditPage([audit(1), audit(2, { actor_kind: 'human', verb: 'notify-human', status: 403, target_label: '', group_name: '', detail: '' })], 2));
  const loads = [];
  const mounted = await harness.mount(harness.html`<${PhoneAudit} state=${state} actions=${{ load: () => loads.push(state.view.value.outcome) }} />`);
  const c = mounted.container;
  assert.equal(loads.length, 1, 'the shown view loads');
  const rows = [...c.querySelectorAll('.phone-diag-row')];
  assert.deepEqual(rows.map((row) => row.querySelector('.phone-diag-head').textContent), ['lead → spawn', 'operator → notify-human']);
  assert.equal(rows[0].querySelector('.phone-diag-sub').textContent, 'tclaude reviewer-3 · cli');
  assert.equal(rows[1].querySelector('.phone-diag-sub'), null, 'nothing to say on the second line');
  assert.deepEqual(rows.map((row) => row.querySelector('.phone-diag-outcome').textContent), ['ok', 'denied']);
  assert.match(rows[0].querySelector('.phone-diag-detail').textContent, /Targettclaude reviewer-3/);
  assert.equal(c.querySelector('.phone-diag-count').textContent, '2 of 40 events');

  const failed = [...c.querySelectorAll('button')].find((b) => b.textContent === 'Failed only');
  await harness.act(() => failed.click());
  assert.equal(state.view.value.outcome, 'failure');
  assert.equal(failed.getAttribute('aria-pressed'), 'true');
  assert.deepEqual(loads.at(-1), 'failure', 'the filter reloads');
  await harness.act(() => failed.click());
  assert.equal(state.view.value.outcome, '');

  await harness.input(c.querySelector('input[type=search]'), 'push');
  assert.equal(state.view.value.query, 'push');

  const now = new Date(2026, 6, 12, 18, 0);
  assert.equal(clock(new Date(2026, 6, 12, 9, 5).toISOString(), now), '09:05', 'today: the time');
  assert.notEqual(clock(new Date(2026, 6, 10, 9, 5).toISOString(), now), '09:05', 'before: the date');
  await mounted.unmount();
});

test('audit: pages and empty states', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createAuditState }, { PhoneAudit }] = await Promise.all([
    harness.importDashboardModule('js/audit-state.js'),
    harness.importDashboardModule('js/phone-diagnostics.js'),
  ]);
  const state = createAuditState({ activeTab: harness.signals.signal('diagnostics'), activitySubtab: harness.signals.signal('audit') });
  const token = state.beginRequest();
  state.commitRequest(token, { ...auditPage([audit(1)], 250), page_size: 100 });
  const pages = [];
  const mounted = await harness.mount(harness.html`<${PhoneAudit} state=${state} actions=${{ load: () => pages.push(state.view.value.page) }} />`);
  const button = (label) => [...mounted.container.querySelectorAll('button')].find((b) => b.textContent.includes(label));
  assert.equal(button('Newer').disabled, true);
  await harness.act(() => button('Older').click());
  assert.equal(pages.at(-1), 2);
  await harness.act(() => { const next = state.beginRequest(); state.commitRequest(next, { ...auditPage([], 0), total_unfiltered: 0 }); });
  assert.match(mounted.container.textContent, /No audit events recorded yet/);
  await mounted.unmount();
});

test('logs: the message over its fields, the level on the right, a level filter', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createLogsState }, { PhoneLogs }] = await Promise.all([
    harness.importDashboardModule('js/logs-state.js'),
    harness.importDashboardModule('js/phone-diagnostics.js'),
  ]);
  const state = createLogsState({ activeTab: harness.signals.signal('diagnostics'), activitySubtab: harness.signals.signal('logs') });
  const token = state.beginRequest();
  state.commitRequest(token, {
    entries: [{ key: 'a', time: '2026-07-12T09:41:00Z', level: 'WARN', msg: 'tmux pane gone, marking idle', fields: { conv: 'c_81', pane: '%14' } }],
    total: 1, total_unfiltered: 1, page: 1, page_size: 100,
  });
  const loads = [];
  const mounted = await harness.mount(harness.html`<${PhoneLogs} state=${state} actions=${{ load: () => loads.push(state.view.value.level) }} />`);
  const row = mounted.container.querySelector('.phone-diag-row');
  assert.equal(row.querySelector('.phone-diag-head').textContent, 'tmux pane gone, marking idle');
  assert.equal(row.querySelector('.phone-diag-sub').textContent, 'conv=c_81  pane=%14');
  assert.equal(row.querySelector('.phone-diag-outcome').textContent, 'warn');
  const level = mounted.container.querySelector('select');
  // LinkeDOM's select value is read-only.
  Object.defineProperty(level, 'value', { configurable: true, writable: true, value: 'error' });
  await harness.act(() => harness.fireEvent(level, 'change'));
  assert.equal(state.view.value.level, 'error');
  assert.equal(loads.at(-1), 'error');
  await mounted.unmount();
});
