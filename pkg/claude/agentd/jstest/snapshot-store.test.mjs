import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('snapshot store rejects stale requests and drives a Preact subscriber', async (t) => {
  const harness = await createPreactHarness(t);
  const { createDashboardState } = await harness.importDashboardModule('js/snapshot-store.js');
  let clock = 100;
  const state = createDashboardState({ now: () => ++clock });
  const jobs = state.select((snapshot) => snapshot?.jobs ?? []);

  const first = state.beginRequest();
  const second = state.beginRequest();
  assert.equal(state.commitRequest(first, { jobs: [{ id: 'stale' }] }), false);
  assert.equal(state.snapshot.value, null);

  const accepted = {
    generated_at: '2026-07-12T00:00:00Z',
    jobs: [{ id: 'job-1', name: 'First job' }],
    paging: { jobs: { offset: 0, total: 1 } },
  };
  assert.equal(state.commitRequest(second, accepted), true);
  assert.equal(state.commitRequest(second, { jobs: [{ id: 'duplicate' }] }), false);
  assert.equal(state.poll.value.phase, 'ready');
  assert.equal(state.lastRefreshAt.value, clock);
  assert.equal(state.generatedAt.value, accepted.generated_at);
  assert.deepEqual(jobs.value, accepted.jobs);

  state.setActiveTab('jobs');
  assert.deepEqual(state.activeTabView.value, {
    tab: 'jobs',
    data: accepted.jobs,
    paging: accepted.paging.jobs,
  });

  function Subscriber() {
    const view = state.activeTabView.value;
    return harness.html`<output role="status">${view.data?.[0]?.name ?? 'empty'}</output>`;
  }
  const component = await harness.mount(harness.html`<${Subscriber} />`);
  assert.equal(harness.getByRole(component.container, 'status').textContent, 'First job');

  const third = state.beginRequest();
  await harness.act(() => state.commitRequest(third, {
    ...accepted,
    jobs: [{ id: 'job-2', name: 'Updated job' }],
  }));
  assert.equal(harness.getByRole(component.container, 'status').textContent, 'Updated job');
  await component.unmount();

  const discarded = state.beginRequest();
  assert.equal(state.discardRequest(discarded, { responded: true }), true);
  assert.equal(state.poll.value.phase, 'ready');
  assert.equal(state.isCurrentRequest(discarded), false);
});

test('snapshot store represents poll errors and connection recovery', async (t) => {
  const harness = await createPreactHarness(t);
  const { createDashboardState } = await harness.importDashboardModule('js/snapshot-store.js');
  const state = createDashboardState({ now: () => 42 });

  const request = state.beginRequest();
  assert.equal(state.failRequest(request, new Error('offline')), true);
  assert.deepEqual(state.poll.value, {
    phase: 'error',
    requestId: request,
    startedAt: 42,
    completedAt: 42,
    responded: false,
    error: 'offline',
  });

  state.setConnection('retrying', { consecutiveFailures: 1, error: 'refused' });
  assert.equal(state.connection.value.status, 'retrying');
  state.setConnection('disconnected', { consecutiveFailures: 2, error: 'refused' });
  assert.equal(state.connection.value.status, 'disconnected');
  state.setConnection('connected');
  assert.deepEqual(state.connection.value, {
    status: 'connected',
    consecutiveFailures: 0,
    changedAt: 42,
    error: null,
  });
});
