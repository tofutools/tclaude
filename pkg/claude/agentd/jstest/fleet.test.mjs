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
    harness.importDashboardModule('js/fleet-model.js'), harness.importDashboardModule('js/fleet-state.js'),
    harness.importDashboardModule('js/fleet-actions.js'), harness.importDashboardModule('js/fleet-island.js'),
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
  assert.equal(model.pollDelay({ random: () => 0.5 }), 5000);
  assert.equal(model.pollDelay({ random: () => 0 }), 4000);
  assert.equal(model.pollDelay({ random: () => 1 }), 6000);
  assert.equal(model.pollDelay({ failures: 2, random: () => 0.5 }), 20000);
  assert.equal(model.pollDelay({ failures: 9, random: () => 0.5 }), model.MAX_BACKOFF_MS);
});

test('card view distinguishes stale data, unreachable peers and absent summaries', async (t) => {
  const { model } = await load(t);
  const peer = { id: 'p', local: false, online: true };
  const ok = model.cardView(peer, { summary: { shared_groups: 2, shared_agents: 4, online_agents: 3, waiting_for_input: 1, peer_view: { omitted: [{ feature: 'costs' }] } }, receivedAt: 1000 }, 2000);
  assert.equal(ok.presence, 'online'); assert.equal(ok.waiting, 1); assert.deepEqual(ok.omitted, ['costs']); assert.equal(ok.stale, false);
  // Transport features (terminals, file links, inline spawn, the dashboard
  // shell) work on their own channels: never listed as "not shared".
  const unrestricted = model.cardView(peer, { summary: { peer_view: { omitted: [{ feature: 'local_dashboard', requires: 'local_only' }, { feature: 'sessions.files.read', requires: 'sessions.files.read', transport: true }, { feature: 'spawn.inline' }, 'terminals', { feature: 'custom.thing', transport: true }] } }, receivedAt: 1000 }, 2000);
  assert.deepEqual(unrestricted.omitted, []);
  // A requestable or non-transport entry always shows, whatever its name.
  const restricted = model.cardView(peer, { summary: { peer_view: { omitted: [{ feature: 'node.exec', requires: 'node.exec', requestable: true, transport: false }, { feature: 'terminals', requestable: true }, { feature: 'spawn.inline', transport: false }] } }, receivedAt: 1000 }, 2000);
  assert.deepEqual(restricted.omitted, ['node.exec', 'terminals', 'spawn.inline']);
  const stale = model.cardView(peer, { summary: { shared_agents: 4 }, receivedAt: 0, failure: model.classifyFailure(502, { code: 'peer_unreachable', reason: 'peer_offline', last_seen: 'x' }) }, 840000);
  assert.equal(stale.presence, 'offline'); assert.equal(stale.stale, true); assert.equal(model.fmtAge(stale.ageMs), '14 min'); assert.equal(stale.lastSeen, 'x');
  assert.equal(model.classifyFailure(504, { code: 'peer_unreachable', reason: 'peer_timeout' }).kind, 'timeout');
  assert.equal(model.classifyFailure(404, null).kind, 'unavailable');
  assert.equal(model.classifyFailure(403, { code: 'not_trusted' }).kind, 'not_trusted');
  assert.deepEqual(model.resourceView({ cpu: { logical_cores: 4, load_average: [2, 1, 1] }, ram: { total_bytes: 100, available_bytes: 25 } }), { cpu: 50, mem: 75 });
});

test('actions send If-None-Match and keep the body on 304; a missing status route hides the row', async (t) => {
  const { harness, stateMod, actionsMod } = await load(t);
  const state = stateMod.createFleetState({ activeTab: harness.signals.signal('groups'), now: () => 5 });
  const calls = [];
  let reply = { status: 200, body: { shared_agents: 2 }, etag: '"a"' };
  const fetchImpl = async (url, init) => { calls.push({ url, init }); return { ok: reply.status < 400, status: reply.status, headers: { get: () => reply.etag }, json: async () => reply.body }; };
  const actions = actionsMod.createFleetActions({ state, fetchImpl });
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

test('chip row is always there, greyed out until the hub connects; peer chips switch the whole UI to that node', async (t) => {
  const { harness, stateMod, island } = await load(t);
  const activeTab = harness.signals.signal('access');
  const state = stateMod.createFleetState({ activeTab });
  const nav = []; const switched = [];
  const mounted = await harness.mount(harness.html`<${island.NodeChips} state=${state} navigate=${(tab) => nav.push(tab)} remote="" switchNode=${(id) => switched.push(id)} />`);
  const home = () => getByRole(mounted.container, 'button', { name: /this node/ });
  const fuse = () => getByRole(mounted.container, 'button', { name: /Show several nodes together/ });
  assert.equal(home().disabled, true, 'before any status read: ⌂ shown, disabled');

  await harness.act(() => { state.setStatus({ enabled: false, instance_id: 'inst_self', name: 'desk', peers: [] }); });
  assert.equal(home().disabled, true, 'federation off');
  assert.match(home().title, /federation is off/);
  assert.equal(fuse().disabled, true);

  await harness.act(() => { state.setStatus({ ...status([]), hub: { state: 'connecting' } }); });
  assert.equal(home().disabled, true, 'a hub not connected yet greys the row out, peers or not');
  assert.match(home().title, /connecting/);
  assert.equal(mounted.container.querySelectorAll('button.node-chip:not(.fleet-entry)').length, 2, 'peers only appear once connected');

  await harness.act(() => { state.setStatus({ ...status([]), peers: [] }); });
  assert.equal(home().disabled, false, 'connected without peers: ⌂ usable');
  assert.equal(fuse().disabled, false, 'connected without peers: the nodes dropdown opens');
  assert.match(fuse().title, /No linked nodes yet/);
  await harness.act(() => harness.fireEvent(fuse(), 'click'));
  const pop = mounted.container.querySelector('.node-fuse-pop');
  assert.ok(pop, 'the dropdown lists this node alone');
  assert.deepEqual([...pop.querySelectorAll('.node-pop-name')].map((b) => b.textContent), ['⌂ desk']);
  assert.equal(pop.querySelector('input[type="checkbox"]').disabled, true, 'nothing to fuse yet');
  assert.equal(pop.querySelector('[data-fuse="all"]'), null);
  assert.match(pop.textContent, /No linked nodes yet/);
  assert.equal(state.fused.value, null, 'opening it turns on no fused view');
  await harness.act(() => harness.fireEvent(fuse(), 'click'));
  assert.equal(mounted.container.querySelector('[data-fleet-entry]').dataset.fleetTarget, 'map', 'connected without peers: ⌘ Fleet opens the map, showing this node alone');
  await harness.act(() => { activeTab.value = 'fleet-admin'; });
  await harness.act(() => harness.fireEvent(home(), 'click'));
  assert.deepEqual(nav, ['access'], 'with no peers ⌂ still leaves Fleet administration');
  nav.length = 0;
  await harness.act(() => { activeTab.value = 'access'; state.setStatus({ enabled: false, instance_id: 'inst_self', name: 'desk', peers: [] }); });
  await harness.act(() => { activeTab.value = 'fleet-admin'; });
  assert.equal(home().disabled, false, 'federation off, but ⌂ is the way out of Fleet administration');
  await harness.act(() => { activeTab.value = 'access'; state.setStatus({ ...status([]), peers: [] }); });

  await harness.act(() => { state.setStatus(status([])); });
  assert.equal(mounted.container.querySelector('[data-fleet-entry]').dataset.fleetTarget, 'fleet-admin', 'connected with peers: ⌘ Fleet ends the row and returns to Fleet config, the fleet view used last');
  assert.equal(fuse().disabled, false);
  const local = getByRole(mounted.container, 'button', { name: 'desk (this node)' });
  assert.equal(local.getAttribute('aria-current'), 'page');
  await harness.act(() => harness.fireEvent(getByRole(mounted.container, 'button', { name: 'forge, online, restricted peer' }), 'click'));
  assert.deepEqual(switched, ['inst_forge']);
  await harness.act(() => { activeTab.value = 'map'; });
  assert.equal(local.getAttribute('aria-current'), null);
  await harness.act(() => harness.fireEvent(local, 'click'));
  assert.deepEqual(nav, ['access'], 'the ⌂ chip returns to the tab left for the map');
  await harness.act(() => { activeTab.value = 'access'; state.setStatus({ ...status([]), hub: { state: 'disconnected' } }); });
  assert.equal(home().disabled, true, 'a hub drop greys the row out again, live');
  assert.match(home().title, /hub unreachable/);
  await harness.act(() => { state.setStatus({ ...status([]), hub: { state: 'refused', last_error: 'hub refused: shutting_down: hub shutting down' } }); });
  assert.match(home().title, /shutting down/, 'a refusal shows the hub\'s reason');
  assert.equal(mounted.container.querySelector('[data-fleet-entry]').dataset.fleetTarget, 'fleet-admin', 'with the hub down ⌘ Fleet opens Fleet config');
  await mounted.unmount(); state.dispose();
});

test('without a fleet ⌘ Fleet is always clickable, and a node never set up says so until the guide was opened', async (t) => {
  const { harness, stateMod, island } = await load(t);
  const activeTab = harness.signals.signal('groups');
  const stored = new Map();
  const prefs = { getItem: (k) => stored.get(k) ?? null, setItem: (k, v) => { stored.set(k, v); } };
  const state = stateMod.createFleetState({ activeTab, prefs });
  const nav = [];
  const mounted = await harness.mount(harness.html`<${island.NodeChips} state=${state} navigate=${(tab) => nav.push(tab)} remote="" switchNode=${() => {}} />`);
  const fleet = () => mounted.container.querySelector('[data-fleet-entry]');
  assert.ok(fleet(), 'there before any status read');
  assert.equal(fleet().disabled, false);
  assert.equal(fleet().querySelector('.node-chip-hint'), null, 'no hint before the node is known');

  await harness.act(() => { state.setStatus({ enabled: false, instance_id: 'inst_self', name: 'desk', peers: [] }); });
  assert.equal(fleet().disabled, false, 'federation off, but Fleet is where it is turned on');
  assert.equal(fleet().querySelector('.node-chip-hint').textContent, 'set up');
  assert.match(fleet().getAttribute('aria-label'), /set up/);
  assert.match(fleet().title, /link this node.*federation is off/);
  await harness.act(() => harness.fireEvent(fleet(), 'click'));
  assert.deepEqual(nav, ['fleet-admin']);
  await harness.act(() => { state.setStatus({ ...status([]), peers: [], hub: { state: 'disconnected' } }); });
  assert.equal(fleet().querySelector('.node-chip-hint'), null, 'a node with a hub set never shows the hint');
  assert.match(fleet().title, /hub connection and peers.*unreachable/);
  await harness.act(() => { state.setStatus({ enabled: false, instance_id: 'inst_self', name: 'desk', peers: [] }); });

  await harness.act(() => { activeTab.value = 'fleet-admin'; state.markSetupSeen(); });
  assert.equal(fleet().getAttribute('aria-current'), 'page');
  assert.equal(fleet().querySelector('.node-chip-hint'), null, 'opening the guide retires the hint');
  assert.equal(stored.get(stateMod.SETUP_SEEN_PREF), '1', 'and remembers it');
  await harness.act(() => harness.fireEvent(fleet(), 'click'));
  assert.deepEqual(nav, ['fleet-admin'], 'no navigation while already there');

  const fresh = stateMod.createFleetState({ activeTab: harness.signals.signal('groups'), prefs });
  fresh.setStatus({ enabled: false, instance_id: 'inst_self', name: 'desk', peers: [] });
  assert.equal(fresh.view.value.setupSeen, true, 'a reload keeps it retired');
  fresh.dispose();

  await harness.act(() => { activeTab.value = 'groups'; state.setStatus({ ...status([]), peers: [] }); });
  assert.match(fleet().title, /trust a peer/);
  await harness.act(() => { state.setStatus(status([])); });
  assert.ok(fleet(), 'with a fleet, ⌘ Fleet stays at the end of the row');
  assert.match(fleet().title, /every linked node/);
  await mounted.unmount(); state.dispose();
});

test('on a peer view without a local fleet ⌂ still goes home', async (t) => {
  const { harness, stateMod, island } = await load(t);
  const state = stateMod.createFleetState({ activeTab: harness.signals.signal('groups') });
  state.clearFleet();
  const switched = [];
  const mounted = await harness.mount(harness.html`<${island.NodeChips} state=${state} navigate=${() => {}} remote="inst_forge" switchNode=${(id) => switched.push(id)} />`);
  const home = getByRole(mounted.container, 'button', { name: /this node/ });
  assert.equal(home.disabled, false);
  await harness.act(() => harness.fireEvent(home, 'click'));
  assert.deepEqual(switched, ['']);
  await mounted.unmount(); state.dispose();
});

test('on a peer view the peer chip is current and ⌂ switches back to this node', async (t) => {
  const { harness, stateMod, island } = await load(t);
  const state = stateMod.createFleetState({ activeTab: harness.signals.signal('groups') });
  state.setStatus(status([]));
  const nav = []; const switched = [];
  const mounted = await harness.mount(harness.html`<${island.NodeChips} state=${state} navigate=${(tab) => nav.push(tab)} remote="inst_forge" switchNode=${(id) => switched.push(id)} />`);
  const forge = getByRole(mounted.container, 'button', { name: 'forge, online, restricted peer' });
  assert.equal(forge.getAttribute('aria-current'), 'page');
  assert.equal(getByRole(mounted.container, 'button', { name: 'desk (this node)' }).getAttribute('aria-current'), null);
  await harness.act(() => harness.fireEvent(forge, 'click'));
  assert.deepEqual(nav, ['groups'], 're-picking the shown node stays on it');
  await harness.act(() => harness.fireEvent(getByRole(mounted.container, 'button', { name: 'desk (this node)' }), 'click'));
  assert.deepEqual(switched, ['']);
  await mounted.unmount(); state.dispose();
});

test('node switching: URLs keep the tab and theme; Alt+digit never fires in a terminal or a field', async (t) => {
  const { harness, model, island } = await load(t);
  assert.equal(model.nodeHref('inst_forge', { pathname: '/costs', search: '?wizard=1' }), '/costs?wizard=1&node=inst_forge');
  assert.equal(model.nodeHref('', { pathname: '/costs', search: '?node=inst_forge&wizard=1' }), '/costs?wizard=1');
  const fleet = model.normalizeFleet(status([]));
  assert.deepEqual(model.switchOrder(fleet).map((n) => n.name), ['desk', 'forge', 'lab']);
  const doc = harness.document;
  const term = doc.createElement('div'); term.className = 'xterm'; const inner = doc.createElement('textarea'); term.appendChild(inner); doc.body.appendChild(term);
  const plain = doc.createElement('div'); doc.body.appendChild(plain);
  const ev = (target, extra = {}) => ({ altKey: true, code: 'Digit2', target, ...extra });
  assert.equal(island.switchKeyAllowed(ev(plain)), true);
  assert.equal(island.switchKeyAllowed(ev(inner)), false, 'a focused web terminal owns Alt+digit');
  assert.equal(island.switchKeyAllowed(ev(plain, { ctrlKey: true })), false);
  assert.equal(island.switchKeyAllowed(ev(plain, { code: 'Digit0' })), false);
  assert.equal(island.switchKeyAllowed(ev(plain, { altKey: false })), false);
});

test('remote marker shows the node, what it shares, and stale data honestly', async (t) => {
  const { harness, model, stateMod, island } = await load(t);
  assert.deepEqual(model.remoteHealthView({ ok: true }), { state: 'live', label: '' });
  assert.equal(model.remoteHealthView({ ok: false, lastOK: 0, failure: { code: 'peer_unreachable', reason: 'peer_offline' } }, 840000).label, 'offline · data 14 min old');
  assert.equal(model.remoteHealthView({ ok: false, lastOK: null, failure: { code: 'peer_busy' } }).state, 'busy');
  assert.deepEqual(model.peerViewSummary({ included: ['groups'], omitted: [{ feature: 'costs', requires: 'costs.read' }] }).omitted, [{ feature: 'costs', requires: 'costs.read' }]);
  assert.deepEqual(model.peerViewSummary({ included: ['groups'], omitted: [{ feature: 'terminals', requires: 'sessions.attach', transport: true }, { feature: 'costs', requires: 'costs.read', requestable: true }] }).omitted, [{ feature: 'costs', requires: 'costs.read', requestable: true }]);
  const state = stateMod.createFleetState({ activeTab: harness.signals.signal('groups') });
  state.setStatus(status([]));
  const snapshot = harness.signals.signal({ peer_view: { peer: 'desk', level: 'unrestricted', included: ['groups'], omitted: [{ feature: 'costs', requires: 'costs.read' }] } });
  const switched = [];
  const mounted = await harness.mount(harness.html`<${island.RemoteMarker} state=${state} remote="inst_forge" snapshot=${snapshot} switchNode=${(id) => switched.push(id)} />`);
  assert.match(mounted.container.textContent, /forge/);
  await harness.act(() => harness.window.dispatchEvent(new harness.window.CustomEvent('tclaude:remote-health', { detail: { ok: false, lastOK: Date.now() - 120000, failure: { code: 'peer_unreachable', reason: 'peer_offline' } } })));
  const pill = getByRole(mounted.container, 'button', { name: /peer view/ });
  assert.match(pill.textContent, /offline · data 2 min old/);
  await harness.act(() => harness.fireEvent(pill, 'click'));
  const pop = mounted.container.querySelector('[role="dialog"]');
  assert.match(pop.textContent, /Shared groups/); assert.match(pop.textContent, /Hidden from you costs/);
  assert.match(pop.textContent, /You trust it restricted · It trusts you unrestricted/, 'both directions of trust are named');
  await harness.act(() => harness.fireEvent(getByRole(pop, 'button', { name: /Back to desk/ }), 'click'));
  assert.deepEqual(switched, ['']);
  await mounted.unmount();
  const none = await harness.mount(harness.html`<${island.RemoteMarker} state=${state} remote="" />`);
  assert.equal(none.container.innerHTML, '', 'this node: no marker');
  await none.unmount(); state.dispose();
});

test('map polls only while active, staggered, and stops on leave', async (t) => {
  const { harness, stateMod, island } = await load(t);
  const activeTab = harness.signals.signal('groups');
  const state = stateMod.createFleetState({ activeTab });
  state.setStatus(status([]));
  const timers = fakeTimers(); const loaded = [];
  const actions = { loadSummary: async (node) => { loaded.push(node.id); state.commitSummary(node.id, { shared_groups: 1, shared_agents: 2, online_agents: 2, waiting_for_input: 0 }, ''); } };
  const mounted = await harness.mount(harness.html`<${island.FleetMap} state=${state} actions=${actions} timers=${timers} />`);
  assert.equal(timers.queue.length, 0, 'inactive map schedules nothing');
  await harness.act(() => { activeTab.value = 'map'; });
  assert.deepEqual(timers.queue.map((q) => q.ms), [0, 1667, 3333], 'three nodes, staggered over one interval');
  await harness.act(async () => { await timers.queue.shift().fn(); });
  assert.deepEqual(loaded, ['inst_self']);
  assert.ok(mounted.container.querySelector('[aria-label="desk node"]'));
  assert.ok(mounted.container.querySelector('[aria-label="lab node"].stale'), 'an offline peer renders as stale');
  await harness.act(() => { activeTab.value = 'groups'; });
  assert.equal(timers.queue.length, 0, 'leaving the map cancels every pending poll');
  await mounted.unmount(); state.dispose();
});

test('a link names its trust in each direction; the popover spells out who can do what', async (t) => {
  const { harness, stateMod, island, model } = await load(t);
  const link = model.linkView('desk', { name: 'lab', level: 'unrestricted' }, { theirLevel: 'restricted', omitted: ['costs', 'spawn'] });
  assert.deepEqual([link.in.from, link.in.to, link.in.short], ['lab', 'desk', 'full access']);
  assert.deepEqual([link.out.from, link.out.to, link.out.short], ['desk', 'lab', 'limited · 2 hidden']);
  assert.equal(link.asymmetric, true);
  assert.equal(model.linkView('desk', { name: 'forge', level: 'restricted' }, { theirLevel: '', omitted: [] }).out.short, 'not reported', 'an older peer does not say');
  assert.equal(model.linkView('desk', { name: 'forge', level: 'restricted' }, { theirLevel: '', omitted: [] }).asymmetric, false);

  const activeTab = harness.signals.signal('map');
  const state = stateMod.createFleetState({ activeTab });
  state.setStatus(status([]));
  state.commitSummary('inst_lab', { shared_groups: 0, shared_agents: 0, online_agents: 0, waiting_for_input: 0, peer_view: { level: 'restricted', omitted: [{ feature: 'costs', requires: 'costs.read' }] } }, '');
  const nav = [];
  const mounted = await harness.mount(harness.html`<${island.FleetMap} state=${state} actions=${{ loadSummary: async () => true }} timers=${fakeTimers()} navigate=${(tab) => nav.push(tab)} />`);
  const lab = mounted.container.querySelector('[aria-label="lab node"]');
  assert.doesNotMatch(lab.textContent, /unrestricted peer/, 'no bare level badge on the card');
  assert.match(lab.textContent, /You trust it⚠ unrestricted/);
  assert.match(lab.textContent, /It trusts yourestricted/);
  assert.match(lab.textContent, /Hidden from youcosts/);
  const badge = getByRole(mounted.container, 'button', { name: 'Trust between lab and desk' });
  assert.match(badge.textContent, /⚠ lab → desk full access/);
  assert.match(badge.textContent, /desk → lab limited · 1 hidden/);
  assert.ok(badge.classList.contains('asymmetric'));
  await harness.act(() => harness.fireEvent(badge, 'click'));
  const pop = mounted.container.querySelector('.fleet-link-pop');
  assert.match(pop.textContent, /Asymmetric link/);
  assert.match(pop.textContent, /lab has full peer access to desk.*You set this/);
  assert.match(pop.textContent, /desk can use only what lab shares\. lab's operator set this\. Hidden from you: costs\./);
  await harness.act(() => harness.fireEvent(harness.document, 'keydown', { key: 'Escape' }));
  assert.equal(mounted.container.querySelector('.fleet-link-pop'), null, 'Escape closes the popover');
  await harness.act(() => harness.fireEvent(badge, 'click'));
  await harness.act(() => harness.fireEvent(getByRole(mounted.container, 'button', { name: 'Change in Fleet config…' }), 'click'));
  assert.deepEqual(nav, ['fleet-admin']);
  const forge = getByRole(mounted.container, 'button', { name: 'Trust between forge and desk' });
  assert.match(forge.textContent, /desk → forge not reported/, 'no summary yet: the other direction is unknown');
  await mounted.unmount(); state.dispose();
});

test('the map never strands the operator: no fleet or a cycle key returns to the last per-node tab', async (t) => {
  const { harness, stateMod, island } = await load(t);
  const activeTab = harness.signals.signal('costs');
  const state = stateMod.createFleetState({ activeTab });
  state.setStatus(status([]));
  const nav = []; const timers = fakeTimers();
  const mounted = await harness.mount(harness.html`<${island.FleetMap} state=${state} actions=${{ loadSummary: async () => true }} timers=${timers} navigate=${(tab) => nav.push(tab)} />`);
  await harness.act(() => { activeTab.value = 'map'; });
  await harness.act(() => harness.document.dispatchEvent(new harness.window.CustomEvent('tclaude:leave-map', { detail: { dir: 1 } })));
  assert.deepEqual(nav, ['costs'], 'cycling out of the map returns to the tab it was opened from');
  await harness.act(() => { state.clearFleet(); });
  assert.deepEqual(nav, ['costs'], 'before the first status read settles, a deep link waits');
  await harness.act(() => { state.markStatusLoaded(); });
  assert.deepEqual(nav, ['costs', 'costs'], 'no linked nodes after loading: leave the map');
  await mounted.unmount(); state.dispose();
});

test('a connected node with no peers gets a map of itself; the top bar counts one node', async (t) => {
  const { harness, stateMod, island } = await load(t);
  const activeTab = harness.signals.signal('map');
  const state = stateMod.createFleetState({ activeTab });
  state.setStatus({ ...status([]), peers: [], hub: { state: 'connected', hub_id: 'hub_123', spaces: ['space-a'], since: '2026-10-17T10:00:00Z' } });
  state.markStatusLoaded();
  const nav = []; const timers = fakeTimers();
  const mounted = await harness.mount(harness.html`<${island.FleetMap} state=${state} actions=${{ loadSummary: async () => true }} timers=${timers} navigate=${(tab) => nav.push(tab)} />`);
  assert.deepEqual(nav, [], 'the map stays open');
  assert.ok(mounted.container.querySelector('[aria-label="desk node"]'), 'this node has its card');
  assert.match(mounted.container.textContent, /No linked nodes yet/);
  const hub = mounted.container.querySelector('[aria-label="federation hub"]');
  assert.ok(hub, 'the hub has its card between this node and its peers');
  assert.match(hub.textContent, /connected/);
  assert.match(hub.textContent, /wss:\/\/hub/);
  assert.match(hub.textContent, /space-a/);
  assert.equal(timers.queue.length, 1, 'only this node is polled');
  const bar = await harness.mount(harness.html`<${island.TopLevelBar} state=${state} navigate=${(tab) => nav.push(tab)} />`);
  assert.deepEqual([...bar.container.querySelectorAll('.fleet-seg-btn')].map((b) => b.textContent.trim()), ['▦ Groups · all nodes', 'Map', '⚙ Fleet config']);
  assert.match(bar.container.textContent, /1 node · hub connected/);
  assert.doesNotMatch(bar.container.textContent, /pick ⌂/);
  await harness.act(() => { state.setStatus({ ...status([]), peers: [], hub: { state: 'disconnected' } }); });
  assert.deepEqual(nav, ['groups'], 'the hub dropping leaves the map');
  await bar.unmount(); await mounted.unmount(); state.dispose();
});

test('"Show untrusted" asks for strangers, draws them apart from peers, and trusts through the fingerprint dialog', async (t) => {
  const { harness, stateMod, actionsMod, island } = await load(t);
  const stored = new Map();
  const prefs = { getItem: (k) => stored.get(k) ?? null, setItem: (k, v) => { stored.set(k, v); } };
  const state = stateMod.createFleetState({ activeTab: harness.signals.signal('map'), prefs });
  const urls = [];
  const fetchImpl = async (url, init) => { urls.push(init?.method === 'POST' ? `POST ${url} ${init.body}` : url); return { ok: true, status: 200, json: async () => status([{ instance_id: 'inst_zed', trusted: false, online: false, fingerprint: 'aaaa-bbbb-cccc-dddd' }]) }; };
  const actions = actionsMod.createFleetActions({ state, fetchImpl });
  await actions.loadStatus();
  assert.equal(urls.at(-1), '/api/federation/status?summary=1', 'unticked: the cheap read');
  const toasts = []; const opened = [];
  const TrustDialog = ({ row, onDone }) => harness.html`<div id="fake-trust">${row.id}<button type="button" onClick=${() => onDone(`Trusted ${row.label} (restricted)`)}>done</button></div>`;
  const loadTrust = async () => { opened.push('load'); return { TrustDialog, actions: {} }; };
  const mounted = await harness.mount(harness.html`<${island.FleetMap} state=${state} actions=${actions} timers=${fakeTimers()} toast=${(m) => toasts.push(m)} loadTrust=${loadTrust} />`);
  assert.equal(mounted.container.querySelector('.fleet-card.untrusted'), null, 'strangers hidden by default');
  const box = mounted.container.querySelector('[data-fleet="show-untrusted"]');
  assert.equal(box.hasAttribute('checked'), false);
  await harness.act(async () => { box.checked = true; harness.fireEvent(box, 'change'); await Promise.resolve(); });
  await harness.act(async () => { await Promise.resolve(); });
  assert.equal(urls.at(-1), '/api/federation/status?summary=1&untrusted=1');
  assert.equal(stored.get(stateMod.SHOW_UNTRUSTED_PREF), '1', 'the choice is remembered');
  const cards = [...mounted.container.querySelectorAll('.fleet-card.untrusted')];
  assert.deepEqual(cards.map((c) => c.dataset.nodeId), ['inst_stranger', 'inst_zed']);
  assert.match(cards[0].textContent, /carol/);
  assert.match(cards[0].textContent, /not trusted/);
  assert.match(cards[1].textContent, /unnamed/);
  assert.match(cards[1].textContent, /aaaa-…-dddd/);
  assert.equal(mounted.container.querySelectorAll('.fleet-card:not(.untrusted):not(.hub):not(.local)').length, 2, 'trusted peers still render as nodes');
  await harness.act(async () => { harness.fireEvent(cards[0].querySelector('[data-fleet="trust"]'), 'click'); await Promise.resolve(); await Promise.resolve(); });
  assert.deepEqual(opened, ['load']);
  assert.match(mounted.container.querySelector('#fake-trust').textContent, /inst_stranger/);
  await harness.act(async () => { harness.fireEvent(getByRole(mounted.container, 'button', { name: 'done' }), 'click'); for (let i = 0; i < 6; i++) await Promise.resolve(); });
  assert.deepEqual(toasts, ['Trusted carol (restricted) · asked carol to trust this node back']);
  assert.ok(urls.includes('POST /api/federation/peers/trust-request {"instance":"inst_stranger"}'), 'trusting also asks it to trust back');
  assert.equal(mounted.container.querySelector('#fake-trust'), null, 'the dialog closes');
  assert.equal(urls.at(-1), '/api/federation/status?summary=1&untrusted=1', 'trusting re-reads the status');
  await mounted.unmount();
  const fresh = stateMod.createFleetState({ activeTab: harness.signals.signal('map'), prefs });
  assert.equal(fresh.view.value.showUntrusted, true, 'a reload keeps the box ticked');
  fresh.setShowUntrusted(false);
  assert.deepEqual(fresh.view.value.strangers, []);
  const remote = await harness.mount(harness.html`<${island.FleetMap} state=${state} actions=${actions} timers=${fakeTimers()} remote="inst_forge" />`);
  assert.equal(remote.container.querySelector('[data-fleet="show-untrusted"]'), null, 'a peer view never trusts from the map');
  assert.equal(remote.container.querySelector('.fleet-card.untrusted'), null);
  await remote.unmount(); state.dispose(); fresh.dispose();
});

test('trust requests: shown without "Show untrusted", dismissable, and this node can ask with a note', async (t) => {
  const { harness, stateMod, actionsMod, island } = await load(t);
  const prefs = { getItem: () => null, setItem: () => {} };
  const activeTab = harness.signals.signal('map');
  const state = stateMod.createFleetState({ activeTab, prefs });
  const posts = [];
  const asker = { instance_id: 'inst_asker', name: 'dave', trusted: false, online: true, fingerprint: 'dddd-eeee', trust_request: { note: 'hi from dave', trusts_you: true, received_at: new Date(Date.now() - 120000).toISOString() } };
  const fetchImpl = async (url, init) => {
    if (init?.method === 'POST') { posts.push([url, JSON.parse(init.body)]); return { ok: true, status: 200, json: async () => ({ ok: true }) }; }
    return { ok: true, status: 200, json: async () => status([asker]) };
  };
  const actions = actionsMod.createFleetActions({ state, fetchImpl });
  await actions.loadStatus();
  const toasts = [];
  const mounted = await harness.mount(harness.html`<${island.FleetMap} state=${state} actions=${actions} timers=${fakeTimers()} toast=${(m) => toasts.push(m)} />`);
  const cards = () => [...mounted.container.querySelectorAll('.fleet-card.untrusted')];
  assert.deepEqual(cards().map((c) => c.dataset.nodeId), ['inst_asker'], 'only the asker, with the box unticked');
  assert.ok(cards()[0].classList.contains('asks'));
  assert.match(cards()[0].textContent, /Trusts this node, asks to be trusted back/);
  assert.match(cards()[0].textContent, /“hi from dave”/);
  assert.match(mounted.container.textContent, /1 asking to be trusted/);
  const bar = await harness.mount(harness.html`<${island.TopLevelBar} state=${state} navigate=${() => {}} />`);
  assert.match(bar.container.textContent, /🤝 1 asking to be trusted/);
  await bar.unmount();
  await harness.act(async () => { harness.fireEvent(cards()[0].querySelector('[data-fleet="dismiss"]'), 'click'); await Promise.resolve(); await Promise.resolve(); });
  assert.deepEqual(posts.at(-1), ['/api/federation/peers/trust-request/dismiss', { instance: 'inst_asker' }]);

  // With the box ticked, a stranger that has not asked offers "Ask to be trusted…".
  await harness.act(async () => { state.setShowUntrusted(true); await actions.loadStatus(); });
  const carol = mounted.container.querySelector('[data-node-id="inst_stranger"]');
  await harness.act(() => harness.fireEvent(carol.querySelector('[data-fleet="ask"]'), 'click'));
  const input = carol.querySelector('[data-fleet="ask-note"]');
  input.value = '  it is alice  ';
  await harness.act(() => harness.fireEvent(input, 'input'));
  await harness.act(async () => { harness.fireEvent(carol.querySelector('[data-fleet="ask-send"]'), 'click'); for (let i = 0; i < 4; i++) await Promise.resolve(); });
  assert.deepEqual(posts.at(-1), ['/api/federation/peers/trust-request', { instance: 'inst_stranger', note: 'it is alice' }]);
  assert.deepEqual(toasts, ['Asked carol to trust this node']);
  assert.equal(carol.querySelector('[data-fleet="ask-note"]'), null, 'the form closes');
  await mounted.unmount(); state.dispose();
});

test('a 5xx status read keeps the known fleet; 404 clears it', async (t) => {
  const { harness, stateMod, actionsMod } = await load(t);
  const state = stateMod.createFleetState({ activeTab: harness.signals.signal('map') });
  let code = 200;
  const actions = actionsMod.createFleetActions({ state, fetchImpl: async () => ({ ok: code < 400, status: code, json: async () => status([]) }) });
  await actions.loadStatus(); assert.equal(state.fleet.value.peers.length, 2); assert.equal(state.statusLoaded.value, true);
  code = 502; await actions.loadStatus(); assert.ok(state.fleet.value, 'transient failure keeps the chips and map');
  code = 404; await actions.loadStatus(); assert.equal(state.fleet.value, null);
  state.dispose();
});

test('node self-update: the card shows the version and an available update; the dialog confirms, applies and follows the job across the restart', async (t) => {
  const { harness, stateMod, island } = await load(t);
  const upd = await harness.importDashboardModule('js/node-update.js');
  assert.equal(upd.updateBase({ local: true }), '/api/node/update');
  assert.equal(upd.updateBase({ id: 'inst_a/b' }), '/api/peer/inst_a%2Fb/node/update');
  assert.deepEqual(upd.versionView({ version: 'v1.2.0', latest_version: 'v1.3.0', update_available: null }), { current: 'v1.2.0', latest: 'v1.3.0', update: false });
  const activeTab = harness.signals.signal('map');
  const state = stateMod.createFleetState({ activeTab });
  state.setStatus(status([]));
  state.commitSummary('inst_self', { version: 'v1.2.0', latest_version: 'v1.3.0', update_available: true, shared_groups: 1, shared_agents: 2, online_agents: 2, waiting_for_input: 0 }, '');
  state.commitSummary('inst_forge', { version: 'v1.3.0', update_available: false, shared_groups: 1, shared_agents: 1, online_agents: 1, waiting_for_input: 0 }, '');
  const timers = fakeTimers(); const confirms = []; const calls = [];
  let job = null; let reads = 0;
  const updateActions = {
    status: async (node) => {
      calls.push(['status', node.id]);
      if (node.id === 'inst_forge') { const e = new Error('node.update is not shared'); e.status = 403; throw e; }
      reads++;
      if (job && reads === 3) throw new Error('daemon restarting');
      if (job && reads >= 4) job = { ...job, state: 'succeeded', phase: 'restarted' };
      return { current_version: job?.state === 'succeeded' ? 'v1.3.0' : 'v1.2.0', install_method: 'release', latest_version: 'v1.3.0', update_available: job?.state !== 'succeeded', rollback_available: !!job, warnings: [], binaries: [{ name: 'tclaude', version: 'v1.2.0', path: '/bin/tclaude' }], job };
    },
    start: async (node, action, version) => { calls.push(['start', node.id, action, version]); job = { id: 'u1', action, version, state: 'running', phase: 'downloading' }; return job; },
  };
  const confirm = async (o) => { confirms.push(o); return true; };
  const mounted = await harness.mount(harness.html`<${island.FleetMap} state=${state} actions=${{ loadSummary: async () => true }} timers=${timers} confirm=${confirm} toast=${() => {}} updateActions=${updateActions} />`);
  const q = (s) => mounted.container.ownerDocument.querySelector(s);
  const selfCard = mounted.container.querySelector('[aria-label="desk node"]');
  assert.match(selfCard.textContent, /v1\.2\.0 ↑ v1\.3\.0 update…/);
  assert.match(mounted.container.querySelector('[aria-label="forge node"]').textContent, /v1\.3\.0 manage…/);
  const settle = () => harness.act(() => new Promise((r) => setTimeout(r, 25)));
  await harness.act(() => selfCard.querySelector('[data-fleet="node-update"]').click());
  await settle();
  assert.match(q('#fleet-node-update-modal').textContent, /Version\s*v1\.2\.0/);
  await harness.act(() => q('#fleet-node-update-apply').click());
  await settle();
  assert.match(confirms[0].body, /replaces its tclaude binaries .*restarts its daemon\. Its agent sessions keep running/);
  assert.deepEqual(calls.find((c) => c[0] === 'start'), ['start', 'inst_self', 'apply', 'v1.3.0']);
  for (let i = 0; i < 4 && /running|restarting/.test(q('#fleet-node-update-job')?.textContent || 'running'); i++) {
    const tick = timers.queue.find((x) => x.ms === 1000);
    assert.ok(tick, 'an active job is re-read every second');
    await harness.act(() => { timers.queue.splice(timers.queue.indexOf(tick), 1); tick.fn(); });
    await settle();
  }
  assert.match(q('#fleet-node-update-job').textContent, /succeeded/);
  assert.ok(q('#fleet-node-update-rollback'), 'rollback is offered once a backup exists');
  await harness.act(() => [...q('#fleet-node-update-modal').querySelectorAll('button')].find((b) => b.textContent === 'Close').click());
  await harness.act(() => mounted.container.querySelector('[aria-label="forge node"] [data-fleet="node-update"]').click());
  await settle();
  assert.match(q('#fleet-node-update-modal').textContent, /forge does not let you manage its updates \(needs node\.update\)/);
  await mounted.unmount(); state.dispose();
});

test('⌘ Fleet is lit on the fleet views and returns to the one used last', async (t) => {
  const { harness, stateMod, island } = await load(t);
  const activeTab = harness.signals.signal('groups');
  const stored = new Map();
  const prefs = { getItem: (k) => stored.get(k) ?? null, setItem: (k, v) => { stored.set(k, v); } };
  const state = stateMod.createFleetState({ activeTab, prefs });
  state.setStatus(status([]));
  const nav = [];
  const mounted = await harness.mount(harness.html`<${island.NodeChips} state=${state} navigate=${(tab) => nav.push(tab)} remote="" switchNode=${() => {}} />`);
  const chip = () => mounted.container.querySelector('[data-fleet-entry]');
  assert.equal(chip().dataset.fleetTarget, 'map', 'the map by default');
  assert.equal(chip().getAttribute('aria-current'), null);
  await harness.act(() => { activeTab.value = 'fleet-admin'; });
  assert.equal(chip().getAttribute('aria-current'), 'page', 'lit on Fleet config');
  await harness.act(() => harness.fireEvent(chip(), 'click'));
  assert.deepEqual(nav, [], 'no navigation while on a fleet view');
  await harness.act(() => { activeTab.value = 'groups'; });
  assert.equal(chip().dataset.fleetTarget, 'fleet-admin', 'back to Fleet config, the view used last');
  assert.equal(stored.get(stateMod.LAST_VIEW_PREF), 'fleet-admin');
  await harness.act(() => harness.fireEvent(chip(), 'click'));
  assert.deepEqual(nav, ['fleet-admin']);
  await harness.act(() => { activeTab.value = 'map'; });
  await harness.act(() => { activeTab.value = 'groups'; });
  assert.equal(chip().dataset.fleetTarget, 'map');
  const fresh = stateMod.createFleetState({ activeTab: harness.signals.signal('groups'), prefs });
  assert.equal(fresh.lastFleetTab(), 'map', 'a reload keeps the last view');
  fresh.dispose();
  // The state is built at module load, before initDashPrefs() fills the
  // cache, so a pref that lands afterwards must still be honoured.
  const late = new Map();
  const early = stateMod.createFleetState({ activeTab: harness.signals.signal('groups'),
    prefs: { getItem: (k) => late.get(k) ?? null, setItem: (k, v) => { late.set(k, v); } } });
  assert.equal(early.lastFleetTab(), 'map');
  late.set(stateMod.LAST_VIEW_PREF, 'fleet-admin');
  assert.equal(early.lastFleetTab(), 'fleet-admin', 'prefs loaded after the state was built still count');
  early.dispose();
  await mounted.unmount();

  const peerView = await harness.mount(harness.html`<${island.NodeChips} state=${state} navigate=${(tab) => nav.push(tab)} remote="inst_forge" switchNode=${() => {}} />`);
  assert.equal(peerView.container.querySelector('[data-fleet-entry]').dataset.fleetTarget, 'map', 'a peer view opens the map');
  await harness.act(() => { state.setStatus({ ...status([]), hub: { state: 'disconnected' } }); });
  await peerView.unmount(); state.dispose();
});

test('an offline peer card shows its offline notices and silences this disconnection', async (t) => {
  const { harness, stateMod, actionsMod, island, model } = await load(t);
  const now = Date.parse('2026-10-09T21:00:00Z');
  const lab = { offline_notice: { since: '2026-10-09T20:37:00Z', notices: 1, next_at: '2026-10-09T21:07:00Z', silenced: false } };
  const withNotice = (notice) => { const s = status([]); s.peers[1] = { ...s.peers[1], offline_notice: notice }; return s; };
  assert.deepEqual(model.offlineNotice({ offline_notice: lab.offline_notice }), { since: Date.parse(lab.offline_notice.since), notices: 1, nextAt: now + 420000, silenced: false });
  assert.equal(model.offlineNotice({}), null);
  const state = stateMod.createFleetState({ activeTab: harness.signals.signal('map'), prefs: { getItem: () => null, setItem: () => {} } });
  let reply = withNotice(lab.offline_notice);
  const posts = [];
  const fetchImpl = async (url, init) => {
    if (init?.method === 'POST') { posts.push(url); reply = withNotice({ ...lab.offline_notice, next_at: undefined, silenced: true }); return { ok: true, status: 200, json: async () => ({}) }; }
    return { ok: true, status: 200, json: async () => reply };
  };
  const actions = actionsMod.createFleetActions({ state, fetchImpl });
  await actions.loadStatus();
  const toasts = [];
  const mounted = await harness.mount(harness.html`<${island.FleetMap} state=${state} actions=${actions} timers=${fakeTimers()} now=${() => now} toast=${(m) => toasts.push(m)} />`);
  const card = () => mounted.container.querySelector('[aria-label="lab node"]');
  assert.match(card().querySelector('.fleet-card-notice').textContent, /1 notice sent · next notice in 7 min/);
  assert.equal(mounted.container.querySelector('[aria-label="forge node"] .fleet-card-notice'), null, 'online peers show no notice row');
  await harness.act(async () => { harness.fireEvent(card().querySelector('[data-fleet="silence-offline"]'), 'click'); for (let i = 0; i < 6; i++) await Promise.resolve(); });
  assert.deepEqual(posts, ['/api/federation/nodes/health/silence?peer=inst_lab']);
  assert.deepEqual(toasts, ['Offline notices for lab silenced until it reconnects']);
  assert.match(card().querySelector('.fleet-card-notice.silenced').textContent, /silenced until it reconnects/);
  assert.equal(card().querySelector('[data-fleet="silence-offline"]'), null);
  await mounted.unmount();
  reply = withNotice(lab.offline_notice);
  await actions.loadStatus();
  const remote = await harness.mount(harness.html`<${island.FleetMap} state=${state} actions=${actions} timers=${fakeTimers()} remote="inst_forge" />`);
  assert.equal(remote.container.querySelector('[data-fleet="silence-offline"]'), null, 'a peer view never silences');
  await remote.unmount(); state.dispose();
});
