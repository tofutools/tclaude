import test from 'node:test';
import assert from 'node:assert/strict';
import { layoutProcessGraph } from '../dashboard/js/process-layout.js';

const linear = {
  nodes: [
    { id: 'start', type: 'start', label: 'Start' },
    { id: 'work', type: 'task', label: 'Do the work' },
    { id: 'wait', type: 'wait', label: 'Wait' },
    { id: 'end', type: 'end', label: 'Done' },
  ],
  edges: [
    { from: 'start', to: 'work' },
    { from: 'work', to: 'wait' },
    { from: 'wait', to: 'end' },
  ],
};

function byID(layout) {
  return new Map(layout.nodes.map((node) => [node.id, node]));
}

function overlaps(a, b) {
  return Math.abs(a.x - b.x) < (a.width + b.width) / 2
    && Math.abs(a.y - b.y) < (a.height + b.height) / 2;
}

function segmentCrossesNode(a, b, node) {
  const left = node.x - node.width / 2;
  const right = node.x + node.width / 2;
  const top = node.y - node.height / 2;
  const bottom = node.y + node.height / 2;
  if (a.x === b.x) return a.x > left && a.x < right && Math.max(a.y, b.y) > top && Math.min(a.y, b.y) < bottom;
  return a.y > top && a.y < bottom && Math.max(a.x, b.x) > left && Math.min(a.x, b.x) < right;
}

function assertRouteAvoids(edge, nodes) {
  for (let i = 1; i < edge.points.length; i += 1) {
    for (const node of nodes) {
      assert.equal(segmentCrossesNode(edge.points[i - 1], edge.points[i], node), false,
        `${edge.from} -> ${edge.to} segment ${i - 1} crosses ${node.id}`);
    }
  }
}

test('forward edges advance to a downstream layer and route with arrow paths', () => {
  const result = layoutProcessGraph(linear);
  const nodes = byID(result);
  for (const edge of result.edges) {
    assert.equal(edge.kind, 'forward');
    assert.ok(nodes.get(edge.to).layer > nodes.get(edge.from).layer, `${edge.from} -> ${edge.to} must advance layers`);
    assert.ok(edge.path.startsWith('M '));
    assert.ok(edge.points.at(-1).y > edge.points[0].y, `${edge.from} -> ${edge.to} must point down`);
  }
});

test('sanctioned retry back-edge is excluded from layering and routed distinctly', () => {
  const result = layoutProcessGraph({
    nodes: [
      { id: 'implement', type: 'task' },
      { id: 'review', type: 'decision' },
      { id: 'escalate', type: 'task' },
      { id: 'done', type: 'end' },
    ],
    edges: [
      { from: 'implement', to: 'review' },
      { from: 'review', to: 'done', outcome: 'approved' },
      { from: 'review', to: 'escalate', outcome: 'poison' },
      { from: 'escalate', to: 'implement', outcome: 'retry', back: true },
    ],
  });
  const retry = result.edges.find((edge) => edge.back);
  assert.ok(retry);
  assert.equal(retry.kind, 'back');
  assert.equal(retry.outcome, 'retry');
  assert.match(retry.path, /^M .* C /);
  assert.ok(retry.points[1].x > Math.max(...result.nodes.map((node) => node.x + node.width / 2)), 'return lane routes outside nodes');
  assert.equal(byID(result).get('implement').layer, 0, 'retry edge must not push its target downstream');
});

test('decision fan-out and join do not overlap nodes', () => {
  const result = layoutProcessGraph({
    nodes: [
      { id: 'start', type: 'start' },
      { id: 'choice', type: 'decision' },
      { id: 'left', type: 'task' },
      { id: 'right', type: 'task' },
      { id: 'join', type: 'task' },
      { id: 'end', type: 'end' },
    ],
    edges: [
      { from: 'start', to: 'choice' },
      { from: 'choice', to: 'left', outcome: 'yes' },
      { from: 'choice', to: 'right', outcome: 'no' },
      { from: 'left', to: 'join', joinOnTarget: 'all' },
      { from: 'right', to: 'join', joinOnTarget: 'all' },
      { from: 'join', to: 'end' },
    ],
  });
  for (let i = 0; i < result.nodes.length; i += 1) {
    for (let j = i + 1; j < result.nodes.length; j += 1) {
      assert.equal(overlaps(result.nodes[i], result.nodes[j]), false, `${result.nodes[i].id} overlaps ${result.nodes[j].id}`);
    }
  }
});

test('pinned nodes retain exact editor-owned coordinates and auto nodes avoid them', () => {
  const result = layoutProcessGraph({
    nodes: [
      { id: 'a', type: 'start' },
      { id: 'pinned', type: 'task', pinned: { x: 156, y: 272 } },
      { id: 'auto', type: 'task' },
      { id: 'z', type: 'end' },
    ],
    edges: [
      { from: 'a', to: 'pinned' },
      { from: 'pinned', to: 'auto' },
      { from: 'auto', to: 'z' },
    ],
  });
  const nodes = byID(result);
  assert.deepEqual({ x: nodes.get('pinned').x, y: nodes.get('pinned').y }, { x: 156, y: 272 });
  assert.equal(nodes.get('pinned').pinned, true);
  assert.equal(overlaps(nodes.get('pinned'), nodes.get('auto')), false);
});

test('long forward edges route outside intermediate ranks', () => {
  const result = layoutProcessGraph({
    nodes: ['a', 'b', 'c', 'd'].map((id) => ({ id, type: 'task' })),
    edges: [
      { from: 'a', to: 'b' }, { from: 'b', to: 'c' },
      { from: 'c', to: 'd' }, { from: 'a', to: 'd', outcome: 'skip' },
    ],
  });
  const long = result.edges.find((edge) => edge.from === 'a' && edge.to === 'd');
  const intermediates = result.nodes.filter((node) => node.id === 'b' || node.id === 'c');
  assertRouteAvoids(long, intermediates);
  assert.match(long.path, / L /, 'long edge uses explicit obstacle waypoints');
});

test('long edge escape avoids siblings on its source and target ranks', () => {
  const result = layoutProcessGraph({
    nodes: ['a', 'z', 'b', 'c', 'd'].map((id) => ({ id, type: 'task' })),
    edges: [
      { from: 'z', to: 'b' }, { from: 'b', to: 'c' },
      { from: 'c', to: 'd' }, { from: 'z', to: 'd', outcome: 'skip' },
    ],
  });
  const long = result.edges.find((edge) => edge.from === 'z' && edge.to === 'd');
  assertRouteAvoids(long, result.nodes.filter((node) => node.id !== 'z' && node.id !== 'd'));
});

test('pinned visual inversion exits and enters the facing rectangle sides', () => {
  const result = layoutProcessGraph({
    nodes: [
      { id: 'lower-source', type: 'task', pinned: { x: 240, y: 480 } },
      { id: 'upper-target', type: 'task', pinned: { x: 240, y: 120 } },
    ],
    edges: [{ from: 'lower-source', to: 'upper-target' }],
  });
  const nodes = byID(result);
  const edge = result.edges[0];
  assert.equal(edge.points[0].y, nodes.get('lower-source').y - nodes.get('lower-source').height / 2, 'source exits its top face');
  assert.equal(edge.points.at(-1).y, nodes.get('upper-target').y + nodes.get('upper-target').height / 2, 'target enters its bottom face');
});

test('same input produces byte-for-byte deterministic layout output', () => {
  const graph = {
    nodes: [
      { id: 'c', type: 'end' },
      { id: 'a', type: 'start' },
      { id: 'b2', type: 'task' },
      { id: 'b1', type: 'task', compound: { stages: ['one', 'two'], collapsed: true } },
    ],
    edges: [
      { from: 'b2', to: 'c' },
      { from: 'a', to: 'b2' },
      { from: 'a', to: 'b1' },
      { from: 'b1', to: 'c' },
    ],
  };
  assert.equal(JSON.stringify(layoutProcessGraph(graph)), JSON.stringify(layoutProcessGraph(graph)));
});

test('semantic edge identity survives unrelated insertion and reorder', () => {
  const nodes = ['a', 'b', 'c'].map((id) => ({ id, type: 'task' }));
  const before = layoutProcessGraph({ nodes, edges: [{ from: 'a', to: 'b' }, { from: 'b', to: 'c' }] });
  const identity = before.edges.find((edge) => edge.from === 'b').id;
  const after = layoutProcessGraph({ nodes, edges: [
    { from: 'a', to: 'c', outcome: 'new' }, { from: 'b', to: 'c' }, { from: 'a', to: 'b' },
  ] });
  assert.equal(after.edges.find((edge) => edge.from === 'b').id, identity);
});

test('duplicate explicit edge IDs are rejected before interaction identity can alias', () => {
  assert.throws(() => layoutProcessGraph({
    nodes: ['a', 'b', 'c'].map((id) => ({ id, type: 'task' })),
    edges: [
      { id: 'same', from: 'a', to: 'b' },
      { id: 'same', from: 'b', to: 'c' },
    ],
  }), /duplicate edge id same/);
});

test('cycle-breaking heuristic stays isolated at the feedback-arc seam', () => {
  let called = 0;
  const result = layoutProcessGraph({
    nodes: [{ id: 'a', type: 'task' }, { id: 'b', type: 'task' }],
    edges: [{ from: 'a', to: 'b' }, { from: 'b', to: 'a' }],
  }, {
    feedbackArc(nodes, edges) {
      called += 1;
      assert.equal(nodes.length, 2);
      return new Set([edges.find((edge) => edge.from === 'b').inputIndex]);
    },
  });
  assert.equal(called, 1);
  assert.equal(result.edges.filter((edge) => edge.back).length, 1);
});
