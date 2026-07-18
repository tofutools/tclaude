import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

async function mountedAdapter(t, events = {}, options = {}) {
  const harness = await createPreactHarness(t);
  const previousRAF = globalThis.requestAnimationFrame;
  globalThis.requestAnimationFrame = () => 1;
  const previousCSS = globalThis.CSS;
  globalThis.CSS = { escape: (value) => String(value) };
  t.after(() => {
    if (previousRAF === undefined) delete globalThis.requestAnimationFrame;
    else globalThis.requestAnimationFrame = previousRAF;
    if (previousCSS === undefined) delete globalThis.CSS;
    else globalThis.CSS = previousCSS;
  });
  const { createProcessGraphAdapter } = await harness.importDashboardModule('js/process-graph-adapter.js');
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const adapter = createProcessGraphAdapter(host, {
    graph: {
      nodes: [{ id: 'start', type: 'start', label: 'Start' }, { id: 'end', type: 'end', label: 'End' }],
      edges: [{ id: 'start:pass', from: 'start', outcome: 'pass', to: 'end' }],
    },
    events,
    ...options,
  });
  return { harness, host, adapter };
}

test('editor connection feedback covers pointer, keyboard, timing, and cleanup without changing hit geometry', async (t) => {
  const received = [];
  const feedback = ({ phase, source, candidate = {} }) => {
    if (source.nodeId === 'end' && source.port === 'out') {
      return { state: 'disabled', enabled: false, message: 'End nodes cannot have outgoing connections.' };
    }
    if (phase === 'source') return { state: 'available', enabled: true, message: `Start from ${source.port}.` };
    if (candidate.nodeId === source.nodeId && candidate.port === source.port) {
      return { state: 'source', enabled: true, message: `Start from ${source.port}.` };
    }
    if (source.port === 'in' && candidate.port === 'in') {
      return { state: 'invalid', enabled: false, message: 'Connect this input to an output port.' };
    }
    if (candidate.nodeId === source.nodeId) {
      return { state: 'invalid', enabled: false, message: 'Self-loop connections are not supported.' };
    }
    return { state: 'valid', enabled: true, message: 'Drop to connect Start to End.' };
  };
  const { harness, adapter, host } = await mountedAdapter(t, {
    portDragStart: (payload) => received.push(['start', payload]),
    portDragEnd: (payload) => received.push(['end', payload]),
  }, { connectionFeedback: feedback, actionFeedbackDelay: 50, keyboardFeedbackDelay: 50 });
  const svg = host.querySelector('.process-graph-svg');
  svg.setPointerCapture = () => {};
  svg.releasePointerCapture = () => {};
  const startIn = host.querySelector('[data-node-id="start"] .process-port-in');
  const startOut = host.querySelector('[data-node-id="start"] .process-port-out');
  const endIn = host.querySelector('[data-node-id="end"] .process-port-in');
  const endOut = host.querySelector('[data-node-id="end"] .process-port-out');
  const tooltip = host.querySelector('.process-action-tooltip');
  for (const port of [startIn, startOut, endIn, endOut]) assert.equal(port.getAttribute('r'), '6');
  assert.equal(endOut.getAttribute('aria-disabled'), 'true');
  assert.ok(endOut.classList.contains('is-action-disabled'));

  harness.fireEvent(endOut, 'focusin');
  assert.equal(tooltip.classList.contains('is-visible'), false, 'focus disclosure observes its keyboard delay');
  harness.fireEvent(endOut, 'pointerdown', { button: 0, pointerId: 20, pointerType: 'mouse' });
  await new Promise((resolve) => setTimeout(resolve, 2));
  assert.equal(received.length, 0, 'a disabled source never starts a semantic drag');
  assert.match(tooltip.textContent, /End nodes cannot/);
  assert.ok(tooltip.classList.contains('is-visible'));
  assert.equal(endOut.getAttribute('aria-describedby'), tooltip.id);

  const frames = [];
  const stubRAF = globalThis.requestAnimationFrame;
  globalThis.requestAnimationFrame = (callback) => { frames.push(callback); return frames.length; };
  t.after(() => { globalThis.requestAnimationFrame = stubRAF; });
  let hitTests = 0;
  let hitTarget = endIn;
  const originalHitTest = harness.document.elementFromPoint;
  harness.document.elementFromPoint = () => { hitTests += 1; return hitTarget; };
  t.after(() => { harness.document.elementFromPoint = originalHitTest; });
  harness.fireEvent(startIn, 'pointerdown', {
    button: 0, pointerId: 21, pointerType: 'mouse', clientX: 1, clientY: 2,
  });
  assert.ok(host.querySelector('.process-graph').classList.contains('is-connecting'));
  assert.ok(startIn.classList.contains('is-connection-source'));
  assert.ok(endIn.classList.contains('is-connection-invalid'));
  assert.ok(endOut.classList.contains('is-connection-valid'));
  for (let index = 0; index < 8; index += 1) {
    harness.fireEvent(svg, 'pointermove', {
      pointerId: 21, pointerType: 'mouse', clientX: 10 + index, clientY: 20 + index,
    });
  }
  assert.equal(frames.length, 1, 'pointer feedback is coalesced to one animation frame');
  assert.equal(hitTests, 0);
  frames.shift()();
  assert.equal(hitTests, 1, 'one frame performs one elementFromPoint lookup');
  await new Promise((resolve) => setTimeout(resolve, 55));
  assert.match(tooltip.textContent, /output port/);
  assert.ok(tooltip.classList.contains('is-visible'));
  assert.equal(endIn.getAttribute('aria-describedby'), tooltip.id);

  adapter.setGraph({
    nodes: [{ id: 'start', type: 'start', label: 'Start' }, { id: 'end', type: 'end', label: 'End' }],
    edges: [],
  });
  hitTarget = host.querySelector('[data-node-id="end"] .process-port-in');
  assert.equal(tooltip.textContent, '', 'rerender removes the detached target disclosure immediately');
  assert.equal(frames.length, 1, 'active pointer feedback schedules one recovery frame');
  frames.shift()();
  await new Promise((resolve) => setTimeout(resolve, 2));
  assert.match(tooltip.textContent, /output port/);
  assert.ok(tooltip.classList.contains('is-visible'), 'rerender preserves the elapsed disclosure delay');
  assert.equal(hitTarget.getAttribute('aria-describedby'), tooltip.id);

  harness.fireEvent(svg, 'pointercancel', { pointerId: 21, pointerType: 'mouse', clientX: 18, clientY: 28 });
  assert.equal(host.querySelector('.process-graph').classList.contains('is-connecting'), false);
  assert.equal(host.querySelector('.is-connection-valid, .is-connection-invalid, .is-connection-source'), null);
  assert.equal(tooltip.textContent, '');
  assert.equal(hitTarget.hasAttribute('aria-describedby'), false);
  assert.equal(received.at(-1)[1].cancelled, true);

  const keyboardStartIn = host.querySelector('[data-node-id="start"] .process-port-in');
  const keyboardEndIn = host.querySelector('[data-node-id="end"] .process-port-in');
  harness.fireEvent(keyboardStartIn, 'keydown', { key: 'Enter' });
  assert.equal(received.at(-1)[0], 'start');
  assert.equal(received.at(-1)[1].keyboard, true);
  harness.fireEvent(keyboardEndIn, 'keydown', { key: 'Enter' });
  assert.ok(keyboardStartIn.classList.contains('is-keyboard-source'), 'an invalid keyboard target keeps the gesture active');
  await new Promise((resolve) => setTimeout(resolve, 2));
  assert.match(tooltip.textContent, /output port/);
  adapter.setGraph({
    nodes: [{ id: 'start', type: 'start', label: 'Start' }, { id: 'end', type: 'end', label: 'End' }],
    edges: [],
  });
  assert.equal(tooltip.textContent, '', 'rerender removes stale target disclosure');
  const rerenderedSource = host.querySelector('[data-node-id="start"] .process-port-in');
  assert.ok(rerenderedSource.classList.contains('is-keyboard-source'), 'rerender restores the active source identity');
  harness.fireEvent(rerenderedSource, 'keydown', { key: 'Escape' });
  assert.equal(host.querySelector('.process-graph').classList.contains('is-connecting'), false);
  assert.equal(received.at(-1)[1].cancelled, true);

  adapter.dispose();
  assert.equal(host.childNodes.length, 0);
});

test('undo-style graph removal cancels a missing keyboard source exactly once', async (t) => {
  const received = [];
  const feedback = ({ phase, source, candidate = {} }) => {
    if (phase === 'source') return { state: 'available', enabled: true, message: 'Start a connection.' };
    if (candidate.nodeId === source.nodeId && candidate.port === source.port) {
      return { state: 'source', enabled: true, message: 'Start a connection.' };
    }
    return { state: 'valid', enabled: true, message: 'Drop to connect.' };
  };
  const { harness, adapter, host } = await mountedAdapter(t, {
    portDragStart: (payload) => received.push(['start', payload]),
    portDragEnd: (payload) => received.push(['end', payload]),
  }, { connectionFeedback: feedback });
  const graph = host.querySelector('.process-graph');
  const graphFocus = graph.focus.bind(graph);
  let graphFocusRestorations = 0;
  Object.defineProperty(graph, 'focus', {
    configurable: true,
    value(options) { graphFocusRestorations += 1; return graphFocus(options); },
  });
  const source = host.querySelector('[data-node-id="start"] .process-port-out');
  source.focus();
  harness.fireEvent(source, 'keydown', { key: 'Enter' });
  const active = adapter.interactionSnapshot();
  assert.equal(active.active, true);
  assert.ok(host.querySelector('.process-editor-band'), 'keyboard gesture owns a rubber band');
  assert.ok(host.querySelector('.process-graph').classList.contains('is-connecting'));

  adapter.setGraph({ nodes: [{ id: 'end', type: 'end', label: 'End' }], edges: [] });
  const cancelled = received.filter(([kind]) => kind === 'end');
  assert.equal(cancelled.length, 1, 'source removal emits one canonical semantic end');
  const { point, ...cancelPayload } = cancelled[0][1];
  assert.ok(Number.isFinite(point.x) && Number.isFinite(point.y));
  assert.deepEqual(cancelPayload, {
    nodeId: 'start', port: 'out',
    targetNodeId: null, targetPort: null, keyboard: true, cancelled: true,
    cancellation: 'source-removed',
  });
  assert.equal(host.querySelector('.process-editor-band'), null, 'cancellation removes the rubber band');
  assert.equal(host.querySelector('.process-graph').classList.contains('is-connecting'), false);
  assert.deepEqual(adapter.interactionSnapshot(), {
    generation: active.generation + 1, active: false,
  }, 'reload freshness sees the completed interaction generation and no active gesture');
  assert.equal(graphFocusRestorations, 1,
    'focus falls back to the graph when the focused source disappears');

  adapter.setGraph({ nodes: [{ id: 'end', type: 'end', label: 'End' }], edges: [] });
  harness.fireEvent(host.querySelector('.process-graph-svg'), 'keydown', { key: 'Escape' });
  assert.equal(received.filter(([kind]) => kind === 'end').length, 1,
    'rerender and Escape cannot double-cancel an already completed gesture');
  adapter.dispose();
});

test('graph adapter translates semantic events and keeps transient pointer frames private', async (t) => {
  const received = [];
  const { harness, adapter, host } = await mountedAdapter(t, {
    nodeDragStart: (payload) => received.push(['start', payload]),
    nodeDragEnd: (payload) => received.push(['commit', payload]),
    nodeDragCancel: (payload) => received.push(['cancel', payload]),
    portDragStart: (payload) => received.push(['port-start', payload]),
    portDragEnd: (payload) => received.push(['port-end', payload]),
  });

  const svg = host.querySelector('.process-graph-svg');
  assert.equal(host.querySelector('.process-action-tooltip'), null,
    'viewer adapters without an editor feedback policy stay inert');
  svg.setPointerCapture = () => {};
  svg.releasePointerCapture = () => {};
  const node = host.querySelector('.process-node[data-node-id="start"]');
  harness.fireEvent(node, 'pointerdown', {
    button: 0, pointerId: 1, pointerType: 'mouse', clientX: 0, clientY: 0,
  });
  harness.fireEvent(svg, 'pointermove', { pointerId: 1, pointerType: 'mouse', clientX: 10, clientY: 20 });
  harness.fireEvent(svg, 'pointermove', { pointerId: 1, pointerType: 'mouse', clientX: 30, clientY: 40 });
  assert.deepEqual(received.map(([kind]) => kind), ['start'],
    'per-frame node movement is retained inside the adapter');
  harness.fireEvent(svg, 'pointerup', { pointerId: 1, pointerType: 'mouse', clientX: 30, clientY: 40 });

  harness.fireEvent(node, 'pointerdown', {
    button: 0, pointerId: 2, pointerType: 'mouse', clientX: 0, clientY: 0,
  });
  harness.fireEvent(svg, 'pointermove', { pointerId: 2, pointerType: 'mouse', clientX: 10, clientY: 10 });
  harness.fireEvent(svg, 'lostpointercapture', { pointerId: 2 });
  assert.deepEqual(received.map(([kind]) => kind), ['start', 'commit', 'cancel']);

  const port = host.querySelector('.process-node-ports[data-node-id="start"] [data-port="out"]');
  harness.fireEvent(port, 'pointerdown', {
    button: 0, pointerId: 3, pointerType: 'mouse', clientX: 0, clientY: 0,
  });
  assert.ok(host.querySelector('.process-editor-band'));
  harness.fireEvent(svg, 'pointermove', { pointerId: 3, pointerType: 'mouse', clientX: 30, clientY: 40 });
  assert.match(host.querySelector('.process-editor-band').getAttribute('d'), /30 40$/);
  harness.fireEvent(svg, 'pointercancel', { pointerId: 3, pointerType: 'mouse', clientX: 30, clientY: 40 });
  assert.equal(host.querySelector('.process-editor-band'), null);
  assert.deepEqual(received.map(([kind]) => kind), ['start', 'commit', 'cancel', 'port-start', 'port-end']);
});

test('graph adapter snapshots layout, preserves viewport on graph input, and disposes idempotently', async (t) => {
  let clicks = 0;
  const { harness, adapter, host } = await mountedAdapter(t, { canvasClick: () => { clicks += 1; } });
  const svg = host.querySelector('.process-graph-svg');
  svg.getBoundingClientRect = () => ({ left: 0, top: 0, width: 800, height: 600, right: 800, bottom: 600 });
  assert.equal(adapter.zoomBy(1.5), true);
  adapter.centerOn(255, 206);
  const expectedView = adapter.viewSnapshot();
  const first = adapter.layoutSnapshot();
  first.nodes[0].x = 9999;
  assert.notEqual(adapter.layoutSnapshot().nodes[0].x, 9999, 'layout outputs are detached snapshots');

  adapter.setGraph({ nodes: [{ id: 'only', type: 'task' }], edges: [] });
  assert.deepEqual(adapter.viewSnapshot(), expectedView);
  harness.fireEvent(svg, 'click');
  assert.equal(clicks, 1);

  adapter.dispose();
  adapter.dispose();
  harness.fireEvent(svg, 'click');
  assert.equal(clicks, 1, 'detached graph events stop after disposal');
  assert.equal(host.childNodes.length, 0);
  assert.deepEqual(adapter.layoutSnapshot(), { nodes: [], edges: [] });
  assert.deepEqual(adapter.viewSnapshot(), { x: 0, y: 0, k: 1 });
  assert.deepEqual(adapter.setGraph({ nodes: [], edges: [] }), { nodes: [], edges: [] });
  assert.equal(adapter.hasActiveInteraction(), false);
  assert.deepEqual(adapter.interactionSnapshot(), { generation: 0, active: false });
  assert.deepEqual(adapter.clientToGraph(1, 2), { x: 0, y: 0 });
  assert.deepEqual(adapter.canvasCenter(), { x: 0, y: 0 });
  assert.deepEqual(adapter.graphPointToHost({ x: 1, y: 2 }), { left: 0, top: 0 });
  assert.deepEqual(adapter.clientPointToHost({ clientX: 1, clientY: 2 }), { left: 0, top: 0 });
  assert.equal(adapter.zoomBy(2), false);
  assert.equal(adapter.resetZoom(), false);
  assert.doesNotThrow(() => {
    adapter.setSelection(null);
    adapter.fit();
    adapter.centerOn(1, 2);
    adapter.focus();
    adapter.focusNode('only');
    adapter.beginConnectionBand({ x: 1, y: 2 });
    adapter.updateConnectionBand({ x: 3, y: 4 });
    adapter.endConnectionBand();
  });
});

test('lost capture and window blur cancel active adapter gestures exactly once', async (t) => {
  const cancelled = [];
  const { harness, host } = await mountedAdapter(t, { nodeDragCancel: (payload) => cancelled.push(payload) });
  const svg = host.querySelector('.process-graph-svg');
  svg.setPointerCapture = () => {};
  svg.releasePointerCapture = () => {};
  const start = host.querySelector('.process-node[data-node-id="start"]');
  harness.fireEvent(start, 'pointerdown', { button: 0, pointerId: 41, clientX: 0, clientY: 0 });
  harness.fireEvent(svg, 'pointermove', { pointerId: 41, clientX: 12, clientY: 34 });
  harness.fireEvent(svg, 'lostpointercapture', { pointerId: 41 });
  harness.fireEvent(svg, 'lostpointercapture', { pointerId: 41 });
  assert.equal(cancelled.length, 1);

  const end = host.querySelector('.process-node[data-node-id="end"]');
  harness.fireEvent(end, 'pointerdown', { button: 0, pointerId: 42, clientX: 0, clientY: 0 });
  harness.fireEvent(svg, 'pointermove', { pointerId: 42, clientX: 9, clientY: 8 });
  harness.window.dispatchEvent(new harness.window.Event('blur'));
  harness.window.dispatchEvent(new harness.window.Event('blur'));
  assert.equal(cancelled.length, 2);
});
