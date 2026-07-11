import test from 'node:test';
import assert from 'node:assert/strict';
import { ProcessGraph, normalizeWheelDelta } from '../dashboard/js/process-graph.js';

test('wheel delta modes normalize to useful pixel-scale zoom input', () => {
  assert.equal(normalizeWheelDelta(120, 0, 900), 120);
  assert.equal(normalizeWheelDelta(3, 1, 900), 72);
  assert.equal(normalizeWheelDelta(1, 2, 900), 900);
  assert.equal(normalizeWheelDelta(Number.NaN, 0, 900), 0);
});

// onPointerCancel is exercised on a hand-rolled `this` (no DOM needed): a
// browser-cancelled gesture must end an in-flight port drag with
// cancelled: true and NO hit-testing — a cancelled touch/pen drag whose last
// position sits over another node must never read as a deliberate drop.
test('pointercancel ends a port drag cancelled, with no hit-testing', () => {
  const ends = [];
  const fake = {
    pointer: { id: 7, mode: 'port', nodeID: 'a', port: 'out' },
    options: { onPortDragEnd: (payload) => ends.push(payload) },
    svg: { releasePointerCapture() {} },
    dragMoved: true,
    clientToGraph() { return { x: 4, y: 5 }; },
  };
  ProcessGraph.prototype.onPointerCancel.call(fake, { pointerId: 7, clientX: 0, clientY: 0 });
  assert.equal(ends.length, 1);
  assert.equal(ends[0].cancelled, true);
  assert.equal(ends[0].targetNodeId, null);
  assert.equal(ends[0].targetPort, null);
  assert.equal(fake.pointer, null, 'the drag is over');
  ProcessGraph.prototype.onPointerCancel.call(fake, { pointerId: 7 });
  assert.equal(ends.length, 1, 'a second cancel for the cleared pointer is a no-op');
});

test('pointercancel snaps a node drag home instead of committing it', () => {
  const snapped = [];
  const fake = {
    pointer: { id: 3, mode: 'node', nodeID: 'n1' },
    options: {},
    svg: { releasePointerCapture() {} },
    dragMoved: true,
    snapNodeHome(id) { snapped.push(id); },
    snapNodesHome(ids) { ids.forEach((id) => this.snapNodeHome(id)); },
    restoreTransientEdges() {},
  };
  ProcessGraph.prototype.onPointerCancel.call(fake, { pointerId: 3 });
  assert.deepEqual(snapped, ['n1']);
  assert.equal(fake.pointer, null);
});

test('pointercancel for a foreign pointer id leaves the drag alone', () => {
  const fake = {
    pointer: { id: 3, mode: 'port', nodeID: 'n1', port: 'out' },
    options: { onPortDragEnd: () => { throw new Error('must not fire'); } },
    svg: { releasePointerCapture() {} },
    dragMoved: false,
    clientToGraph() { return { x: 0, y: 0 }; },
  };
  ProcessGraph.prototype.onPointerCancel.call(fake, { pointerId: 99 });
  assert.ok(fake.pointer, 'the in-flight drag survives');
});

test('canvas pointerdown focuses the graph so editor Delete receives keyboard events', () => {
  let focused = 0;
  const fake = {
    root: { focus(options) { focused += 1; assert.equal(options.preventScroll, true); } },
    options: {},
    selected: null,
    view: { x: 0, y: 0, k: 1 },
    svg: { setPointerCapture() {} },
    viewport: { append() {} },
    eventTarget() { return { node: null, edge: null, port: null }; },
    clientToGraph() { return { x: 12, y: 34 }; },
  };
  let prevented = false;
  ProcessGraph.prototype.onPointerDown.call(fake, {
    button: 0, pointerId: 4, clientX: 12, clientY: 34,
    preventDefault() { prevented = true; },
  });
  assert.equal(focused, 1);
  assert.equal(prevented, true);
  assert.equal(fake.pointer.mode, 'pan');
});

test('pointer target survives focus-triggered graph refresh', () => {
  for (const kind of ['node', 'port']) {
    let targetIsLive = true;
    const node = { dataset: { nodeId: 'a' } };
    const port = kind === 'port' ? { dataset: { port: 'out' } } : null;
    const fake = {
      root: { focus() { targetIsLive = false; } },
      options: {}, selected: null, view: { x: 0, y: 0, k: 1 },
      svg: { setPointerCapture() {} },
      eventTarget() {
        return targetIsLive ? { node, edge: null, port } : { node: null, edge: null, port: null };
      },
      clientToGraph() { return { x: 0, y: 0 }; },
      clearKeyboardPort() {},
    };
    ProcessGraph.prototype.onPointerDown.call(fake, {
      button: 0, pointerId: 6, clientX: 0, clientY: 0,
      preventDefault() {}, stopPropagation() {},
    });
    assert.equal(fake.pointer.mode, kind, `${kind} classification survives blur refresh`);
    assert.equal(fake.pointer.nodeID, 'a');
  }
});

test('middle pointerdown pans even when it starts over a node', () => {
  const fake = {
    root: { focus() {} }, options: { marqueeSelect: true }, selected: null,
    view: { x: 5, y: 6, k: 1 }, svg: { setPointerCapture() {} },
    eventTarget() { return { node: { dataset: { nodeId: 'a' } }, edge: null, port: null }; },
    clientToGraph() { return { x: 0, y: 0 }; },
  };
  ProcessGraph.prototype.onPointerDown.call(fake, {
    button: 1, pointerId: 9, clientX: 10, clientY: 20, preventDefault() {},
  });
  assert.equal(fake.pointer.mode, 'pan');
});

test('touch and pen pan empty canvas but still drag nodes', () => {
  for (const pointerType of ['touch', 'pen']) {
    const fake = {
      root: { focus() {} }, options: { marqueeSelect: true }, selected: null,
      view: { x: 0, y: 0, k: 1 }, svg: { setPointerCapture() {} },
      eventTarget(event) { return event.overNode
        ? { node: { dataset: { nodeId: 'a' } }, edge: null, port: null }
        : { node: null, edge: null, port: null }; },
      clientToGraph() { return { x: 0, y: 0 }; },
    };
    const event = {
      button: 0, pointerType, pointerId: 12, clientX: 0, clientY: 0,
      preventDefault() {}, overNode: false,
    };
    ProcessGraph.prototype.onPointerDown.call(fake, event);
    assert.equal(fake.pointer.mode, 'pan', `${pointerType} pans empty canvas`);
    event.overNode = true;
    ProcessGraph.prototype.onPointerDown.call(fake, event);
    assert.equal(fake.pointer.mode, 'node', `${pointerType} still drags nodes`);
  }
});

test('empty canvas click clears both graph and consumer selection', () => {
  const selected = [];
  let notified = 0;
  const fake = {
    dragMoved: false, suppressClick: false,
    options: { onCanvasClick() { notified += 1; } },
    eventTarget() { return { node: null, edge: null, port: null }; },
    select(value) { selected.push(value); },
  };
  ProcessGraph.prototype.onClick.call(fake, {});
  assert.deepEqual(selected, [null]);
  assert.equal(notified, 1);
});

test('captured pointer identity selects nodes and edges after click retargets to SVG', () => {
  const selected = [];
  const clicked = [];
  const fake = {
    dragMoved: false, suppressClick: false,
    layout: {
      nodes: [{ id: 'node-a' }],
      edges: [{ id: 'edge-a', from: 'node-a', to: 'node-b' }],
    },
    options: {
      onNodeClick({ node }) { clicked.push(`node:${node.id}`); },
      onEdgeClick({ edge }) { clicked.push(`edge:${edge.id}`); },
    },
    eventTarget() { return { node: null, edge: null, port: null }; },
    select(value) { selected.push(value); },
  };
  fake.pendingClickTarget = { nodeID: 'node-a', edgeID: null, port: null };
  ProcessGraph.prototype.onClick.call(fake, {});
  fake.pendingClickTarget = { nodeID: null, edgeID: 'edge-a', port: null };
  ProcessGraph.prototype.onClick.call(fake, {});
  assert.deepEqual(selected, [{ type: 'node', id: 'node-a' }, { type: 'edge', id: 'edge-a' }]);
  assert.deepEqual(clicked, ['node:node-a', 'edge:edge-a']);
});
