import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

function richEnvelope() {
  return {
    run: { id: 'run-rich', effectiveStatus: 'running' },
    viewerV2: {
      routingAvailable: true,
      exactTopology: {
        start: 'fork',
        nodes: [
          { id: 'fork', type: 'parallel' }, { id: 'left', type: 'task' },
          { id: 'merge', type: 'end', join: 'any' },
        ],
        edges: [
          { id: 'genesis', from: '', outcome: 'start', to: 'fork' },
          { id: 'edge-left', from: 'fork', outcome: 'left', to: 'left' },
          { id: 'edge-merge', from: 'left', outcome: 'pass', to: 'merge' },
        ],
      },
      routing: {
        edges: [
          { edgeId: 'edge-left', state: 'consumed', count: 1 },
          { edgeId: 'edge-merge', state: 'arrived', count: 1 },
        ],
        joins: [{ nodeId: 'merge', reservationId: 'join-1', policy: 'any', state: 'activated', arrived: 1, open: 1, impossible: 0, failed: 0, skipped: 0, canceled: 0, winnerPathId: 'winner-1', detached: 1 }],
        stateCounts: { paths: [{ state: 'arrived', count: 1 }], scopes: [], reservations: [], propagation: [], detachedPathCount: 1, detachedSinkCount: 0 },
        details: {}, aggregate: { paths: 4, reservations: 3 },
      },
    },
    report: { nodes: { left: { timeline: [{ seq: 2, event: 'settled' }, { seq: 1, event: 'started' }] } } },
  };
}

test('viewer graph uses exact topology plus checkpoint overlay and ignores evidence order/content', async (t) => {
  const harness = await createPreactHarness(t);
  const { buildViewerGraph, sanitizedTimeline } = await harness.importDashboardModule('js/process-viewer-core.js');
  const envelope = richEnvelope();
  const graph = buildViewerGraph(envelope);
  assert.deepEqual(graph.nodes.map((node) => node.id), ['fork', 'left', 'merge']);
  assert.deepEqual(graph.edges.map((edge) => edge.id), ['edge-left', 'edge-merge']);
  assert.match(graph.nodes.find((node) => node.id === 'merge').overlay.label, /any · activated/);
  assert.match(graph.nodes.find((node) => node.id === 'merge').overlay.badge, /1 detached/);
  assert.match(graph.edges.find((edge) => edge.id === 'edge-merge').badge, /arrived 1/);

  const reordered = structuredClone(envelope);
  reordered.report.nodes.left.timeline.reverse();
  reordered.report.nodes.left.timeline.push({ seq: 99, event: 'extra sanitized evidence' });
  assert.deepEqual(buildViewerGraph(reordered), graph, 'evidence absence/reordering/addition cannot drive topology or overlay');
  assert.deepEqual(sanitizedTimeline(envelope).map((entry) => entry.seq), [1, 2]);
});

test('unavailable viewer preserves explicit and implicit exact-topology joins', async (t) => {
  const harness = await createPreactHarness(t);
  const { buildViewerGraph } = await harness.importDashboardModule('js/process-viewer-core.js');
  const explicit = richEnvelope();
  explicit.viewerV2.routingAvailable = false;
  delete explicit.viewerV2.routing;
  const explicitGraph = buildViewerGraph(explicit);
  assert.match(explicitGraph.nodes.find((node) => node.id === 'merge').overlay.label, /any · exact topology/);
  assert.equal(explicitGraph.edges.find((edge) => edge.id === 'edge-merge').joinOnTarget, 'any');

  const implicit = structuredClone(explicit);
  delete implicit.viewerV2.exactTopology.nodes.find((node) => node.id === 'merge').join;
  implicit.viewerV2.exactTopology.nodes.push({ id: 'right', type: 'task' });
  implicit.viewerV2.exactTopology.edges.push(
    { id: 'edge-right', from: 'fork', outcome: 'right', to: 'right' },
    { id: 'edge-right-merge', from: 'right', outcome: 'pass', to: 'merge' },
  );
  const implicitGraph = buildViewerGraph(implicit);
  assert.match(implicitGraph.nodes.find((node) => node.id === 'merge').overlay.label, /all · exact topology/);
  assert.deepEqual(
    implicitGraph.edges.filter((edge) => edge.to === 'merge').map((edge) => edge.joinOnTarget),
    ['all', 'all'],
  );
});

test('viewer graph renders healthy, failed, and terminal-warning edge classes honestly', async (t) => {
  const harness = await createPreactHarness(t);
  const { buildViewerGraph } = await harness.importDashboardModule('js/process-viewer-core.js');
  const { ProcessGraph } = await harness.importDashboardModule('js/process-graph.js');
  const envelope = richEnvelope();
  envelope.viewerV2.routing.edges.find((edge) => edge.edgeId === 'edge-merge').state = 'failed';
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const widget = new ProcessGraph(host, buildViewerGraph(envelope), { fitOnRender: false });
  assert.ok(host.querySelector('[data-edge-id="id:edge-left"] .process-edge-badge-info'), 'consumed edge uses non-error info styling');
  assert.ok(host.querySelector('[data-edge-id="id:edge-merge"] .process-edge-badge-error'), 'failed edge uses error styling');

  envelope.viewerV2.routing.edges.find((edge) => edge.edgeId === 'edge-merge').state = 'impossible';
  widget.setGraph(buildViewerGraph(envelope));
  assert.ok(host.querySelector('[data-edge-id="id:edge-merge"] .process-edge-badge-warning'), 'impossible edge uses warning styling');
  widget.destroy();
});

test('viewer helpers render closed unavailable vocabulary and typed page cells', async (t) => {
  const harness = await createPreactHarness(t);
  const { detailPage, detailRowCells, viewerUnavailable, viewerStateChips } = await harness.importDashboardModule('js/process-viewer-core.js');
  assert.deepEqual(viewerUnavailable({ routingAvailable: false, routingUnavailableReason: 'over_budget' }), {
    reason: 'over_budget', title: 'Viewer budget exceeded',
    detail: 'The dashboard failed closed instead of rendering a partial or misleading routing view.',
  });
  const routing = richEnvelope().viewerV2.routing;
  routing.details.detachments = {
    page: { offset: 25, limit: 25, total: 26, hasMore: false },
    items: [{ reservationId: 'r', candidateId: 'c', winnerPathId: 'w', reasonCode: 'any_loser' }],
  };
  assert.equal(detailPage(routing, 'detachments').page.offset, 25);
  assert.deepEqual(detailRowCells('detachments', routing.details.detachments.items[0]), ['r', 'c', 'w', 'any_loser']);
  assert.deepEqual(viewerStateChips(routing), [['Paths', 'arrived 1'], ['Detached paths', '1']]);
});

test('viewer component renders explicit unavailable state without evidence fallback', async (t) => {
  const harness = await createPreactHarness(t);
  const { ProcessViewerBoundary } = await harness.importDashboardModule('js/process-viewer-island.js');
  const actions = { loadRunView: async () => ({
    run: { id: 'legacy-run', effectiveStatus: 'running' },
    viewerV2: { stateSchemaVersion: 6, pathProtocol: 'legacy_v6', routingAvailable: false, routingUnavailableReason: 'legacy_schema' },
    report: { nodes: { work: { timeline: [{ seq: 1, event: 'attempt_started' }] } } },
  }) };
  const mounted = await harness.mount(harness.html`<${ProcessViewerBoundary} spec=${{ id: 'legacy-run', key: 'legacy-run' }} actions=${actions} />`);
  for (let i = 0; i < 5; i++) await harness.act(() => Promise.resolve());
  const unavailable = mounted.container.querySelector('.process-viewer-unavailable.reason-legacy_schema');
  assert.ok(unavailable);
  assert.match(unavailable.textContent, /without reconstructing path state from legacy evidence/);
  assert.match(mounted.container.querySelector('.process-viewer-timeline').textContent, /attempt_started/);
  assert.match(mounted.container.querySelector('.process-viewer-graph-panel').textContent, /failed closed.*will not fall back/i);
  await mounted.unmount();
});

test('viewer detail tabs expose complete ARIA relationships and automatic keyboard activation without resetting pagination', async (t) => {
  const harness = await createPreactHarness(t);
  const { ProcessViewerBoundary } = await harness.importDashboardModule('js/process-viewer-island.js');
  const keys = ['generations', 'scopes', 'closures', 'causeSets', 'causes', 'detachments', 'detachedSinks'];
  const totals = { generations: 51, scopes: 51, closures: 3, causeSets: 51, causes: 51, detachments: 51, detachedSinks: 51 };
  const calls = [];
  const actions = { loadRunView: async (_id, offset, limit) => {
    calls.push({ offset, limit });
    const envelope = richEnvelope();
    envelope.viewerV2.routing.details = Object.fromEntries(keys.map((key) => [key, {
      page: { offset, limit, total: totals[key], hasMore: offset + limit < totals[key] },
      items: Array.from({ length: Math.min(limit, Math.max(0, totals[key] - offset)) }, () => ({})),
    }]));
    return envelope;
  } };
  const mounted = await harness.mount(harness.html`<${ProcessViewerBoundary}
    spec=${{ id: 'run-rich', key: 'run-rich' }} actions=${actions} />`);
  for (let i = 0; i < 5; i++) await harness.act(() => Promise.resolve());

  const tabs = [...mounted.container.querySelectorAll('[role="tab"]')];
  assert.equal(tabs.length, keys.length);
  for (const [index, button] of tabs.entries()) {
    assert.equal(button.id, `process-viewer-detail-tab-${keys[index]}`);
    assert.equal(button.getAttribute('aria-controls'), `process-viewer-detail-panel-${keys[index]}`);
    const panel = mounted.container.querySelector(`#${button.getAttribute('aria-controls')}`);
    assert.ok(panel, `${keys[index]} tab controls a stable panel`);
    assert.equal(panel.getAttribute('role'), 'tabpanel');
    assert.equal(panel.getAttribute('aria-labelledby'), button.id);
  }
  assert.deepEqual(tabs.map((button) => button.getAttribute('aria-selected')), ['true', 'false', 'false', 'false', 'false', 'false', 'false']);
  assert.deepEqual(tabs.map((button) => button.getAttribute('tabIndex')), ['0', '-1', '-1', '-1', '-1', '-1', '-1']);
  assert.equal(mounted.container.querySelectorAll('[role="tabpanel"]:not([hidden])').length, 1);

  const nextPage = [...mounted.container.querySelectorAll('.process-viewer-detail-summary button')]
    .find((button) => /next/.test(button.textContent));
  await harness.act(() => harness.fireEvent(nextPage, 'click'));
  for (let i = 0; i < 5; i++) await harness.act(() => Promise.resolve());
  assert.deepEqual(calls, [{ offset: 0, limit: 25 }, { offset: 25, limit: 25 }]);

  const selectedTab = () => mounted.container.querySelector('[role="tab"][aria-selected="true"]');
  const press = async (key) => {
    let event;
    await harness.act(() => { event = harness.fireEvent(harness.document.activeElement, 'keydown', { key }); });
    assert.equal(event.defaultPrevented, true, `${key} prevents page-level keyboard behavior`);
  };
  tabs[0].focus();
  await press('ArrowRight');
  assert.equal(selectedTab().id, 'process-viewer-detail-tab-scopes');
  assert.equal(harness.document.activeElement, selectedTab());
  assert.deepEqual(calls, [{ offset: 0, limit: 25 }, { offset: 25, limit: 25 }],
    'keyboard selection retains a page that is valid for the target tab');
  assert.match(mounted.container.querySelector('.process-viewer-detail-summary').textContent, /26–50 of 51/);
  await press('ArrowRight');
  for (let i = 0; i < 5; i++) await harness.act(() => Promise.resolve());
  assert.equal(selectedTab().id, 'process-viewer-detail-tab-closures');
  assert.deepEqual(calls.at(-1), { offset: 0, limit: 25 }, 'keyboard selection clamps an invalid target page');
  assert.match(mounted.container.querySelector('.process-viewer-detail-summary').textContent, /1–3 of 3/);
  await press('Home');
  assert.equal(selectedTab().id, 'process-viewer-detail-tab-generations');
  await press('ArrowLeft');
  assert.equal(selectedTab().id, 'process-viewer-detail-tab-detachedSinks', 'Left wraps to the last tab');
  await press('ArrowRight');
  assert.equal(selectedTab().id, 'process-viewer-detail-tab-generations');
  await press('End');
  assert.equal(selectedTab().id, 'process-viewer-detail-tab-detachedSinks');

  const nextLastPage = [...mounted.container.querySelectorAll('.process-viewer-detail-summary button')]
    .find((button) => /next/.test(button.textContent));
  await harness.act(() => harness.fireEvent(nextLastPage, 'click'));
  for (let i = 0; i < 5; i++) await harness.act(() => Promise.resolve());
  const closures = mounted.container.querySelector('#process-viewer-detail-tab-closures');
  await harness.act(() => harness.fireEvent(closures, 'click'));
  for (let i = 0; i < 5; i++) await harness.act(() => Promise.resolve());
  assert.deepEqual(calls.at(-1), { offset: 0, limit: 25 }, 'mouse activation preserves its existing first-page behavior');
  assert.equal(selectedTab(), closures);
  await mounted.unmount();
});

test('viewer component requests bounded next pages and preserves aggregate context', async (t) => {
  const harness = await createPreactHarness(t);
  const { ProcessViewerBoundary } = await harness.importDashboardModule('js/process-viewer-island.js');
  const calls = [];
  const actions = { loadRunView: async (_id, offset, limit) => {
    calls.push({ offset, limit });
    const details = Object.fromEntries(['generations', 'scopes', 'closures', 'causeSets', 'causes', 'detachments', 'detachedSinks'].map((key) => [key, {
      page: { offset, limit, total: key === 'generations' ? 26 : 0, hasMore: key === 'generations' && offset === 0 },
      items: key === 'generations' ? [{ nodeId: offset ? 'second-page' : 'first-page', generation: offset + 1, policy: 'exclusive', reservationState: 'open' }] : [],
    }]));
    return {
      run: { id: 'paged-run', effectiveStatus: 'running' }, report: { nodes: {} },
      viewerV2: { stateSchemaVersion: 7, pathProtocol: 'path_v1', routingAvailable: true, routing: {
        edges: [], joins: [], stateCounts: {}, aggregate: { paths: 99, reservations: 26 }, details,
      } },
    };
  } };
  const mounted = await harness.mount(harness.html`<${ProcessViewerBoundary} spec=${{ id: 'paged-run', key: 'paged-run' }} actions=${actions} />`);
  for (let i = 0; i < 5; i++) await harness.act(() => Promise.resolve());
  assert.match(mounted.container.querySelector('.process-viewer-details').textContent, /99 paths · 26 generations/);
  assert.match(mounted.container.querySelector('.process-viewer-table').textContent, /first-page/);
  const next = [...mounted.container.querySelectorAll('.process-viewer-detail-summary button')].find((button) => /next/.test(button.textContent));
  await harness.act(() => harness.fireEvent(next, 'click'));
  for (let i = 0; i < 5; i++) await harness.act(() => Promise.resolve());
  assert.deepEqual(calls, [{ offset: 0, limit: 25 }, { offset: 25, limit: 25 }]);
  assert.match(mounted.container.querySelector('.process-viewer-table').textContent, /second-page/);
  await mounted.unmount();
});

test('mounted viewer refreshes checkpoint state without resetting active tab or page', async (t) => {
  const harness = await createPreactHarness(t);
  const { ProcessViewerBoundary, VIEWER_REFRESH_MS } = await harness.importDashboardModule('js/process-viewer-island.js');
  let nextTimer = 1;
  const timers = new Map();
  const setTimeoutImpl = (callback, delay) => {
    assert.equal(delay, VIEWER_REFRESH_MS);
    const id = nextTimer++;
    timers.set(id, callback);
    return id;
  };
  const clearTimeoutImpl = (id) => timers.delete(id);
  const calls = [];
  let checkpoint = 1;
  let pendingResolve = null;
  const envelopeAt = (offset) => {
    const envelope = richEnvelope();
    envelope.run.effectiveStatus = checkpoint === 1 ? 'running' : 'completed';
    envelope.viewerV2.routing.joins[0].winnerPathId = `winner-${checkpoint}`;
    envelope.viewerV2.routing.joins[0].detached = checkpoint;
    envelope.viewerV2.routing.stateCounts.paths = [{ state: checkpoint === 1 ? 'arrived' : 'consumed', count: checkpoint }];
    envelope.viewerV2.routing.details = Object.fromEntries(
      ['generations', 'scopes', 'closures', 'causeSets', 'causes', 'detachments', 'detachedSinks'].map((key) => [key, {
        page: { offset, limit: 25, total: key === 'scopes' ? 26 : 0, hasMore: key === 'scopes' && offset === 0 },
        items: key === 'scopes' ? [{ id: `scope-${checkpoint}-page-${offset}`, generation: 1, state: 'open' }] : [],
      }]),
    );
    return envelope;
  };
  const actions = { loadRunView: async (_id, offset, limit) => {
    calls.push({ offset, limit });
    if (checkpoint === 3) return new Promise((resolve) => { pendingResolve = resolve; });
    return envelopeAt(offset);
  } };
  const viewer = (active = true) => harness.html`<${ProcessViewerBoundary}
    spec=${{ id: 'run-rich', key: 'run-rich' }} actions=${actions}
    active=${active} setTimeoutImpl=${setTimeoutImpl} clearTimeoutImpl=${clearTimeoutImpl} />`;
  const mounted = await harness.mount(viewer());
  for (let i = 0; i < 6; i++) await harness.act(() => Promise.resolve());
  const scopes = [...mounted.container.querySelectorAll('.process-viewer-tabs button')].find((button) => /Scopes/.test(button.textContent));
  await harness.act(() => harness.fireEvent(scopes, 'click'));
  const next = [...mounted.container.querySelectorAll('.process-viewer-detail-summary button')].find((button) => /next/.test(button.textContent));
  await harness.act(() => harness.fireEvent(next, 'click'));
  for (let i = 0; i < 6; i++) await harness.act(() => Promise.resolve());
  const graphRoot = mounted.container.querySelector('.process-graph');
  const viewport = graphRoot.querySelector('.process-graph-viewport');
  harness.fireEvent(graphRoot.querySelector('.process-graph-svg'), 'wheel', {
    deltaX: 18, deltaY: 9, deltaMode: 0, ctrlKey: false, shiftKey: false, clientX: 0, clientY: 0,
  });
  const viewportBeforeRefresh = viewport.getAttribute('transform');

  checkpoint = 2;
  const [timerID, refresh] = timers.entries().next().value;
  timers.delete(timerID);
  await harness.act(() => refresh());
  for (let i = 0; i < 6; i++) await harness.act(() => Promise.resolve());
  assert.deepEqual(calls, [{ offset: 0, limit: 25 }, { offset: 25, limit: 25 }, { offset: 25, limit: 25 }]);
  assert.match(mounted.container.querySelector('.process-viewer-run-state').textContent, /completed/);
  assert.match(mounted.container.querySelector('.process-viewer-state-chips').textContent, /consumed 2/);
  assert.match(mounted.container.querySelector('.process-node[data-node-id="merge"]').getAttribute('aria-label'), /2 detached/);
  assert.match(mounted.container.querySelector('.process-viewer-tabs button.active').textContent, /Scopes/);
  assert.match(mounted.container.querySelector('.process-viewer-table').textContent, /scope-2-page-25/);
  assert.equal(mounted.container.querySelector('.process-graph'), graphRoot, 'checkpoint refresh retains one graph widget per run');
  assert.equal(mounted.container.querySelector('.process-graph-viewport').getAttribute('transform'), viewportBeforeRefresh,
    'checkpoint refresh preserves operator pan and zoom');

  await mounted.rerender(viewer(false));
  assert.equal(timers.size, 0, 'inactive Processes tab cancels the viewer poll');
  const callsWhileInactive = calls.length;
  for (let i = 0; i < 3; i++) await harness.act(() => Promise.resolve());
  assert.equal(calls.length, callsWhileInactive, 'inactive Processes tab does not load a checkpoint');
  await mounted.rerender(viewer(true));
  for (let i = 0; i < 6; i++) await harness.act(() => Promise.resolve());
  assert.equal(calls.at(-1).offset, 25, 'reactivation refreshes the current page immediately');
  assert.equal(calls.length, callsWhileInactive + 1);

  checkpoint = 3;
  const [pendingTimerID, pendingRefresh] = timers.entries().next().value;
  timers.delete(pendingTimerID);
  await harness.act(() => pendingRefresh());
  for (let i = 0; i < 3; i++) await harness.act(() => Promise.resolve());
  assert.ok(pendingResolve, 'refresh request is in flight before cleanup');
  await mounted.unmount();
  pendingResolve(envelopeAt(25));
  for (let i = 0; i < 3; i++) await harness.act(() => Promise.resolve());
  assert.equal(timers.size, 0, 'unmounted viewer neither retains nor reschedules refresh timers');
});
