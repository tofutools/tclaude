import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

function response({ ok = true, status = 200, body = {}, raw } = {}) {
  return {
    ok,
    status,
    headers: { get: () => 'application/json' },
    text: async () => raw ?? JSON.stringify(body),
  };
}

test('dashboard actions expose refresh, retry, and same-origin mutations', async (t) => {
  const harness = await createPreactHarness(t);
  const { createDashboardActions, DashboardActionError } =
    await harness.importDashboardModule('js/dashboard-actions.js');
  const refreshCalls = [];
  const requests = [];
  const actions = createDashboardActions({
    baseURL: 'http://dashboard.test/groups',
    refresh: async (options) => { refreshCalls.push(options); },
    fetchImpl: async (path, options) => {
      requests.push({ path, options });
      return response({ body: { saved: true } });
    },
  });

  await actions.refresh();
  await actions.retry();
  assert.deepEqual(refreshCalls, [undefined, undefined]);

  const result = await actions.requestMutation('/api/jobs/job-1', {
    method: 'PATCH',
    body: { paused: true },
  });
  assert.deepEqual(result, { saved: true });
  assert.equal(requests[0].path, '/api/jobs/job-1');
  assert.deepEqual(requests[0].options, {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: '{"paused":true}',
    credentials: 'same-origin',
  });
  assert.equal(refreshCalls.length, 3, 'successful mutation refreshes authoritative poll');

  for (const unsafePath of [
    '/api/../admin',
    '/api/%2e%2e/admin',
    '/api/%2F..%2F..%2Fadmin',
    'https://example.com/api/jobs',
  ]) {
    await assert.rejects(
      () => actions.requestMutation(unsafePath),
      /same-origin \/api\//,
    );
  }

  let malformedRefreshes = 0;
  const malformed = createDashboardActions({
    baseURL: 'http://dashboard.test/',
    refresh: async () => { malformedRefreshes += 1; },
    fetchImpl: async () => response({ raw: '{not json' }),
  });
  assert.equal(await malformed.requestMutation('/api/jobs/job-1'), '{not json');
  assert.equal(malformedRefreshes, 1, 'successful mutation refreshes despite malformed JSON');

  const failing = createDashboardActions({
    baseURL: 'http://dashboard.test/',
    refresh: async () => { throw new Error('must not refresh'); },
    fetchImpl: async () => response({ ok: false, status: 409, raw: '{bad json' }),
  });
  await assert.rejects(
    () => failing.requestMutation('/api/jobs/job-1'),
    (error) => error instanceof DashboardActionError &&
      error.status === 409 && error.body === '{bad json',
  );
});
