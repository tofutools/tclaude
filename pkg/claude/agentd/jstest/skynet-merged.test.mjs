import test from 'node:test'; import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const self = { id: 'inst_self', name: 'desk', color: '#58a6ff', local: true };
const forge = { id: 'inst_forge', name: 'forge', color: '#f0883e', local: false };
const lab = { id: 'inst_lab', name: 'lab', color: '#39c5cf', local: false };
const group = (name, extra = {}) => ({ name, members: [], online: 0, ...extra });

test('merge names groups group@node, keeps parents per node, and marks stale peers', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/skynet-merged-model.js');
  const merged = m.mergeSnapshots([
    { node: self, entry: { snapshot: { group_attachments_mode: 'fixed', groups: [group('ops'), group('ops-sub', { parent: 'ops' })], agents: [{ agent_id: 'agt_1' }] }, receivedAt: 1000 } },
    { node: forge, entry: { snapshot: { groups: [group('ops')], agents: [{ agent_id: 'agt_2' }], peer_view: { included: [] } }, receivedAt: 1000 } },
    { node: lab, entry: { snapshot: { groups: [group('build')] }, receivedAt: 0, failure: { status: 502, code: 'peer_unreachable' } } },
  ], 61000);
  assert.deepEqual(merged.groups.map((g) => g.name), ['ops@desk', 'ops-sub@desk', 'ops@forge', 'build@lab']);
  assert.equal(merged.groups[1].parent, 'ops@desk');
  assert.equal(merged.group_attachments_mode, 'fixed', 'presentation settings come from this node');
  assert.equal(merged.peer_view, undefined);
  assert.deepEqual(merged.agents.map((a) => a.agent_id), ['agt_1', 'agt_2']);
  const labGroup = merged.groups[3];
  assert.equal(labGroup.fleet_node.stale, true); assert.equal(labGroup.fleet_node.label, 'stale · data 1 min old');
  assert.equal(merged.groups[2].fleet_node.stale, true, 'a minute-old snapshot is stale even without a failure');
  assert.equal(merged.groups[0].fleet_node.stale, false, 'this node is never stale');
  assert.deepEqual(m.mergeSnapshots([{ node: forge, entry: null }]).fleet_nodes[0].loaded, false);
  const offline = m.mergeSnapshots([{ node: { ...forge, online: false }, entry: null }]).fleet_nodes[0];
  assert.equal(offline.label, 'offline', 'a peer the hub reports offline is not "loading…"');
  assert.equal(offline.stale, true);
  assert.equal(m.mergeSnapshots([{ node: { ...forge, online: false }, entry: { failure: { status: 502, code: 'peer_unreachable' } } }]).fleet_nodes[0].label, 'offline', 'a failed poll keeps it offline');
});

test('node names that collide (self-reported or containing @) never merge two nodes\' groups', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/skynet-merged-model.js');
  const fakeDesk = { id: 'inst_imposter9', name: 'desk', color: '#f00', local: false };
  const ab = { id: 'inst_abnode77', name: 'a@b', color: '#0f0', local: false };
  const merged = m.mergeSnapshots([
    { node: { ...self, name: 'b' }, entry: { snapshot: { groups: [group('ops@a')] }, receivedAt: 0 } },
    { node: { ...forge, name: 'desk' }, entry: { snapshot: { groups: [group('ops')] }, receivedAt: 0 } },
    { node: fakeDesk, entry: { snapshot: { groups: [group('ops')] }, receivedAt: 0 } },
    { node: ab, entry: { snapshot: { groups: [group('ops')] }, receivedAt: 0 } },
  ], 0);
  const names = merged.groups.map((g) => g.name);
  assert.equal(new Set(names).size, names.length, `unique names: ${names.join(', ')}`);
  assert.deepEqual(names, ['ops@a@b', 'ops@desk~forge', 'ops@desk~impost', 'ops@a_b']);
});

function fakeTimers() {
  const queue = []; let seq = 0;
  return { queue, setTimeout(fn, ms) { const id = ++seq; queue.push({ id, fn, ms }); return id; }, clearTimeout(id) { const i = queue.findIndex((q) => q.id === id); if (i >= 0) queue.splice(i, 1); } };
}

test('the merged view polls peers only while shown, renders group@node, and stays read-only', async (t) => {
  const harness = await createPreactHarness(t);
  const [stateMod, island] = await Promise.all([harness.importDashboardModule('js/skynet-state.js'), harness.importDashboardModule('js/skynet-merged-island.js')]);
  const activeTab = harness.signals.signal('groups');
  const state = stateMod.createSkynetState({ activeTab });
  state.setStatus({ instance_id: 'inst_self', name: 'desk', peers: [{ instance_id: 'inst_forge', label: 'forge', trusted: true, online: true }] });
  const snapshot = harness.signals.signal({ groups: [group('ops')], agents: [] });
  const timers = fakeTimers(); const calls = []; const toasts = []; const switched = []; const opened = [];
  const fetchImpl = async (url) => { calls.push(url); return { ok: true, status: 200, json: async () => ({ groups: [group('build')], agents: [], peer_view: { included: ['agents.status'] } }) }; };
  const host = harness.document.createElement('div'); harness.document.body.appendChild(host);
  const mounted = await harness.mount(harness.html`<${island.MergedGroups} state=${state} host=${host} snapshot=${snapshot} fetchImpl=${fetchImpl} timers=${timers} remote="" toast=${(m) => toasts.push(m)} switchNode=${(id) => switched.push(id)} openTerminal=${(o) => opened.push(o)} />`);
  assert.equal(timers.queue.length, 0, 'hidden view polls nothing');
  await harness.act(() => { activeTab.value = 'fleet'; });
  assert.equal(timers.queue.length, 1);
  await harness.act(async () => { await timers.queue.shift().fn(); });
  assert.deepEqual(calls, ['/api/peer/inst_forge/snapshot']);
  const text = mounted.container.textContent;
  assert.match(text, /ops@desk/); assert.match(text, /build@forge/);
  const suffix = [...mounted.container.querySelectorAll('[data-fleet-open]')].find((el) => el.textContent === '@forge');
  assert.ok(suffix, 'the @node suffix is a jump to that node');
  // Read-only: the status dot (a power control), keyboard activation and the
  // row context menu are stopped; view-only controls pass.
  const el = (html) => { const d = harness.document.createElement('div'); d.innerHTML = html; host.appendChild(d); return d.firstElementChild; };
  const dot = el('<span data-act="dot-toggle" data-agent="agt_1">●</span>');
  const chip = el('<span data-act="set-group-descr" role="button" tabindex="0">📝</span>');
  const menu = el('<button data-act="group-menu">⚙</button>');
  assert.equal(harness.fireEvent(dot, 'click').defaultPrevented, true, 'the dot wakes or shuts down agents: blocked');
  assert.equal(harness.fireEvent(chip, 'keydown', { key: 'Enter' }).defaultPrevented, true);
  assert.equal(harness.fireEvent(chip, 'contextmenu').defaultPrevented, true);
  assert.equal(harness.fireEvent(menu, 'click').defaultPrevented, false, 'menus still open');
  assert.ok(toasts.length >= 2);
  // A peer agent's terminal opens in the browser; this node's stays an overview.
  const inGroup = (node, name, inner) => { const d = el(`<details data-fleet-node="${node}" data-fleet-node-name="${name}">${inner}</details>`); return d.firstElementChild; };
  const peerWin = inGroup('inst_forge', 'forge', '<button data-act="web-open-window" data-agent="agt_ada1">web window</button>');
  assert.equal(harness.fireEvent(peerWin, 'click').defaultPrevented, true);
  assert.deepEqual({ instance: opened[0].instance, agent: opened[0].agent, peerLabel: opened[0].peerLabel }, { instance: 'inst_forge', agent: 'agt_ada1', peerLabel: 'forge' });
  const localWin = inGroup('inst_self', 'desk', '<button data-act="web-open-window" data-agent="agt_me1">web window</button>');
  harness.fireEvent(localWin, 'click');
  assert.equal(opened.length, 1, "this node's rows keep the overview rule");
  await harness.act(() => { activeTab.value = 'groups'; });
  assert.equal(timers.queue.length, 0, 'leaving the view cancels polling');
  await mounted.unmount(); state.dispose();
});

test('a peer row runs what the peer shares through its peer routes; this node and unshared controls stay an overview', async (t) => {
  const harness = await createPreactHarness(t);
  const [stateMod, island] = await Promise.all([harness.importDashboardModule('js/skynet-state.js'), harness.importDashboardModule('js/skynet-merged-island.js')]);
  const activeTab = harness.signals.signal('fleet');
  const state = stateMod.createSkynetState({ activeTab });
  state.setStatus({ instance_id: 'inst_self', name: 'desk', peers: [{ instance_id: 'inst_forge', label: 'forge', trusted: true, online: true }] });
  const snapshot = harness.signals.signal({ groups: [group('ops')], agents: [] });
  const timers = fakeTimers(); const toasts = []; const calls = []; const confirms = [];
  const peerSnap = { groups: [group('build', { members: [{ agent_id: 'agt_ada1', title: 'ada' }] })], agents: [], peer_view: { included: ['agents.status', 'lifecycle.resume', 'spawn', 'messaging'] } };
  const fetchImpl = async () => ({ ok: true, status: 200, json: async () => peerSnap });
  const peerActions = (node) => ({
    resume: async (id) => { calls.push([node, 'resume', id]); return {}; },
    spawn: async (g, o) => { calls.push([node, 'spawn', g, o.brief]); return { id: 3, status: 'approved' }; },
    message: async (to, o) => { calls.push([node, 'message', to, o.body]); return {}; },
  });
  const confirm = async (o) => { confirms.push(o); return o.action(); };
  const host = harness.document.createElement('div'); harness.document.body.appendChild(host);
  const mounted = await harness.mount(harness.html`<${island.MergedGroups} state=${state} host=${host} snapshot=${snapshot} fetchImpl=${fetchImpl} timers=${timers} remote="" toast=${(m) => toasts.push(m)} switchNode=${() => {}} peerActions=${peerActions} confirm=${confirm} />`);
  await harness.act(async () => { await timers.queue.shift().fn(); });
  const doc = harness.document;
  const inGroup = (node, name, inner) => { const d = doc.createElement('div'); d.innerHTML = `<details data-fleet-node="${node}" data-fleet-node-name="${name}">${inner}</details>`; host.appendChild(d); return d.firstElementChild.firstElementChild; };
  // A stopped agent's dot wakes it on forge.
  const dot = inGroup('inst_forge', 'forge', '<span data-act="dot-toggle" data-online="0" data-agent="agt_ada1" data-label="ada">○</span>');
  await harness.act(() => harness.fireEvent(dot, 'click'));
  assert.match(doc.querySelector('#peer-action-title').textContent, /ada on forge/);
  await harness.act(() => doc.querySelector('#peer-action-submit').click());
  await harness.act(() => new Promise((r) => setTimeout(r, 10)));
  assert.deepEqual(calls.shift(), ['inst_forge', 'resume', 'agt_ada1']);
  assert.match(confirms[0].body, /ada starts again on forge/);
  // Spawn and message carry the peer's own group name, not group@node.
  const spawn = inGroup('inst_forge', 'forge', '<button data-act="spawn-agent" data-group="build@forge">+</button>');
  await harness.act(() => harness.fireEvent(spawn, 'click'));
  assert.match(doc.querySelector('#peer-spawn-title').textContent, /Spawn in build on forge/);
  await harness.act(() => doc.querySelector('#peer-spawn-modal button').click());
  const msg = inGroup('inst_forge', 'forge', `<button data-act="message-new" data-prefill='{"targetMode":"group","groupName":"build@forge"}'>✉</button>`);
  await harness.act(() => harness.fireEvent(msg, 'click'));
  const body = doc.querySelector('#peer-message-body');
  body.value = 'hello';
  await harness.act(() => harness.fireEvent(body, 'input'));
  await harness.act(() => doc.querySelector('#peer-message-send').click());
  await harness.act(() => new Promise((r) => setTimeout(r, 10)));
  assert.deepEqual(calls.shift(), ['inst_forge', 'message', 'agt_ada1', 'hello']);
  // Unshared (stop), this node's rows and unknown controls keep the overview toast.
  const before = toasts.length;
  const running = inGroup('inst_forge', 'forge', '<span data-act="dot-toggle" data-online="1" data-agent="agt_ada1">●</span>');
  const local = inGroup('inst_self', 'desk', '<span data-act="dot-toggle" data-online="0" data-agent="agt_me1">○</span>');
  assert.equal(harness.fireEvent(running, 'click').defaultPrevented, true);
  assert.equal(harness.fireEvent(local, 'click').defaultPrevented, true);
  assert.equal(doc.querySelector('#peer-action-modal'), null);
  assert.equal(toasts.length, before + 2);
  assert.equal(calls.length, 0);
  await mounted.unmount(); state.dispose();
});
