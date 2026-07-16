import test from 'node:test';
import assert from 'node:assert/strict';
import {
  defaultFeedbackArc, edgeEndpoint, endpointTangent, layoutProcessGraph, rerouteProcessLayout,
  terminalTangent,
} from '../dashboard/js/process-layout.js';

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

test('transient reroute tracks single and multi-node positions without mutating the stable layout', () => {
  const stable = layoutProcessGraph({
    nodes: ['a', 'b', 'c'].map((id) => ({ id, type: 'task' })),
    edges: [{ from: 'a', to: 'b' }, { from: 'b', to: 'c' }],
  });
  const before = JSON.stringify(stable);
  const byNode = byID(stable);
  const transient = rerouteProcessLayout(stable, new Map([
    ['a', { x: byNode.get('a').x + 80, y: byNode.get('a').y + 30 }],
    ['b', { x: byNode.get('b').x + 80, y: byNode.get('b').y + 30 }],
  ]));
  const moved = byID(transient);
  assert.equal(moved.get('a').x, byNode.get('a').x + 80);
  assert.equal(moved.get('b').x, byNode.get('b').x + 80);
  assert.equal(moved.get('c').x, byNode.get('c').x);
  assert.notEqual(transient.edges[0].path, stable.edges[0].path, 'internal dragged edge follows both endpoints');
  assert.notEqual(transient.edges[1].path, stable.edges[1].path, 'external edge follows its dragged endpoint');
  assert.equal(JSON.stringify(stable), before, 'stable layout remains untouched until commit');
});

test('transient reroute invalidates a non-incident long edge when a moved node obstructs it', () => {
  const stable = layoutProcessGraph({
    nodes: ['a', 'b', 'c', 'x'].map((id) => ({ id, type: 'task' })),
    edges: [{ from: 'a', to: 'b' }, { from: 'b', to: 'c' }, { from: 'a', to: 'c' }],
  });
  const long = stable.edges.find((edge) => edge.from === 'a' && edge.to === 'c');
  const waypoint = long.points[Math.floor(long.points.length / 2)];
  const transient = rerouteProcessLayout(stable, new Map([['x', waypoint]]));
  const rerouted = transient.edges.find((edge) => edge.from === 'a' && edge.to === 'c');
  assert.notEqual(rerouted.path, long.path, 'the newly obstructed non-incident edge is rerouted');
  assertRouteAvoids(rerouted, [transient.nodes.find((node) => node.id === 'x')]);
});

test('transient reroute invalidates return lanes when moved nodes change the right bound', () => {
  const stable = layoutProcessGraph({
    nodes: ['a', 'b', 'c', 'x'].map((id) => ({ id, type: 'task' })),
    edges: [{ from: 'a', to: 'b' }, { from: 'b', to: 'c' }, { from: 'c', to: 'a' }],
  });
  const back = stable.edges.find((edge) => edge.back);
  const x = stable.nodes.find((node) => node.id === 'x');
  const transient = rerouteProcessLayout(stable, new Map([[
    'x', { x: stable.bounds.maxX + 500, y: x.y },
  ]]));
  const rerouted = transient.edges.find((edge) => edge.id === back.id);
  const movedX = transient.nodes.find((node) => node.id === 'x');
  assert.notEqual(rerouted.path, back.path, 'return edge follows the expanded graph bound');
  assert.ok(rerouted.points[1].x > movedX.x + movedX.width / 2, 'return lane stays outside moved node');
});

test('transient reroute stays responsive on a representative larger graph', () => {
  const count = 120;
  const layout = layoutProcessGraph({
    nodes: Array.from({ length: count }, (_, index) => ({ id: `n${index}`, type: 'task' })),
    edges: [
      ...Array.from({ length: count - 1 }, (_, index) => ({ from: `n${index}`, to: `n${index + 1}` })),
      ...Array.from({ length: count - 2 }, (_, index) => ({ from: 'n0', to: `n${index + 2}` })),
    ],
  });
  const start = performance.now();
  let frame = layout;
  for (let index = 0; index < 30; index += 1) {
    // Moving the far leaf exercises a long obstacle-routed incident edge while
    // the many unrelated long edges must retain their already-computed routes.
    const node = layout.nodes.find((candidate) => candidate.id === `n${count - 1}`);
    frame = rerouteProcessLayout(layout, new Map([[node.id, { x: node.x + index, y: node.y + index }]]));
  }
  assert.equal(frame.edges.length, (count - 1) + (count - 2));
  assert.equal(
    frame.edges.find((edge) => edge.from === 'n0' && edge.to === 'n60').path,
    layout.edges.find((edge) => edge.from === 'n0' && edge.to === 'n60').path,
    'unrelated long edges retain their stable route',
  );
  assert.notEqual(
    frame.edges.find((edge) => edge.from === 'n0' && edge.to === 'n119').path,
    layout.edges.find((edge) => edge.from === 'n0' && edge.to === 'n119').path,
    'the moved leaf long edge is rerouted',
  );
  assert.ok(performance.now() - start < 2000, '30 transient frames should stay comfortably interactive');
});

test('natural top-down geometry snaps every node shape to its fixed port anchors', () => {
  const shapes = [
    { type: 'task', width: 168, height: 68 },
    { type: 'decision', width: 108, height: 108 },
    { type: 'parallel', width: 108, height: 108 },
    { type: 'wait', width: 78, height: 78 },
    { type: 'start', width: 58, height: 58 },
    { type: 'end', width: 62, height: 62 },
    { type: 'task', compound: { collapsed: true }, width: 190, height: 88 },
  ];
  for (const shape of shapes) {
    const source = { ...shape, x: 100, y: 100 };
    const target = { ...shape, x: 180, y: 260 };
    assert.deepEqual(edgeEndpoint(source, target, true), {
      x: source.x, y: source.y + source.height / 2,
    }, `${shape.compound ? 'compound' : shape.type} output snaps`);
    assert.deepEqual(edgeEndpoint(target, source, false), {
      x: target.x, y: target.y - target.height / 2,
    }, `${shape.compound ? 'compound' : shape.type} input snaps`);
  }
});

test('inverted and sideways geometry retains the shape-boundary fallback', () => {
  const task = { type: 'task', x: 100, y: 100, width: 168, height: 68 };
  assert.deepEqual(edgeEndpoint(task, { x: 100, y: 20 }, true), { x: 100, y: 66 });
  assert.deepEqual(edgeEndpoint(task, { x: 320, y: 140 }, true), { x: 184, y: 115.27272727272728 });

  const decision = { type: 'decision', x: 100, y: 100, width: 108, height: 108 };
  assert.deepEqual(edgeEndpoint(decision, { x: 320, y: 140 }, true), { x: 145.69230769230768, y: 108.3076923076923 });

  const wait = { type: 'wait', x: 100, y: 100, width: 78, height: 78 };
  const sideways = edgeEndpoint(wait, { x: 320, y: 140 }, true);
  assert.ok(sideways.x > 138 && sideways.x < 139);
  assert.ok(sideways.y > 106 && sideways.y < 108);
});

test('rendered terminal tangents align snapped, side, diagonal, and back-edge arrowheads', () => {
  const cases = layoutProcessGraph({
    nodes: [
      { id: 'snap-from', type: 'task', pinned: { x: 100, y: 100 } },
      { id: 'snap-to', type: 'end', pinned: { x: 140, y: 300 } },
      { id: 'side-from', type: 'task', pinned: { x: 420, y: 100 } },
      { id: 'side-to', type: 'end', pinned: { x: 760, y: 130 } },
      { id: 'diagonal-from', type: 'decision', pinned: { x: 390, y: 380 } },
      { id: 'diagonal-to', type: 'wait', pinned: { x: 740, y: 500 } },
    ],
    edges: [
      { id: 'snapped', from: 'snap-from', to: 'snap-to' },
      { id: 'side', from: 'side-from', to: 'side-to' },
      { id: 'diagonal', from: 'diagonal-from', to: 'diagonal-to' },
      { id: 'back', from: 'side-to', to: 'side-from', back: true },
    ],
  });
  const nodes = byID(cases);
  const edges = new Map(cases.edges.map((edge) => [`${edge.from}->${edge.to}`, edge]));

  assert.deepEqual(terminalTangent(edges.get('snap-from->snap-to').points), { x: 0, y: 1 },
    'a top-port arrow retains its downward terminal tangent');
  for (const [label, key] of [['side', 'side-from->side-to'], ['diagonal', 'diagonal-from->diagonal-to']]) {
    const edge = edges.get(key);
    const expected = endpointTangent(nodes.get(edge.to), edge.points.at(-1), false);
    const actual = terminalTangent(edge.points);
    assert.ok(Math.abs(actual.x - expected.x) < 1e-12 && Math.abs(actual.y - expected.y) < 1e-12,
      `${label} arrow follows the target endpoint ray`);
  }
  assert.deepEqual(terminalTangent(edges.get('side-to->side-from').points), { x: -1, y: 0 },
    'the right-hand return lane approaches its target laterally');
});

test('transient side routing keeps the rendered tangent coherent while dragging', () => {
  const stable = layoutProcessGraph({
    nodes: [
      { id: 'from', type: 'task', pinned: { x: 100, y: 100 } },
      { id: 'to', type: 'end', pinned: { x: 380, y: 130 } },
    ],
    edges: [{ from: 'from', to: 'to' }],
  });
  const moved = rerouteProcessLayout(stable, new Map([['to', { x: 460, y: 170 }]]));
  const target = moved.nodes.find((node) => node.id === 'to');
  const edge = moved.edges[0];
  const actual = terminalTangent(edge.points);
  const expected = endpointTangent(target, edge.points.at(-1), false);
  assert.ok(Math.abs(actual.x - expected.x) < 1e-12 && Math.abs(actual.y - expected.y) < 1e-12);
});

test('parallel fallback edges keep distinct lanes without changing their terminal tangents', () => {
  const result = layoutProcessGraph({
    nodes: [
      { id: 'from', type: 'task', pinned: { x: 100, y: 100 } },
      { id: 'to', type: 'end', pinned: { x: 420, y: 130 } },
    ],
    edges: [
      { from: 'from', to: 'to', outcome: 'yes' },
      { from: 'from', to: 'to', outcome: 'no' },
    ],
  });
  const target = result.nodes.find((node) => node.id === 'to');
  assert.equal(new Set(result.edges.map((edge) => edge.path)).size, 2, 'parallel routes do not overlap');
  assert.equal(new Set(result.edges.map((edge) => `${edge.label.x},${edge.label.y}`)).size, 2,
    'parallel labels retain their lane separation');
  for (const edge of result.edges) {
    const actual = terminalTangent(edge.points);
    const expected = endpointTangent(target, edge.points.at(-1), false);
    assert.ok(Math.abs(actual.x - expected.x) < 1e-12 && Math.abs(actual.y - expected.y) < 1e-12);
  }
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

test('overlapping pin obstacle that culls a route endpoint never throws', () => {
  const graph = {
    nodes: [
      { id: 'start', type: 'task', pinned: { x: 100, y: 100 } },
      { id: 'overlap', type: 'task', pinned: { x: 110, y: 105 } },
      { id: 'mid', type: 'task', pinned: { x: 260, y: 260 } },
      { id: 'end', type: 'end', pinned: { x: 420, y: 420 } },
    ],
    edges: [
      { from: 'start', to: 'mid' }, { from: 'mid', to: 'end' },
      { from: 'start', to: 'end', outcome: 'skip' },
    ],
  };
  let result;
  assert.doesNotThrow(() => { result = layoutProcessGraph(graph); });
  assert.equal(result.edges.length, 3);
  result.edges.forEach((edge) => assert.match(edge.path, /^M /));
});

test('coincident multi-rank endpoints produce a visible fallback route', () => {
  const result = layoutProcessGraph({
    nodes: [
      { id: 'start', type: 'task', pinned: { x: 100, y: 100 } },
      { id: 'mid', type: 'task', pinned: { x: 240, y: 240 } },
      { id: 'end', type: 'end', pinned: { x: 100, y: 100 } },
    ],
    edges: [
      { from: 'start', to: 'mid' }, { from: 'mid', to: 'end' },
      { from: 'start', to: 'end', outcome: 'skip' },
    ],
  });
  const skip = result.edges.find((edge) => edge.outcome === 'skip');
  assert.ok(skip.points.length >= 4);
  assert.ok(new Set(skip.points.map((point) => `${point.x},${point.y}`)).size > 1);
});

test('deterministic overlap fuzz keeps layout total', () => {
  let seed = 0x293;
  const next = () => {
    seed = (seed * 1664525 + 1013904223) >>> 0;
    return seed;
  };
  for (let iteration = 0; iteration < 80; iteration += 1) {
    const nodes = Array.from({ length: 7 }, (_, index) => ({
      id: `n${index}`, type: index === 6 ? 'end' : 'task',
      pinned: { x: 100 + next() % 45, y: 100 + next() % 45 },
    }));
    const edges = Array.from({ length: 6 }, (_, index) => ({ from: `n${index}`, to: `n${index + 1}` }));
    edges.push({ from: 'n0', to: 'n6', outcome: 'skip' });
    assert.doesNotThrow(() => layoutProcessGraph({ nodes, edges }), `overlap fuzz iteration ${iteration}`);
  }
});

test('self-loop is classified and rendered as a visible return arc', () => {
  const result = layoutProcessGraph({
    nodes: [{ id: 'again', type: 'task' }],
    edges: [{ from: 'again', to: 'again', outcome: 'retry' }],
  });
  assert.equal(result.edges[0].kind, 'back');
  assert.match(result.edges[0].path, / C /);
  assert.ok(new Set(result.edges[0].points.map((point) => `${point.x},${point.y}`)).size > 1);
});

test('initial layer order is total for mixed pinned and automatic nodes', () => {
  const nodes = [
    { id: 'a', type: 'task', pinned: { x: 300, y: 100 } },
    { id: 'm', type: 'task' },
    { id: 'z', type: 'task', pinned: { x: 100, y: 100 } },
  ];
  const forward = layoutProcessGraph({ nodes, edges: [] });
  const reverse = layoutProcessGraph({ nodes: [...nodes].reverse(), edges: [] });
  assert.deepEqual(forward.layers, [['z', 'a', 'm']]);
  assert.deepEqual(reverse.layers, forward.layers);
  assert.deepEqual(reverse.nodes.map(({ id, x, y }) => ({ id, x, y })),
    forward.nodes.map(({ id, x, y }) => ({ id, x, y })));
});

test('crossing reduction improves a provably crossed identity ordering', () => {
  const result = layoutProcessGraph({
    nodes: ['a', 'b', 'x', 'y'].map((id) => ({ id, type: 'task' })),
    edges: [{ from: 'a', to: 'y' }, { from: 'b', to: 'x' }],
  });
  const sourceOrder = new Map(result.layers[0].map((id, index) => [id, index]));
  const targetOrder = new Map(result.layers[1].map((id, index) => [id, index]));
  const [first, second] = result.edges;
  const crossed = (sourceOrder.get(first.from) - sourceOrder.get(second.from))
    * (targetOrder.get(first.to) - targetOrder.get(second.to)) < 0;
  assert.equal(crossed, false);
  assert.notDeepEqual(result.layers, [['a', 'b'], ['x', 'y']], 'identity ordering has one crossing');
});

test('feedback-arc DFS handles a deep chain without recursion overflow', () => {
  const count = 20000;
  const nodes = Array.from({ length: count }, (_, index) => ({ id: `n${index}` }));
  const edges = Array.from({ length: count - 1 }, (_, index) => ({
    from: `n${index}`, to: `n${index + 1}`, inputIndex: index, key: String(index), back: false,
  }));
  assert.equal(defaultFeedbackArc(nodes, edges).size, 0);
});
