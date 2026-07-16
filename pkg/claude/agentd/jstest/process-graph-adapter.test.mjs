import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

async function mountedAdapter(t, events = {}) {
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
  });
  return { harness, host, adapter };
}

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
