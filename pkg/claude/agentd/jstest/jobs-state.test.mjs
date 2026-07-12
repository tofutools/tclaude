import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

function fakePrefs(initial = {}) {
  const values = new Map(Object.entries(initial));
  return {
    getItem: (key) => values.get(key) ?? null,
    setItem: (key, value) => values.set(key, String(value)),
    removeItem: (key) => values.delete(key),
    values,
  };
}

test('Jobs state explicitly owns query, paging, sort, requests, and derived rows', async (t) => {
  const harness = await createPreactHarness(t);
  const { createJobsState } = await harness.importDashboardModule('js/jobs-state.js');
  const snapshot = harness.signals.signal(null);
  const prefs = fakePrefs({
    'tclaude.dash.filter.jobs': 'agent one',
    'tclaude.dash.list.jobs.pagesize': '25',
    'tclaude.dash.sort': JSON.stringify({ jobs: { col: 'name', dir: 'desc' }, sudo: { col: 'slug', dir: 'asc' } }),
  });
  const state = createJobsState({ snapshot, prefs });

  assert.equal(state.initialize(), true);
  assert.equal(state.initialize(), false);
  assert.equal(state.query.value, 'agent one');
  assert.equal(state.limit.value, 25);
  assert.equal(state.params.value, 'offset=0&limit=25&q=agent+one');

  state.setQuery('cron & owner');
  assert.equal(state.offset.value, 0);
  assert.match(state.params.value, /q=cron%20%26%20owner|q=cron\+%26\+owner/);
  assert.equal(prefs.values.get('tclaude.dash.filter.jobs'), 'cron & owner');

  assert.equal(state.page('next', 80), true);
  assert.equal(state.offset.value, 25);
  assert.equal(state.page('last', 80), true);
  assert.equal(state.offset.value, 75);
  state.setPageSize(100);
  assert.equal(state.limit.value, 100);
  assert.equal(state.offset.value, 0);

  state.cycleSort('kind');
  assert.deepEqual(state.sort.value, { col: 'kind', dir: 'asc' });
  const persistedSort = JSON.parse(prefs.values.get('tclaude.dash.sort'));
  assert.deepEqual(persistedSort.sudo, { col: 'slug', dir: 'asc' }, 'other table preferences survive');

  state.beginRequest(1);
  state.beginRequest(2);
  assert.equal(state.commitRequest(1), false, 'stale request cannot settle newer state');
  snapshot.value = {
    export_jobs_active: 2,
    jobs: [
      { kind: 'export', export: { id: 2, title: 'Zed' } },
      { kind: 'cron', cron: { id: 1, name: 'Alpha' } },
    ],
    paging: { jobs: { offset: 0, limit: 100, total: 2, total_unfiltered: 3 } },
  };
  assert.equal(state.commitRequest(2), true);
  assert.equal(state.view.value.activeExports, 2);
  assert.deepEqual(state.view.value.rows.map((row) => row.kind), ['cron', 'export']);

  state.upsertCron({ id: 1, name: 'Updated' });
  assert.equal(state.view.value.rows.find((row) => row.kind === 'cron').cron.name, 'Updated');
  assert.equal(state.upsertCron({ id: 99, name: 'Outside this page' }), false);
  assert.equal(state.view.value.rows.length, 2, 'canonical refetch, not local insertion, owns new rows');
  state.beginRequest(3);
  state.failRequest(3, new Error('jobs endpoint unavailable'));
  assert.equal(state.request.value.phase, 'error');
  assert.equal(state.request.value.hasLoaded, true);
  assert.equal(state.view.value.rows.length, 2, 'failed refresh retains the last successful page');
});

test('Jobs parameter changes immediately invalidate requests but retain requested paging', async (t) => {
  const harness = await createPreactHarness(t);
  const { createJobsState } = await harness.importDashboardModule('js/jobs-state.js');
  const snapshot = harness.signals.signal({
    jobs: [],
    paging: { jobs: { offset: 0, limit: 25, total: 80, total_unfiltered: 80 } },
  });
  const state = createJobsState({ snapshot, prefs: fakePrefs() });
  state.initialize();
  state.setPageSize(25);

  state.beginRequest(10);
  assert.equal(state.acceptsRequest(10), true);
  state.setQuery('new query');
  assert.equal(state.acceptsRequest(10), false, 'old-query response is invalid immediately');
  assert.equal(state.commitRequest(10), false);

  state.beginRequest(11);
  assert.equal(state.page('next', 80), true);
  assert.equal(state.offset.value, 25);
  assert.equal(state.acceptsRequest(11), false, 'old-page response is invalid immediately');
  state.beginRequest(12);
  assert.equal(state.failRequest(12, new Error('network down')), true);
  assert.equal(state.request.value.phase, 'error');
  assert.equal(state.offset.value, 25, 'failed request does not restore the previous offset');
});
