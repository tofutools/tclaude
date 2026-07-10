import test from 'node:test';
import assert from 'node:assert/strict';
import {
  makeSelection, nodesInMarquee, normalizeMarquee, selectionContains, selectionItems, toggleSelection,
} from '../dashboard/js/process-selection.js';

test('selection grows from single to deduplicated set and collapses again', () => {
  const one = makeSelection([{ type: 'node', id: 'a' }]);
  assert.deepEqual(one, { type: 'node', id: 'a' });
  const many = toggleSelection(one, { type: 'node', id: 'b' });
  assert.deepEqual(selectionItems(many), [{ type: 'node', id: 'a' }, { type: 'node', id: 'b' }]);
  assert.equal(selectionContains(many, { type: 'node', id: 'a' }), true);
  assert.deepEqual(toggleSelection(many, { type: 'node', id: 'a' }), { type: 'node', id: 'b' });
  assert.equal(toggleSelection({ type: 'node', id: 'b' }, { type: 'node', id: 'b' }), null);
});

test('edge identity remains distinct when separators occur in semantic fields', () => {
  const selected = makeSelection([
    { type: 'edge', from: 'a:b', outcome: 'c' },
    { type: 'edge', from: 'a', outcome: 'b:c' },
  ]);
  assert.equal(selectionItems(selected).length, 2);
});

test('marquee math normalizes reverse drags and selects intersecting nodes', () => {
  assert.deepEqual(normalizeMarquee({ x: 90, y: 80 }, { x: 10, y: 20 }), {
    left: 10, right: 90, top: 20, bottom: 80,
  });
  const nodes = [
    { id: 'inside', x: 50, y: 50, width: 20, height: 20 },
    { id: 'touching', x: 100, y: 50, width: 20, height: 20 },
    { id: 'outside', x: 130, y: 50, width: 20, height: 20 },
  ];
  assert.deepEqual(nodesInMarquee(nodes, { x: 90, y: 80 }, { x: 10, y: 20 }).map((node) => node.id),
    ['inside', 'touching']);
});
