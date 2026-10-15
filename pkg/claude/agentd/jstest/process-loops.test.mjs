// Unit tests for the bounded-loop helpers (dashboard/js/process-loops.js).

import test from 'node:test';
import assert from 'node:assert/strict';
import {
  cycleFixEdge, edgeGoesBack, exhaustionChoices, loopBound, loopEdgeSuffix, loopSummary, loopableEdge,
  normalizeRounds, setLoopBound,
} from '../dashboard/js/process-loops.js';

const nodes = () => ({
  implement: { type: 'task' },
  review: { type: 'task' },
  decide: { type: 'decision' },
  merge: { type: 'task' },
});
const edges = [
  { from: '', outcome: 'start', to: 'implement' },
  { from: 'implement', outcome: 'next', to: 'review' },
  { from: 'review', outcome: 'next', to: 'decide' },
  { from: 'decide', outcome: 'changes', to: 'implement' },
  { from: 'decide', outcome: 'approved', to: 'merge' },
];

test('edgeGoesBack finds the edge closing a cycle, bounded or not', () => {
  const graph = nodes();
  assert.equal(edgeGoesBack(graph, edges, 'decide', 'changes'), true);
  assert.equal(edgeGoesBack(graph, edges, 'decide', 'approved'), false);
  assert.equal(edgeGoesBack(graph, edges, 'implement', 'next'), false, 'an edge on the cycle that does not close it goes forward');
  setLoopBound(graph.decide, 'changes', { maxIterations: 3 });
  assert.equal(edgeGoesBack(graph, edges, 'decide', 'changes'), true, 'a bounded edge still goes back');
  assert.equal(edgeGoesBack(graph, edges, 'decide', 'changes'), true);
  assert.equal(loopableEdge(graph, edges, 'decide', 'changes'), true);
  assert.equal(loopableEdge(graph, edges, 'implement', 'next'), false);
  assert.equal(edgeGoesBack(graph, [...edges, { from: 'decide', outcome: 'wait', to: 'decide' }], 'decide', 'wait'), false,
    'a self edge is no loop');
});

test('bounds are written, read, and removed without leaving an empty loops map', () => {
  const node = { type: 'decision' };
  setLoopBound(node, 'changes', { maxIterations: 3, onExhausted: 'escalate' });
  setLoopBound(node, 'redo', { maxIterations: 2, onExhausted: '' });
  assert.deepEqual(node.loops, { changes: { maxIterations: 3, onExhausted: 'escalate' }, redo: { maxIterations: 2 } });
  assert.deepEqual(loopBound(node, 'redo'), { maxIterations: 2, onExhausted: '' });
  assert.deepEqual(exhaustionChoices(node, ['approved', 'changes', 'escalate', 'redo'], 'changes'), ['approved', 'escalate']);
  setLoopBound(node, 'changes', null);
  setLoopBound(node, 'redo', null);
  assert.equal(node.loops, undefined);
  assert.equal(loopBound(node, 'changes'), null);
});

test('round counts and labels', () => {
  assert.equal(normalizeRounds(' 3 '), 3);
  assert.equal(normalizeRounds('1'), null);
  assert.equal(normalizeRounds('101'), null);
  assert.equal(normalizeRounds('2.5'), null);
  assert.equal(normalizeRounds(''), null);
  assert.equal(loopEdgeSuffix(3), '≤3×');
  assert.equal(loopEdgeSuffix(3, 2), 'round 2 of 3');
  assert.equal(loopEdgeSuffix(0), '');
  assert.equal(loopSummary({ maxIterations: 3, onExhausted: 'escalate' }), 'max 3 rounds, then escalate');
  assert.equal(loopSummary({ maxIterations: 2, onExhausted: '' }), 'max 2 rounds');
});

test('with two decisions on one cycle, only the edge closing it from the start goes back', () => {
  // start -> approve -go-> work -> zcheck -redo-> approve. Bounding approve's
  // "go" would make a loop entered at approve, past its top at work.
  const graph = { approve: { type: 'decision' }, work: { type: 'task' }, zcheck: { type: 'decision' }, end: { type: 'end' } };
  const cycle = [
    { from: '', outcome: 'start', to: 'approve' },
    { from: 'approve', outcome: 'go', to: 'work' },
    { from: 'approve', outcome: 'no', to: 'end' },
    { from: 'work', outcome: 'next', to: 'zcheck' },
    { from: 'zcheck', outcome: 'redo', to: 'approve' },
    { from: 'zcheck', outcome: 'ok', to: 'end' },
  ];
  assert.equal(loopableEdge(graph, cycle, 'approve', 'go'), false);
  assert.equal(loopableEdge(graph, cycle, 'zcheck', 'redo'), true);
  for (const id of ['approve', 'work', 'zcheck']) {
    assert.deepEqual(cycleFixEdge(graph, cycle, id), { from: 'zcheck', outcome: 'redo' });
  }
});
