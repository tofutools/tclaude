import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('snapshot poll starts immediately and installs exactly one 2-second interval', async (t) => {
  const harness = await createPreactHarness(t);
  const { SNAPSHOT_POLL_MS, startSnapshotPoll } =
    await harness.importDashboardModule('js/snapshot-poll.js');
  const calls = [];
  const refresh = () => { calls.push('refresh'); };
  const interval = startSnapshotPoll(refresh, {
    setIntervalImpl: (callback, milliseconds) => {
      calls.push({ callback, milliseconds });
      return 42;
    },
  });

  assert.equal(interval, 42);
  assert.equal(SNAPSHOT_POLL_MS, 2000);
  assert.equal(calls[0], 'refresh');
  assert.deepEqual(calls[1], { callback: refresh, milliseconds: 2000 });
  assert.equal(calls.length, 2);
});
