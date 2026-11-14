// viewer-controls.js: the touch gesture math the attachment viewers share.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

// The module imports Preact, which only the harness can resolve.
async function controls(t) {
  const harness = await createPreactHarness(t);
  return harness.importDashboardModule('js/viewer-controls.js');
}

test('pinchStep scales by the fingers\' span and keeps their midpoint still', async (t) => {
  const { pinchStep } = await controls(t);
  const step = pinchStep([{ x: 0, y: 0 }, { x: 30, y: 40 }], [{ x: 0, y: 0 }, { x: 60, y: 80 }]);
  assert.equal(step.factor, 2);
  assert.deepEqual(step.point, { x: 30, y: 40 });
  assert.equal(pinchStep([{ x: 1, y: 1 }, { x: 1, y: 1 }], [{ x: 0, y: 0 }, { x: 5, y: 5 }]), null, 'a zero span has no ratio');
});

test('tapTracker tells taps, double taps and drags apart', async (t) => {
  const { tapTracker, DOUBLE_TAP_MS, TAP_SLOP, DOUBLE_TAP_SLOP } = await controls(t);
  const taps = tapTracker();
  taps.start(10, 10, 0);
  assert.equal(taps.end(12, 11, 80), 'tap');
  taps.start(15, 12, 200);
  assert.equal(taps.end(15, 12, 260), 'double', 'a second tap close by and soon after');
  taps.start(15, 12, 300);
  assert.equal(taps.end(15, 12, 340), 'tap', 'a double tap starts over');

  taps.start(0, 0, 1000);
  assert.equal(taps.end(TAP_SLOP + 5, 0, 1050), null, 'a drag is not a tap');
  taps.start(0, 0, 2000);
  assert.equal(taps.end(0, 0, 2000 + DOUBLE_TAP_MS + 1), null, 'a long press is not a tap');

  taps.start(0, 0, 3000);
  assert.equal(taps.end(0, 0, 3050), 'tap');
  taps.start(DOUBLE_TAP_SLOP + 5, 0, 3100);
  assert.equal(taps.end(DOUBLE_TAP_SLOP + 5, 0, 3150), 'tap', 'a second tap far away is a new tap');

  taps.start(0, 0, 4000);
  assert.equal(taps.end(0, 0, 4050), 'tap');
  taps.cancel();
  taps.start(0, 0, 4100);
  assert.equal(taps.end(0, 0, 4150), 'tap', 'a cancelled gesture forgets the last tap');
});
