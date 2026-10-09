import test from 'node:test'; import assert from 'node:assert/strict';
import { createPreactHarness, getByRole } from './preact-harness.mjs';

const status = (peers) => ({
  enabled: true, instance_id: 'inst_self', name: 'desk', hub_url: 'wss://hub', hub: { state: 'connected' },
  peers: [
    { instance_id: 'inst_forge', label: 'forge', trusted: true, online: true, level: 'restricted' },
    { instance_id: 'inst_lab', label: 'lab', trusted: true, online: false, level: 'unrestricted', last_seen: '2026-10-09T20:37:00Z' },
    { instance_id: 'inst_stranger', name: 'carol', trusted: false, online: true },
    ...peers,
  ],
});

async function load(t) {
  const harness = await createPreactHarness(t);
  const [model, stateMod, actionsMod, island] = await Promise.all([
    harness.importDashboardModule('js/skynet-model.js'), harness.importDashboardModule('js/skynet-state.js'),
    harness.importDashboardModule('js/skynet-actions.js'), harness.importDashboardModule('js/skynet-island.js'),
  ]);
  return { harness, model, stateMod, actionsMod, island };
}

function fakeTimers() {
  const queue = []; let seq = 0;
  return {
    queue,
    setTimeout(fn, ms) { const id = ++seq; queue.push({ id, fn, ms }); return id; },
    clearTimeout(id) { const i = queue.findIndex((q) => q.id === id); if (i >= 0) queue.splice(i, 1); },
  };
}

test('fleet model keeps trusted peers only, hides the row without peers, and never uses purple', async (t) => {
  const { model } = await load(t);
  const fleet = model.normalizeFleet(status([]));
  assert.deepEqual(fleet.peers.map((p) => p.name), ['forge', 'lab']);
  assert.equal(fleet.self.name, 'desk');
  assert.equal(fleet.peers[1].level, 'unrestricted');
  assert.equal(model.normalizeFleet({ instance_id: 'inst_self', peers: [] }), null, 'no trusted peers: no chip row');
  assert.equal(model.normalizeFleet(null), null);
  assert.equal(model.nodeColor('inst_forge'), model.nodeColor('inst_forge'), 'colour is stable');
  for (const c of model.PEER_COLORS) assert.doesNotMatch(c, /#(bc8cff|a371f7|8957e5|d2a8ff)/i);
  assert.equal(model.summaryURL({ id: 'inst_a/b', local: false }), '/api/peer/inst_a%2Fb/node-summary');
  assert.equal(model.summaryURL({ id: 'inst_self', local: true }), '/api/node-summary');
});

test('polling contract: staggered starts, relaxed jittered interval, capped backoff', async (t) => {
  const { model } = await load(t);
  assert.deepEqual([0, 1, 2, 3].map((i) => model.staggerOffset(i, 4, 10000)), [0, 2500, 5000, 7500]);
  assert.equal(model.pollDelay({ random: () => 0.5 }), 10000);
  assert.equal(model.pollDelay({ random: () => 0 }), 8000);
  assert.equal(model.pollDelay({ random: () => 1 }), 12000);
  assert.equal(model.pollDelay({ failures: 2, random: () => 0.5 }), 40000);
  assert.equal(model.pollDelay({ failures: 9, random: () => 0.5 }), model.MAX_BACKOFF_MS);
});

test('card view distinguishes stale data, unreachable peers and absent summaries', async (t) => {
  const { model } = await load(t);
  const peer = { id: 'p', local: false, online: true };
  const ok = model.cardView(peer, { summary: { shared_groups: 2, shared_agents: 4, online_agents: 3, waiting_for_input: 1, peer_view: { omitted: [{ feature: 'costs' }] } }, receivedAt: 1000 }, 2000);
  assert.equal(ok.presence, 'online'); assert.equal(ok.waiting, 1); assert.deepEqual(ok.omitted, ['costs']); assert.equal(ok.stale, false);
  const stale = model.cardView(peer, { summary: { shared_agents: 4 }, receivedAt: 0, failure: model.classifyFailure(502, { code: 'peer_unreachable', reason: 'peer_offline', last_seen: 'x' }) }, 840000);
  assert.equal(stale.presence, 'offline'); assert.equal(stale.stale, true); assert.equal(model.fmtAge(stale.ageMs), '14 min'); assert.equal(stale.lastSeen, 'x');
  assert.equal(model.classifyFailure(504, { code: 'peer_unreachable', reason: 'peer_timeout' }).kind, 'timeout');
  assert.equal(model.classifyFailure(404, null).kind, 'unavailable');
  assert.equal(model.classifyFailure(403, { code: 'not_trusted' }).kind, 'not_trusted');
  assert.deepEqual(model.resourceView({ cpu: { logical_cores: 4, load_average: [2, 1, 1] }, ram: { total_bytes: 100, available_bytes: 25 } }), { cpu: 50, mem: 75 });
});

test('actions send If-None-Match and keep the body on 304; a missing status route hides the row', async (t) => {
  const { harness, stateMod, actionsMod } = await load(t);
  const state = stateMod.createSkynetState({ activeTab: harness.signals.signal('groups'), now: () => 5 });
  const calls = [];
  let reply = { status: 200, body: { shared_agents: 2 }, etag: '"a"' };
  const fetchImpl = async (url, init) => { calls.push({ url, init }); return { ok: reply.status < 400, status: reply.status, headers: { get: () => reply.etag }, json: async () => reply.body }; };
  const actions = actionsMod.createSkynetActions({ state, fetchImpl });
  const node = { id: 'inst_forge', local: false };
  await actions.loadSummary(node);
  reply = { status: 304, body: null, etag: '' };
  await actions.loadSummary(node);
  assert.equal(calls[1].init.headers['If-None-Match'], '"a"');
  assert.equal(state.entry('inst_forge').summary.shared_agents, 2);
  reply = { status: 404, body: null };
  await actions.loadStatus();
  assert.equal(state.fleet.value, null);
  reply = { status: 200, body: status([]) };
  await actions.loadStatus();
  assert.equal(state.fleet.value.peers.length, 2);
});

test('chip row renders only with trusted peers and routes through the nav', async (t) => {
  const { harness, stateMod, island } = await load(t);
  const activeTab = harness.signals.signal('access');
  const state = stateMod.createSkynetState({ activeTab });
  const nav = [];
  const mounted = await harness.mount(harness.html`<${island.NodeChips} state=${state} navigate=${(tab) => nav.push(tab)} />`);
  assert.equal(mounted.container.textContent, '', 'no fleet: nothing rendered');
  await harness.act(() => { state.setStatus(status([])); });
  const local = getByRole(mounted.container, 'button', { name: 'desk (this node)' });
  assert.equal(local.getAttribute('aria-current'), 'page');
  await harness.act(() => harness.fireEvent(getByRole(mounted.container, 'button', { name: 'forge, online, restricted peer' }), 'click'));
  assert.deepEqual(nav, ['map']); assert.equal(state.focused.value, 'inst_forge');
  await harness.act(() => { activeTab.value = 'map'; });
  assert.equal(local.getAttribute('aria-current'), null);
  await harness.act(() => harness.fireEvent(local, 'click'));
  assert.deepEqual(nav, ['map', 'access'], 'the ⌂ chip returns to the tab left for the map');
  await mounted.unmount(); state.dispose();
});

test('map polls only while active, staggered, and stops on leave', async (t) => {
  const { harness, stateMod, island } = await load(t);
  const activeTab = harness.signals.signal('groups');
  const state = stateMod.createSkynetState({ activeTab });
  state.setStatus(status([]));
  const timers = fakeTimers(); const loaded = [];
  const actions = { loadSummary: async (node) => { loaded.push(node.id); state.commitSummary(node.id, { shared_groups: 1, shared_agents: 2, online_agents: 2, waiting_for_input: 0 }, ''); } };
  const mounted = await harness.mount(harness.html`<${island.SkynetMap} state=${state} actions=${actions} timers=${timers} />`);
  assert.equal(timers.queue.length, 0, 'inactive map schedules nothing');
  await harness.act(() => { activeTab.value = 'map'; });
  assert.deepEqual(timers.queue.map((q) => q.ms), [0, 3333, 6667], 'three nodes, staggered over one interval');
  await harness.act(async () => { await timers.queue.shift().fn(); });
  assert.deepEqual(loaded, ['inst_self']);
  assert.ok(mounted.container.querySelector('[aria-label="desk node"]'));
  assert.ok(mounted.container.querySelector('[aria-label="lab node"].stale'), 'an offline peer renders as stale');
  await harness.act(() => { activeTab.value = 'groups'; });
  assert.equal(timers.queue.length, 0, 'leaving the map cancels every pending poll');
  await mounted.unmount(); state.dispose();
});

test('the map never strands the operator: no fleet or a cycle key returns to the last per-node tab', async (t) => {
  const { harness, stateMod, island } = await load(t);
  const activeTab = harness.signals.signal('costs');
  const state = stateMod.createSkynetState({ activeTab });
  state.setStatus(status([]));
  const nav = []; const timers = fakeTimers();
  const mounted = await harness.mount(harness.html`<${island.SkynetMap} state=${state} actions=${{ loadSummary: async () => true }} timers=${timers} navigate=${(tab) => nav.push(tab)} />`);
  await harness.act(() => { activeTab.value = 'map'; });
  await harness.act(() => harness.document.dispatchEvent(new harness.window.CustomEvent('tclaude:leave-map', { detail: { dir: 1 } })));
  assert.deepEqual(nav, ['costs'], 'cycling out of the map returns to the tab it was opened from');
  await harness.act(() => { state.clearFleet(); });
  assert.deepEqual(nav, ['costs'], 'before the first status read settles, a deep link waits');
  await harness.act(() => { state.markStatusLoaded(); });
  assert.deepEqual(nav, ['costs', 'costs'], 'no linked nodes after loading: leave the map');
  await mounted.unmount(); state.dispose();
});

test('a 5xx status read keeps the known fleet; 404 clears it', async (t) => {
  const { harness, stateMod, actionsMod } = await load(t);
  const state = stateMod.createSkynetState({ activeTab: harness.signals.signal('map') });
  let code = 200;
  const actions = actionsMod.createSkynetActions({ state, fetchImpl: async () => ({ ok: code < 400, status: code, json: async () => status([]) }) });
  await actions.loadStatus(); assert.equal(state.fleet.value.peers.length, 2); assert.equal(state.statusLoaded.value, true);
  code = 502; await actions.loadStatus(); assert.ok(state.fleet.value, 'transient failure keeps the chips and map');
  code = 404; await actions.loadStatus(); assert.equal(state.fleet.value, null);
  state.dispose();
});
