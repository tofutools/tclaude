import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('Jobs actions preserve confirmation, mutation, modal, download, and error behavior', async (t) => {
  const harness = await createPreactHarness(t);
  const { createJobsActions } = await harness.importDashboardModule('js/jobs-actions.js');
  const mutations = [];
  const notices = [];
  const downloads = [];
  const edits = [];
  let confirms = true;
  let creates = 0;
  let duplicates = 0;
  let orderCreates = 0;
  const orderEdits = [];
  const state = {
    upsertCron: () => {},
    openCronCreate: () => { creates += 1; },
    openCronEdit: (job) => edits.push(job),
    openCronDuplicate: () => { duplicates += 1; },
    closeCronDialog: () => {},
    openStandingOrderCreate: () => { orderCreates += 1; },
    openStandingOrderEdit: (order) => orderEdits.push(order),
    closeStandingOrderDialog: () => {},
  };
  const actions = createJobsActions({
    state,
    requestMutation: async (path, options) => { mutations.push({ path, options }); },
    refresh: async () => {},
    confirm: async () => confirms,
    notify: (...args) => notices.push(args),
    download: (id) => downloads.push(id),
  });

  actions.openCronCreate();
  actions.openCronEdit({ id: 4 });
  actions.openCronDuplicate({ id: 4 });
  actions.openStandingOrderCreate();
  actions.openStandingOrderEdit({ id: 12 });
  actions.downloadExport({ id: 9 });
  assert.equal(creates, 1);
  assert.deepEqual(edits, [{ id: 4 }]);
  assert.equal(duplicates, 1);
  assert.equal(orderCreates, 1);
  assert.deepEqual(orderEdits, [{ id: 12 }]);
  assert.deepEqual(downloads, [9]);

  await actions.toggleCron({ id: 4, name: 'daily', enabled: true });
  await actions.runCron({ id: 4, name: 'daily' });
  await actions.deleteCron({ id: 4, name: 'daily' });
  await actions.dismissExport({ id: 9, title: 'summary' });
  await actions.toggleStandingOrder({
    id: 12, revision: 4, row_version: 6,
    name: 'pr-early', enabled: true,
  });
  await actions.deleteStandingOrder({
    id: 12, revision: 4, row_version: 6, name: 'pr-early',
  });
  assert.deepEqual(mutations, [
    { path: '/api/cron/4/disable', options: { method: 'POST' } },
    { path: '/api/cron/4/run-now', options: { method: 'POST' } },
    { path: '/api/cron/4', options: { method: 'DELETE' } },
    { path: '/api/export-jobs/9', options: { method: 'DELETE' } },
    { path: '/api/standing-orders/12/disable?row_version=6', options: { method: 'POST' } },
    { path: '/api/standing-orders/12?row_version=6', options: { method: 'DELETE' } },
  ]);
  assert.equal(notices.length, 6);

  confirms = false;
  assert.equal(await actions.deleteCron({ id: 5, name: 'keep' }), false);
  assert.equal(mutations.length, 6, 'cancelled destructive action does not mutate');

  confirms = false;
  assert.equal(await actions.toggleStandingOrder({
    id: 13, revision: 2, row_version: 3,
    name: 'retired', enabled: false, disabled_reason: 'group-retired',
  }), false);
  assert.equal(mutations.length, 6, 'automatic retirement requires explicit re-enable confirmation');

  const failing = createJobsActions({
    state,
    requestMutation: async () => {
      const error = new Error('dashboard mutation failed: HTTP 409');
      error.body = {
        code: 'not_runnable',
        error: 'cron job owner is retired; the requested action was not applied',
      };
      throw error;
    },
    refresh: async () => {}, confirm: async () => true,
    notify: (...args) => notices.push(args),
    download: () => {},
  });
  assert.equal(await failing.toggleCron({ id: 7, name: 'broken', enabled: false }), false);
  assert.deepEqual(notices.at(-1), [
    'Request failed: dashboard mutation failed: HTTP 409: cron job owner is retired; the requested action was not applied', true,
  ]);
});

test('Jobs cron transport returns canonical rows without awaiting the follow-up refresh', async (t) => {
  const harness = await createPreactHarness(t);
  const { createJobsActions } = await harness.importDashboardModule('js/jobs-actions.js');
  const calls = [];
  const upserts = [];
  let refreshed = 0;
  const state = {
    upsertCron: (cron) => upserts.push(cron),
    openCronCreate: () => {}, openCronEdit: () => {}, openCronDuplicate: () => {}, closeCronDialog: () => {},
  };
  const actions = createJobsActions({
    state,
    requestMutation: async (path, options) => {
      calls.push({ path, options });
      if (path === '/api/cron/explain') return { valid: true, description: 'daily' };
      if (path.includes('/logs')) return { runs: [{ id: 3, status: 'spawned', worker_agent: 'agt_worker' }] };
      return { id: 8, name: 'saved' };
    },
    refresh: () => { refreshed += 1; return new Promise(() => {}); },
    confirm: async () => true, notify: () => {}, download: () => {},
  });
  assert.deepEqual(await actions.explainCron('@daily'), { valid: true, description: 'daily' });
  assert.deepEqual(await actions.loadCronLogs(8), [{ id: 3, status: 'spawned', worker_agent: 'agt_worker' }]);
  const saved = await actions.saveCron({
    path: '/api/cron', method: 'POST', payload: { target: 'agt_one' },
  });
  assert.deepEqual(saved, { id: 8, name: 'saved' });
  assert.deepEqual(upserts, [{ id: 8, name: 'saved' }]);
  assert.equal(refreshed, 1, 'refresh starts but cannot pin the accepted dialog mutation');
  assert.deepEqual(calls, [
    { path: '/api/cron/explain', options: { body: { expr: '@daily' }, refreshAfter: false } },
    { path: '/api/cron/8/logs?limit=25', options: { method: 'GET', refreshAfter: false } },
    { path: '/api/cron', options: { method: 'POST', body: { target: 'agt_one' }, refreshAfter: false } },
  ]);
});

test('Jobs cron PATCH target and owner denials reject without optimistic success state', async (t) => {
  const harness = await createPreactHarness(t);
  const { createJobsActions } = await harness.importDashboardModule('js/jobs-actions.js');
  const upserts = [];
  const notices = [];
  let refreshed = 0;
  const state = {
    upsertCron: (cron) => upserts.push(cron),
    openCronCreate: () => {}, openCronEdit: () => {}, openCronDuplicate: () => {}, closeCronDialog: () => {},
  };
  for (const denied of [
    {
      payload: { target: 'private-agent' },
      message: 'caller is not authorized to schedule the proposed cron target',
    },
    {
      payload: { owner: 'private-agent' },
      message: 'caller is not authorized to assign the proposed cron owner',
    },
  ]) {
    const actions = createJobsActions({
      state,
      requestMutation: async () => {
        const error = new Error('dashboard mutation failed: HTTP 403');
        error.body = { code: 'permission', error: denied.message };
        throw error;
      },
      refresh: async () => { refreshed += 1; },
      confirm: async () => true,
      notify: (...args) => notices.push(args),
      download: () => {},
    });

    await assert.rejects(
      actions.saveCron({
        path: '/api/cron/7', method: 'PATCH', payload: denied.payload,
      }),
      new RegExp(`HTTP 403: ${denied.message}`),
    );
  }
  assert.deepEqual(upserts, [], 'a denied PATCH must not optimistically replace the stored row');
  assert.equal(refreshed, 0, 'a denied PATCH must not schedule a success refresh');
  assert.deepEqual(notices, [], 'the dialog owns error presentation; no success notice is emitted');
});

test('Trigger actions use the frozen REST contract and row-version CAS', async (t) => {
  const harness = await createPreactHarness(t);
  const { createJobsActions } = await harness.importDashboardModule('js/jobs-actions.js');
  const calls = [];
  const notices = [];
  const state = {
    upsertCron: () => {},
    openCronCreate: () => {}, openCronEdit: () => {}, openCronDuplicate: () => {}, closeCronDialog: () => {},
    openStandingOrderCreate: () => {}, openStandingOrderEdit: () => {}, closeStandingOrderDialog: () => {},
    openTriggerCreate: () => {}, openTriggerEdit: () => {}, closeTriggerDialog: () => {},
    invalidateTriggers: () => {},
  };
  const actions = createJobsActions({
    state,
    requestMutation: async (path, options) => {
      calls.push({ path, options });
      if (path === '/api/triggers') return { triggers: [{ id: 4, name: 'review' }] };
      if (path.includes('/firings')) return { firings: [{ id: 8, outcome: 'ok' }] };
      return { id: 4, name: 'review' };
    },
    refresh: async () => {}, confirm: async () => true,
    notify: (...args) => notices.push(args), download: () => {},
  });

  assert.deepEqual(await actions.loadTriggers(), [{ id: 4, name: 'review', firings: [{ id: 8, outcome: 'ok' }] }]);
  assert.deepEqual(await actions.loadTriggerFirings(4), [{ id: 8, outcome: 'ok' }]);
  await actions.saveTrigger({ editing: false, payload: { name: 'review' } });
  await actions.saveTrigger({ editing: true, id: 4, payload: { name: 'review 2', row_version: 7 } });
  await actions.toggleTrigger({ id: 4, row_version: 7, name: 'review', enabled: true });
  await actions.deleteTrigger({ id: 4, row_version: 8, name: 'review' });

  assert.deepEqual(calls.map((call) => [call.path, call.options.method]), [
    ['/api/triggers', 'GET'],
    ['/api/triggers/4/firings?limit=1', 'GET'],
    ['/api/triggers/4/firings?limit=20', 'GET'],
    ['/api/triggers', 'POST'],
    ['/api/triggers/4', 'PATCH'],
    ['/api/triggers/4/disable?row_version=7', 'POST'],
    ['/api/triggers/4?row_version=8', 'DELETE'],
  ]);
  assert.match(notices.at(-1)[0], /trigger delete/);
});
