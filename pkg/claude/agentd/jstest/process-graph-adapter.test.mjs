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

test('editor interaction layering raises one node without changing semantic layers or keyboard order', async (t) => {
  const { harness, adapter, host } = await mountedAdapter(t, {}, { interactionLayering: true });
  const svg = host.querySelector('.process-graph-svg');
  const viewport = host.querySelector('.process-graph-viewport');
  const nodeLayer = host.querySelector('.process-node-layer');
  const portLayer = host.querySelector('.process-port-layer');
  const frontNodeLayer = host.querySelector('.process-front-node-layer');
  const frontPortLayer = host.querySelector('.process-front-port-layer');
  const edgePath = host.querySelector('.process-edge-path').getAttribute('d');
  const nodeOrder = () => [...nodeLayer.children].map((node) => node.dataset.nodeId);
  const portOrder = () => [...portLayer.children].map((node) => node.dataset.nodeId);
  const canonicalNodeOrder = nodeOrder();
  const canonicalPortOrder = portOrder();
  assert.deepEqual([...viewport.children].map((layer) => layer.dataset.key),
    ['edges', 'nodes', 'front-node', 'ports', 'front-ports'],
    'edges, nodes, ports, and the two contained paint layers keep fixed semantic stacking');

  const start = nodeLayer.querySelector('[data-node-id="start"]');
  const focusElement = harness.window.HTMLElement.prototype.focus;
  Object.defineProperty(start, 'focus', {
    configurable: true,
    value(options) { return focusElement.call(this, options); },
  });
  let blurs = 0;
  start.addEventListener('blur', () => { blurs += 1; });
  start.focus();
  adapter.setSelection({ type: 'node', id: 'start' });
  assert.equal(harness.document.activeElement, start,
    'painting the focused node at the front leaves the canonical focus owner live');
  assert.equal(blurs, 0, 'raising does not synthesize blur');
  assert.equal(frontNodeLayer.firstElementChild?.dataset.nodeId, 'start');
  assert.equal(frontPortLayer.firstElementChild?.dataset.nodeId, 'start');
  assert.equal(frontNodeLayer.getAttribute('aria-hidden'), 'true');
  assert.equal(frontPortLayer.getAttribute('aria-hidden'), 'true');
  assert.equal(frontNodeLayer.querySelector('[role], [tabindex], [aria-label]'), null,
    'the paint/hit copy never joins the accessibility tree');
  assert.equal(frontPortLayer.querySelector('[role], [tabindex], [aria-label]'), null,
    'copied ports never duplicate canonical button ownership');
  assert.deepEqual(nodeOrder(), canonicalNodeOrder);
  assert.deepEqual(portOrder(), canonicalPortOrder);
  assert.equal(host.querySelector('.process-edge-path').getAttribute('d'), edgePath,
    'raising a node never changes edge routing');

  const canonicalTraversal = () => [
    ...nodeLayer.querySelectorAll('[tabindex="0"]'),
    ...portLayer.querySelectorAll('[tabindex="0"]'),
  ].map((element) => `${element.closest('[data-node-id]')?.dataset.nodeId}:${element.dataset.port || 'node'}`);
  const traversal = canonicalTraversal();
  adapter.setSelection({ type: 'node', id: 'end' });
  adapter.setSelection({ type: 'node', id: 'start' });
  assert.deepEqual(canonicalTraversal(), traversal,
    'sequential node/port traversal is canonical rather than click-history ordered');
  for (const port of portLayer.querySelectorAll('.process-port')) {
    assert.equal(port.getAttribute('role'), 'button');
    assert.equal(port.getAttribute('tabindex'), '0');
    assert.ok(port.getAttribute('aria-label'));
  }

  let captured = 0;
  svg.setPointerCapture = () => { captured += 1; };
  svg.releasePointerCapture = () => {};
  const end = nodeLayer.querySelector('[data-node-id="end"]');
  harness.fireEvent(end, 'pointerdown', {
    button: 0, pointerId: 77, pointerType: 'mouse', clientX: 1, clientY: 2,
  });
  assert.equal(captured, 1, 'raising during pointerdown retains SVG pointer capture');
  assert.equal(adapter.interactionSnapshot().active, true);
  harness.fireEvent(svg, 'pointerup', {
    pointerId: 77, pointerType: 'mouse', clientX: 1, clientY: 2,
  });
  assert.equal(adapter.interactionSnapshot().active, false);

  adapter.setGraph({
    nodes: [{ id: 'start', type: 'start' }, { id: 'end', type: 'end' }], edges: [],
  });
  assert.equal(frontNodeLayer.firstElementChild?.dataset.nodeId, 'end',
    'ordinary rerenders preserve the one live semantic front identity');
  adapter.setGraph({ nodes: [{ id: 'start', type: 'start' }], edges: [] });
  assert.equal(frontNodeLayer.childElementCount, 0, 'node deletion prunes the front identity');
  assert.equal(frontPortLayer.childElementCount, 0);
  adapter.setGraph({
    nodes: [{ id: 'start', type: 'start' }, { id: 'end', type: 'end' }], edges: [],
  });
  assert.equal(frontNodeLayer.childElementCount, 0,
    'reusing a deleted ID does not resurrect stale presentation state');
  adapter.setSelection({ type: 'node', id: 'end' });
  adapter.setGraph({
    nodes: [{ id: 'start', type: 'start' }, { id: 'end', type: 'end' }], edges: [],
  }, { resetInteractionLayering: true });
  assert.equal(frontNodeLayer.childElementCount, 0,
    'whole-model replacement explicitly resets even when node IDs are reused');
});

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

test('undo-style graph removal cancels a missing pointer source exactly once', async (t) => {
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
  const svg = host.querySelector('.process-graph-svg');
  svg.setPointerCapture = () => {};
  svg.releasePointerCapture = () => {};
  const source = host.querySelector('[data-node-id="start"] .process-port-out');
  harness.fireEvent(source, 'pointerdown', {
    button: 0, pointerId: 63, pointerType: 'mouse', clientX: 12, clientY: 24,
  });
  const active = adapter.interactionSnapshot();
  assert.equal(active.active, true);
  assert.ok(host.querySelector('.process-editor-band'));
  assert.ok(host.querySelector('.process-graph').classList.contains('is-connecting'));

  adapter.setGraph({ nodes: [{ id: 'end', type: 'end', label: 'End' }], edges: [] });
  const ends = received.filter(([kind]) => kind === 'end');
  assert.equal(ends.length, 1, 'source removal emits one canonical pointer end');
  const { event, point, ...payload } = ends[0][1];
  assert.ok(Number.isFinite(point.x) && Number.isFinite(point.y));
  assert.equal(event.type, 'source-removed');
  assert.deepEqual(payload, {
    nodeId: 'start', port: 'out', targetNodeId: null, targetPort: null,
    cancelled: true, cancellation: 'source-removed',
  });
  assert.equal(host.querySelector('.process-editor-band'), null);
  assert.equal(host.querySelector('.process-graph').classList.contains('is-connecting'), false);
  assert.deepEqual(adapter.interactionSnapshot(), {
    generation: active.generation + 1, active: false,
  }, 'reload freshness observes the pointer cancellation generation');
  assert.equal(harness.document.activeElement, host.querySelector('.process-graph'),
    'stable graph focus survives removal of the captured connector');
  assert.equal(host.querySelector('[data-node-id="end"] .process-port-out').getAttribute('r'), '6');

  harness.fireEvent(svg, 'pointerup', {
    pointerId: 63, pointerType: 'mouse', clientX: 12, clientY: 24,
  });
  harness.fireEvent(svg, 'keydown', { key: 'Escape' });
  assert.equal(received.filter(([kind]) => kind === 'end').length, 1,
    'late release and Escape cannot double-cancel the removed pointer source');
  adapter.dispose();
});

test('editor metadata removes semantic ports and cancels stale pointer/keyboard sources on the same live node', async (t) => {
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
  const editorNodes = (startPorts) => [
    { id: 'start', type: 'start', label: 'Start', portAvailability: startPorts },
    { id: 'end', type: 'end', label: 'End', portAvailability: { in: true, out: false } },
  ];
  adapter.setGraph({ nodes: editorNodes({ in: false, out: true }), edges: [] });
  assert.equal(host.querySelector('[data-node-id="start"] .process-port-in'), null);
  assert.ok(host.querySelector('[data-node-id="start"] .process-port-out'));
  assert.ok(host.querySelector('[data-node-id="end"] .process-port-in'));
  assert.equal(host.querySelector('[data-node-id="end"] .process-port-out'), null);

  let source = host.querySelector('[data-node-id="start"] .process-port-out');
  harness.fireEvent(source, 'keydown', { key: 'Enter' });
  adapter.setGraph({ nodes: editorNodes({ in: false, out: false }), edges: [] });
  assert.equal(received.filter(([kind]) => kind === 'end').length, 1);
  assert.equal(received.at(-1)[1].keyboard, true);
  assert.equal(received.at(-1)[1].cancellation, 'source-removed');
  assert.equal(adapter.interactionSnapshot().active, false);

  adapter.setGraph({ nodes: editorNodes({ in: false, out: true }), edges: [] });
  const svg = host.querySelector('.process-graph-svg');
  svg.setPointerCapture = () => {};
  svg.releasePointerCapture = () => {};
  source = host.querySelector('[data-node-id="start"] .process-port-out');
  harness.fireEvent(source, 'pointerdown', {
    button: 0, pointerId: 91, pointerType: 'mouse', clientX: 10, clientY: 20,
  });
  adapter.setGraph({ nodes: editorNodes({ in: false, out: false }), edges: [] });
  assert.equal(received.filter(([kind]) => kind === 'end').length, 2);
  assert.equal(received.at(-1)[1].cancellation, 'source-removed');
  assert.equal(adapter.interactionSnapshot().active, false);
  adapter.dispose();
});

test('same-source activation toggles keyboard feedback off with pointer lifecycle parity', async (t) => {
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
  const svg = host.querySelector('.process-graph-svg');
  svg.setPointerCapture = () => {};
  svg.releasePointerCapture = () => {};
  const source = host.querySelector('[data-node-id="start"] .process-port-out');
  assert.equal(source.getAttribute('r'), '6');

  harness.fireEvent(source, 'keydown', { key: 'Enter' });
  const keyboardActive = adapter.interactionSnapshot();
  assert.equal(keyboardActive.active, true);
  assert.equal(source.getAttribute('aria-pressed'), 'true');
  assert.ok(host.querySelector('.process-editor-band'));
  harness.fireEvent(source, 'keydown', { key: 'Enter' });
  const keyboardEnds = received.filter(([kind]) => kind === 'end');
  assert.equal(keyboardEnds.length, 1,
    'second activation emits one semantic end');
  assert.equal(keyboardEnds[0][1].targetNodeId, 'start');
  assert.equal(keyboardEnds[0][1].targetPort, 'out');
  assert.equal(keyboardEnds[0][1].keyboard, true);
  assert.equal(source.getAttribute('aria-pressed'), 'false');
  assert.equal(host.querySelector('.process-editor-band'), null);
  assert.equal(host.querySelector('.process-graph').classList.contains('is-connecting'), false);
  const keyboardEnded = adapter.interactionSnapshot();
  assert.deepEqual(keyboardEnded, {
    generation: keyboardActive.generation + 1, active: false,
  }, 'reload freshness observes the keyboard toggle completion');

  const originalHitTest = harness.document.elementFromPoint;
  harness.document.elementFromPoint = () => source;
  t.after(() => { harness.document.elementFromPoint = originalHitTest; });
  harness.fireEvent(source, 'pointerdown', {
    button: 0, pointerId: 61, pointerType: 'mouse', clientX: 10, clientY: 20,
  });
  assert.ok(host.querySelector('.process-editor-band'));
  harness.fireEvent(svg, 'pointerup', {
    pointerId: 61, pointerType: 'mouse', clientX: 10, clientY: 20,
  });
  const pointerEnds = received.filter(([kind]) => kind === 'end');
  assert.equal(pointerEnds.length, 2,
    'same-source pointer release ends through the same adapter lifecycle');
  assert.equal(pointerEnds[1][1].targetNodeId, 'start');
  assert.equal(pointerEnds[1][1].targetPort, 'out');
  assert.equal(host.querySelector('.process-editor-band'), null);
  assert.deepEqual(adapter.interactionSnapshot(), {
    generation: keyboardEnded.generation + 2, active: false,
  });
  assert.equal(source.getAttribute('r'), '6');
  adapter.dispose();
});

test('disabled pointer feedback rebinds after a synchronous focus rerender', async (t) => {
  const feedback = ({ phase, source }) => source.nodeId === 'end' && source.port === 'out'
    ? { state: 'disabled', enabled: false, message: 'End nodes cannot have outgoing connections.' }
    : { state: phase === 'source' ? 'available' : 'valid', enabled: true, message: 'Connect.' };
  const graph = {
    nodes: [{ id: 'start', type: 'start', label: 'Start' }, { id: 'end', type: 'end', label: 'End' }],
    edges: [{ id: 'start:pass', from: 'start', outcome: 'pass', to: 'end' }],
  };
  const { harness, adapter, host } = await mountedAdapter(t, {}, {
    connectionFeedback: feedback, actionFeedbackDelay: 50, keyboardFeedbackDelay: 50,
  });
  const stalePort = host.querySelector('[data-node-id="end"] .process-port-out');
  const staleFocus = stalePort.focus.bind(stalePort);
  let freshPort = null;
  let freshFocuses = 0;
  Object.defineProperty(stalePort, 'focus', {
    configurable: true,
    value(options) {
      adapter.setGraph(graph);
      freshPort = host.querySelector('[data-node-id="end"] .process-port-out');
      const freshFocus = freshPort.focus.bind(freshPort);
      Object.defineProperty(freshPort, 'focus', {
        configurable: true,
        value(nextOptions) { freshFocuses += 1; return freshFocus(nextOptions); },
      });
      return staleFocus(options);
    },
  });
  const frames = [];
  const stubRAF = globalThis.requestAnimationFrame;
  globalThis.requestAnimationFrame = (callback) => { frames.push(callback); return frames.length; };
  t.after(() => { globalThis.requestAnimationFrame = stubRAF; });
  harness.fireEvent(stalePort, 'pointermove', {
    pointerType: 'mouse', clientX: 1, clientY: 2,
  });
  assert.equal(frames.length, 1, 'pre-click pointer feedback has one queued frame');

  harness.fireEvent(stalePort, 'pointerdown', {
    button: 0, pointerId: 62, pointerType: 'mouse', clientX: 1, clientY: 2,
  });
  await new Promise((resolve) => setTimeout(resolve, 2));
  const tooltip = host.querySelector('.process-action-tooltip');
  assert.ok(freshPort && freshPort !== stalePort && host.contains(freshPort));
  assert.equal(freshFocuses, 1, 'the live connector regains focus after blur-driven replacement');
  assert.equal(freshPort.getAttribute('r'), '6', 'rebind preserves connector geometry');
  assert.equal(freshPort.getAttribute('aria-describedby'), tooltip.id);
  assert.equal(stalePort.hasAttribute('aria-describedby'), false,
    'detached event target never owns the live tooltip relationship');
  assert.ok(tooltip.classList.contains('is-visible'));
  assert.match(tooltip.textContent, /End nodes cannot/);
  frames.shift()();
  assert.equal(freshPort.getAttribute('aria-describedby'), tooltip.id,
    'the invalidated pre-rerender frame cannot clear fresh feedback ownership');
  assert.match(tooltip.textContent, /End nodes cannot/);
  adapter.dispose();
});

test('disabled connectors preserve middle-button and Space-primary pan priority', async (t) => {
  let portStarts = 0;
  const feedback = ({ phase, source }) => source.nodeId === 'end' && source.port === 'out'
    ? { state: 'disabled', enabled: false, message: 'End nodes cannot have outgoing connections.' }
    : { state: phase === 'source' ? 'available' : 'valid', enabled: true, message: 'Connect.' };
  const { harness, adapter, host } = await mountedAdapter(t, {
    portDragStart: () => { portStarts += 1; },
  }, { connectionFeedback: feedback });
  const root = host.querySelector('.process-graph');
  const svg = host.querySelector('.process-graph-svg');
  svg.setPointerCapture = () => {};
  svg.releasePointerCapture = () => {};
  const port = host.querySelector('[data-node-id="end"] .process-port-out');
  const tooltip = host.querySelector('.process-action-tooltip');
  // LinkeDOM does not model SVG focus and its SVG isConnected traversal
  // recurses indefinitely. Reuse the harness's browsing-context focus shim;
  // production focus and event propagation otherwise remain unchanged.
  const focusPort = harness.window.HTMLElement.prototype.focus;
  Object.defineProperty(port, 'focus', {
    configurable: true,
    value(options) { return focusPort.call(this, options); },
  });
  Object.defineProperty(port, 'isConnected', { configurable: true, value: true });
  assert.equal(port.getAttribute('r'), '6');

  const beforeMiddle = adapter.viewSnapshot();
  harness.fireEvent(port, 'pointerdown', {
    button: 1, pointerId: 65, pointerType: 'mouse', clientX: 10, clientY: 20,
  });
  assert.equal(adapter.interactionSnapshot().active, true);
  assert.equal(host.querySelector('.process-editor-band'), null);
  harness.fireEvent(svg, 'pointermove', {
    button: 1, pointerId: 65, pointerType: 'mouse', clientX: 30, clientY: 50,
  });
  harness.fireEvent(svg, 'pointerup', {
    button: 1, pointerId: 65, pointerType: 'mouse', clientX: 30, clientY: 50,
  });
  const afterMiddle = adapter.viewSnapshot();
  assert.equal(afterMiddle.x, beforeMiddle.x + 20);
  assert.equal(afterMiddle.y, beforeMiddle.y + 30);

  harness.fireEvent(port, 'pointerdown', {
    button: 0, pointerId: 66, pointerType: 'mouse', clientX: 40, clientY: 60,
  });
  assert.equal(harness.document.activeElement, port, 'disabled connector owns focus before Space');
  assert.match(tooltip.textContent, /End nodes cannot/);
  assert.equal(port.getAttribute('aria-describedby'), tooltip.id);

  harness.fireEvent(port, 'keydown', { key: ' ', code: 'Space' });
  assert.ok(root.classList.contains('is-space-pan'));
  assert.match(tooltip.textContent, /End nodes cannot/,
    'tapping Space retains disabled keyboard feedback until a pan begins');
  const beforeSpace = adapter.viewSnapshot();
  harness.fireEvent(port, 'pointerdown', {
    button: 0, pointerId: 67, pointerType: 'mouse', clientX: 40, clientY: 60,
  });
  assert.equal(adapter.interactionSnapshot().active, true);
  assert.equal(host.querySelector('.process-editor-band'), null);
  assert.equal(tooltip.textContent, '', 'starting navigation clears disabled feedback');
  assert.equal(port.hasAttribute('aria-describedby'), false);
  harness.fireEvent(svg, 'pointermove', {
    button: 0, pointerId: 67, pointerType: 'mouse', clientX: 55, clientY: 85,
  });
  harness.fireEvent(svg, 'pointerup', {
    button: 0, pointerId: 67, pointerType: 'mouse', clientX: 55, clientY: 85,
  });
  harness.fireEvent(root, 'keyup', { key: ' ', code: 'Space' });
  const afterSpace = adapter.viewSnapshot();
  assert.equal(afterSpace.x, beforeSpace.x + 15);
  assert.equal(afterSpace.y, beforeSpace.y + 25);
  assert.equal(root.classList.contains('is-space-pan'), false);
  assert.equal(portStarts, 0, 'navigation modifiers never start a connector gesture');
  assert.equal(tooltip.textContent, '');
  assert.equal(port.hasAttribute('aria-describedby'), false);
  assert.equal(port.getAttribute('r'), '6');
  adapter.dispose();
});

test('pointerleave invalidates queued feedback before its animation frame', async (t) => {
  const feedback = ({ phase }) => ({
    state: phase === 'source' ? 'available' : 'valid', enabled: true, message: 'Connect from this port.',
  });
  const { harness, adapter, host } = await mountedAdapter(t, {}, {
    connectionFeedback: feedback, actionFeedbackDelay: 0, keyboardFeedbackDelay: 0,
  });
  const frames = [];
  const previousRAF = globalThis.requestAnimationFrame;
  globalThis.requestAnimationFrame = (callback) => { frames.push(callback); return frames.length; };
  t.after(() => { globalThis.requestAnimationFrame = previousRAF; });
  const svg = host.querySelector('.process-graph-svg');
  const port = host.querySelector('[data-node-id="start"] .process-port-out');
  const tooltip = host.querySelector('.process-action-tooltip');

  harness.fireEvent(port, 'pointermove', { pointerId: 64, pointerType: 'mouse', clientX: 1, clientY: 2 });
  assert.equal(frames.length, 1);
  harness.fireEvent(svg, 'pointerleave', { pointerId: 64, pointerType: 'mouse' });
  frames.shift()();
  await new Promise((resolve) => setTimeout(resolve, 2));
  assert.equal(tooltip.textContent, '');
  assert.equal(port.hasAttribute('aria-describedby'), false);
  assert.equal(port.classList.contains('is-connection-hover'), false);

  harness.fireEvent(port, 'pointermove', { pointerId: 64, pointerType: 'mouse', clientX: 3, clientY: 4 });
  assert.equal(frames.length, 1, 'a fresh move schedules independently of the invalidated frame');
  frames.shift()();
  await new Promise((resolve) => setTimeout(resolve, 2));
  assert.ok(tooltip.classList.contains('is-visible'));
  assert.equal(port.getAttribute('aria-describedby'), tooltip.id);
  harness.fireEvent(svg, 'pointerleave', { pointerId: 64, pointerType: 'mouse' });
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

test('graph adapter observes in-canvas pointer coordinates passively and invalidates them on lifecycle exits', async (t) => {
  const observed = [];
  const { harness, adapter, host } = await mountedAdapter(t, {
    canvasPointerMove: (payload) => observed.push(['move', payload.clientX, payload.clientY]),
    canvasPointerLeave: (payload) => observed.push(['leave', payload.reason]),
  });
  const svg = host.querySelector('.process-graph-svg');
  svg.getBoundingClientRect = () => ({
    left: 100.125, top: 50.25, width: 800.5, height: 600.75,
    right: 900.625, bottom: 651,
  });
  const beforeFocus = harness.document.activeElement;
  const move = harness.fireEvent(svg, 'pointermove', {
    pointerId: 81, pointerType: 'mouse', clientX: 450.25, clientY: 280.5,
  });
  assert.equal(move.defaultPrevented, false);
  assert.equal(adapter.hasActiveInteraction(), false, 'observation never starts pointer ownership');
  assert.equal(harness.document.activeElement, beforeFocus, 'observation never moves focus');
  assert.deepEqual(observed, [['move', 450.25, 280.5]]);
  assert.equal(adapter.containsClientPoint(450.25, 280.5), true);
  assert.equal(adapter.containsClientPoint(901, 280.5), false);

  harness.fireEvent(svg, 'pointermove', {
    pointerId: 81, pointerType: 'mouse', clientX: 901, clientY: 280.5,
  });
  assert.deepEqual(observed.at(-1), ['leave', 'bounds']);
  harness.window.dispatchEvent(new harness.window.Event('blur'));
  assert.deepEqual(observed.at(-1), ['leave', 'blur']);
  adapter.dispose();
  assert.deepEqual(observed.at(-1), ['leave', 'dispose']);
  assert.equal(adapter.containsClientPoint(450.25, 280.5), false);
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
  assert.equal(adapter.containsClientPoint(1, 2), false);
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

test('a drag whose terminal event was lost cannot poison the next drag commit', async (t) => {
  const ends = [];
  const cancels = [];
  const { harness, host, adapter } = await mountedAdapter(t, {
    nodeDragEnd: (payload) => ends.push(payload),
    nodeDragCancel: (payload) => cancels.push(payload),
  });
  const svg = host.querySelector('.process-graph-svg');
  svg.setPointerCapture = () => {};
  svg.releasePointerCapture = () => {};
  const laidStart = () => adapter.layoutSnapshot().nodes.find((node) => node.id === 'start');
  const before = laidStart();

  // Gesture A moves 'start' but its pointerup never reaches the SVG.
  const start = host.querySelector('.process-node[data-node-id="start"]');
  harness.fireEvent(start, 'pointerdown', { button: 0, pointerId: 1, pointerType: 'mouse', clientX: before.x, clientY: before.y });
  harness.fireEvent(svg, 'pointermove', { pointerId: 1, pointerType: 'mouse', clientX: before.x + 30, clientY: before.y + 20 });

  // Gesture B reuses the mouse's pointer id: the dead gesture must cancel and
  // the new one must commit from the CURRENT layout, not gesture A's frame.
  harness.fireEvent(start, 'pointerdown', { button: 0, pointerId: 1, pointerType: 'mouse', clientX: before.x + 30, clientY: before.y + 20 });
  harness.fireEvent(svg, 'pointermove', { pointerId: 1, pointerType: 'mouse', clientX: before.x + 70, clientY: before.y + 60 });
  harness.fireEvent(svg, 'pointerup', { pointerId: 1, pointerType: 'mouse', clientX: before.x + 70, clientY: before.y + 60 });

  assert.equal(cancels.length, 1, 'the dead gesture ends through nodeDragCancel');
  assert.equal(ends.length, 1, 'only the live gesture commits');
  assert.deepEqual(ends[0].starts, [{ id: 'start', x: before.x, y: before.y }],
    'the commit starts from the layout at the live gesture, not a stale snapshot');
  assert.deepEqual(ends[0].delta, { x: 40, y: 40 },
    'the delta covers only the live gesture');
});
