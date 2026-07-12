import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('request lifecycle rejects stale responses and supports retry after failure', async (t) => {
  const harness = await createPreactHarness(t);
  const { createRequestLifecycle } = await harness.importDashboardModule('js/request-lifecycle.js');
  const payload = harness.signals.signal(null);
  const lifecycle = createRequestLifecycle({
    payload, retainPayloadOnRefresh: true, retainPayloadOnError: false,
  });

  const stale = lifecycle.beginRequest();
  const current = lifecycle.beginRequest();
  assert.equal(lifecycle.commitRequest(stale, { value: 'stale' }), false);
  assert.equal(lifecycle.commitRequest(current, { value: 'current' }), true);
  assert.equal(payload.value.value, 'current');

  const failed = lifecycle.beginRequest();
  assert.equal(lifecycle.request.value.phase, 'refreshing');
  assert.equal(lifecycle.failRequest(failed, Object.assign(new Error('offline'), { body: 'retry later' })), true);
  assert.equal(payload.value, null);
  assert.match(lifecycle.request.value.error, /retry later/);

  const retry = lifecycle.beginRequest();
  assert.equal(lifecycle.request.value.phase, 'loading');
  assert.equal(lifecycle.request.value.error, null);
  assert.equal(lifecycle.commitRequest(retry, { value: 'recovered' }), true);
});

test('request lifecycle makes retain-versus-clear failure policy explicit', async (t) => {
  const harness = await createPreactHarness(t);
  const { createRequestLifecycle } = await harness.importDashboardModule('js/request-lifecycle.js');
  const payload = harness.signals.signal({ value: 'previous' });
  const lifecycle = createRequestLifecycle({
    payload, retainPayloadOnRefresh: false, retainPayloadOnError: true,
  });
  const token = lifecycle.beginRequest();
  assert.equal(payload.value, null, 'refresh policy can clear the previous payload before loading');
  payload.value = { value: 'previous' };
  lifecycle.failRequest(token, new Error('offline'));
  assert.equal(payload.value.value, 'previous');
  assert.throws(
    () => createRequestLifecycle({ payload, retainPayloadOnRefresh: true }),
    /explicit retainPayloadOnError policy/,
  );
});
