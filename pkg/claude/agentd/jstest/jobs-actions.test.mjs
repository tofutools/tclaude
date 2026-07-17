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
  const state = {
    upsertCron: () => {},
    openCronCreate: () => { creates += 1; },
    openCronEdit: (job) => edits.push(job),
    openCronDuplicate: () => { duplicates += 1; },
    closeCronDialog: () => {},
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
  actions.downloadExport({ id: 9 });
  assert.equal(creates, 1);
  assert.deepEqual(edits, [{ id: 4 }]);
  assert.equal(duplicates, 1);
  assert.deepEqual(downloads, [9]);

  await actions.toggleCron({ id: 4, name: 'daily', enabled: true });
  await actions.runCron({ id: 4, name: 'daily' });
  await actions.deleteCron({ id: 4, name: 'daily' });
  await actions.dismissExport({ id: 9, title: 'summary' });
  assert.deepEqual(mutations, [
    { path: '/api/cron/4/disable', options: { method: 'POST' } },
    { path: '/api/cron/4/run-now', options: { method: 'POST' } },
    { path: '/api/cron/4', options: { method: 'DELETE' } },
    { path: '/api/export-jobs/9', options: { method: 'DELETE' } },
  ]);
  assert.equal(notices.length, 4);

  confirms = false;
  assert.equal(await actions.deleteCron({ id: 5, name: 'keep' }), false);
  assert.equal(mutations.length, 4, 'cancelled destructive action does not mutate');

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
      return { id: 8, name: 'saved' };
    },
    refresh: () => { refreshed += 1; return new Promise(() => {}); },
    confirm: async () => true, notify: () => {}, download: () => {},
  });
  assert.deepEqual(await actions.explainCron('@daily'), { valid: true, description: 'daily' });
  const saved = await actions.saveCron({
    path: '/api/cron', method: 'POST', payload: { target: 'agt_one' },
  });
  assert.deepEqual(saved, { id: 8, name: 'saved' });
  assert.deepEqual(upserts, [{ id: 8, name: 'saved' }]);
  assert.equal(refreshed, 1, 'refresh starts but cannot pin the accepted dialog mutation');
  assert.deepEqual(calls, [
    { path: '/api/cron/explain', options: { body: { expr: '@daily' }, refreshAfter: false } },
    { path: '/api/cron', options: { method: 'POST', body: { target: 'agt_one' }, refreshAfter: false } },
  ]);
});
