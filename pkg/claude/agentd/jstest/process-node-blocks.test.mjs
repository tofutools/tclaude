// Unit tests for the docked node inspector's pure block vocabulary
// (dashboard/js/process-node-blocks.js), run with Node's built-in runner.

import test from 'node:test';
import assert from 'node:assert/strict';
import {
  blocksForKind, visibleBlocks, addableBlocks, removeBlock, orderKeys, moveKey, moveKeyBefore,
  mergeOrder, upstreamInputs, stageSummary,
} from '../dashboard/js/process-node-blocks.js';
import { ProcessEditModel } from '../dashboard/js/process-edit-model.js';

test('each kind lists its valid blocks in default order', () => {
  assert.deepEqual(blocksForKind('task').map((b) => b.key),
    ['performer', 'prompt', 'stages', 'inputs', 'retry', 'captures', 'outcomes', 'bounds', 'about']);
  assert.deepEqual(blocksForKind('decision').map((b) => b.key), ['performer', 'prompt', 'inputs', 'outcomes', 'bounds', 'about']);
  assert.deepEqual(blocksForKind('wait').map((b) => b.key), ['wait', 'outcomes', 'about']);
  assert.deepEqual(blocksForKind('end').map((b) => b.key), ['about']);
});

test('visible blocks: required, present, opened; filtered by performer; in preferred order', () => {
  const node = { type: 'task', performer: { kind: 'agent', timeout: '1h' }, retry: { maxAttempts: 2 } };
  assert.deepEqual(visibleBlocks(node), ['performer', 'retry', 'outcomes', 'bounds']);
  assert.deepEqual(visibleBlocks(node, { opened: ['about'], order: ['bounds', 'nope'] }), ['bounds', 'performer', 'retry', 'outcomes', 'about']);
  const program = { type: 'task', performer: { kind: 'program', run: 'make' } };
  assert.ok(!visibleBlocks(program, { opened: ['prompt'] }).includes('prompt'), 'a program performer has no prompt');
  assert.ok(!addableBlocks(program, visibleBlocks(program)).some((b) => b.key === 'prompt'));
  assert.ok(visibleBlocks({ type: 'task' }, { context: { inputs: { captures: [{ name: 'x' }] } } }).includes('inputs'));
});

test('removing a block clears its slice; required blocks refuse', () => {
  const node = { type: 'task', performer: { kind: 'agent', prompt: 'p', timeout: '1h', contact: { cadence: '5m' } }, plan: { id: 'plan' }, checks: [{ id: 'c' }] };
  removeBlock(node, 'prompt');
  removeBlock(node, 'bounds');
  removeBlock(node, 'stages');
  assert.deepEqual(node, { type: 'task', performer: { kind: 'agent' } });
  assert.throws(() => removeBlock(node, 'performer'), /required/);
  assert.throws(() => removeBlock(node, 'nope'), /unknown block/);
});

test('ordering helpers move, drop and merge without losing hidden slots', () => {
  assert.deepEqual(orderKeys(['a', 'b', 'c'], ['c', 'x', 'c']), ['c', 'a', 'b']);
  assert.deepEqual(moveKey(['a', 'b', 'c'], 'c', -1), ['a', 'c', 'b']);
  assert.deepEqual(moveKey(['a', 'b', 'c'], 'a', -1), ['a', 'b', 'c']);
  assert.deepEqual(moveKeyBefore(['a', 'b', 'c'], 'c', 'a'), ['c', 'a', 'b']);
  assert.deepEqual(moveKeyBefore(['a', 'b', 'c'], 'a', null), ['b', 'c', 'a']);
  assert.deepEqual(mergeOrder('wait', [], ['outcomes', 'wait']), ['outcomes', 'wait', 'about']);
});

test('inputs are the direct predecessors\' captures plus the template params', () => {
  const model = new ProcessEditModel({
    template: {
      id: 't', start: 'a', params: { zeta: {}, alpha: {} },
      nodes: { a: { type: 'task', name: 'A', captures: ['x', 'y'] }, b: { type: 'task', captures: ['z'] }, c: { type: 'task' } },
    },
    edges: [{ from: 'a', outcome: 'pass', to: 'c' }, { from: 'a', outcome: 'fail', to: 'c' }, { from: 'b', outcome: 'pass', to: 'c' }],
    layout: { nodes: {} },
  });
  assert.deepEqual(upstreamInputs(model, 'c'), {
    captures: [{ from: 'a', fromName: 'A', name: 'x' }, { from: 'a', fromName: 'A', name: 'y' }, { from: 'b', fromName: 'b', name: 'z' }],
    params: ['alpha', 'zeta'],
  });
});

test('stage summary reads plan → work → checks → review', () => {
  assert.deepEqual(stageSummary({ performer: { kind: 'agent' } }).map((s) => s.label), ['work']);
  assert.deepEqual(stageSummary({
    plan: { approval: 'human' }, performer: { kind: 'agent' }, checks: [{ id: 'lint' }], review: { performer: { kind: 'human' } },
  }).map((s) => `${s.label}:${s.detail}`), ['plan:human approval', 'work:agent', 'lint:check', 'review:human']);
});
