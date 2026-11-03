// The scaled clock (fast-clock.mjs) that suites with second-scale production
// timers run on. This file is its own process under node --test, so the clock
// it installs reaches no other suite.

import test from 'node:test';
import assert from 'node:assert/strict';
import { installFastClock } from './fast-clock.mjs';

const RealDate = Date;
const realNow = () => RealDate.now();
installFastClock(0.1);

test('timers fire after a tenth of their delay, and the clock reads the full delay', async () => {
  const realStart = realNow();
  const clockStart = Date.now();
  await new Promise((resolve) => setTimeout(resolve, 1000));
  const real = realNow() - realStart;
  const clock = Date.now() - clockStart;
  assert.ok(real < 500, `a 1000 ms timer took ${real} ms of real time`);
  assert.ok(clock >= 990, `the clock moved ${clock} ms over a 1000 ms timer`);
  assert.ok(new Date().getTime() - Date.now() <= 1, 'new Date() reads the same clock');
});

test('timers keep their order, and intervals repeat on the scaled period', async () => {
  const fired = [];
  setTimeout(() => fired.push('late'), 300);
  setTimeout(() => fired.push('early'), 200);
  let ticks = 0;
  const interval = setInterval(() => { ticks += 1; }, 100);
  await new Promise((resolve) => setTimeout(resolve, 450));
  clearInterval(interval);
  assert.deepEqual(fired, ['early', 'late']);
  // At real speed a 100 ms interval would not tick in the 45 ms this takes.
  // A loaded runner may drop ticks, so only a lower bound is pinned.
  assert.ok(ticks >= 1, `${ticks} ticks of a 100 ms interval in 450 ms`);
});

test('Date with arguments, parsing and instanceof are unchanged', () => {
  const fixed = new Date('2026-01-02T03:04:05Z');
  assert.equal(fixed.toISOString(), '2026-01-02T03:04:05.000Z');
  assert.equal(new Date(0).getTime(), 0);
  assert.equal(Date.parse('2026-01-02T03:04:05Z'), fixed.getTime());
  assert.equal(Date.UTC(2026, 0, 2), new RealDate(Date.UTC(2026, 0, 2)).getTime());
  assert.ok(fixed instanceof Date);
  assert.ok(new RealDate() instanceof Date);
  assert.equal(typeof Date(), 'string');
});

test('performance.now advances with the fast clock', async () => {
  const start = performance.now();
  await new Promise((resolve) => setTimeout(resolve, 500));
  assert.ok(performance.now() - start >= 490);
});

test('it installs once, and only with a scale in (0, 1]', () => {
  assert.throws(() => installFastClock(0), RangeError);
  assert.throws(() => installFastClock(2), RangeError);
  assert.throws(() => installFastClock(0.5), /twice/);
});
