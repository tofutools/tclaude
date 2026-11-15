import test from 'node:test';
import assert from 'node:assert/strict';
import { claudeWheelState, claudeWheelStep } from '../dashboard/js/claude-wheel.js';

// lines runs notches (dir, at ms) through the model and returns what each
// scrolls, carrying the state.
function lines(notches, state = claudeWheelState()) {
  const out = [];
  for (const [dir, at] of notches) {
    const step = claudeWheelStep(state, dir, at);
    out.push(step.lines);
    state = step.state;
  }
  return { out, state };
}
const spaced = (n, dir, from, gap) => Array.from({ length: n }, (_, i) => [dir, from + i * gap]);
const sum = (xs) => xs.reduce((a, b) => a + b, 0);

// The numbers measured on Claude Code 2.1.323 in a tmux pane, fullscreen.
test('the model scrolls what Claude Code measured', () => {
  assert.deepEqual(lines([[-1, 1000]]).out, [1], 'one notch, one line');
  assert.equal(sum(lines(spaced(4, -1, 1000, 80)).out), 4, 'four notches 80ms apart, four lines');
  assert.equal(sum(lines(spaced(4, -1, 1000, 0)).out), 4, 'four at once: 1+1+1+1');
  assert.deepEqual(lines(spaced(8, -1, 1000, 10)).out, [1, 1, 1, 1, 2, 2, 2, 3], 'eight 10ms apart: 13 lines');
  const after = lines(spaced(4, -1, 1000, 0)).state;
  assert.deepEqual(lines([[1, 3000], [1, 4600]], after).out, [0, 1],
    'a reversal\'s first notch scrolls nothing, the next one a line');
  assert.deepEqual(lines(spaced(4, 1, 3000, 15), after).out, [0, 1, 1, 1],
    'a reversal that carries on starts over at the base');
});

test('the acceleration tops out at six lines a notch, and a pause starts it over', () => {
  const { out, state } = lines(spaced(30, -1, 1000, 5));
  assert.equal(Math.max(...out), 6);
  assert.deepEqual(lines([[-1, 1000 + 29 * 5 + 41]], state).out, [1], 'more than 40ms later: the base again');
});

test('a stray tick and a notch back the old way within 200ms climb faster, until a burst or a pause', () => {
  const start = lines(spaced(2, -1, 1000, 100)).state;
  const { out, state } = lines([[1, 2000], [-1, 2020], [-1, 2040], [-1, 2060]], start);
  assert.equal(out[0], 0, 'the stray tick');
  assert.ok(out[1] >= 3 && out[2] > out[1], `wheel mode climbs by up to three a notch: ${out}`);
  assert.deepEqual(lines(spaced(5, -1, 2061, 1), state).out.slice(0, 4), [1, 1, 1, 1], 'a burst scrolls a line a notch');
  assert.deepEqual(lines([[-1, 2060 + 1501]], state).out, [1], 'idle for 1.5s leaves it');
});

test('a step leaves the state it was given as it was', () => {
  const state = claudeWheelState();
  const before = { ...state };
  claudeWheelStep(state, -1, 1000);
  assert.deepEqual(state, before);
});
