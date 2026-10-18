import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const fleetOf = (harness, fused) => ({
  fleet: harness.signals.signal({
    self: { id: 'inst_self', name: 'desk', local: true },
    peers: [{ id: 'inst_forge', name: 'forge' }, { id: 'inst_lab', name: 'lab' }],
  }),
  fused: harness.signals.signal(fused),
});

test('the Logs/Performance node switch offers the ticked nodes of a fused view only', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/activity-node.js');
  assert.deepEqual(mod.activityNodes(fleetOf(harness, null), undefined), [], 'not fused: this node only');
  assert.deepEqual(mod.activityNodes(fleetOf(harness, 'all'), { id: 'inst_forge' }), [], 'a peer\'s own view never fuses');
  const nodes = mod.activityNodes(fleetOf(harness, ['inst_self', 'inst_forge']), undefined);
  assert.deepEqual(nodes.map((n) => n.name), ['desk', 'forge']);
  assert.equal(mod.activityTarget(nodes, ''), null);
  assert.equal(mod.activityTarget(nodes, 'inst_forge').name, 'forge');
  assert.equal(mod.activityTarget(nodes, 'inst_lab'), null, 'a peer no longer ticked falls back to this node');
});

test('the chosen peer is read through its peer routes; a refusal names the trust it needs', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ activityFetch }, { createLogsState }, { createLogsActions }] = await Promise.all([
    harness.importDashboardModule('js/activity-node.js'),
    harness.importDashboardModule('js/logs-state.js'), harness.importDashboardModule('js/logs-actions.js'),
  ]);
  const local = () => { throw new Error('read this node'); };
  assert.equal(activityFetch(local, { target: null }), local);
  const calls = [];
  const fetchFor = (id) => async (url) => { calls.push([id, url]); return { ok: false, status: 403, text: async () => 'forbidden' }; };
  const target = { id: 'inst_forge', name: 'forge' };
  const state = createLogsState({ activeTab: harness.signals.signal('activity'), activitySubtab: harness.signals.signal('logs') });
  const actions = createLogsActions({ state, fetchImpl: local, now: () => 10000, route: (f) => activityFetch(f, { target, fetchFor }) });
  assert.equal(await actions.load(), false);
  assert.equal(calls[0][0], 'inst_forge');
  assert.match(calls[0][1], /^\/api\/logs\?/);
  assert.match(state.view.value.request.error, /forge does not share its daemon log or timings with you \(needs unrestricted trust from forge\)/);
});
