import test from 'node:test';
import assert from 'node:assert/strict';
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
    ],
    paging: { jobs: { offset: 0, limit: 50, total: 2, total_unfiltered: 2 } },
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
    createCron: () => calls.push('create'), editCron: () => calls.push('edit'),
    runCron: () => calls.push('run'), toggleCron: () => calls.push('toggle'),
    deleteCron: () => calls.push('delete'), downloadExport: () => calls.push('download'),
    dismissExport: () => calls.push('dismiss'),
  };
  const mounted = await harness.mount(harness.html`<${JobsApp} state=${state} actions=${actions} />`);
  const badge = await harness.mount(harness.html`<${JobsBadge} state=${state} />`);
  assert.equal(badge.container.querySelector('#jobs-badge').textContent, '1');
  assert.equal(badge.container.querySelector('#jobs-badge').hidden, false);

  const filter = getByRole(mounted.container, 'textbox', { name: 'Filter jobs' });
  assert.equal(filter.value, '');
  const cronRow = mounted.container.querySelector('tr[data-key="cron-1"]');
  const edit = getByRole(cronRow, 'button', { name: 'edit' });
  edit.focus();
  const selectedTextNode = cronRow.querySelector('.rowname').firstChild;

  await harness.act(() => {
    snapshot.value = page('Daily summary');
    state.beginRequest(2);
    state.commitRequest(2);
  });
  assert.equal(mounted.container.querySelector('tr[data-key="cron-1"]'), cronRow);
  assert.equal(cronRow.querySelector('.rowname').firstChild, selectedTextNode,
    'unchanged text node remains a valid browser-selection anchor');
  assert.equal(harness.document.activeElement, edit);

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
  assert.equal(mounted.container.querySelectorAll('tbody tr').length, 2, 'stale page remains visible');
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
    refresh: () => {}, createCron: () => {}, editCron: () => {}, runCron: () => {},
    toggleCron: () => {}, deleteCron: () => {}, downloadExport: () => {}, dismissExport: () => {},
  };
  const mounted = await harness.mount(harness.html`<${JobsApp} state=${state} actions=${actions} />`);
  assert.match(mounted.container.textContent, /Loading jobs/);

  await harness.act(() => state.failRequest(1, new Error('offline')));
  assert.match(getByRole(mounted.container, 'alert').textContent, /offline/);
  assert.doesNotMatch(mounted.container.textContent, /No jobs yet/, 'a failed first load is not an empty result');

  state.beginRequest(2);

  await harness.act(() => {
    snapshot.value = { jobs: [], export_jobs_active: 0, paging: { jobs: { total: 0, total_unfiltered: 0 } } };
    state.commitRequest(2);
  });
  assert.match(mounted.container.textContent, /No jobs yet/);
  assert.equal(mounted.container.querySelector('#filter-jobs-count').textContent, '0 jobs');
  await mounted.unmount();
});

test('production loader dynamically mounts and unmounts the Jobs feature graph', async (t) => {
  const harness = await createPreactHarness(t);
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  host.id = 'jobs-root';
  const badgeHost = harness.document.body.appendChild(harness.document.createElement('span'));
  badgeHost.id = 'jobs-badge-root';
  const { mountJobsFeature } = await harness.importDashboardModule('js/preact-loader.js');
  const cleanup = await mountJobsFeature({
    requestMutation: async () => {}, refresh: async () => {}, confirm: async () => true,
    notify: () => {}, download: () => {}, createCron: () => {}, editCron: () => {},
  });
  assert.equal(typeof cleanup, 'function');
  assert.ok(host.querySelector('#filter-jobs'));
  assert.ok(badgeHost.querySelector('#jobs-badge'));
  cleanup();
  assert.equal(host.childElementCount, 0);
  assert.equal(badgeHost.childElementCount, 0);
});
