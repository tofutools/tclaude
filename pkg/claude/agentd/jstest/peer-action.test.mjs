import test from 'node:test'; import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const pv = { peer: 'desk', included: ['agents.status', 'lifecycle.stop', 'lifecycle.retire', 'lifecycle.teleport', 'spawn'], omitted: [{ feature: 'lifecycle.clone', requires: 'groups.members.clone' }] };

function fakeTimers() {
  const queue = []; let seq = 0;
  return {
    queue,
    setTimeout(fn, ms) { const id = ++seq; queue.push({ id, fn, ms }); return id; },
    clearTimeout(id) { const i = queue.findIndex((q) => q.id === id); if (i >= 0) queue.splice(i, 1); },
  };
}

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

test('spawn sends a brief under the peer\'s launch policy and follows the request', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/peer-action.js');
  const limits = await harness.importDashboardModule('js/peer-view-limits.js');
  const doc = harness.document;
  doc.documentElement.dataset.remoteNodeName = 'forge';
  const calls = []; const confirms = []; const timers = fakeTimers();
  const actions = {
    spawn: async (group, o) => { calls.push(['spawn', group, o]); return { id: 7, status: 'pending' }; },
    spawnStatus: async (id) => { calls.push(['status', id]); return { id, status: 'approved', result_agent: 'agt_new1' }; },
  };
  const confirm = async (o) => { confirms.push(o); return o.action ? o.action() : true; };
  const mounted = await harness.mount(harness.html`<${mod.PeerActionHost} snapshot=${harness.signals.signal({ peer_view: pv })} remote=${{ id: 'inst_forge7' }} actions=${actions} confirm=${confirm} toast=${() => {}} timers=${timers} doc=${doc} />`);
  await harness.act(() => doc.dispatchEvent(new harness.window.CustomEvent(limits.PEER_ACTION_EVENT, { detail: { action: 'spawn', group: 'ops' } })));
  const brief = doc.querySelector('#peer-spawn-brief');
  brief.value = 'Fix the flaky test';
  await harness.act(() => harness.fireEvent(brief, 'input'));
  await harness.act(() => doc.querySelector('#peer-spawn-submit').click());
  await harness.act(() => new Promise((r) => setTimeout(r, 25)));
  assert.match(confirms[0].body, /forge starts a new agent in its group ops.*own launch policy.*cannot override/);
  assert.deepEqual(calls[0], ['spawn', 'ops', { brief: 'Fix the flaky test', name: '', role: '', profile: '' }]);
  assert.match(doc.querySelector('#peer-spawn-status').textContent, /Request 7: pending/);
  const tick = timers.queue.find((x) => x.ms === 2000);
  assert.ok(tick, 'a pending request is polled');
  await harness.act(() => { timers.queue.splice(timers.queue.indexOf(tick), 1); tick.fn(); });
  await harness.act(() => new Promise((r) => setTimeout(r, 25)));
  assert.match(doc.querySelector('#peer-spawn-status').textContent, /approved — agent agt_new1/);
  assert.equal(timers.queue.length, 0, 'polling stops once decided');
  await mounted.unmount();
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
  await a.spawn('ops', { brief: 'b' });
  assert.deepEqual(await a.localGroups('inst_p'), [{ name: 'c', receives: true }, { name: 'a', receives: false }, { name: 'b', receives: false }]);
  assert.deepEqual(sent, [
    ['POST', '/api/agents/agt_1/stop?force=1', undefined],
    ['POST', '/api/agents/agt_1/retire', {}],
    ['POST', '/api/agents/agt_1/clone', { follow_up: 'go on' }],
    ['POST', '/api/agents/agt_1/move', { group: 'alpha' }],
    ['POST', '/api/groups/ops/spawn', { brief: 'b' }],
    ['GET', '/api/federation/links', undefined],
  ]);
});
