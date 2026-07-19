import test from 'node:test';
import assert from 'node:assert/strict';
import {
  ProcessGraph, interactionNode, isGraphTypingTarget, normalizeWheelDelta,
} from '../dashboard/js/process-graph.js';
import { parseHTML } from './vendor/linkedom.mjs';

test('interaction node identity is exact, deterministic, and bounded to live nodes', () => {
  const nodes = [{ id: 'alpha' }, { id: '10' }, { id: 'beta' }];
  assert.equal(interactionNode(nodes, 'alpha'), nodes[0]);
  assert.equal(interactionNode(nodes, 10), nodes[1]);
  assert.equal(interactionNode(nodes, 'missing'), null);
  assert.equal(interactionNode(nodes, null), null);
  assert.deepEqual(nodes.map((node) => node.id), ['alpha', '10', 'beta'],
    'identity resolution never reorders or annotates canonical nodes');
});

test('every node kind keeps its bounded label inside the shape and clear of connector ports', () => {
  const previousDocument = globalThis.document;
  globalThis.document = parseHTML('<!doctype html><html><body></body></html>').window.document;
  try {
    const cases = [
      { type: 'task', width: 168, height: 68 },
      { type: 'decision', width: 108, height: 108 },
      { type: 'parallel', width: 108, height: 108 },
      { type: 'wait', width: 78, height: 78 },
      { type: 'start', width: 58, height: 58 },
      { type: 'end', width: 62, height: 62 },
      { type: 'task', width: 190, height: 88, compound: { collapsed: true, stages: ['one', 'two'] } },
    ];
    const fullLabel = `${'W'.repeat(24)} 設計レビュー🙂超長識別子withoutspaces-and-more`;
    for (const [index, entry] of cases.entries()) {
      const node = { id: `node-${index}`, label: fullLabel, x: 100, y: 200, ...entry };
      const fake = { instanceID: 41, labelSerial: index };
      const rendered = ProcessGraph.prototype.renderNode.call(fake, node);
      const label = rendered.querySelector('.process-node-label-inside');
      const clip = rendered.querySelector('.process-node-label-clip rect');
      assert.ok(label && clip, `${entry.type} renders the shared inside-label frame`);
      assert.equal(rendered.querySelector('.process-node-label-peripheral'), null);
      assert.equal(rendered.getAttribute('aria-label'), `${fullLabel}, ${entry.compound ? 'collapsed compound' : entry.type}`,
        `${entry.type} retains the untruncated accessible name`);
      assert.ok(label.querySelectorAll('tspan').length <= Number(label.dataset.labelMaxLines));
      assert.match(label.textContent, /…$/, `${entry.type} gives bounded overflow an explicit ellipsis`);

      const top = Number(clip.getAttribute('y'));
      const bottom = top + Number(clip.getAttribute('height'));
      const inputPortBottom = -entry.height / 2 + 6;
      const outputPortTop = entry.height / 2 - 6;
      assert.ok(top > inputPortBottom, `${entry.type} label clears the full input-port disc`);
      assert.ok(bottom < outputPortTop, `${entry.type} label clears the full output-port disc`);

      const ports = ProcessGraph.prototype.renderPortNode.call(fake, node);
      assert.equal(ports.parentNode, null, 'ports remain a sibling-layer group, not node descendants');
      assert.equal(ports.querySelector('.process-port-in').getAttribute('cy'), String(-entry.height / 2));
      assert.equal(ports.querySelector('.process-port-out').getAttribute('cy'), String(entry.height / 2));
      assert.equal(ports.querySelector('.process-port-in').getAttribute('aria-label'), `Input port for ${fullLabel}`);
      assert.equal(ports.querySelector('.process-port-out').getAttribute('aria-label'), `Output port for ${fullLabel}`);
    }
    const short = ProcessGraph.prototype.renderNode.call({ instanceID: 42, labelSerial: 0 }, {
      id: 'short', label: 'Start', type: 'start', x: 0, y: 0, width: 58, height: 58,
    });
    assert.equal(short.querySelector('.process-node-label-inside').textContent, 'Start',
      'a label that fits is not presented as truncated');
  } finally {
    if (previousDocument === undefined) delete globalThis.document;
    else globalThis.document = previousDocument;
  }
});

test('node overlay anchors render only for information disclosed by the node', () => {
  const previousDocument = globalThis.document;
  globalThis.document = parseHTML('<!doctype html><html><body></body></html>').window.document;
  try {
    const base = { id: 'work', label: 'Work', type: 'task', x: 100, y: 200, width: 168, height: 68 };
    const render = (extra = {}) => ProcessGraph.prototype.renderNode.call(
      { instanceID: 51, labelSerial: 0 }, { ...base, ...extra },
    );

    const clean = render();
    const empty = render({ overlay: {} });
    for (const [label, node] of [['undefined', clean], ['empty', empty]]) {
      assert.equal(node.querySelector('.process-overlay-anchor'), null, `${label} overlay has no placeholder anchor`);
      assert.equal(node.getAttribute('aria-label'), 'Work, task', `${label} overlay adds no accessible disclosure`);
    }

    const diagnostic = render({
      overlay: { glyph: '!', severity: 'error', issues: ['E_PERFORMER: Work performer is required'] },
    });
    const diagnosticAnchor = diagnostic.querySelector('.process-overlay-anchor');
    assert.ok(diagnosticAnchor?.classList.contains('has-overlay'));
    assert.equal(diagnosticAnchor.getAttribute('aria-hidden'), 'true');
    assert.equal(diagnosticAnchor.hasAttribute('role'), false, 'the marker is disclosure presentation, not a separate action');
    assert.equal(diagnosticAnchor.hasAttribute('tabindex'), false);
    assert.match(diagnostic.getAttribute('aria-label'), /has error.*Work performer is required/);
    assert.match(diagnosticAnchor.querySelector('.process-overlay-tooltip').textContent, /Work performer is required/);

    const status = render({
      stateOverlay: { glyph: '●', status: 'running', progress: { current: 2, total: 4 }, attempt: 1, retry: 0 },
    });
    const statusAnchor = status.querySelector('.process-overlay-anchor');
    assert.ok(statusAnchor, 'viewer/status information retains the shared anchor');
    assert.equal(statusAnchor.querySelector('.process-overlay-tooltip'), null, 'status without issues does not invent a tooltip');
    assert.match(status.getAttribute('aria-label'), /●, running, 2\/4, attempt 1, retry 0/);

    for (const node of [clean, empty, diagnostic, status]) {
      const shape = node.querySelector('.process-node-shape');
      assert.equal(shape.getAttribute('x'), String(-base.width / 2));
      assert.equal(shape.getAttribute('y'), String(-base.height / 2));
      assert.equal(shape.getAttribute('width'), String(base.width));
      assert.equal(shape.getAttribute('height'), String(base.height));
      const ports = ProcessGraph.prototype.renderPortNode.call({}, base);
      assert.equal(ports.querySelector('.process-port-in').getAttribute('cy'), String(-base.height / 2));
      assert.equal(ports.querySelector('.process-port-out').getAttribute('cy'), String(base.height / 2));
    }
  } finally {
    if (previousDocument === undefined) delete globalThis.document;
    else globalThis.document = previousDocument;
  }
});

test('edge renderer gives the exact routed path to the auto-oriented SVG marker', () => {
  const previousDocument = globalThis.document;
  globalThis.document = parseHTML('<!doctype html><html><body></body></html>').window.document;
  try {
    const edge = {
      id: 'side', inputIndex: 0, from: 'source', to: 'target', back: false,
      path: 'M 10 20 C 40 20, 70 30, 80 30',
      label: { x: 45, y: 17 },
    };
    const rendered = ProcessGraph.prototype.renderEdge.call({
      markerID: 'forward-marker', backMarkerID: 'back-marker',
    }, edge);
    const visible = rendered.querySelector('.process-edge-path');
    const hit = rendered.querySelector('.process-edge-hit');
    assert.equal(visible.getAttribute('d'), edge.path);
    assert.equal(hit.getAttribute('d'), edge.path, 'the hit target tracks the identical route');
    assert.equal(visible.getAttribute('marker-end'), 'url(#forward-marker)',
      'the browser orients the marker from that rendered terminal tangent');
  } finally {
    if (previousDocument === undefined) delete globalThis.document;
    else globalThis.document = previousDocument;
  }
});

test('live edge reroutes update geometry without replacing interactive edge DOM', () => {
  const previousDocument = globalThis.document;
  globalThis.document = parseHTML('<!doctype html><html><body></body></html>').window.document;
  try {
    const edge = {
      id: 'work:pass', inputIndex: 0, from: 'work', to: 'done', outcome: 'pass',
      path: 'M 10 20 L 30 40', label: { x: 20, y: 22 }, badge: '!',
    };
    const fake = {
      markerID: 'forward-marker', backMarkerID: 'back-marker',
      edgeLayer: document.createElementNS('http://www.w3.org/2000/svg', 'g'),
    };
    const rendered = ProcessGraph.prototype.renderEdge.call(fake, edge);
    fake.edgeLayer.append(rendered);
    ProcessGraph.prototype.updateEdgeGeometry.call(fake, [{
      ...edge, path: 'M 50 60 L 70 80', label: { x: 61, y: 63 },
    }]);
    assert.equal(fake.edgeLayer.firstElementChild, rendered,
      'drag frames retain edge focus, listeners, and selection classes');
    assert.equal(rendered.querySelector('.process-edge-path').getAttribute('d'), 'M 50 60 L 70 80');
    assert.equal(rendered.querySelector('.process-edge-hit').getAttribute('d'), 'M 50 60 L 70 80');
    assert.equal(rendered.querySelector('.process-edge-label').getAttribute('transform'), 'translate(61 63)');
    assert.equal(rendered.querySelector('.process-edge-issue-marker').getAttribute('transform'), 'translate(61 50)');
  } finally {
    if (previousDocument === undefined) delete globalThis.document;
    else globalThis.document = previousDocument;
  }
});

test('wheel delta modes normalize to useful pixel-scale zoom input', () => {
  assert.equal(normalizeWheelDelta(120, 0, 900), 120);
  assert.equal(normalizeWheelDelta(3, 1, 900), 72);
  assert.equal(normalizeWheelDelta(1, 2, 900), 900);
  assert.equal(normalizeWheelDelta(Number.NaN, 0, 900), 0);
});

test('command zoom helpers preserve the viewport center and reset to 100%', () => {
  const transforms = [];
  const fake = {
    view: { x: 100, y: 50, k: 2 },
    svg: { getBoundingClientRect: () => ({ left: 20, top: 10, width: 800, height: 600 }) },
    applyView() { transforms.push({ ...this.view }); },
    setZoom: ProcessGraph.prototype.setZoom,
  };
  assert.equal(ProcessGraph.prototype.zoomBy.call(fake, 1.2), true);
  assert.equal(fake.view.k, 2.4);
  assert.deepEqual({
    x: (400 - fake.view.x) / fake.view.k,
    y: (300 - fake.view.y) / fake.view.k,
  }, { x: 150, y: 125 }, 'the same graph point remains under the viewport center');
  assert.equal(ProcessGraph.prototype.resetZoom.call(fake), true);
  assert.equal(fake.view.k, 1);
  assert.equal(transforms.length, 2);
});

test('editor wheel pans while Ctrl-wheel pinches, and viewer wheel still zooms', () => {
  const graph = (options) => ({
    options, view: { x: 10, y: 20, k: 1 },
    svg: { getBoundingClientRect: () => ({ left: 0, top: 0, width: 800, height: 600 }) },
    applyView() {},
  });
  const event = (overrides = {}) => ({
    deltaX: 7, deltaY: 11, deltaMode: 0, clientX: 200, clientY: 150,
    preventDefault() {}, ...overrides,
  });

  const editor = graph({ wheelPan: true });
  ProcessGraph.prototype.onWheel.call(editor, event());
  assert.deepEqual(editor.view, { x: 3, y: 9, k: 1 }, 'two-finger wheel delta pans the editor canvas');
  ProcessGraph.prototype.onWheel.call(editor, event({ deltaX: 0, deltaY: -20, ctrlKey: true }));
  assert.ok(editor.view.k > 1, 'browser pinch/Ctrl-wheel keeps cursor-centered zoom');

  const viewer = graph({});
  ProcessGraph.prototype.onWheel.call(viewer, event({ deltaX: 0 }));
  assert.ok(viewer.view.k < 1, 'the shared read-only viewer retains wheel zoom');
});

test('node drags keep their captured view until release or cancellation', () => {
  const initial = { x: 10, y: 20, k: 1.5 };
  const fake = {
    pointer: { mode: 'node' }, view: { ...initial }, options: { wheelPan: true },
    svg: { getBoundingClientRect() { throw new Error('a node drag must not read or mutate view geometry'); } },
    applyView() { throw new Error('a node drag must not repaint the viewport'); },
  };
  let prevented = 0;
  ProcessGraph.prototype.onWheel.call(fake, {
    deltaX: 10, deltaY: 20, deltaMode: 0, clientX: 100, clientY: 100,
    preventDefault() { prevented += 1; },
  });
  assert.equal(ProcessGraph.prototype.setZoom.call(fake, 2), false);
  assert.equal(ProcessGraph.prototype.fitToView.call(fake), false);
  assert.equal(ProcessGraph.prototype.centerOn.call(fake, 100, 200), false);
  assert.equal(prevented, 1, 'wheel input is still claimed while the drag owns the canvas');
  assert.deepEqual(fake.view, initial,
    'queued drag coordinates cannot race a wheel, fit, zoom, or center mutation');
});

test('Space arms panning only outside editable controls and never steals an owned gesture', () => {
  const toggles = [];
  const fake = {
    pointer: null, spaceHeld: false,
    root: {
      contains: () => false, matches: () => true,
      classList: { toggle(name, value) { toggles.push([name, value]); } },
    },
    setSpaceHeld: ProcessGraph.prototype.setSpaceHeld,
  };
  const plain = { closest: () => null };
  let prevented = 0;
  ProcessGraph.prototype.onSpaceKey.call(fake, {
    key: ' ', type: 'keydown', target: plain, preventDefault() { prevented += 1; },
  });
  assert.equal(fake.spaceHeld, true);
  ProcessGraph.prototype.onSpaceKey.call(fake, { key: ' ', type: 'keyup', target: plain });
  assert.equal(fake.spaceHeld, false);

  const editable = { closest: (selector) => selector.includes('contenteditable') ? editable : null };
  assert.equal(isGraphTypingTarget(editable), true);
  ProcessGraph.prototype.onSpaceKey.call(fake, { key: ' ', type: 'keydown', target: editable });
  assert.equal(fake.spaceHeld, false, 'typing a space in a field does not arm graph panning');
  fake.pointer = { id: 1, mode: 'node' };
  ProcessGraph.prototype.onSpaceKey.call(fake, { key: ' ', type: 'keydown', target: plain });
  assert.equal(fake.spaceHeld, false, 'an active pointer gesture keeps ownership');
  assert.equal(prevented, 1, 'Space is consumed only when the graph owns the shortcut');
  assert.deepEqual(toggles, [['is-space-pan', true], ['is-space-pan', false]]);

  fake.pointer = null;
  const button = { closest: (selector) => selector.includes('button') ? button : null };
  ProcessGraph.prototype.onSpaceKey.call(fake, { key: ' ', type: 'keydown', target: button });
  assert.equal(fake.spaceHeld, false, 'buttons and keyboard-focused graph nodes retain Space activation');

  const summary = { closest: (selector) => selector.includes('summary') ? summary : null };
  assert.equal(isGraphTypingTarget(summary), true);
  ProcessGraph.prototype.onSpaceKey.call(fake, { key: ' ', type: 'keydown', target: summary });
  assert.equal(fake.spaceHeld, false,
    'a focused Issues summary retains native Space activation while the graph is hovered');
  assert.equal(prevented, 1);
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

test('empty-canvas port release reports the exact pan/zoom graph coordinate', () => {
  const previousDocument = globalThis.document;
  const hit = {};
  globalThis.document = { elementFromPoint: () => hit };
  try {
    const ended = [];
    const fake = {
      pointer: { id: 17, mode: 'port', nodeID: 'source', port: 'out' },
      view: { x: 20, y: -10, k: 2 }, dragMoved: true,
      svg: {
        getBoundingClientRect: () => ({ left: 100, top: 50 }),
        releasePointerCapture() {},
        contains: (candidate) => candidate === hit,
      },
      options: { onPortDragEnd: (value) => ended.push(value) },
      eventTarget: () => ({ node: null, edge: null, port: null }),
      clientToGraph: ProcessGraph.prototype.clientToGraph,
    };
    ProcessGraph.prototype.onPointerUp.call(fake, {
      pointerId: 17, clientX: 180, clientY: 140,
    });
    assert.deepEqual(ended[0].point, { x: 30, y: 50 });
    assert.equal(ended[0].targetNodeId, null);
    assert.equal(ended[0].emptyCanvas, true);
  } finally {
    if (previousDocument === undefined) delete globalThis.document;
    else globalThis.document = previousDocument;
  }
});

test('pointercancel snaps a node drag home instead of committing it', () => {
  const snapped = [];
  const fake = {
    pointer: { id: 3, mode: 'node', nodeID: 'n1' },
    options: {},
    svg: { releasePointerCapture() {} },
    dragMoved: true,
    cancelNodeDragFrame() {},
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

test('released lost capture completes every graph gesture through pointerup', () => {
  for (const mode of ['node', 'port', 'marquee', 'pan']) {
    const completed = [];
    const cancelled = [];
    const fake = {
      pointer: {
        id: 31, mode, button: 0, pointerType: 'mouse',
        lastClientX: 120, lastClientY: 85,
      },
      onPointerUp(event, options) {
        completed.push({ event, options });
        this.pointer = null;
      },
      onPointerCancel(event) {
        cancelled.push(event);
        this.pointer = null;
      },
    };
    ProcessGraph.prototype.onLostPointerCapture.call(fake, {
      type: 'lostpointercapture', pointerId: 31, buttons: 0,
    });
    assert.equal(completed.length, 1, `${mode} completes instead of cancelling`);
    assert.equal(cancelled.length, 0);
    assert.deepEqual({
      pointerId: completed[0].event.pointerId,
      clientX: completed[0].event.clientX,
      clientY: completed[0].event.clientY,
      buttons: completed[0].event.buttons,
      captureAlreadyLost: completed[0].options.captureAlreadyLost,
    }, {
      pointerId: 31, clientX: 120, clientY: 85,
      buttons: 0, captureAlreadyLost: true,
    });
  }
});

test('lost capture while a button remains pressed still cancels', () => {
  const cancelled = [];
  const fake = {
    pointer: {
      id: 32, mode: 'node', button: 0, pointerType: 'mouse',
      lastClientX: 44, lastClientY: 55,
    },
    onPointerUp() { throw new Error('capture loss mid-press must not complete'); },
    onPointerCancel(event) { cancelled.push(event); this.pointer = null; },
  };
  ProcessGraph.prototype.onLostPointerCapture.call(fake, {
    type: 'lostpointercapture', pointerId: 32, buttons: 1,
  });
  assert.deepEqual(cancelled, [{
    type: 'lostpointercapture', pointerId: 32, clientX: 44, clientY: 55,
  }]);
});

test('normal pointerup cannot be completed twice by synchronous capture loss', () => {
  const completed = [];
  const fake = {
    pointer: {
      id: 33, mode: 'node', nodeID: 'work', nodeIDs: ['work'],
      startPoint: { x: 10, y: 10 }, starts: [{ id: 'work', x: 100, y: 200 }],
      nodeDelta: { x: 20, y: 15 },
    },
    dragMoved: true,
    options: { onNodeDragEnd: (event) => completed.push(event) },
    svg: {
      releasePointerCapture(pointerId) {
        ProcessGraph.prototype.onLostPointerCapture.call(fake, { pointerId, buttons: 0 });
      },
    },
    clientToGraph: () => ({ x: 30, y: 25 }),
    flushNodeDragFrame() {},
    snapNodesHome() {}, restoreTransientEdges() {},
  };
  ProcessGraph.prototype.onPointerUp.call(fake, {
    type: 'pointerup', pointerId: 33, clientX: 30, clientY: 25,
  });
  assert.equal(completed.length, 1);
  assert.equal(fake.pointer, null);
});

test('canvas pointerdown focuses its shortcut target so editor Delete receives keyboard events', () => {
  let focused = 0;
  const fake = {
    focusCanvas: ProcessGraph.prototype.focusCanvas,
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
      focusCanvas: ProcessGraph.prototype.focusCanvas,
      root: { focus() { targetIsLive = false; } },
      options: {}, selected: null, view: { x: 0, y: 0, k: 1 },
      svg: { setPointerCapture() {} },
      layout: { nodes: [] },
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

test('captured item click completes on pointerup when refresh prevents a synthetic click', () => {
  const selected = [];
  const clicked = [];
  const fake = {
    pointer: {
      id: 6, mode: 'node', nodeID: 'a', nodeIDs: ['a'],
      startPoint: { x: 0, y: 0 },
    },
    layout: { nodes: [{ id: 'a' }], edges: [] },
    dragMoved: false,
    options: { onNodeClick: ({ node }) => clicked.push(node.id) },
    svg: { releasePointerCapture() {} },
    clientToGraph() { return { x: 0, y: 0 }; },
    flushNodeDragFrame() {},
    snapNodesHome() {},
    restoreTransientEdges() {},
    select(value) { selected.push(value); },
    onClick: ProcessGraph.prototype.onClick,
  };
  ProcessGraph.prototype.onPointerUp.call(fake, {
    pointerId: 6, clientX: 0, clientY: 0,
  });
  assert.deepEqual(selected, [{ type: 'node', id: 'a' }]);
  assert.deepEqual(clicked, ['a']);
  assert.equal(fake.suppressClick, true);
  ProcessGraph.prototype.onClick.call(fake, { target: { closest: () => null } });
  assert.deepEqual(clicked, ['a'], 'a surviving synthetic click is suppressed');
});

test('middle pointerdown pans even when it starts over a node', () => {
  const fake = {
    focusCanvas: ProcessGraph.prototype.focusCanvas,
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

test('Space-primary drag pans over a node and a second pointer cannot replace it', () => {
  const fake = {
    focusCanvas: ProcessGraph.prototype.focusCanvas,
    root: { focus() {} }, options: { marqueeSelect: true }, selected: null, spaceHeld: true,
    view: { x: 5, y: 6, k: 1 }, svg: { setPointerCapture() {} },
    eventTarget() { return { node: { dataset: { nodeId: 'a' } }, edge: null, port: null }; },
    clientToGraph() { return { x: 0, y: 0 }; },
  };
  ProcessGraph.prototype.onPointerDown.call(fake, {
    button: 0, pointerId: 9, clientX: 10, clientY: 20, preventDefault() {},
  });
  assert.equal(fake.pointer.mode, 'pan');
  ProcessGraph.prototype.onPointerDown.call(fake, {
    button: 0, pointerId: 10, clientX: 30, clientY: 40,
    preventDefault() { throw new Error('owned gestures ignore later pointers'); },
  });
  assert.equal(fake.pointer.id, 9);
});

test('a pointerdown reusing the armed pointer id cancels the dead gesture instead of being swallowed', () => {
  // A pointer cannot physically press twice, so a same-id pointerdown proves
  // the previous gesture's pointerup/pointercancel never reached the SVG.
  // Silently ignoring it left an armed ghost gesture that swallowed the next
  // drag and committed it against a stale start frame.
  const cancels = [];
  const snapped = [];
  const fake = {
    pointer: { id: 1, button: 0, mode: 'node', nodeID: 'a', nodeIDs: ['a'], lastClientX: 40, lastClientY: 30 },
    dragMoved: true,
    focusCanvas: ProcessGraph.prototype.focusCanvas,
    root: { focus() {} }, selected: null, spaceHeld: false,
    options: { onNodeDragCancel: (value) => cancels.push(value) },
    view: { x: 0, y: 0, k: 1 },
    svg: { setPointerCapture() {}, releasePointerCapture() {} },
    eventTarget() { return { node: { dataset: { nodeId: 'a' } }, edge: null, port: null }; },
    clientToGraph: (x, y) => ({ x, y }),
    layout: { nodes: [{ id: 'a', x: 12, y: 34 }] },
    snapNodesHome(ids) { snapped.push(...ids); },
    cancelNodeDragFrame() {},
    restoreTransientEdges() {},
    raiseNode() {},
    onPointerCancel: ProcessGraph.prototype.onPointerCancel,
  };
  ProcessGraph.prototype.onPointerDown.call(fake, {
    button: 0, pointerId: 1, clientX: 40, clientY: 30, preventDefault() {},
  });
  assert.equal(cancels.length, 1, 'the dead gesture ends through its cancel hook');
  assert.deepEqual(snapped, ['a'], 'the dead gesture snaps its nodes home');
  assert.ok(fake.pointer, 'the new gesture is armed rather than swallowed');
  assert.equal(fake.pointer.id, 1);
  assert.equal(fake.pointer.mode, 'node');
  assert.deepEqual(fake.pointer.starts, [{ id: 'a', x: 12, y: 34 }],
    'the new gesture snapshots start positions from the live layout');
});

test('a secondary mouse button pressed mid-drag never destroys the live gesture', () => {
  // A mouse keeps ONE pointer id across all its buttons, so pressing right or
  // middle while left-dragging delivers a same-id pointerdown with a
  // different button and no intervening pointerup. That is a live gesture,
  // not a lost one: it must be left completely alone.
  const pointer = {
    id: 1, button: 0, mode: 'node', nodeID: 'a', nodeIDs: ['a'],
    lastClientX: 40, lastClientY: 30,
  };
  const fake = {
    pointer,
    dragMoved: true,
    root: { focus() { throw new Error('a live gesture must not refocus'); } },
    selected: null, spaceHeld: false,
    options: {
      onNodeDragCancel() { throw new Error('a live gesture must not cancel'); },
    },
    view: { x: 0, y: 0, k: 1 },
    svg: {
      setPointerCapture() { throw new Error('a live gesture must not recapture'); },
      releasePointerCapture() {},
    },
    eventTarget() { return { node: { dataset: { nodeId: 'a' } }, edge: null, port: null }; },
    clientToGraph: (x, y) => ({ x, y }),
    layout: { nodes: [{ id: 'a', x: 12, y: 34 }] },
    snapNodesHome() { throw new Error('a live gesture must not snap home'); },
    restoreTransientEdges() {},
    raiseNode() {},
    onPointerCancel: ProcessGraph.prototype.onPointerCancel,
  };
  for (const button of [1, 2]) {
    ProcessGraph.prototype.onPointerDown.call(fake, {
      button, pointerId: 1, clientX: 44, clientY: 33, preventDefault() {},
    });
    assert.equal(fake.pointer, pointer, `button ${button} leaves the armed gesture untouched`);
    assert.equal(fake.dragMoved, true);
  }
});

test('node drag end reports the start positions captured by its own gesture', () => {
  const ends = [];
  const fake = {
    pointer: {
      id: 4, mode: 'node', nodeID: 'work', nodeIDs: ['work'],
      startPoint: { x: 10, y: 10 },
      starts: [{ id: 'work', x: 100, y: 200 }],
    },
    dragMoved: true,
    options: { onNodeDragEnd: (value) => ends.push(value) },
    svg: { releasePointerCapture() {} },
    clientToGraph: () => ({ x: 40, y: 25 }),
    flushNodeDragFrame() {},
    snapNodesHome() {}, restoreTransientEdges() {},
  };
  const gestureStarts = fake.pointer.starts;
  ProcessGraph.prototype.onPointerUp.call(fake, { pointerId: 4, clientX: 40, clientY: 25 });
  assert.equal(ends.length, 1);
  assert.deepEqual(ends[0].starts, [{ id: 'work', x: 100, y: 200 }]);
  assert.deepEqual(ends[0].delta, { x: 30, y: 15 });
  assert.notEqual(ends[0].starts[0], gestureStarts[0],
    'consumers receive copies, not the live gesture state');
});

test('node drag end commits the last rendered frame when release coordinates lag behind it', () => {
  const ends = [];
  const fake = {
    pointer: {
      id: 4, mode: 'node', nodeID: 'work', nodeIDs: ['work'],
      startPoint: { x: 10, y: 10 },
      starts: [{ id: 'work', x: 100, y: 200 }],
      nodeDelta: { x: 30, y: 15 },
    },
    dragMoved: true,
    options: { onNodeDragEnd: (value) => ends.push(value) },
    svg: { releasePointerCapture() {} },
    // A terminal sample may lag a fast-moving pointer stream. The node was
    // visibly rendered at nodeDelta, so snapping it to this stale coordinate
    // would make the completed gesture rubber-band back to its start.
    clientToGraph: () => ({ x: 10, y: 10 }),
    flushNodeDragFrame() {},
    snapNodesHome() {}, restoreTransientEdges() {},
  };
  ProcessGraph.prototype.onPointerUp.call(fake, { pointerId: 4, clientX: 10, clientY: 10 });
  assert.deepEqual(ends[0].delta, { x: 30, y: 15 });
});

test('an unmoved pan gesture never falls through to select its underlying node', () => {
  let selected = 0;
  const fake = {
    pointer: { id: 9, mode: 'pan', nodeID: 'a' },
    dragMoved: false,
    options: { onSelect: () => { selected += 1; } },
    svg: { releasePointerCapture() {} },
    clientToGraph: () => ({ x: 0, y: 0 }),
  };
  ProcessGraph.prototype.onPointerUp.call(fake, {
    pointerId: 9, clientX: 0, clientY: 0,
  });
  assert.equal(fake.suppressClick, true);
  assert.equal(fake.pendingClickTarget, null);
  ProcessGraph.prototype.onClick.call(fake, {
    target: { closest: () => null }, preventDefault() {},
  });
  assert.equal(selected, 0);
});

test('an unmoved empty-canvas pan still clears selection in the viewer', () => {
  const selections = [];
  let canvasClicks = 0;
  const fake = {
    pointer: { id: 8, mode: 'pan', nodeID: null, edgeID: null, port: null },
    dragMoved: false,
    options: { onCanvasClick: () => { canvasClicks += 1; } },
    svg: { releasePointerCapture() {} },
    clientToGraph: () => ({ x: 0, y: 0 }),
    select: (value) => selections.push(value),
  };
  ProcessGraph.prototype.onPointerUp.call(fake, {
    pointerId: 8, clientX: 0, clientY: 0,
  });
  assert.equal(fake.suppressClick, false);
  ProcessGraph.prototype.onClick.call(fake, { target: { closest: () => null } });
  assert.deepEqual(selections, [null]);
  assert.equal(canvasClicks, 1);
});

test('dragging an unselected node selects it once when movement crosses the threshold', () => {
  const previousCSS = globalThis.CSS;
  globalThis.CSS = { escape: (value) => value };
  try {
    const starts = [];
    const fake = {
      pointer: {
        id: 4, mode: 'node', nodeID: 'work', nodeIDs: ['work'],
        startClientX: 0, startClientY: 0, startPoint: { x: 0, y: 0 },
        startView: { x: 0, y: 0, k: 1 }, selectionStarted: false,
      },
      selected: null, dragMoved: false,
      options: { onNodeDragStart: (value) => starts.push(value), onNodeDrag() {} },
      nodeLayer: { querySelector: () => null }, portLayer: { querySelector: () => null },
      layout: { nodes: [] },
      select(value) { this.selected = value; },
      queueNodeDragFrame() {}, updatePortHover() {},
    };
    const moved = { pointerId: 4, clientX: 5, clientY: 0 };
    ProcessGraph.prototype.onPointerMove.call(fake, moved);
    ProcessGraph.prototype.onPointerMove.call(fake, { ...moved, clientX: 8 });
    assert.deepEqual(fake.selected, { type: 'node', id: 'work' });
    assert.equal(starts.length, 1, 'selection is synchronized only at actual drag start');
    assert.equal(starts[0].nodeId, 'work');
  } finally {
    if (previousCSS === undefined) delete globalThis.CSS;
    else globalThis.CSS = previousCSS;
  }
});

test('node pointer samples coalesce into one display frame using the newest position', () => {
  const previousCSS = globalThis.CSS;
  const previousRAF = globalThis.requestAnimationFrame;
  globalThis.CSS = { escape: (value) => value };
  const frames = [];
  globalThis.requestAnimationFrame = (callback) => { frames.push(callback); return frames.length; };
  try {
    const node = { setAttribute(name, value) { this[name] = value; } };
    const ports = { setAttribute(name, value) { this[name] = value; } };
    const rendered = [];
    const fake = {
      pointer: {
        id: 4, mode: 'node', nodeID: 'work', nodeIDs: ['work'],
        startClientX: 0, startClientY: 0, startPoint: { x: 20, y: 30 },
        startView: { x: 5, y: 6, k: 2 }, selectionStarted: false,
      },
      selected: { type: 'node', id: 'work' }, dragMoved: false, destroyed: false,
      pendingNodeDrag: null, nodeDragFrame: null, frontNodeID: null,
      options: { onNodeDrag: ({ delta }) => rendered.push(['hook', delta]) },
      nodeLayer: { querySelector: () => node }, portLayer: { querySelector: () => ports },
      layout: { nodes: [{ id: 'work', x: 100, y: 200 }] },
      updatePortHover() {},
      queueNodeDragFrame: ProcessGraph.prototype.queueNodeDragFrame,
      flushNodeDragFrame: ProcessGraph.prototype.flushNodeDragFrame,
      setNodeTransform: ProcessGraph.prototype.setNodeTransform,
      renderTransientEdges(ids, delta) { rendered.push(['edges', ids, delta]); },
    };
    ProcessGraph.prototype.onPointerMove.call(fake, { pointerId: 4, clientX: 10, clientY: 12 });
    ProcessGraph.prototype.onPointerMove.call(fake, { pointerId: 4, clientX: 16, clientY: 20 });
    assert.equal(frames.length, 1, 'many input samples schedule one browser paint');
    assert.equal(node.transform, undefined, 'DOM work waits for the display frame');
    frames[0]();
    assert.equal(node.transform, 'translate(108 210)');
    assert.equal(ports.transform, 'translate(108 210)');
    assert.deepEqual(fake.pointer.nodeDelta, { x: 8, y: 10 });
    assert.deepEqual(rendered, [
      ['hook', { x: 8, y: 10 }],
      ['edges', ['work'], { x: 8, y: 10 }],
    ]);
  } finally {
    if (previousCSS === undefined) delete globalThis.CSS;
    else globalThis.CSS = previousCSS;
    if (previousRAF === undefined) delete globalThis.requestAnimationFrame;
    else globalThis.requestAnimationFrame = previousRAF;
  }
});

test('touch and pen pan empty canvas but still drag nodes', () => {
  for (const pointerType of ['touch', 'pen']) {
    const fake = {
      focusCanvas: ProcessGraph.prototype.focusCanvas,
      root: { focus() {} }, options: { marqueeSelect: true }, selected: null,
      view: { x: 0, y: 0, k: 1 }, svg: { setPointerCapture() {} },
      layout: { nodes: [] },
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
    fake.pointer = null;
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
