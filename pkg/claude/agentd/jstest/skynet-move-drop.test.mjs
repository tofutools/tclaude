import test from 'node:test'; import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const DROP = {
  agent: { id: 'agt_b1', name: 'builder-1' },
  source: { node: 'inst_self', name: 'desk', group: 'builders' },
  target: { node: 'inst_forge', name: 'forge', group: 'reviewers' },
};

function fakeTimers() {
  const queue = []; let seq = 0;
  return { queue, setTimeout(fn, ms) { const id = ++seq; queue.push({ id, fn, ms }); return id; }, clearTimeout(id) { const i = queue.findIndex((q) => q.id === id); if (i >= 0) queue.splice(i, 1); } };
}

test('a drop is a push from this node, a pull to it, or nothing this node can do', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/skynet-move-drop.js');
  const n = (node, group = 'g') => ({ node, group });
  assert.equal(m.dropPlan({ source: n('inst_self'), target: n('inst_forge'), self: 'inst_self' }).kind, 'push');
  assert.equal(m.dropPlan({ source: n('inst_forge'), target: n('inst_self'), self: 'inst_self' }).kind, 'pull');
  assert.equal(m.dropPlan({ source: n('inst_forge'), target: n('inst_lab'), self: 'inst_self' }).kind, 'third');
  assert.equal(m.dropPlan({ source: n('inst_self'), target: n('inst_self'), self: 'inst_self' }).kind, 'same');
  assert.equal(m.dropPlan({ source: n('inst_self'), target: n('inst_forge', ''), self: 'inst_self' }).kind, 'none');
  assert.deepEqual(m.moveProgress({ state: 'awaiting_running', disposition: 'checking' }), { phase: 'checking' });
  assert.deepEqual(m.moveProgress({ state: 'confirmed', disposition: 'landed', target_agent: 'agt_n1', cwd: '/w' }), { phase: 'landed', agent: 'agt_n1', cwd: '/w' });
  assert.deepEqual(m.moveProgress({ state: 'awaiting_confirmation', disposition: 'pending_acceptance' }), { phase: 'waiting' });
  assert.deepEqual(m.moveProgress({ state: 'declined', last_error: 'no' }), { phase: 'failed', state: 'declined', error: 'no' });
  assert.equal(m.moveProgress({ state: 'blocked', disposition: 'landed', last_error: 'retire failed' }).phase, 'failed', 'a blocked retirement is not a clean landing');
});

async function mountDialog(t, { plan = { kind: 'push' }, actions, peerActions = () => ({}) }) {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/skynet-move-drop.js');
  const timers = fakeTimers(); let clock = 0; let closed = 0; const pages = [];
  const mounted = await harness.mount(harness.html`<${m.MoveDropDialog} drop=${DROP} plan=${plan} actions=${actions} peerActions=${peerActions}
    timers=${timers} now=${() => clock} onClose=${() => { closed += 1; }} openPage=${(p) => pages.push(p)} />`);
  const q = (s) => mounted.container.querySelector(s) || harness.document.querySelector(s);
  const settle = () => harness.act(() => new Promise((r) => setTimeout(r, 5)));
  const click = async (el) => { await harness.act(() => el.click()); await settle(); };
  const tick = async (ms = 2000) => { clock += ms; const next = timers.queue.shift(); await harness.act(async () => { await next.fn(); }); await settle(); };
  return { harness, q, click, tick, timers, closed: () => closed, pages };
}

test('a pushed move confirms where it goes, then follows the record until it lands', async (t) => {
  const log = []; const records = [{ state: 'awaiting_running', disposition: 'checking' }, { state: 'confirmed', disposition: 'landed', target_agent: 'agt_n1', cwd: '/srv/review' }];
  const s = await mountDialog(t, { actions: {
    moveAgent: async (b) => { log.push(['move', b]); return { move_id: 'mv_1', disposition: 'checking' }; },
    moveDetail: async (id) => { log.push(['detail', id]); return records.shift(); },
  } });
  const text = s.q('#skynet-move-drop').textContent;
  assert.match(text, /Move builder-1 to reviewers on forge\?/);
  assert.match(text, /full conversation history.*If forge's permissions allow it, it lands at once; otherwise it waits for forge to accept it.*original on desk is retired/);
  assert.match(text, /from.*desk · builders.*to.*forge · reviewers.*starts in.*forge picks it/);
  assert.equal(s.harness.document.activeElement?.id, 'skynet-move-drop-ok', 'Enter confirms');
  await s.click(s.q('#skynet-move-drop-ok'));
  assert.deepEqual(log[0], ['move', { agent: 'agt_b1', peer: 'inst_forge', group: 'reviewers', direct_if_allowed: true }]);
  assert.match(s.q('#skynet-move-drop').textContent, /Checking whether forge takes builder-1 in directly/, 'a 200 alone is not success');
  await s.tick();
  assert.match(s.q('#skynet-move-drop').textContent, /Checking/);
  await s.tick();
  assert.match(s.q('[data-move-drop-state="landed"]').textContent, /builder-1 landed in group reviewers on forge as agt_n1, starting in \/srv\/review\. The original on desk is being retired; follow that in/);
  assert.equal(s.timers.queue.length, 0, 'polling stops once landed');
});

test('a receiver that does not admit it directly leaves it waiting, with a link to follow it', async (t) => {
  const s = await mountDialog(t, { actions: { moveAgent: async () => ({ move_id: 'mv_2', disposition: 'pending_acceptance' }), moveDetail: async () => assert.fail('no poll') } });
  await s.click(s.q('#skynet-move-drop-ok'));
  assert.match(s.q('[data-move-drop-state="waiting"]').textContent, /waiting for forge to accept it.*keeps running here until then/);
  await s.click(s.q('[data-move-drop="moves"]'));
  assert.deepEqual(s.pages, ['moves']);
  assert.equal(s.closed(), 1);
});

test('flagged history needs an explicit choice before the move is sent again', async (t) => {
  const bodies = [];
  const s = await mountDialog(t, { actions: {
    moveAgent: async (b) => { bodies.push(b); if (!b.allow_flagged) { const e = new Error('flagged'); e.code = 'flagged_credentials'; e.body = { findings: [{ kind: 'aws_key', count: 1 }] }; throw e; } return { move_id: 'mv_3', disposition: 'pending_acceptance' }; },
    moveDetail: async () => ({}),
  } });
  await s.click(s.q('#skynet-move-drop-ok'));
  assert.match(s.q('#skynet-move-drop [role=alert]').textContent, /contains credentials/);
  assert.equal(s.q('#skynet-move-drop-ok').disabled, true);
  await s.harness.act(() => { const c = s.q('#skynet-move-drop-allow'); c.checked = true; s.harness.fireEvent(c, 'change'); });
  await s.click(s.q('#skynet-move-drop-ok'));
  assert.equal(bodies[1].allow_flagged, true);
  assert.ok(s.q('[data-move-drop-state="waiting"]'));
});

test('a pull asks the peer and follows this node\'s record, tolerating its late arrival', async (t) => {
  const calls = []; let reads = 0;
  const s = await mountDialog(t, {
    plan: { kind: 'pull' },
    actions: { moveDetail: async (id) => { reads += 1; if (reads < 3) { const e = new Error('no such move'); e.status = 404; throw e; } return { state: 'declined', last_error: 'refused by policy' }; } },
    peerActions: (node) => ({ moveDirect: async (id, body) => { calls.push([node, id, body]); return { move_id: 'mv_4', disposition: 'checking' }; } }),
  });
  assert.match(s.q('#skynet-move-drop').textContent, /builder-1 \(agt_b1\) leaves desk.*to land in group reviewers on this node/);
  await s.click(s.q('#skynet-move-drop-ok'));
  assert.deepEqual(calls, [['inst_self', 'agt_b1', { group: 'reviewers', direct_if_allowed: true }]]);
  await s.tick(); await s.tick(); await s.tick();
  assert.match(s.q('[data-move-drop-state="failed"]').textContent, /The move declined: refused by policy\. builder-1 is still on desk/);
});

test('a peer without direct moves still takes the pull as an offer; a flagged pull says where to go', async (t) => {
  const calls = [];
  const old = await mountDialog(t, {
    plan: { kind: 'pull' },
    actions: { moveDetail: async () => assert.fail('no poll') },
    peerActions: () => ({
      moveDirect: async () => { calls.push('direct'); const e = new Error('invalid remote action body'); e.status = 400; throw e; },
      move: async (id, group) => { calls.push(['move', id, group]); return {}; },
    }),
  });
  await old.click(old.q('#skynet-move-drop-ok'));
  assert.deepEqual(calls, ['direct', ['move', 'agt_b1', 'reviewers']]);
  assert.match(old.q('[data-move-drop-state="waiting"]').textContent, /waiting for you to accept it/);
});

test('a pull refused for flagged history points at the source dashboard', async (t) => {
  const s = await mountDialog(t, {
    plan: { kind: 'pull' },
    actions: { moveDetail: async () => ({}) },
    peerActions: () => ({ moveDirect: async () => { const e = new Error('suspected credentials'); e.status = 422; e.code = 'flagged_credentials'; throw e; } }),
  });
  await s.click(s.q('#skynet-move-drop-ok'));
  assert.match(s.q('#skynet-move-drop [role=alert]').textContent, /contains credentials.*Move it from desk's dashboard/);
  assert.equal(s.q('#skynet-move-drop-allow'), null, 'no override a pull cannot carry');
});
