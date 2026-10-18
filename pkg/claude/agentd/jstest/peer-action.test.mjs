import test from 'node:test'; import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const pv = { peer: 'desk', included: ['agents.status', 'lifecycle.stop', 'lifecycle.retire', 'lifecycle.teleport', 'spawn'], omitted: [{ feature: 'lifecycle.clone', requires: 'groups.members.clone' }] };

test('a peer page runs an agent\'s lifecycle controls through their regular dialogs where granted; the rest stay refused', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/peer-view-limits.js');
  const doc = harness.document;
  doc.body.innerHTML = '<button id="retire" data-act="retire-agent" data-conv="conv-1" data-stable-agent="agt_abc123" data-label="ada">r</button>'
    + '<button id="clone" data-act="clone" data-agent="agt_abc123">c</button>'
    + '<button id="dot-on" data-act="dot-toggle" data-online="1" data-agent="agt_abc123">●</button>'
    + '<button id="dot-off" data-act="dot-toggle" data-online="0" data-agent="agt_abc123">○</button>'
    + '<button id="spawn" data-act="spawn-agent" data-group="ops">+</button>'
    + '<button id="other" data-act="retire-agent" data-stable-agent="agt_other1">r</button>'
    + '<button id="move" data-act="send-elsewhere" data-mode="move" data-agent="agt_abc123">move here…</button>'
    + '<button id="teleport" data-act="send-elsewhere" data-mode="teleport" data-agent="agt_abc123">teleport</button>';
  doc.documentElement.dataset.remoteNodeName = 'forge';
  const events = []; const toasts = [];
  doc.addEventListener(mod.PEER_ACTION_EVENT, (e) => events.push(e.detail));
  const granted = { ...pv, agents: { agt_abc123: ['groups.members.stop', 'groups.members.retire', 'agent.move'] } };
  const dispose = mod.installPeerViewLimits({ doc, snapshot: harness.signals.signal({ peer_view: granted }), toast: (m) => toasts.push(m), remote: { id: 'inst_forge7' } });
  assert.equal(harness.fireEvent(doc.getElementById('retire'), 'click').defaultPrevented, false, 'the regular retire dialog opens');
  assert.equal(harness.fireEvent(doc.getElementById('dot-on'), 'click').defaultPrevented, false, 'and the regular shutdown dialog');
  assert.equal(harness.fireEvent(doc.getElementById('move'), 'click').defaultPrevented, false, 'move here opens the regular move dialog');
  harness.fireEvent(doc.getElementById('spawn'), 'click');
  assert.deepEqual(events, [{ action: 'spawn', group: 'ops' }], 'only spawn takes the peer action path');
  for (const id of ['clone', 'dot-off', 'other', 'teleport']) {
    assert.equal(harness.fireEvent(doc.getElementById(id), 'click').defaultPrevented, true, `${id} is not granted`);
  }
  assert.equal(toasts.length, 4);
  dispose();
});

test('spawn on a peer view opens the regular spawn dialog against the peer, never a small one', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/peer-action.js');
  const limits = await harness.importDashboardModule('js/peer-view-limits.js');
  const doc = harness.document;
  doc.documentElement.dataset.remoteNodeName = 'forge';
  const opened = [];
  const snap = { peer_view: pv, groups: [{ name: 'ops' }], harnesses: [{ name: 'claude' }] };
  const mounted = await harness.mount(harness.html`<${mod.PeerActionHost} snapshot=${harness.signals.signal(snap)} remote=${{ id: 'inst_forge7' }} actions=${{}} toast=${() => {}} openSpawn=${async (o) => { opened.push(o); }} doc=${doc} />`);
  await harness.act(() => doc.dispatchEvent(new harness.window.CustomEvent(limits.PEER_ACTION_EVENT, { detail: { action: 'spawn', group: 'ops' } })));
  assert.equal(opened.length, 1);
  assert.equal(opened[0].nodeId, 'inst_forge7');
  assert.equal(opened[0].node, 'forge');
  assert.equal(opened[0].groupName, 'ops');
  assert.equal(opened[0].snapshot, snap, 'the dialog opens on the peer\'s groups and harnesses');
  assert.equal(doc.querySelector('#peer-spawn-modal'), null, 'the small peer spawn dialog is gone');
  await mounted.unmount();
});

test('a spawn button opens the dialog when the peer takes requests without a grant, or is an older peer offering spawn', async (t) => {
  const harness = await createPreactHarness(t);
  const limits = await harness.importDashboardModule('js/peer-view-limits.js');
  const doc = harness.document;
  doc.body.innerHTML = '<button id="spawn" data-act="spawn-agent" data-group="ops">+</button>';
  const button = doc.getElementById('spawn');
  assert.deepEqual(limits.peerAction(button, { included: ['spawn.request'] }), { action: 'spawn', group: 'ops' });
  assert.deepEqual(limits.peerAction(button, { included: ['spawn'] }), { action: 'spawn', group: 'ops' });
  assert.equal(limits.peerAction(button, { included: ['agents.status'] }), null);
});

test('the action client sends the peer route shapes', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/peer-action.js');
  const sent = [];
  const fetchImpl = async (url, init) => { sent.push([init.method, url, init.body ? JSON.parse(init.body) : undefined]); return { ok: true, json: async () => ({}) }; };
  const a = mod.createPeerActionActions({ fetchImpl });
  await a.move('agt_1', 'alpha');
  await a.moveDirect('agt_1', { group: 'alpha', direct_if_allowed: true });
  assert.deepEqual(sent, [
    ['POST', '/api/agents/agt_1/move', { group: 'alpha' }],
    ['POST', '/api/agents/agt_1/move', { group: 'alpha', direct_if_allowed: true }],
  ]);
});

test('a peer granting stop and resume: the stopped dot, restart and a sandbox restore run; turning the sandbox off takes unrestricted trust', async (t) => {
  const harness = await createPreactHarness(t);
  const limits = await harness.importDashboardModule('js/peer-view-limits.js');
  const doc = harness.document;
  doc.body.innerHTML = '<button id="dot-off" data-act="dot-toggle" data-online="0" data-agent="agt_abc123" data-label="ada">○</button>'
    + '<button id="restart" data-act="restart" data-agent="agt_abc123" data-label="ada">↻</button>'
    + '<span id="restore" data-act="sandbox-restart" data-action="restore" data-agent="agt_abc123">🔓</span>'
    + '<span id="unlock" data-act="sandbox-restart" data-action="unlock" data-agent="agt_abc123">🔒</span>';
  const held = { peer: 'desk', included: ['agents.status'], omitted: [], agents: { agt_abc123: ['groups.members.stop', 'groups.members.resume'] } };
  const dispose = limits.installPeerViewLimits({ doc, snapshot: harness.signals.signal({ peer_view: held }), toast: () => {}, remote: { id: 'inst_forge7' } });
  for (const id of ['dot-off', 'restart', 'restore']) assert.equal(harness.fireEvent(doc.getElementById(id), 'click').defaultPrevented, false, id);
  assert.equal(harness.fireEvent(doc.getElementById('unlock'), 'click').defaultPrevented, true, 'sandbox off is unrestricted only');
  dispose();
  const unrestricted = { ...held, level: 'unrestricted', agents: undefined };
  const sel = limits.grantSelectors(unrestricted);
  assert.ok(sel.includes('[data-act="sandbox-restart"]:not([data-action="restore"])'));
  assert.ok(limits.agentGrantHeld('dot-toggle', 'agt_abc123', held, { online: '0' }));
  assert.ok(!limits.agentGrantHeld('retire-agent', 'agt_abc123', held));
});

test('a node-scoped client addresses the peer routes itself; messages go to one agent', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/peer-action.js');
  const sent = [];
  const fetchImpl = async (url, init) => { sent.push([init.method, url, init.body ? JSON.parse(init.body) : undefined]); return { ok: true, json: async () => ({ groups: [] }) }; };
  const a = mod.createPeerActionActions({ fetchImpl, node: 'inst_forge7' });
  await a.move('agt_1', 'alpha');
  await a.message('agt_1', { subject: 's', body: 'hi' });
  assert.deepEqual(sent, [
    ['POST', '/api/peer/inst_forge7/agents/agt_1/move', { group: 'alpha' }],
    ['POST', '/api/peer/inst_forge7/operator-message', { to: 'agt_1', subject: 's', body: 'hi' }],
  ]);
  assert.deepEqual(mod.groupMembers([{ name: 'ops', members: [{ agent_id: 'agt_ada1', title: 'ada' }, { agent_id: 'agt_x;rm' }, { conv_id: 'c1' }] }], 'ops'), [{ agent: 'agt_ada1', label: 'ada' }]);
});

test('the group message button on a peer view opens a one-agent message dialog', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/peer-action.js');
  const limits = await harness.importDashboardModule('js/peer-view-limits.js');
  const doc = harness.document;
  doc.documentElement.dataset.remoteNodeName = 'forge';
  const shared = { ...pv, included: [...pv.included, 'messaging'] };
  const msg = (prefill) => { const b = doc.createElement('button'); b.dataset.act = 'message-new'; b.dataset.prefill = prefill; return b; };
  assert.deepEqual(limits.peerAction(msg('{"targetMode":"group","groupName":"ops"}'), shared), { action: 'message', group: 'ops' });
  assert.equal(limits.peerAction(msg('{"groupName":"ops"}'), pv), null, 'unshared messaging');
  assert.equal(limits.peerAction(msg('{"groupName":"o ps<"}'), shared), null);
  assert.equal(limits.peerAction(msg('not json'), shared), null);
  const sent = []; const toasts = [];
  const actions = { message: async (to, o) => { sent.push([to, o]); return {}; } };
  const snapshot = harness.signals.signal({ peer_view: shared, groups: [{ name: 'ops', members: [{ agent_id: 'agt_ada1', title: 'ada' }] }] });
  const mounted = await harness.mount(harness.html`<${mod.PeerActionHost} snapshot=${snapshot} remote=${{ id: 'inst_forge7' }} actions=${actions} toast=${(m) => toasts.push(m)} doc=${doc} />`);
  await harness.act(() => doc.dispatchEvent(new harness.window.CustomEvent(limits.PEER_ACTION_EVENT, { detail: { action: 'message', group: 'ops' } })));
  assert.match(doc.querySelector('#peer-message-title').textContent, /Message ops on forge/);
  assert.equal(doc.querySelector('#peer-message-send').disabled, true, 'nothing to send yet');
  const body = doc.querySelector('#peer-message-body');
  body.value = 'status?';
  await harness.act(() => harness.fireEvent(body, 'input'));
  await harness.act(() => doc.querySelector('#peer-message-send').click());
  await harness.act(() => new Promise((r) => setTimeout(r, 10)));
  assert.deepEqual(sent, [['agt_ada1', { subject: '', body: 'status?' }]], 'the only member is preselected');
  assert.match(toasts[0], /Message sent to ada on forge/);
  assert.equal(doc.querySelector('#peer-message-modal'), null);
  await mounted.unmount();
});
