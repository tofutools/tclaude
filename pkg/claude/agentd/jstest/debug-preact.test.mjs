import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness, getByRole } from './preact-harness.mjs';

const phase = (name, p50 = 2, children = []) => ({
  name,
  latest_ms: p50,
  p50_ms: p50,
  p90_ms: p50 * 2,
  p99_ms: p50 * 3,
  max_ms: p50 * 4,
  children,
});

const endpoint = (name, total, phases = []) => ({
  endpoint: name,
  count: 1,
  p50_ms: total,
  p90_ms: total,
  p99_ms: total,
  max_ms: total,
  samples: [{ total_ms: total }],
  phases,
});

const payload = (endpoints, generatedAt = '2026-07-13T12:00:00Z') => ({
  generated_at: generatedAt,
  endpoints,
});

test('Debug actions sequence overlapping loads, preserve API calls, and abort on cleanup', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createDebugState }, { createDebugActions }] = await Promise.all([
    harness.importDashboardModule('js/debug-state.js'),
    harness.importDashboardModule('js/debug-actions.js'),
  ]);
  const state = createDebugState({ activeTab: harness.signals.signal('debug') });
  const pending = [];
  const actions = createDebugActions({
    state,
    fetchImpl: (url, options) => new Promise((resolve) => pending.push({ url, options, resolve })),
  });

  const old = actions.load();
  const fresh = actions.load();
  assert.equal(pending[0].url, '/api/perf?limit=240');
  assert.equal(pending[0].options.credentials, 'same-origin');
  assert.equal(pending[0].options.signal.aborted, true, 'newer loads abort older network work');
  pending[1].resolve({
    ok: true,
    json: async () => payload([
      endpoint('/api/z-last', 8),
      endpoint('/api/snapshot', 4, [phase('sessions')]),
    ]),
  });
  assert.equal(await fresh, true);
  pending[0].resolve({ ok: true, json: async () => payload([endpoint('/api/stale', 99)]) });
  assert.equal(await old, false, 'a response that ignores abort still loses the state token race');
  assert.deepEqual(
    state.view.value.endpoints.map((entry) => entry.endpoint),
    ['/api/snapshot', '/api/z-last'],
    'snapshot leads and remaining endpoints retain alphabetical order',
  );

  const abandoned = actions.load();
  assert.equal(state.view.value.request.phase, 'refreshing');
  actions.dispose();
  assert.equal(pending[2].options.signal.aborted, true);
  assert.equal(state.view.value.request.phase, 'ready');
  assert.equal(await actions.load(), false, 'disposed actions cannot restart work');
  pending[2].resolve({ ok: true, json: async () => payload([]) });
  assert.equal(await abandoned, false);
});

test('Debug reset POSTs before a fresh GET and exposes reset failures', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createDebugState }, { createDebugActions }, { DebugApp }] = await Promise.all([
    harness.importDashboardModule('js/debug-state.js'),
    harness.importDashboardModule('js/debug-actions.js'),
    harness.importDashboardModule('js/debug-island.js'),
  ]);
  const state = createDebugState({ activeTab: harness.signals.signal('debug') });
  const calls = [];
  const actions = createDebugActions({
    state,
    fetchImpl: async (url, options) => {
      calls.push({ url, options });
      if (url === '/api/perf/reset') return { ok: true };
      return { ok: true, json: async () => payload([]) };
    },
  });
  assert.equal(await actions.reset(), true);
  assert.deepEqual(calls.map((call) => call.url), ['/api/perf/reset', '/api/perf?limit=240']);
  assert.equal(calls[0].options.method, 'POST');
  assert.equal(calls[0].options.credentials, 'same-origin');
  assert.equal(state.view.value.resetting, false);
  assert.equal(state.view.value.request.phase, 'ready');

  const failedState = createDebugState({ activeTab: harness.signals.signal('groups') });
  const failed = createDebugActions({
    state: failedState,
    fetchImpl: async () => ({ ok: false, status: 503, text: async () => 'reset unavailable' }),
  });
  assert.equal(await failed.reset(), false);
  assert.match(failedState.view.value.request.error, /reset unavailable/);
  const mounted = await harness.mount(harness.html`<${DebugApp} state=${failedState}
    actions=${{ load() {}, reset() {}, cancel() {} }} />`);
  assert.match(getByRole(mounted.container, 'alert').textContent, /Failed to reset poll timings/);
  await mounted.unmount();
});

test('Debug island keys endpoint and phase DOM and owns only an active 10s timer', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createDebugState }, { DebugApp, DEBUG_POLL_MS }] = await Promise.all([
    harness.importDashboardModule('js/debug-state.js'),
    harness.importDashboardModule('js/debug-island.js'),
  ]);
  const activeTab = harness.signals.signal('debug');
  const state = createDebugState({ activeTab });
  let token = state.beginRequest();
  state.commitRequest(token, payload([
    endpoint('/api/z-last', 8),
    endpoint('/api/snapshot', 4, [
      phase('sessions', 2, [phase('session_rows', 1)]),
      phase('tmux', 3),
    ]),
  ]));

  const loads = [];
  let resets = 0;
  let cancels = 0;
  const actions = {
    load: () => { loads.push('load'); return Promise.resolve(true); },
    reset: () => { resets++; return Promise.resolve(true); },
    cancel: () => { cancels++; },
  };
  const timers = new Map();
  let nextTimer = 1;
  const setIntervalImpl = (callback, delay) => {
    const id = nextTimer++;
    timers.set(id, { callback, delay });
    return id;
  };
  const clearIntervalImpl = (id) => timers.delete(id);
  const mounted = await harness.mount(harness.html`<${DebugApp}
    state=${state} actions=${actions} setIntervalImpl=${setIntervalImpl}
    clearIntervalImpl=${clearIntervalImpl} />`);

  assert.equal(loads.length, 1, 'activation triggers an immediate load');
  assert.equal(timers.size, 1);
  assert.equal([...timers.values()][0].delay, DEBUG_POLL_MS);
  const cards = mounted.container.querySelectorAll('.debug-card');
  assert.equal(cards[0].getAttribute('data-key'), 'debug-/api/snapshot');
  assert.equal(cards[1].getAttribute('data-key'), 'debug-/api/z-last');
  const snapshotCard = cards[0];
  const phaseRow = snapshotCard.querySelector('tr[data-key="phase-sessions"]');
  assert.ok(phaseRow);
  assert.equal(snapshotCard.querySelector('tr[data-key="phase-sessions.session_rows"]'), null);
  await harness.act(() => harness.fireEvent(
    getByRole(snapshotCard, 'button', { name: /Expand sessions phase breakdown/ }),
    'click',
  ));
  const childRow = snapshotCard.querySelector('tr[data-key="phase-sessions.session_rows"]');
  assert.ok(childRow, 'phase children expand on demand');
  assert.equal(childRow.getAttribute('data-depth'), '1');
  const spark = getByRole(snapshotCard, 'img', { name: /latency sparkline/ });
  assert.match(spark.querySelector('title').textContent, /latest 4.00 ms/);
  assert.equal(snapshotCard.querySelectorAll('.debug-legend-item').length, 2);
  assert.ok(snapshotCard.querySelector('.debug-phasebar'));
  assert.ok(snapshotCard.querySelector('.debug-table'));
  assert.equal(mounted.container.querySelector('#debug-updated').getAttribute('aria-live'), 'polite');

  await harness.act(() => harness.fireEvent(
    getByRole(mounted.container, 'button', { name: /reset stats/ }),
    'click',
  ));
  assert.equal(resets, 1);
  await harness.act(() => [...timers.values()][0].callback());
  assert.equal(loads.length, 2, 'the active timer reloads at its own cadence');

  snapshotCard.tabIndex = 0;
  snapshotCard.focus();
  await harness.act(() => {
    token = state.beginRequest();
    state.commitRequest(token, payload([
      endpoint('/api/snapshot', 5, [
        phase('sessions', 4, [phase('session_rows', 3)]),
        phase('tmux', 5),
      ]),
      endpoint('/api/z-last', 9),
    ], '2026-07-13T12:00:10Z'));
  });
  assert.equal(mounted.container.querySelector('[data-key="debug-/api/snapshot"]'), snapshotCard);
  assert.equal(snapshotCard.querySelector('tr[data-key="phase-sessions"]'), phaseRow);
  assert.equal(phaseRow.children[1].textContent, '4.00 ms', 'phase latest value refreshes in-place');
  assert.equal(childRow.children[1].textContent, '3.00 ms', 'expanded child values refresh in-place');
  assert.equal(harness.document.activeElement, snapshotCard);

  await harness.act(() => { activeTab.value = 'groups'; });
  assert.equal(timers.size, 0, 'inactive Debug owns no timer');
  assert.ok(cancels >= 1, 'deactivation cancels any in-flight request');
  await harness.act(() => { activeTab.value = 'debug'; });
  assert.equal(loads.length, 3, 'reactivation loads immediately');
  assert.equal(timers.size, 1);
  await mounted.unmount();
  assert.equal(timers.size, 0, 'unmount clears the active timer');
});

test('Debug island renders errors and production mount releases its one host', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createDebugState }, { DebugApp }, { mountDebugFeature }] = await Promise.all([
    harness.importDashboardModule('js/debug-state.js'),
    harness.importDashboardModule('js/debug-island.js'),
    harness.importDashboardModule('js/preact-loader.js'),
  ]);
  const state = createDebugState({ activeTab: harness.signals.signal('groups') });
  const token = state.beginRequest();
  state.failRequest(token, new Error('offline'));
  const mounted = await harness.mount(harness.html`<${DebugApp} state=${state}
    actions=${{ load() {}, reset() {}, cancel() {} }} />`);
  assert.match(getByRole(mounted.container, 'alert').textContent, /offline/);
  await mounted.unmount();

  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  host.id = 'debug-root';
  const cleanup = await mountDebugFeature({
    fetchImpl: async () => { throw new Error('inactive Debug must not fetch'); },
  });
  assert.equal(host.dataset.islandOwner, 'debug');
  assert.ok(host.querySelector('#debug-list'));
  cleanup();
  assert.equal(host.childElementCount, 0);
  assert.equal(host.dataset.islandOwner, undefined);
});
