import test from 'node:test';
import assert from 'node:assert/strict';
import {
  formatUsageAxisTick,
  usageAxisStart,
  usageAxisTicks,
} from '../dashboard/js/usage-history-axis.js';

const hour = 60 * 60_000;
const day = 24 * hour;
const now = Date.UTC(2026, 6, 18, 12);

test('usage history axis honors each selected history span', () => {
  const firstObserved = now - hour;
  for (const days of [1, 7, 30, 90]) {
    const requested = now - days * day;
    assert.equal(usageAxisStart(requested, firstObserved, now), requested);
  }
  assert.equal(usageAxisStart(null, firstObserved, now), firstObserved);
});

test('usage history ticks divide the full domain into equal intervals', () => {
  const ticks = usageAxisTicks(now - 7 * day, now + 7 * day);
  assert.equal(ticks.length, 5);
  assert.deepEqual(ticks.map((tick) => tick.ratio), [0, 0.25, 0.5, 0.75, 1]);
  assert.deepEqual(ticks.map((tick) => tick.time), [
    now - 7 * day,
    now - 3.5 * day,
    now,
    now + 3.5 * day,
    now + 7 * day,
  ]);
});

test('usage history tick formatting includes time only for short domains', () => {
  const shortLabel = formatUsageAxisTick(now, now - day, now, 'en-GB');
  const longLabel = formatUsageAxisTick(now, now - 7 * day, now + 7 * day, 'en-GB');
  assert.match(shortLabel, /\d{2}:\d{2}/);
  assert.doesNotMatch(longLabel, /\d{2}:\d{2}/);
  assert.match(longLabel, /18 Jul/);
});
