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
  const timers = fakeTimers(); const calls = []; const toasts = []; const switched = [];
  const fetchImpl = async (url) => { calls.push(url); return { ok: true, status: 200, json: async () => ({ groups: [group('build')], agents: [], peer_view: { included: ['agents.status'] } }) }; };
  const host = harness.document.createElement('div'); harness.document.body.appendChild(host);
  const mounted = await harness.mount(harness.html`<${island.MergedGroups} state=${state} host=${host} snapshot=${snapshot} fetchImpl=${fetchImpl} timers=${timers} remote="" toast=${(m) => toasts.push(m)} switchNode=${(id) => switched.push(id)} />`);
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
  await harness.act(() => { activeTab.value = 'groups'; });
  assert.equal(timers.queue.length, 0, 'leaving the view cancels polling');
  await mounted.unmount(); state.dispose();
});
