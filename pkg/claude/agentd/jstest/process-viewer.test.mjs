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
