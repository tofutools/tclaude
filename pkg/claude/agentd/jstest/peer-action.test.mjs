import test from 'node:test'; import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const pv = { peer: 'desk', included: ['agents.status', 'lifecycle.stop', 'lifecycle.retire', 'lifecycle.teleport', 'spawn'], omitted: [{ feature: 'lifecycle.clone', requires: 'groups.members.clone' }] };

test('shared lifecycle and spawn controls open the peer action dialog; unshared ones stay greyed', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/peer-view-limits.js');
  const doc = harness.document;
  doc.body.innerHTML = '<button id="retire" data-act="retire-agent" data-conv="conv-1" data-stable-agent="agt_abc123" data-label="ada">r</button>'
    + '<button id="clone" data-act="clone" data-agent="agt_abc123">c</button>'
    + '<button id="dot-on" data-act="dot-toggle" data-online="1" data-agent="agt_abc123">●</button>'
    + '<button id="dot-off" data-act="dot-toggle" data-online="0" data-agent="agt_abc123">○</button>'
    + '<button id="spawn" data-act="spawn-agent" data-group="ops">+</button>'
    + '<button id="evil" data-act="retire-agent" data-stable-agent="agt_x;rm -rf">r</button>';
  doc.documentElement.dataset.remoteNodeName = 'forge';
  const events = []; const toasts = [];
  doc.addEventListener(mod.PEER_ACTION_EVENT, (e) => events.push(e.detail));
  const dispose = mod.installPeerViewLimits({ doc, snapshot: harness.signals.signal({ peer_view: pv }), toast: (m) => toasts.push(m), remote: { id: 'inst_forge7' } });
  assert.equal(harness.fireEvent(doc.getElementById('retire'), 'click').defaultPrevented, true, 'the local retire dialog never opens');
  harness.fireEvent(doc.getElementById('dot-on'), 'click');
  harness.fireEvent(doc.getElementById('spawn'), 'click');
  assert.deepEqual(events, [
    { action: 'retire', agent: 'agt_abc123', label: 'ada' },
    { action: 'stop', agent: 'agt_abc123', label: '' },
    { action: 'spawn', group: 'ops' },
  ]);
  harness.fireEvent(doc.getElementById('clone'), 'click');
  harness.fireEvent(doc.getElementById('dot-off'), 'click');
  harness.fireEvent(doc.getElementById('evil'), 'click');
  assert.equal(events.length, 3, 'unshared clone, waking and an unsafe agent ID never become peer actions');
  assert.equal(toasts.length, 3);
  dispose();
});

test('the agent dialog offers what the peer shares, confirms the consequence and sends peer-shaped requests', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/peer-action.js');
  const limits = await harness.importDashboardModule('js/peer-view-limits.js');
  const doc = harness.document;
  doc.documentElement.dataset.remoteNodeName = 'forge';
  const calls = []; const confirms = []; const toasts = [];
  const actions = {
    stop: async (id, force) => { calls.push(['stop', id, force]); return {}; },
    retire: async (id) => { calls.push(['retire', id]); return {}; },
    teleport: async (id, o) => { calls.push(['teleport', id, o]); return { id: 'tp1' }; },
    localGroups: async (peer) => { calls.push(['groups', peer]); return [{ name: 'beta', receives: true }, { name: 'alpha', receives: false }]; },
  };
  const confirm = async (o) => { confirms.push(o); return o.action ? o.action() : true; };
  const snapshot = harness.signals.signal({ peer_view: pv });
  const mounted = await harness.mount(harness.html`<${mod.PeerActionHost} snapshot=${snapshot} remote=${{ id: 'inst_forge7' }} actions=${actions} confirm=${confirm} toast=${(m, e) => toasts.push([m, e])} doc=${doc} />`);
  const q = (s) => doc.querySelector(s);
  const settle = () => harness.act(() => new Promise((r) => setTimeout(r, 25)));
  await harness.act(() => doc.dispatchEvent(new harness.window.CustomEvent(limits.PEER_ACTION_EVENT, { detail: { action: 'retire', agent: 'agt_abc123', label: 'ada' } })));
  const choices = [...q('#peer-action-modal').querySelectorAll('.peer-action-choices label')].map((l) => l.textContent.trim());
  assert.deepEqual(choices, ['Stop', 'Retire', 'Teleport here'], 'clone and move are not shared');
  assert.match(q('#peer-action-modal').textContent, /ada on forge is retired: its running session is asked to exit.*worktree is kept/);
  const teleport = [...q('#peer-action-modal').querySelectorAll('input[type=radio]')].find((r) => r.value === 'teleport');
  teleport.checked = true;
  await harness.act(() => harness.fireEvent(teleport, 'change'));
  await settle();
  assert.deepEqual([...q('#peer-action-group').querySelectorAll('optgroup')].map((o) => [o.getAttribute('label'), [...o.querySelectorAll('option')].map((x) => x.textContent)]), [['receives agents from forge', ['beta']], ['needs an agents.receive grant for forge', ['alpha']]]);
  assert.deepEqual(calls.find((c) => c[0] === 'groups'), ['groups', 'inst_forge7']);
  const keep = q('#peer-action-keep');
  keep.checked = true;
  await harness.act(() => harness.fireEvent(keep, 'change'));
  await harness.act(() => q('#peer-action-submit').click());
  await settle();
  assert.match(confirms.at(-1).body, /teleports from forge to this node, continuing its history in group beta; the original keeps running on forge/);
  assert.match(confirms.at(-1).body, /Where this node lands teleports in that group automatically, it starts in the first fit \(a matching Fleet repo, its own path if that exists here, the group's default dir, then the landing policy\); otherwise it waits in Fleet → Offers/);
  assert.deepEqual(calls.at(-1), ['teleport', 'agt_abc123', { group: 'beta', note: '', clone: true }]);
  assert.equal(q('#peer-action-modal'), null, 'the dialog closes once sent');
  assert.match(toasts.at(-1)[0], /comes to group beta on this node/);

  await harness.act(() => doc.dispatchEvent(new harness.window.CustomEvent(limits.PEER_ACTION_EVENT, { detail: { action: 'stop', agent: 'agt_abc123' } })));
  const force = q('#peer-action-force');
  force.checked = true;
  await harness.act(() => harness.fireEvent(force, 'change'));
  await harness.act(() => q('#peer-action-submit').click());
  await settle();
  assert.match(confirms.at(-1).body, /killed at once/);
  assert.deepEqual(calls.at(-1), ['stop', 'agt_abc123', true]);
  await mounted.unmount();
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
  const fetchImpl = async (url, init) => { sent.push([init.method, url, init.body ? JSON.parse(init.body) : undefined]); return { ok: true, json: async () => ({ groups: [{ name: 'b' }, { name: 'a' }, { name: 'c', federation_links: [{ peer: 'inst_p', slugs: ['agents.receive'] }, { peer: 'inst_q', slugs: ['agents.receive'] }] }] }) }; };
  const a = mod.createPeerActionActions({ fetchImpl });
  await a.stop('agt_1', true);
  await a.retire('agt_1');
  await a.clone('agt_1', { followUp: 'go on' });
  await a.move('agt_1', 'alpha');
  assert.deepEqual(await a.localGroups('inst_p'), [{ name: 'c', receives: true }, { name: 'a', receives: false }, { name: 'b', receives: false }]);
  assert.deepEqual(sent, [
    ['POST', '/api/agents/agt_1/stop?force=1', undefined],
    ['POST', '/api/agents/agt_1/retire', {}],
    ['POST', '/api/agents/agt_1/clone', { follow_up: 'go on' }],
    ['POST', '/api/agents/agt_1/move', { group: 'alpha' }],
    ['GET', '/api/federation/links', undefined],
  ]);
});

test('a peer sharing resume and restart: the stopped dot wakes, restart and sandbox restart go to the peer, and sandbox-off says what it means', async (t) => {
  const harness = await createPreactHarness(t);
  const limits = await harness.importDashboardModule('js/peer-view-limits.js');
  const mod = await harness.importDashboardModule('js/peer-action.js');
  const doc = harness.document;
  const shared = { peer: 'desk', included: ['agents.status', 'lifecycle.stop', 'lifecycle.resume', 'lifecycle.restart', 'lifecycle.sandbox-restart'], omitted: [] };
  doc.body.innerHTML = '<button id="dot-off" data-act="dot-toggle" data-online="0" data-agent="agt_abc123" data-label="ada">○</button>'
    + '<button id="restart" data-act="restart" data-agent="agt_abc123" data-label="ada">↻</button>'
    + '<span id="unlock" data-act="sandbox-restart" data-action="unlock" data-agent="agt_abc123" data-label="ada">🔒</span>';
  doc.documentElement.dataset.remoteNodeName = 'forge';
  const events = [];
  doc.addEventListener(limits.PEER_ACTION_EVENT, (e) => events.push(e.detail));
  const snapshot = harness.signals.signal({ peer_view: shared });
  const dispose = limits.installPeerViewLimits({ doc, snapshot, toast: () => {}, remote: { id: 'inst_forge7' } });
  for (const id of ['dot-off', 'restart', 'unlock']) harness.fireEvent(doc.getElementById(id), 'click');
  assert.deepEqual(events, [
    { action: 'resume', agent: 'agt_abc123', label: 'ada' },
    { action: 'restart', agent: 'agt_abc123', label: 'ada' },
    { action: 'sandbox-restart', agent: 'agt_abc123', label: 'ada', direction: 'unlock' },
  ]);
  dispose();

  const calls = []; const confirms = [];
  const actions = {
    resume: async (id) => { calls.push(['resume', id]); return {}; },
    restart: async (id) => { calls.push(['restart', id]); return {}; },
    sandboxRestart: async (id, d) => { calls.push(['sandbox-restart', id, d]); return {}; },
  };
  const confirm = async (o) => { confirms.push(o); return o.action ? o.action() : true; };
  const mounted = await harness.mount(harness.html`<${mod.PeerActionHost} snapshot=${snapshot} remote=${{ id: 'inst_forge7' }} actions=${actions} confirm=${confirm} toast=${() => {}} doc=${doc} />`);
  const q = (s) => doc.querySelector(s);
  const send = async (detail) => {
    await harness.act(() => doc.dispatchEvent(new harness.window.CustomEvent(limits.PEER_ACTION_EVENT, { detail })));
    await harness.act(() => q('#peer-action-submit').click());
    await harness.act(() => new Promise((r) => setTimeout(r, 25)));
  };
  await send(events[0]);
  assert.match(confirms.at(-1).body, /ada starts again on forge.*missing launch directory or switching a Codex drive is left to forge's operator/);
  await send(events[1]);
  assert.match(confirms.at(-1).body, /stopped and started again on forge.*fully idle/);
  await send(events[2]);
  assert.match(confirms.at(-1).body, /sandbox OFF: full access to forge's machine.*only for a peer it trusts unrestricted/);
  assert.deepEqual(calls, [['resume', 'agt_abc123'], ['restart', 'agt_abc123'], ['sandbox-restart', 'agt_abc123', 'unlock']]);
  await mounted.unmount();
});

test('the peer action routes for resume, restart and sandbox restart', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/peer-action.js');
  const sent = [];
  const a = mod.createPeerActionActions({ fetchImpl: async (url, init) => { sent.push([init.method, url, init.body ? JSON.parse(init.body) : null]); return { ok: true, json: async () => ({}) }; } });
  await a.resume('agt_1'); await a.restart('agt_1'); await a.sandboxRestart('agt_1', 'unlock'); await a.sandboxRestart('agt_1', 'x');
  assert.deepEqual(sent, [
    ['POST', '/api/agents/agt_1/resume', null],
    ['POST', '/api/agents/agt_1/restart', null],
    ['POST', '/api/agents/agt_1/sandbox-restart', { action: 'unlock' }],
    ['POST', '/api/agents/agt_1/sandbox-restart', { action: 'restore' }],
  ]);
});

test('a node-scoped client addresses the peer routes itself; messages go to one agent', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/peer-action.js');
  const sent = [];
  const fetchImpl = async (url, init) => { sent.push([init.method, url, init.body ? JSON.parse(init.body) : undefined]); return { ok: true, json: async () => ({ groups: [] }) }; };
  const a = mod.createPeerActionActions({ fetchImpl, node: 'inst_forge7' });
  await a.resume('agt_1');
  await a.message('agt_1', { subject: 's', body: 'hi' });
  await a.localGroups('inst_forge7');
  assert.deepEqual(sent, [
    ['POST', '/api/peer/inst_forge7/agents/agt_1/resume', undefined],
    ['POST', '/api/peer/inst_forge7/operator-message', { to: 'agt_1', subject: 's', body: 'hi' }],
    ['GET', '/api/federation/links', undefined],
  ], 'move/teleport destinations stay this node\'s groups');
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
