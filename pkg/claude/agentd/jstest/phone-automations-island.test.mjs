// js/phone-automations-island.js: Automations on a phone, over the real jobs
// state and recorded actions, driven through what the operator taps.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

function prefs() {
  const values = new Map();
  return {
    getItem: (key) => values.get(key) ?? null,
    setItem: (key, value) => values.set(key, String(value)),
    removeItem: (key) => values.delete(key),
  };
}

const CRON = {
  id: 1, name: 'standup', enabled: true, target_kind: 'group', group_name: 'alpha', group_id: 4,
  owner_label: 'lead', last_run_status: 'ok', last_run_at: '2026-07-11T10:00:00Z',
  cron_expr: '0 9 * * 1-5', cron_desc: 'At 09:00, Monday through Friday', subject: 'Standup', body: 'Post your standup',
  next_due_at: '2026-07-12T09:00:00Z',
};

function page({ cron = CRON } = {}) {
  return {
    jobs: [
      { kind: 'cron', cron },
      { kind: 'export', export: { id: 2, title: 'Agent export', conv_label: 'worker', status: 'ready', ready: true, created_at: '2026-07-11T11:00:00Z' } },
      { kind: 'standing-order', order: {
        id: 3, name: 'pr-early', enabled: false, disabled_reason: 'retired target',
        target: { kind: 'group', group_id: 7, group_name: 'tclaude', role: 'worker' },
        summary: 'Push the PR early.', trigger: { event: 'session.start', label: 'session start' },
        last_evaluation: { at: '2026-07-11T12:00:00Z', outcome: 'delivered' },
      } },
    ],
    paging: { jobs: { offset: 0, limit: 25, total: 3, total_unfiltered: 3 } },
  };
}

async function setup(t, { kind = 'all', rules = [] } = {}) {
  const harness = await createPreactHarness(t);
  const [island, { createJobsState }, { phonePage }] = await Promise.all([
    harness.importDashboardModule('js/phone-automations-island.js'),
    harness.importDashboardModule('js/jobs-state.js'),
    harness.importDashboardModule('js/phone-page.js'),
  ]);
  const { signal } = harness.signals;
  const snapshot = signal(null);
  const state = createJobsState({ snapshot, prefs: prefs() });
  state.initialize();
  state.setKind(kind);
  snapshot.value = page();
  state.beginRequest(1);
  state.commitRequest(1);
  const calls = [];
  const record = (name, result = true) => (...args) => { calls.push([name, ...args]); return Promise.resolve(result); };
  const actions = {
    refresh: record('refresh'), runCron: record('runCron'), toggleCron: record('toggleCron'),
    toggleStandingOrder: record('toggleStandingOrder', false), downloadExport: record('downloadExport'),
    dismissExport: record('dismissExport'), toggleTrigger: record('toggleTrigger'),
    loadTriggers: async () => { calls.push(['loadTriggers']); return rules; },
    loadCronLogs: async (id) => { calls.push(['loadCronLogs', id]); return [{ id: 9, fired_at: '2026-07-11T10:00:00Z', status: 'failed', error_msg: 'agent offline' }]; },
    loadTriggerFirings: async () => [],
  };
  const activeTab = signal('jobs');
  const context = { context: signal(null) };
  const mounted = await harness.mount(harness.html`<${island.PhoneJobsApp} state=${state} actions=${actions}
    activeTab=${activeTab} context=${context} />`);
  const root = mounted.container;
  const $ = (selector) => root.querySelector(selector);
  const $$ = (selector) => [...root.querySelectorAll(selector)];
  const tap = (element) => harness.act(() => element.click());
  // flip turns a switch as a finger does: the box changes, then 'change'.
  const flip = (box) => harness.act(() => { box.checked = !box.checked; harness.fireEvent(box, 'change'); });
  const settle = async () => {
    for (let i = 0; i < 4; i++) await harness.act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)); });
  };
  return { harness, island, state, snapshot, actions, calls, activeTab, phonePage, root, $, $$, tap, flip, settle };
}

test('the row helpers say when, where to and how it last went', async (t) => {
  const { island } = await setup(t);
  assert.equal(island.cronSchedule(CRON), 'At 09:00, Monday through Friday');
  assert.equal(island.cronSchedule({ cron_expr: '*/5 * * * *' }), 'cron */5 * * * *');
  assert.equal(island.cronSchedule({ interval_seconds: 1800 }), 'every 30m');
  assert.match(island.cronTarget(CRON), /alpha/);
  assert.equal(island.cronTarget({ target_conv: 'c1', target_label: 'reviewer' }), 'reviewer');
  assert.equal(island.cronOutcome({ enabled: true }).text, 'never run');
  assert.equal(island.cronOutcome({ ...CRON, last_run_status: 'failed' }).tone, 'bad');
  assert.match(island.cronOutcome({ ...CRON, enabled: false }).text, /^off · ✓ ok/);
  assert.equal(island.orderTarget({ target: { kind: 'global' } }), 'every agent');
  assert.equal(island.orderOutcome({ enabled: true, last_evaluation: { outcome: 'delivered' } }).tone, 'ok');
  const rows = page().jobs;
  const fresh = { ...CRON, enabled: false };
  assert.equal(island.openedItem({ kind: 'cron', item: CRON }, [{ kind: 'cron', cron: fresh }], []), fresh, 'the listed copy wins');
  assert.equal(island.openedItem({ kind: 'cron', item: { id: 99 } }, rows, []).id, 99, 'one that left the list stays as seen');
});

test('the list shows one row per automation with its switch, and Run now on cron jobs', async (t) => {
  const { $$, calls, tap, flip, settle } = await setup(t);
  const rows = $$('.phone-auto-row');
  assert.deepEqual(rows.map((row) => row.dataset.key), ['cron-1', 'export-2', 'standing-order-3']);
  const [cron, exported, order] = rows;
  assert.match(cron.querySelector('.phone-auto-sub').textContent, /Monday through Friday · → /);
  assert.ok(order.classList.contains('off'));
  assert.equal(exported.querySelector('[role="switch"]'), null, 'an export has no switch');
  assert.deepEqual([...exported.querySelectorAll('.ui-btn')].map((b) => b.textContent.trim()), ['Download', 'Dismiss']);

  await tap(cron.querySelector('.phone-auto-run'));
  assert.deepEqual(calls.at(-1), ['runCron', page().jobs[0].cron]);

  const cronSwitch = cron.querySelector('[role="switch"]');
  await flip(cronSwitch);
  await settle();
  assert.equal(calls.at(-1)[0], 'toggleCron');

  // A toggle the operator cancels (or the server refuses) puts the switch back.
  const orderSwitch = order.querySelector('[role="switch"]');
  await flip(orderSwitch);
  await settle();
  assert.equal(calls.at(-1)[0], 'toggleStandingOrder');
  assert.equal(orderSwitch.getAttribute('aria-label'), 'Turn on pr-early', 'the switch is back off');
});

test('a cron job opens as a page with its details and recent runs, and back returns to the list', async (t) => {
  const { $, phonePage, calls, tap, settle, snapshot, state, harness } = await setup(t);
  await tap($('.phone-auto-row[data-key="cron-1"] .phone-auto-open'));
  await settle();
  assert.equal($('.phone-auto-list'), null, 'the page replaces the list');
  assert.equal(phonePage.value.title, 'standup');
  assert.equal(phonePage.value.actions[0].icon, '▶ Run now');
  assert.match($('.phone-auto-facts').textContent, /0 9 \* \* 1-5/);
  assert.deepEqual(calls.find(([name]) => name === 'loadCronLogs'), ['loadCronLogs', 1]);
  assert.match($('.phone-auto-history').textContent, /agent offline/);

  // A poll that turns it off shows on the open page.
  await harness.act(() => {
    snapshot.value = page({ cron: { ...CRON, enabled: false } });
    state.beginRequest(2);
    state.commitRequest(2);
  });
  assert.equal($('.phone-auto-facts [role="switch"]').getAttribute('aria-label'), 'Turn on standup');

  await harness.act(() => phonePage.value.actions[0].onClick());
  assert.equal(calls.at(-1)[0], 'runCron');
  await harness.act(() => phonePage.value.onBack());
  assert.ok($('.phone-auto-list'));
  assert.equal(phonePage.value, null);
});

test('triggers load from their own collection and toggle in place', async (t) => {
  const rules = [{ id: 5, name: 'nudge', enabled: true, source: 'agent.awaiting_input', scope: 'all', actions: [], firings: [] }];
  const { $, $$, calls, flip, settle } = await setup(t, { kind: 'trigger', rules });
  await settle();
  assert.ok(calls.some(([name]) => name === 'loadTriggers'));
  assert.deepEqual($$('.phone-auto-row').map((row) => row.dataset.key), ['trigger-5']);
  assert.equal($('.phone-auto-search'), null, 'triggers have no server filter here');
  const loads = calls.filter(([name]) => name === 'loadTriggers').length;
  await flip($('.phone-auto-row [role="switch"]'));
  await settle();
  assert.equal(calls.filter(([name]) => name === 'toggleTrigger').length, 1);
  assert.equal(calls.filter(([name]) => name === 'loadTriggers').length, loads + 1, 'a toggle reads the rules again');
});

test('leaving the tab closes an open page', async (t) => {
  const { $, activeTab, phonePage, tap, harness } = await setup(t);
  await tap($('.phone-auto-row[data-key="cron-1"] .phone-auto-open'));
  assert.ok(phonePage.value);
  await harness.act(() => { activeTab.value = 'groups'; });
  assert.equal(phonePage.value, null);
  await harness.act(() => { activeTab.value = 'jobs'; });
  assert.ok($('.phone-auto-list'), 'coming back shows the list');
});
