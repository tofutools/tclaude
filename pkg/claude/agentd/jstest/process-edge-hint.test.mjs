import test from 'node:test';
import assert from 'node:assert/strict';

import {
  EDGE_HINT_STORAGE_KEY, readEdgeHintDismissed, resolveEdgeHint, writeEdgeHintDismissed,
} from '../dashboard/js/process-edge-hint.js';

const A = { from: 'build', outcome: 'pass' };
const B = { from: 'build', outcome: 'fail' };
const labelled = () => true;

test('pinned by default: selection shows the hint without a hover', () => {
  assert.deepEqual(resolveEdgeHint({ selected: A, labelled }), { open: true, edge: A, pinned: true });
  assert.equal(resolveEdgeHint({ labelled }).open, false);
});

test('selection wins over hover so the hint does not flee the pointer', () => {
  assert.equal(resolveEdgeHint({ selected: A, hovered: B, labelled }).edge, A);
});

test('once dismissed the hint is hover-only', () => {
  assert.equal(resolveEdgeHint({ dismissed: true, selected: A, labelled }).open, false);
  const hovered = resolveEdgeHint({ dismissed: true, hovered: B, labelled });
  assert.deepEqual(hovered, { open: true, edge: B, pinned: false });
});

test('a dismissed hint survives the pointer moving onto the bubble itself', () => {
  const onBubble = resolveEdgeHint({ dismissed: true, selected: A, hintHovered: true, labelled });
  assert.equal(onBubble.open, true);
  assert.equal(onBubble.edge, A);
});

test('unlabelled connectors never carry a hint', () => {
  const none = resolveEdgeHint({ selected: A, labelled: () => false });
  assert.deepEqual(none, { open: false, edge: null, pinned: true });
});

test('dismissal persists and tolerates hostile storage', () => {
  const store = new Map();
  const storage = {
    getItem: (k) => (store.has(k) ? store.get(k) : null),
    setItem: (k, v) => store.set(k, v),
    removeItem: (k) => store.delete(k),
  };
  assert.equal(readEdgeHintDismissed(storage), false);
  assert.equal(writeEdgeHintDismissed(storage, true), true);
  assert.equal(store.get(EDGE_HINT_STORAGE_KEY), 'dismissed');
  assert.equal(readEdgeHintDismissed(storage), true);
  assert.equal(writeEdgeHintDismissed(storage, false), true);
  assert.equal(readEdgeHintDismissed(storage), false);

  // Safari private mode and embedded webviews throw on access; a hint
  // preference must never take the editor down with it.
  const hostile = { getItem() { throw new Error('denied'); }, setItem() { throw new Error('denied'); } };
  assert.equal(readEdgeHintDismissed(hostile), false);
  assert.equal(writeEdgeHintDismissed(hostile, true), false);
  assert.equal(readEdgeHintDismissed(undefined), false);
});
