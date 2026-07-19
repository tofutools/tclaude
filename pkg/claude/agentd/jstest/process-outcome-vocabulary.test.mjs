import test from 'node:test';
import assert from 'node:assert/strict';

import {
  PASS_OUTCOMES, UNNAMED_OUTCOME, defaultPinned, edgeLabelVisible,
} from '../dashboard/js/process-outcome-vocabulary.js';

test('a lone generic outcome is not worth drawing by default; a branch always is', () => {
  assert.equal(defaultPinned(UNNAMED_OUTCOME, 1), false);
  assert.equal(defaultPinned(UNNAMED_OUTCOME, 2), true);
  assert.equal(defaultPinned('', 1), false);
});

test('templates predating this feature declutter without any clicking', () => {
  for (const generic of ['pass', 'done', 'success']) {
    assert.equal(defaultPinned(generic, 1), false, generic);
    assert.equal(defaultPinned(generic, 2), true, generic);
  }
});

test('a deliberately chosen name survives even on a lone connector', () => {
  assert.equal(defaultPinned('approved', 1), true);
  assert.equal(defaultPinned('escalated', 1), true);
});

test('a decision label is never hidden, even as the only way out', () => {
  // plan.DecisionEdge matches a verdict EXACTLY and has no lone-edge fallback,
  // unlike ResolvePassEdge, so a decision's only outcome is load-bearing.
  assert.equal(defaultPinned('pass', 1, 'decision'), true);
  assert.equal(defaultPinned('next', 1, 'decision'), true);
  for (const type of ['task', 'wait', 'start', 'parallel', '']) {
    assert.equal(defaultPinned('pass', 1, type), false, type);
  }
});

test('UNNAMED_OUTCOME is the precedence-winning pass alias', () => {
  // If it were 'next' -- the last alias -- a later 'pass' sibling would take the
  // pass routing away from the connector drawn first.
  assert.equal(UNNAMED_OUTCOME, PASS_OUTCOMES[0]);
  assert.equal(UNNAMED_OUTCOME, 'pass');
});

test('a selected connector always shows its key, whatever the pin says', () => {
  // Selecting is how you read and rename the key, so it must be legible even
  // when the author has explicitly pinned it off.
  const base = { outcome: 'pass', siblingCount: 1, selected: true };
  assert.equal(edgeLabelVisible({ ...base, pinned: false }), true);
  assert.equal(edgeLabelVisible({ ...base, pinned: undefined }), true);
  assert.equal(edgeLabelVisible({ ...base, pinned: true }), true);
});

test('an explicit pin beats the default in both directions', () => {
  // A lone 'pass' the default would hide, pinned on:
  assert.equal(edgeLabelVisible({ outcome: 'pass', siblingCount: 1, pinned: true }), true);
  // A branch label the default would draw, pinned off:
  assert.equal(edgeLabelVisible({ outcome: 'fail', siblingCount: 2, pinned: false }), false);
});

test('no stored opinion falls through to the default rule', () => {
  assert.equal(edgeLabelVisible({ outcome: 'pass', siblingCount: 1 }), false);
  assert.equal(edgeLabelVisible({ outcome: 'pass', siblingCount: 2 }), true);
  assert.equal(edgeLabelVisible({ outcome: 'approved', siblingCount: 1 }), true);
  // Absent is distinct from false: both hide here, but only false survives a
  // sibling appearing.
  assert.equal(edgeLabelVisible({ outcome: 'pass', siblingCount: 2, pinned: false }), false);
});

test('an empty outcome is never drawn, however it is pinned', () => {
  assert.equal(edgeLabelVisible({ outcome: '', siblingCount: 2, pinned: true, selected: true }), false);
});
