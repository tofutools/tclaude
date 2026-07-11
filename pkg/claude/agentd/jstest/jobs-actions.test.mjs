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
  const actions = createJobsActions({
    requestMutation: async (path, options) => { mutations.push({ path, options }); },
    refresh: async () => {},
    confirm: async () => confirms,
    notify: (...args) => notices.push(args),
    download: (id) => downloads.push(id),
    createCron: () => { creates += 1; },
    editCron: (job) => edits.push(job),
  });

  actions.createCron();
  actions.editCron({ id: 4 });
  actions.downloadExport({ id: 9 });
  assert.equal(creates, 1);
  assert.deepEqual(edits, [{ id: 4 }]);
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
    requestMutation: async () => {
      const error = new Error('dashboard mutation failed: HTTP 403');
      error.body = 'permission denied for this job';
      throw error;
    },
    refresh: async () => {}, confirm: async () => true,
    notify: (...args) => notices.push(args),
    download: () => {}, createCron: () => {}, editCron: () => {},
  });
  assert.equal(await failing.toggleCron({ id: 7, name: 'broken', enabled: false }), false);
  assert.deepEqual(notices.at(-1), [
    'Request failed: dashboard mutation failed: HTTP 403: permission denied for this job', true,
  ]);
});
