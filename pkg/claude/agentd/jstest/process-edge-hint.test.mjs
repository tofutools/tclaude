import test from 'node:test';
import assert from 'node:assert/strict';

import {
  EDGE_HINT_STORAGE_KEY, edgeHintText, readEdgeHintDismissed, resolveEdgeHint,
  writeEdgeHintDismissed,
} from '../dashboard/js/process-edge-hint.js';

const A = { from: 'build', outcome: 'pass' };
const labelled = () => true;

test('the hint rides the selected connector', () => {
  assert.deepEqual(resolveEdgeHint({ selected: A, labelled }), { open: true, edge: A });
  assert.deepEqual(resolveEdgeHint({ labelled }), { open: false, edge: null });
});

test('dismissing the hint silences it editor-wide', () => {
  assert.deepEqual(resolveEdgeHint({ dismissed: true, selected: A, labelled }), { open: false, edge: null });
});

test('an unnamed connector carries no hint: there is no key to explain', () => {
  assert.deepEqual(resolveEdgeHint({ selected: A, labelled: () => false }), { open: false, edge: null });
});

test('the hint text names the key and says whether it currently routes', () => {
  const branching = edgeHintText('pass', 2);
  assert.match(branching, /"pass"/);
  assert.match(branching, /Renaming it changes which results come this way/);
  // A named lone connector still shows a label, but the key is not deciding
  // anything yet -- say so rather than overstating the consequence.
  assert.match(edgeHintText('approved', 1), /only way out of this node/);
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
  assert.match(EDGE_HINT_STORAGE_KEY, /^tclaude\.dash\./);
  assert.equal(readEdgeHintDismissed(storage), true);
  assert.equal(writeEdgeHintDismissed(storage, false), true);
  assert.equal(readEdgeHintDismissed(storage), false);

  // dashPrefs is backed by SQLite over the wire, so access can fail; a hint
  // preference must never take the editor down with it.
  const hostile = { getItem() { throw new Error('denied'); }, setItem() { throw new Error('denied'); } };
  assert.equal(readEdgeHintDismissed(hostile), false);
  assert.equal(writeEdgeHintDismissed(hostile, true), false);
  assert.equal(readEdgeHintDismissed(undefined), false);
});
