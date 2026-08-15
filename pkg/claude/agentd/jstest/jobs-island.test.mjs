import test from 'node:test';
import assert from 'node:assert/strict';
import { assertSameNode } from './assertions.mjs';
import { createPreactHarness, getByRole } from './preact-harness.mjs';

function prefs() {
  const values = new Map();
  return {
    getItem: (key) => values.get(key) ?? null,
    setItem: (key, value) => values.set(key, String(value)),
    removeItem: (key) => values.delete(key),
  };
}

function page(name = 'Daily summary') {
  return {
    export_jobs_active: 1,
    jobs: [
      { kind: 'cron', cron: {
        id: 1, name, enabled: true, target_kind: 'group', group_name: 'alpha',
        owner_label: 'Johan', last_run_status: 'ok', last_run_at: '2026-07-11T10:00:00Z',
        interval_seconds: 300, subject: 'Status', body: 'Send status',
      } },
      { kind: 'export', export: {
        id: 2, title: 'Agent export', conv_label: 'worker', conv_id: 'conv-2',
        status: 'ready', ready: true, artifact_name: 'summary.md', artifact_size: 2048,
        created_at: '2026-07-11T11:00:00Z',
      } },
      { kind: 'standing-order', order: {
        id: 3, name: 'pr-early', revision: 2, enabled: true, operator_authored: true,
        target: { kind: 'group', group_id: 7, group_name: 'tclaude', role: 'worker' },
        summary: 'Push the PR early.', trigger: { event: 'session.start', label: 'session start (compact)' },
        timing: 'same-continuation', cadence: 'once-per-generation',
        capability: { status: 'degraded', transport: 'hook-context', detail: 'OpenCode uses a queued turn.' },
        last_evaluation: {
          at: '2026-07-11T12:00:00Z', outcome: 'not-evaluated-trimmed',
          problem: true, detail: 'Tool input was trimmed.',
        },
      } },
    ],
    paging: { jobs: { offset: 0, limit: 50, total: 3, total_unfiltered: 3 } },
  };
}

test('Jobs island renders reactively and preserves keyed DOM/focus across polls', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createJobsState }, { JobsApp, JobsBadge }] = await Promise.all([
    harness.importDashboardModule('js/jobs-state.js'),
    harness.importDashboardModule('js/jobs-island.js'),
  ]);
  const snapshot = harness.signals.signal(null);
  const state = createJobsState({ snapshot, prefs: prefs() });
  state.initialize();
  snapshot.value = page();
  state.beginRequest(1);
  state.commitRequest(1);
  const calls = [];
  const actions = {
    refresh: async () => { calls.push('refresh'); },
    openCronCreate: () => calls.push('create'), openCronEdit: () => calls.push('edit'),
    openCronDuplicate: () => calls.push('duplicate'),
    runCron: () => calls.push('run'), toggleCron: () => calls.push('toggle'),
    deleteCron: () => calls.push('delete'), downloadExport: () => calls.push('download'),
    dismissExport: () => calls.push('dismiss'),
    openStandingOrderCreate: () => calls.push('order-create'),
    openStandingOrderEdit: () => calls.push('order-edit'),
    toggleStandingOrder: () => calls.push('order-toggle'),
    deleteStandingOrder: () => calls.push('order-delete'),
  };
  const mounted = await harness.mount(harness.html`<${JobsApp} state=${state} actions=${actions} />`);
  const badge = await harness.mount(harness.html`<${JobsBadge} state=${state} />`);
  assert.equal(badge.container.querySelector('#jobs-badge').textContent, '1');
  assert.equal(badge.container.querySelector('#jobs-badge').hidden, false);

  const filter = getByRole(mounted.container, 'textbox', { name: 'Filter automations' });
  assert.equal(filter.value, '');
  let navigated = null;
  harness.document.addEventListener('tclaude:navigated', (event) => { navigated = event.detail?.location; });
  const standingOrders = getByRole(mounted.container, 'tab', { name: 'Standing orders' });
  assert.equal(standingOrders.getAttribute('href'), '/automations/standing-orders');
  await harness.act(() => harness.fireEvent(standingOrders, 'click', { button: 0 }));
  assert.equal(state.kind.value, 'standing-order');
  assert.deepEqual(navigated, { tab: 'jobs', subtab: 'standing-orders' });
  assert.match(state.params.value, /kind=standing-order/);
  assert.equal(standingOrders.getAttribute('aria-selected'), 'true');
  assert.ok(calls.includes('refresh'));
  const allJobs = getByRole(mounted.container, 'tab', { name: 'All' });
  assert.equal(allJobs.getAttribute('href'), '/automations');
  await harness.act(() => harness.fireEvent(allJobs, 'click', { button: 0 }));
  assert.equal(state.kind.value, 'all');
  const cronJobs = getByRole(mounted.container, 'tab', { name: 'Cron jobs' });
  const space = harness.fireEvent(cronJobs, 'keydown', { key: ' ' });
  assert.equal(space.defaultPrevented, true, 'Space is handled like the former native button');
  assert.equal(state.kind.value, 'cron');
  await harness.act(() => harness.fireEvent(allJobs, 'click', { button: 0 }));
  assert.equal(state.kind.value, 'all');
  const cronRow = mounted.container.querySelector('tr[data-key="cron-1"]');
  const orderRow = mounted.container.querySelector('tr[data-key="standing-order-3"]');
  assert.match(orderRow.textContent, /pr-early/);
  assert.match(orderRow.textContent, /not-evaluated-trimmed/);
  assert.ok(orderRow.querySelector('.state-error'), 'trimmed evaluation is visually distinct');
  await harness.act(() => harness.fireEvent(getByRole(orderRow, 'button', { name: 'edit' }), 'click'));
  await harness.act(() => harness.fireEvent(getByRole(orderRow, 'button', { name: 'disable' }), 'click'));
  assert.ok(calls.includes('order-edit'));
  assert.ok(calls.includes('order-toggle'));
  const edit = getByRole(cronRow, 'button', { name: 'edit' });
  edit.focus();
  const selectedTextNode = cronRow.querySelector('.rowname').firstChild;

  await harness.act(() => {
    snapshot.value = page('Daily summary');
    state.beginRequest(2);
    state.commitRequest(2);
  });
  assertSameNode(mounted.container.querySelector('tr[data-key="cron-1"]'), cronRow);
  assertSameNode(cronRow.querySelector('.rowname').firstChild, selectedTextNode,
    'unchanged text node remains a valid browser-selection anchor');
  assertSameNode(harness.document.activeElement, edit);

  await harness.act(() => {
    snapshot.value = {
      ...page('Daily summary'),
      paging: { jobs: { offset: 0, limit: 50, total: 60, total_unfiltered: 60 } },
    };
  });
  const nextPage = getByRole(mounted.container, 'button', { name: 'Next page' });
  await harness.act(() => harness.fireEvent(nextPage, 'click'));
  assert.equal(state.offset.value, 50);
  assert.ok(calls.includes('refresh'));

  await harness.act(() => harness.fireEvent(edit, 'click'));
  assert.ok(calls.includes('edit'));
  const duplicate = getByRole(cronRow, 'button', { name: 'duplicate' });
  await harness.act(() => harness.fireEvent(duplicate, 'click'));
  assert.ok(calls.includes('duplicate'));
  const kindHeader = [...mounted.container.querySelectorAll('th')]
    .find((header) => header.textContent.includes('Kind'));
  await harness.act(() => harness.fireEvent(kindHeader, 'keydown', { key: 'Enter' }));
  assert.equal(kindHeader.getAttribute('aria-sort'), 'ascending');

  await harness.input(filter, 'cron');
  assert.equal(state.query.value, 'cron');
  await new Promise((resolve) => setTimeout(resolve, 275));
  assert.ok(calls.includes('refresh'));

  await harness.act(() => harness.fireEvent(nextPage, 'click'));
  assert.equal(state.offset.value, 50, 'failed successor request targets the next page');
  await harness.act(() => {
    state.beginRequest(3);
    state.failRequest(3, new Error('network down'));
  });
  assert.match(getByRole(mounted.container, 'alert').textContent, /network down/);
  assert.equal(mounted.container.querySelectorAll('tbody tr').length, 3, 'stale page remains visible');
  assert.equal(nextPage.disabled, true, 'stale-page navigation is disabled until Retry succeeds');
  const retry = getByRole(mounted.container, 'button', { name: 'Retry' });
  const refreshesBeforeRetry = calls.filter((call) => call === 'refresh').length;
  await harness.act(() => harness.fireEvent(retry, 'click'));
  assert.equal(calls.filter((call) => call === 'refresh').length, refreshesBeforeRetry + 1);
  assert.equal(nextPage.disabled, true, 'pager stays inert while displayed and requested pages differ');
  assert.match(state.params.value, /offset=50/, 'Retry keeps targeting the failed requested page');
  await badge.unmount();
  await mounted.unmount();
});

test('Standing-order target renders from the stable agent without a live conversation', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createJobsState }, { JobsApp }] = await Promise.all([
    harness.importDashboardModule('js/jobs-state.js'),
    harness.importDashboardModule('js/jobs-island.js'),
  ]);
  const snapshot = harness.signals.signal(null);
  const state = createJobsState({ snapshot, prefs: prefs() });
  state.initialize();
  const data = page();
  data.jobs[2].order.target = { kind: 'conv', agent: 'agt_persistent', conv: '' };
  snapshot.value = data;
  state.beginRequest(1);
  state.commitRequest(1);

  const actions = {
    refresh: () => {}, openCronCreate: () => {}, openCronEdit: () => {}, openCronDuplicate: () => {}, runCron: () => {},
    toggleCron: () => {}, deleteCron: () => {}, downloadExport: () => {}, dismissExport: () => {},
  };
  const mounted = await harness.mount(harness.html`<${JobsApp} state=${state} actions=${actions} />`);
  const row = mounted.container.querySelector('tr[data-key="standing-order-3"]');
  assert.match(row.textContent, /agt_persiste/);
  assert.doesNotMatch(row.textContent, /no target/);
  await mounted.unmount();
});

test('Jobs island exposes loading, empty, badge, and retry states', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createJobsState }, { JobsApp }] = await Promise.all([
    harness.importDashboardModule('js/jobs-state.js'),
    harness.importDashboardModule('js/jobs-island.js'),
  ]);
  const snapshot = harness.signals.signal(null);
  const state = createJobsState({ snapshot, prefs: prefs() });
  state.initialize();
  state.beginRequest(1);
  const actions = {
    refresh: () => {}, openCronCreate: () => {}, openCronEdit: () => {}, openCronDuplicate: () => {}, runCron: () => {},
    toggleCron: () => {}, deleteCron: () => {}, downloadExport: () => {}, dismissExport: () => {},
  };
  const mounted = await harness.mount(harness.html`<${JobsApp} state=${state} actions=${actions} />`);
  assert.match(mounted.container.textContent, /Loading automations/);

  await harness.act(() => state.failRequest(1, new Error('offline')));
  assert.match(getByRole(mounted.container, 'alert').textContent, /offline/);
  assert.doesNotMatch(mounted.container.textContent, /No exports, cron jobs, or standing orders yet/,
    'a failed first load is not an empty result');

  state.beginRequest(2);

  await harness.act(() => {
    snapshot.value = { jobs: [], export_jobs_active: 0, paging: { jobs: { total: 0, total_unfiltered: 0 } } };
    state.commitRequest(2);
  });
  assert.match(mounted.container.textContent, /No exports, cron jobs, or standing orders yet/);
  assert.equal(mounted.container.querySelector('#filter-jobs-count').textContent, '0 items');
  await mounted.unmount();
});

test('production loader dynamically mounts and unmounts the Jobs feature graph', async (t) => {
  const harness = await createPreactHarness(t);
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  host.id = 'jobs-root';
  const badgeHost = harness.document.body.appendChild(harness.document.createElement('span'));
  badgeHost.id = 'jobs-badge-root';
  const dialogHost = harness.document.body.appendChild(harness.document.createElement('div'));
  dialogHost.id = 'jobs-cron-dialog-root';
  let refreshes = 0;
  const [{ mountJobsFeature }, { openCronCreateModal }, { jobsState }] = await Promise.all([
    harness.importDashboardModule('js/preact-loader.js'),
    harness.importDashboardModule('js/jobs-controller.js'),
    harness.importDashboardModule('js/jobs-state.js'),
  ]);
  const cleanup = await mountJobsFeature({
    requestMutation: async () => {}, refresh: async () => { refreshes += 1; }, confirm: async () => true,
    notify: () => {}, download: () => {}, confirmDiscard: async () => true,
  });
  assert.equal(typeof cleanup, 'function');
  assert.ok(host.querySelector('#filter-jobs'));
  assert.ok(badgeHost.querySelector('#jobs-badge'));
  assert.equal(dialogHost.childElementCount, 0);
  await harness.act(() => harness.document.dispatchEvent(new harness.window.CustomEvent(
    'tclaude:restore-location',
    { detail: { location: { tab: 'jobs', subtab: 'standing-orders' } } },
  )));
  assert.equal(jobsState.kind.value, 'standing-order',
    'a deep-link/history restore selects the matching Automations view');
  assert.equal(refreshes, 1, 'restoring a different view refreshes its server-side window');
  await harness.act(() => openCronCreateModal({
    targetMode: 'solo', target: 'agt_one', interval: '5m', body: 'hello',
  }));
  assert.ok(dialogHost.querySelector('#cron-create-modal'), 'launcher-only controller opens the Jobs-owned dialog');
  cleanup();
  assert.equal(host.childElementCount, 0);
  assert.equal(badgeHost.childElementCount, 0);
  assert.equal(dialogHost.childElementCount, 0);
});

test('Triggers sub-view renders rule summaries and expands the firing inspector', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createJobsState }, { JobsApp }] = await Promise.all([
    harness.importDashboardModule('js/jobs-state.js'),
    harness.importDashboardModule('js/jobs-island.js'),
  ]);
  const snapshot = harness.signals.signal({ ...page(), groups: [{ id: 2, name: 'alpha' }] });
  const state = createJobsState({ snapshot, prefs: prefs() });
  state.initialize();
  state.beginRequest(1);
  state.commitRequest(1);
  const rule = {
    id: 7, name: 'review new PRs', row_version: 3, enabled: true,
    operator_authored: true, scope: 'group', group: 'alpha', source: 'pr.opened',
    author_is_agent: false, draft_filter: 'only', debounce_seconds: 60, cooldown_seconds: 300,
    actions: [{ type: 'message', message: { target: 'pr.author_agent', body_template: 'Review {{pr.url}}' } }],
  };
  const firing = {
    id: 9, outcome: 'partial_failure', detail: 'one action denied', event_ref: 'pr.opened:agt_author:https://example/pr/2',
    started_at: '2026-07-11T11:00:00Z', finished_at: '2026-07-11T11:00:01Z',
    actions: [
      { id: 10, action_type: 'message', outcome: 'permission_denied', detail: 'message.send not held' },
      { id: 11, action_type: 'spawn', outcome: 'max_live_workers', detail: 'one worker still active' },
    ],
  };
  let toggleSucceeds = false;
  const actions = {
    refresh: async () => {}, loadTriggers: async () => [{ ...rule, firings: [firing] }],
    loadTriggerFirings: async () => [firing], toggleTrigger: async () => toggleSucceeds,
    deleteTrigger: async () => true, openTriggerCreate: state.openTriggerCreate,
  };
  const mounted = await harness.mount(harness.html`<${JobsApp} state=${state} actions=${actions} />`);
  const tab = getByRole(mounted.container, 'tab', { name: 'Triggers' });
  assert.equal(tab.getAttribute('href'), '/automations/triggers');
  await harness.act(() => harness.fireEvent(tab, 'click', { button: 0 }));
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 0)));
  assert.equal(state.kind.value, 'trigger');
  assert.doesNotMatch(state.params.value, /kind=trigger/, 'the separate trigger collection never leaks into /api/jobs');
  const row = mounted.container.querySelector('tr[data-key="trigger-7"]');
  assert.match(row.textContent, /review new PRs/);
  assert.match(row.textContent, /group:alpha/);
  const open = harness.fireEvent(row, 'keydown', { key: 'Enter' });
  assert.equal(open.defaultPrevented, true);
  await harness.act(() => Promise.resolve());
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 0)));
  assert.match(mounted.container.querySelector('.trigger-inspector').textContent, /permission_denied/);
  assert.match(mounted.container.querySelector('.trigger-inspector').textContent, /message.send not held/);
  assert.equal(mounted.container.querySelectorAll('.trigger-verdicts .trigger-fail').length, 0,
    'valid configured alternatives are never presented as failed event facts');
  assert.equal(mounted.container.querySelectorAll('.trigger-action-outcome .trigger-fail').length, 2,
    'permission and concurrency failures are both visibly failures');
  const enabled = row.querySelector('input[type="checkbox"]');
  enabled.checked = false;
  await harness.act(() => harness.fireEvent(enabled, 'change'));
  assert.equal(enabled.checked, true, 'failed toggle rolls the native control back to server state');
  toggleSucceeds = true;
  await harness.act(() => harness.fireEvent(getByRole(mounted.container, 'button', { name: '+ new trigger' }), 'click'));
  assert.equal(state.triggerDialog.value.kind, 'create');
  await mounted.unmount();
});

test('Trigger editor uses stacked WHEN WHERE THEN steps and shows spawn authority provenance', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createJobsState }, { TriggerDialogRoot }] = await Promise.all([
    harness.importDashboardModule('js/jobs-state.js'),
    harness.importDashboardModule('js/jobs-triggers.js'),
  ]);
  const snapshot = harness.signals.signal({ groups: [{ id: 2, name: 'alpha' }] });
  const state = createJobsState({ snapshot, prefs: prefs() });
  state.initialize();
  state.openTriggerCreate();
  const mounted = await harness.mount(harness.html`<${TriggerDialogRoot} state=${state} actions=${{ saveTrigger: async () => ({}) }} />`);
  assert.match(mounted.container.textContent, /WHEN/);
  assert.match(mounted.container.textContent, /WHERE/);
  assert.match(mounted.container.textContent, /THEN/);
  const then = [...mounted.container.querySelectorAll('.trigger-step-head')]
    .find((button) => button.textContent.includes('THEN'));
  await harness.act(() => harness.fireEvent(then, 'click'));
  assert.match(mounted.container.textContent, /Firings are re-authorized as the owning principal/);
  assert.ok(mounted.container.querySelector('.trigger-placeholder-chips'));
  assert.match(mounted.container.querySelector('.trigger-placeholder-chips').textContent, /{{pr.url}}/);
  await mounted.unmount();
});

test('creating a trigger invalidates and reloads the visible list', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createJobsState }, { JobsApp }, { TriggerDialogRoot }, { createJobsActions }] = await Promise.all([
    harness.importDashboardModule('js/jobs-state.js'),
    harness.importDashboardModule('js/jobs-island.js'),
    harness.importDashboardModule('js/jobs-triggers.js'),
    harness.importDashboardModule('js/jobs-actions.js'),
  ]);
  const snapshot = harness.signals.signal({ ...page(), groups: [{ id: 2, name: 'alpha' }] });
  const state = createJobsState({ snapshot, prefs: prefs() });
  state.initialize();
  state.setKind('trigger');
  state.beginRequest(1);
  state.commitRequest(1);
  let rules = [];
  const requests = [];
  const actions = createJobsActions({
    state,
    requestMutation: async (path, options) => {
      requests.push(`${options.method} ${path}`);
      if (path === '/api/triggers' && options.method === 'GET') return { triggers: rules };
      if (path.endsWith('/firings?limit=1')) return { firings: [] };
      if (path === '/api/triggers' && options.method === 'POST') {
        rules = [{ id: 7, row_version: 1, operator_authored: true, ...options.body }];
        return rules[0];
      }
      throw new Error(`unexpected request ${options.method} ${path}`);
    },
    refresh: async () => {}, confirm: async () => true, notify: () => {}, download: () => {},
  });
  const mounted = await harness.mount(harness.html`<${harness.preact.Fragment}>
    <${JobsApp} state=${state} actions=${actions} />
    <${TriggerDialogRoot} state=${state} actions=${actions} />
  </${harness.preact.Fragment}>`);
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 0)));
  assert.match(mounted.container.textContent, /No triggers yet/);
  await harness.act(() => harness.fireEvent(getByRole(mounted.container, 'button', { name: '+ new trigger' }), 'click'));
  const name = mounted.container.querySelector('.trigger-name-field input');
  await harness.input(name, 'new name');
  const then = [...mounted.container.querySelectorAll('.trigger-step-head')]
    .find((button) => button.textContent.includes('THEN'));
  await harness.act(() => harness.fireEvent(then, 'click'));
  const actionFields = mounted.container.querySelector('.trigger-action-fields');
  await harness.input(actionFields.querySelector('input'), 'sol-med');
  await harness.input(actionFields.querySelector('textarea'), 'Review {{pr.url}}');
  await harness.act(() => harness.fireEvent(mounted.container.querySelector('#trigger-modal form'), 'submit'));
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 0)));
  await harness.act(() => new Promise((resolve) => setTimeout(resolve, 0)));
  assert.equal(state.triggerDialog.value, null);
  const updatedRow = mounted.container.querySelector('tr[data-key="trigger-7"]');
  assert.ok(updatedRow, `updated trigger row missing (${requests.join(', ')}): ${mounted.container.textContent}`);
  assert.match(updatedRow.textContent, /new name/,
    'successful POST refreshes the still-mounted trigger list');
  await mounted.unmount();
});
