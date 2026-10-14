// Unit tests for composition on the canvas (E2): splice onto a connector,
// wrap a selection as a ▣ subprocess and unwrap it again, and editing an
// inline-expanded body in context (process-compose.js + the model's
// expansion projection).

import test from 'node:test';
import assert from 'node:assert/strict';
import { ProcessEditModel } from '../dashboard/js/process-edit-model.js';
import { spliceNode, wrapSelection, wrapPlan, unwrapSubprocess, insertNode } from '../dashboard/js/process-compose.js';

function chain() {
  return {
    template: {
      id: 'flow', name: 'Flow', start: 'a', params: { branch: { type: 'string' } },
      nodes: {
        a: { type: 'task', name: 'Plan', performer: { kind: 'agent' } },
        b: { type: 'task', name: 'Build', performer: { kind: 'program', run: 'make {{ params.branch }}' } },
        c: { type: 'decision', name: 'Green?', performer: { kind: 'human', ask: 'ok?' } },
        d: { type: 'end', result: 'success' },
        e: { type: 'end', result: 'failure' },
      },
    },
    edges: [
      { from: '', outcome: 'start', to: 'a' },
      { from: 'a', outcome: 'pass', to: 'b' },
      { from: 'b', outcome: 'pass', to: 'c' },
      { from: 'c', outcome: 'yes', to: 'd' },
      { from: 'c', outcome: 'no', to: 'e' },
    ],
    layout: { nodes: { a: { x: 0, y: 0 }, b: { x: 200, y: 0 }, c: { x: 400, y: 0 }, d: { x: 600, y: 0 }, e: { x: 600, y: 120 } } },
    currentRef: 'flow@sha256:1', sourceHash: 'h', semanticHash: 's',
  };
}

const edgeList = (edges) => edges.map((e) => `${e.from}:${e.outcome}>${e.to}`).sort();

test('splicing onto a connector inserts the node between its ends in one undo step', () => {
  const model = new ProcessEditModel(chain());
  const id = spliceNode(model, { from: 'a', outcome: 'pass' }, 'task', { x: 100, y: 40 });
  assert.equal(id, 'task');
  assert.ok(edgeList(model.edges).includes('a:pass>task'));
  assert.ok(edgeList(model.edges).includes('task:pass>b'));
  assert.deepEqual(model.layout.nodes.task, { x: 100, y: 40 });
  assert.equal(model.undoStack.length, 1);
  model.undo();
  assert.deepEqual(edgeList(model.edges), edgeList(chain().edges));
  assert.throws(() => spliceNode(model, { from: 'a', outcome: 'pass' }, 'end'), /cannot be spliced/);
  const ref = spliceNode(model, { from: 'b', outcome: 'pass' }, 'subprocess', { name: 'QA suite', init: (node) => { node.subprocess = { templateRef: 'qa-suite' }; } });
  assert.deepEqual(model.node(ref).subprocess, { templateRef: 'qa-suite' });
  assert.equal(model.node(ref).name, 'QA suite');
});

test('wrapping derives the start, out-ports and params from the boundary', () => {
  const model = new ProcessEditModel(chain());
  const plan = wrapPlan(model, ['b', 'c']);
  assert.equal(plan.entry, 'b');
  assert.deepEqual(plan.exits.map((exit) => [exit.target, exit.outcome]), [['e', 'no'], ['d', 'yes']]);
  assert.deepEqual(plan.params, ['branch']);
  const id = wrapSelection(model, ['b', 'c'], { name: 'Build and check' });
  assert.equal(id, 'build-and-check');
  const sub = model.node(id).subprocess;
  assert.equal(sub.start, 'b');
  assert.deepEqual(sub.nodes.c.next, { no: 'to-e', yes: 'to-d' });
  assert.deepEqual(sub.outputs, { 'to-d': 'yes', 'to-e': 'no' });
  assert.deepEqual(sub.inputs, { branch: '{{ params.branch }}' });
  assert.deepEqual(sub.params, { branch: { type: 'string' } });
  assert.deepEqual(edgeList(model.edges), [':start>a', 'a:pass>build-and-check', 'build-and-check:no>e', 'build-and-check:yes>d']);
  assert.deepEqual(model.layout.nodes['build-and-check/b'], { x: 200, y: 0 }, 'pins move into the body');
  assert.deepEqual(model.layout.nodes['build-and-check'], { x: 300, y: 0 });
  assert.equal(model.undoStack.length, 1);
  assert.throws(() => wrapSelection(model, ['a', 'd']), /entered at 2 nodes|one start/);
});

test('unwrap restores what wrap collapsed', () => {
  const original = new ProcessEditModel(chain());
  const model = new ProcessEditModel(chain());
  const id = wrapSelection(model, ['b', 'c'], { name: 'Build and check' });
  const ids = unwrapSubprocess(model, id);
  assert.deepEqual(ids.sort(), ['b', 'c']);
  assert.deepEqual(edgeList(model.edges), edgeList(original.edges));
  assert.deepEqual(model.saveBody().template.nodes, original.saveBody().template.nodes);
  assert.deepEqual(model.layout.nodes.b, { x: 200, y: 0 }, 'pins come back');
  model.undo();
  assert.ok(model.node(id), 'unwrap is one undo step');
});

test('unwrapping a fresh ▣ skips its start marker and substitutes bound params', () => {
  const model = new ProcessEditModel(chain());
  const id = insertNode(model, 'subprocess', { x: 0, y: 200 });
  model.addEdge('a', 'other', id);
  model.addEdge(id, 'pass', 'd');
  model.updateNode(id, (node) => {
    node.subprocess.params = { target: { type: 'string' } };
    node.subprocess.inputs = { target: 'main' };
    node.subprocess.nodes.work = { type: 'task', performer: { kind: 'program', run: 'deploy {{ params.target }}' } };
    node.subprocess.nodes.start.next = { pass: 'work' };
    node.subprocess.nodes.work.next = { pass: 'end' };
  });
  unwrapSubprocess(model, id);
  assert.equal(model.node('start'), undefined, 'the start marker is not inlined');
  assert.equal(model.node('work').performer.run, 'deploy main');
  assert.ok(edgeList(model.edges).includes('a:other>work'));
  assert.ok(edgeList(model.edges).includes('work:pass>d'), 'the mapped end dissolves into its target');
});

test('an expanded body is drawn in place and edited in context', () => {
  const model = new ProcessEditModel(chain());
  const id = wrapSelection(model, ['b', 'c'], { name: 'QA' });
  model.expandSubprocess(id, { x: 300, y: 0 });
  const graph = model.graph();
  assert.deepEqual(graph.groups.map((g) => [g.id, g.nodes.sort()]), [['qa', ['qa/b', 'qa/c', 'qa/to-d', 'qa/to-e']]]);
  assert.ok(!graph.nodes.some((n) => n.id === 'qa'), 'the ▣ node gives way to its frame');
  const into = graph.edges.find((e) => e.from === 'a');
  assert.equal(into.to, 'qa/b', 'connectors into the ▣ land on the body start');
  const out = graph.edges.find((e) => e.to === 'd');
  assert.deepEqual([out.from, out.source], ['qa/to-d', { from: 'qa', outcome: 'yes' }]);

  model.renameNode('qa/b', 'Build it');
  assert.equal(model.node('qa/b').name, 'Build it');
  assert.equal(model.saveBody().template.nodes.qa.subprocess.nodes.b.name, 'Build it');
  const before = model.layout.nodes['qa/b'];
  const drawn = model.graph().nodes.find((n) => n.id === 'qa/b').pinned;
  model.moveNode('qa/b', drawn.x + 10, drawn.y);
  assert.deepEqual(model.layout.nodes['qa/b'], { x: before.x + 10, y: before.y }, 'moves store body coordinates');
  assert.throws(() => model.addEdge('a', 'x', 'qa/c'), /cannot cross a subprocess frame/);
  model.deleteItems([{ type: 'node', id: 'qa/to-e' }]);
  assert.equal(model.node('qa/to-e'), undefined);
  model.undo(); model.undo(); model.undo();
  assert.equal(model.node('qa/b').name, 'Build');
  assert.ok(model.isExpanded('qa'), 'undo keeps the expansion');
  model.collapseSubprocess('qa');
  assert.ok(model.graph().nodes.some((n) => n.id === 'qa'));
});
