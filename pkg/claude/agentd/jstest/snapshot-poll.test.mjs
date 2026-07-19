import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('snapshot poll starts immediately and uses visible/hidden cadences', async (t) => {
  const harness = await createPreactHarness(t);
  const { SNAPSHOT_POLL_MS, SNAPSHOT_HIDDEN_POLL_MS, startSnapshotPoll } =
    await harness.importDashboardModule('js/snapshot-poll.js');
  const calls = [];
  const listeners = new Map();
  const documentImpl = {
    hidden: false,
    addEventListener: (name, fn) => listeners.set(name, fn),
    removeEventListener: (name, fn) => {
      if (listeners.get(name) === fn) listeners.delete(name);
    },
  };
  const refresh = () => { calls.push('refresh'); };
  let nextTimer = 0;
  const stop = startSnapshotPoll(refresh, {
    documentImpl,
    setTimeoutImpl: (callback, milliseconds) => {
      calls.push({ callback, milliseconds });
      return ++nextTimer;
    },
    clearTimeoutImpl: (timer) => calls.push({ clear: timer }),
  });

  assert.equal(typeof stop, 'function');
  assert.equal(SNAPSHOT_POLL_MS, 2000);
  assert.equal(SNAPSHOT_HIDDEN_POLL_MS, 10000);
  assert.equal(calls[0], 'refresh');
  assert.equal(calls[1].milliseconds, 2000);
  assert.equal(calls.length, 2);

  documentImpl.hidden = true;
  listeners.get('visibilitychange')();
  assert.deepEqual(calls.at(-2), { clear: 1 });
  assert.equal(calls.at(-1).milliseconds, 10000);
  assert.equal(calls.filter(call => call === 'refresh').length, 1);

  documentImpl.hidden = false;
  listeners.get('visibilitychange')();
  assert.deepEqual(calls.at(-3), { clear: 2 });
  assert.equal(calls.at(-2), 'refresh');
  assert.equal(calls.at(-1).milliseconds, 2000);

  stop();
  assert.deepEqual(calls.at(-1), { clear: 3 });
  assert.equal(listeners.has('visibilitychange'), false);
});

test('snapshot poll can schedule after an awaited bootstrap refresh', async (t) => {
  const harness = await createPreactHarness(t);
  const { startSnapshotPoll } =
    await harness.importDashboardModule('js/snapshot-poll.js');
  const calls = [];
  const stop = startSnapshotPoll(() => calls.push('refresh'), {
    immediate: false,
    documentImpl: { hidden: false, addEventListener() {}, removeEventListener() {} },
    setTimeoutImpl: (callback, milliseconds) => {
      calls.push({ callback, milliseconds });
      return 1;
    },
    clearTimeoutImpl: (timer) => calls.push({ clear: timer }),
  });

  assert.equal(calls.filter(call => call === 'refresh').length, 0);
  assert.equal(calls[0].milliseconds, 2000);
  stop();
});

test('bootstrap waits for published snapshot even when first attempt finishes without one', async (t) => {
  const harness = await createPreactHarness(t);
  const { waitForInitialSnapshot } =
    await harness.importDashboardModule('js/snapshot-poll.js');
  let publishSnapshot;
  const snapshotReady = new Promise((resolve) => { publishSnapshot = resolve; });
  const neverTimeout = new Promise(() => {});
  let attempts = 0;
  let settled = false;

  const waiting = waitForInitialSnapshot(
    async () => { attempts += 1; },
    snapshotReady,
    neverTimeout,
  ).then(() => { settled = true; });

  // The first attempt has completed, modelling either a superseded request or
  // a handled fetch failure. That completion must not release URL restoration.
  await Promise.resolve();
  await Promise.resolve();
  assert.equal(attempts, 1);
  assert.equal(settled, false);

  publishSnapshot();
  await waiting;
  assert.equal(settled, true);
});
