import test from 'node:test';
import assert from 'node:assert/strict';
import { normalizeWheelDelta } from '../dashboard/js/process-graph.js';

test('wheel delta modes normalize to useful pixel-scale zoom input', () => {
  assert.equal(normalizeWheelDelta(120, 0, 900), 120);
  assert.equal(normalizeWheelDelta(3, 1, 900), 72);
  assert.equal(normalizeWheelDelta(1, 2, 900), 900);
  assert.equal(normalizeWheelDelta(Number.NaN, 0, 900), 0);
});
