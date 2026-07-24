import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

// The poll hands its refresh result to a .then, so the in-flight marker clears a
// microtask after the refresh settles. Yield through a macrotask to drain them.
const flush = () => new Promise((resolve) => { setImmediate(resolve); });

// pollHarness records every injected-timer interaction so a test can fire the
// scheduled tick by hand instead of waiting on wall-clock time.
function pollHarness({ refresh, ...options } = {}) {
  const calls = [];
  const listeners = new Map();
  const timers = [];
  const documentImpl = {
    hidden: false,
    addEventListener: (name, fn) => listeners.set(name, fn),
    removeEventListener: (name, fn) => {
      if (listeners.get(name) === fn) listeners.delete(name);
    },
  };
  let nextTimer = 0;
  return {
    calls,
    listeners,
    documentImpl,
    // fireTick invokes the most recently scheduled timer callback.
    fireTick: () => timers.at(-1).callback(),
    options: {
      documentImpl,
      setTimeoutImpl: (callback, milliseconds) => {
        const entry = { callback, milliseconds };
        timers.push(entry);
        calls.push(entry);
        return ++nextTimer;
      },
      clearTimeoutImpl: (timer) => calls.push({ clear: timer }),
      ...options,
    },
  };
}

test('snapshot poll starts immediately and uses visible/hidden cadences', async (t) => {
  const harness = await createPreactHarness(t);
  const { SNAPSHOT_POLL_MS, SNAPSHOT_HIDDEN_POLL_MS, startSnapshotPoll } =
    await harness.importDashboardModule('js/snapshot-poll.js');
  const h = pollHarness();
  const { calls, listeners, documentImpl } = h;
  const stop = startSnapshotPoll(() => { calls.push('refresh'); }, h.options);

  assert.equal(typeof stop, 'function');
  assert.equal(SNAPSHOT_POLL_MS, 2000);
  assert.equal(SNAPSHOT_HIDDEN_POLL_MS, 10000);
  assert.equal(calls[0], 'refresh');
  assert.equal(calls[1].milliseconds, 2000);
  assert.equal(calls.length, 2);

  // Let the bootstrap attempt settle: the in-flight guard deliberately holds
  // back an overlapping refresh, so a test that never yields would be measuring
  // the guard rather than the cadence.
  await flush();

  documentImpl.hidden = true;
  listeners.get('visibilitychange')();
  assert.deepEqual(calls.at(-2), { clear: 1 });
  assert.equal(calls.at(-1).milliseconds, 10000);
  assert.equal(calls.filter(call => call === 'refresh').length, 1);

  await flush();

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

// The starvation this guard exists to prevent: refresh() bumps a shared request
// generation, so a tick landing on a still-running refresh CANCELS it rather
// than duplicating it. With a cycle slower than the interval that starves every
// attempt and nothing ever paints.
test('an overlapping tick does not start a competing refresh', async (t) => {
  const harness = await createPreactHarness(t);
  const { startSnapshotPoll } =
    await harness.importDashboardModule('js/snapshot-poll.js');
  let started = 0;
  let release;
  const h = pollHarness();
  const stop = startSnapshotPoll(() => {
    started += 1;
    return new Promise((resolve) => { release = resolve; });
  }, h.options);

  assert.equal(started, 1);
  // Three ticks land while the bootstrap attempt is still running.
  h.fireTick();
  h.fireTick();
  h.fireTick();
  await flush();
  assert.equal(started, 1, 'a slow refresh must not be superseded by its own poll');

  // Once it finishes, the cadence resumes normally.
  release();
  await flush();
  h.fireTick();
  await flush();
  assert.equal(started, 2);
  stop();
});

// ...but the guard must never become a permanent wedge: a refresh stuck past
// the stall bound stops holding the cadence back, preserving the "later
// generations still get their retry opportunity" property.
test('a refresh stalled past the stall bound stops blocking the cadence', async (t) => {
  const harness = await createPreactHarness(t);
  const { startSnapshotPoll } =
    await harness.importDashboardModule('js/snapshot-poll.js');
  let started = 0;
  const releases = [];
  let now = 1000;
  const h = pollHarness({ nowImpl: () => now, stallMs: 5000 });
  const stop = startSnapshotPoll(() => {
    started += 1;
    return new Promise((resolve) => { releases.push(resolve); });
  }, h.options);

  assert.equal(started, 1);
  now += 4999;
  h.fireTick();
  await flush();
  assert.equal(started, 1, 'still within the stall bound');

  now += 1;
  h.fireTick();
  await flush();
  assert.equal(started, 2, 'past the stall bound a successor may take over');

  // The stalled straggler finally settles. It must NOT unlock the guard its
  // successor now holds — the successor is still running.
  releases[0]();
  await flush();
  h.fireTick();
  await flush();
  assert.equal(started, 2, 'the successor still owns the in-flight marker');

  // When the successor itself finishes, the cadence resumes normally.
  releases[1]();
  await flush();
  h.fireTick();
  await flush();
  assert.equal(started, 3);
  stop();
});

// Boot asks for the snapshot alone so the paint curtain is not held by the
// Groups tab's heavy paginated lists. Only the bootstrap attempt is narrowed.
test('immediateOptions reach the bootstrap attempt and no later tick', async (t) => {
  const harness = await createPreactHarness(t);
  const { startSnapshotPoll } =
    await harness.importDashboardModule('js/snapshot-poll.js');
  const seen = [];
  const h = pollHarness();
  const stop = startSnapshotPoll((options) => { seen.push(options); }, {
    ...h.options,
    immediateOptions: { includeLists: false },
  });

  assert.deepEqual(seen, [{ includeLists: false }]);
  await flush();
  h.fireTick();
  await flush();
  assert.deepEqual(seen, [{ includeLists: false }, undefined]);
  stop();
});

test('bootstrap waits for published snapshot even when first attempt finishes without one', async (t) => {
  const harness = await createPreactHarness(t);
  const { waitForInitialSnapshot } =
    await harness.importDashboardModule('js/snapshot-poll.js');
  let publishSnapshot;
  const snapshotReady = new Promise((resolve) => { publishSnapshot = resolve; });
  const neverTimeout = new Promise(() => {});
  let settled = false;

  // The bootstrap attempt itself belongs to startSnapshotPoll; this helper only
  // gates URL restoration. A completed attempt — superseded, or a handled fetch
  // failure — publishes nothing and must not release the wait.
  const waiting = waitForInitialSnapshot(snapshotReady, neverTimeout)
    .then(() => { settled = true; });

  await flush();
  assert.equal(settled, false);

  publishSnapshot();
  await waiting;
  assert.equal(settled, true);
});

test('bootstrap wait is released by the bounded timeout', async (t) => {
  const harness = await createPreactHarness(t);
  const { waitForInitialSnapshot } =
    await harness.importDashboardModule('js/snapshot-poll.js');
  let bail;
  const bailed = new Promise((resolve) => { bail = resolve; });
  const neverPublishes = new Promise(() => {});

  bail();
  await waitForInitialSnapshot(neverPublishes, bailed);
});
