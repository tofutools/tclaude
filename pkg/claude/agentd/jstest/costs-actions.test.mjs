import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const storage = { getItem: () => null, setItem: () => {}, removeItem: () => {} };
const response = (body, ok = true, status = ok ? 200 : 500) => ({
  ok, status, json: async () => body, text: async () => typeof body === 'string' ? body : JSON.stringify(body),
});
const deferred = () => {
  let resolve;
  const promise = new Promise((done) => { resolve = done; });
  return { promise, resolve };
};

test('Costs actions reject stale loads and expose endpoint failures', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createCostsState }, { createCostsActions }] = await Promise.all([
    harness.importDashboardModule('js/costs-state.js'),
    harness.importDashboardModule('js/costs-actions.js'),
  ]);
  const state = createCostsState({
    snapshot: harness.signals.signal({ cost_tab_visible: true }), activeTab: harness.signals.signal('costs'), prefs: storage,
  });
  const first = deferred();
  let calls = 0;
  const actions = createCostsActions({ state, fetchImpl: async () => {
    calls += 1;
    if (calls === 1) return first.promise;
    if (calls === 2) return response({ from: 'new', to: 'new', days: [], agents: [], total_usd: 0 });
    return response('offline', false, 503);
  } });
  const oldLoad = actions.load();
  assert.equal(await actions.load(), true);
  first.resolve(response({ from: 'old', to: 'old', days: [], agents: [], total_usd: 0 }));
  assert.equal(await oldLoad, false);
  assert.equal(state.payload.value.from, 'new');
  assert.equal(await actions.load(), false);
  assert.match(state.request.value.error, /offline/);
  assert.equal(state.request.value.hasLoaded, false, 'failed range does not retain mismatched stale data');
  assert.equal(state.payload.value, null);
});

test('Cost factor saves are last-writer-wins and errors remain local', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createCostsState }, { createCostsActions }] = await Promise.all([
    harness.importDashboardModule('js/costs-state.js'),
    harness.importDashboardModule('js/costs-actions.js'),
  ]);
  const state = createCostsState({
    snapshot: harness.signals.signal({ cost_tab_visible: true }), activeTab: harness.signals.signal('costs'), prefs: storage,
  });
  const saves = [];
  const persisted = [];
  const actions = createCostsActions({ state, fetchImpl: async (path, options = {}) => {
    if (path === '/api/cost-factor' && options.method === 'POST') {
      const pending = deferred();
      const value = JSON.parse(options.body).estimate_factor;
      saves.push(pending);
      return pending.promise.then((result) => { persisted.push(value); return result; });
    }
    return response({ from: '2026-07-01', to: '2026-07-01', days: [], agents: [], total_usd: 0 });
  } });
  state.editFactor('2');
  const older = actions.saveFactor('2');
  state.editFactor('3');
  const newer = actions.saveFactor('3');
  assert.equal(saves.length, 0, 'queue starts on the next microtask');
  await Promise.resolve();
  assert.equal(saves.length, 1, 'only the older save reaches the server first');
  saves[0].resolve(response({}));
  assert.equal(await older, false);
  await Promise.resolve();
  assert.equal(saves.length, 2, 'newer save starts only after the older response');
  saves[1].resolve(response({}));
  assert.equal(await newer, true);
  assert.deepEqual(persisted, [2, 3], 'server writes are ordered newest-last');
  assert.equal(state.factor.value.raw, '3');
  assert.equal(state.factor.value.status, 'saved');
  assert.equal(await actions.saveFactor('99'), false);
  assert.match(state.factor.value.status, /must be/);
});

test('Harness factors load explicit 1, save independently and reset all', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createCostsState }, { createCostsActions }] = await Promise.all([
    harness.importDashboardModule('js/costs-state.js'), harness.importDashboardModule('js/costs-actions.js'),
  ]);
  const state = createCostsState({ prefs: storage });
  const writes = [];
  const actions = createCostsActions({ state, fetchImpl: async (path, options = {}) => {
    if (options.method === 'POST') { writes.push(JSON.parse(options.body)); return response({}); }
    return response(path === '/api/cost-factor'
      ? { estimate_factor: 2, harness_factors: { claude: 1.2, codex: 1 } }
      : { agents: [], days: [] });
  } });
  await actions.loadFactor();
  assert.equal(state.factor.value.loaded, true);
  assert.equal(state.factor.value.raw, '2');
  assert.equal(state.factor.value.overrides.codex, '1');
  state.editFactor('1.5', 'claude');
  const first = actions.saveFactor('1.5', 'claude');
  state.editFactor('', 'codex');
  const second = actions.saveFactor('', 'codex');
  await Promise.all([first, second]);
  assert.deepEqual(writes, [{ estimate_factor: 1.5, harness: 'claude' }, { estimate_factor: null, harness: 'codex' }]);
  assert.equal(state.factor.value.overrides.claude, '1.5');
  assert.equal(state.factor.value.raw, '2');
  await actions.resetFactors();
  assert.deepEqual(writes.at(-1), { reset_all: true });
  assert.deepEqual(state.factor.value.overrides, {});
  assert.equal(state.factor.value.raw, '');
});

test('A successful harness save cannot hide another harness failure', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createCostsState }, { createCostsActions }] = await Promise.all([
    harness.importDashboardModule('js/costs-state.js'), harness.importDashboardModule('js/costs-actions.js'),
  ]);
  const state = createCostsState({ prefs: storage });
  let failClaude = true;
  let failCodex = false;
  let reloads = 0;
  const actions = createCostsActions({ state, fetchImpl: async (path, options = {}) => {
    if (options.method === 'POST' && JSON.parse(options.body).harness === 'claude' && failClaude) {
      return response('Could not save', false);
    }
    if (options.method === 'POST' && JSON.parse(options.body).harness === 'codex' && failCodex) return response('Codex failed', false);
    if (path.startsWith('/api/costs?')) reloads++;
    return response({ agents: [], days: [] });
  } });
  state.editFactor('1.2', 'claude');
  const first = actions.saveFactor('1.2', 'claude');
  state.editFactor('1.5', 'codex');
  const second = actions.saveFactor('1.5', 'codex');
  await Promise.all([first, second]);
  assert.equal(state.factor.value.error, true);
  assert.match(state.factor.value.status, /claude: Could not save/);
  assert.doesNotMatch(state.factor.value.status, /saved/);
  failClaude = false;
  await actions.saveFactor('1.2', 'claude');
  assert.equal(state.factor.value.error, false);
  assert.equal(state.factor.value.status, 'saved');
  const before = reloads;
  failCodex = true;
  state.editFactor('1.3', 'claude');
  const good = actions.saveFactor('1.3', 'claude');
  state.editFactor('1.6', 'codex');
  const bad = actions.saveFactor('1.6', 'codex');
  await Promise.all([good, bad]);
  assert.equal(reloads, before + 1, 'successful independent save refreshes even when the later save fails');
  assert.match(state.factor.value.status, /codex: Codex failed/);
});
