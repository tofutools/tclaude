// A Collab panel agent row's ⋯ (collab-rail-menu.js): Stop or Resume after
// Attach, and Retire… with Kill session, as the agent's Groups row offers
// them (its status light and its retire), through the same dialogs and
// requests, and on a peer's agent only where the peer grants them.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const agentItem = (agent, extra = {}) => ({ key: `terminal:${agent.conv_id}`, kind: 'terminal', title: agent.title,
  status: { key: agent.online ? 'idle' : 'offline', agent }, ...extra });
const ada = (online) => ({ agent_id: 'agt_ada', conv_id: 'conv-ada', title: 'ada', online });
const ids = ({ open, end }) => ({ open: open.map((a) => a.id), end: end.map((a) => a.id) });

function fakeContext() {
  const calls = [];
  const record = (name) => (...args) => { calls.push([name, ...args]); return Promise.resolve(); };
  return { calls, open: record('open'), stopAgent: record('stopAgent'), resumeAgent: record('resumeAgent'), retireAgent: record('retireAgent'),
    kill: record('kill'), notify: record('notify'), confirm: async (o) => { calls.push(['confirm', o.title]); return o.action(); } };
}

async function load(t) {
  const harness = await createPreactHarness(t);
  return harness.importDashboardModule('js/collab-rail-menu.js');
}

test('a live agent: Attach, Stop…; Kill session… and Retire… in the destructive part', async (t) => {
  const { railRowActions } = await load(t);
  const row = { key: 'tclaude-ada', tmux_session: 'tclaude-ada', killable: true };
  const item = agentItem(ada(true), { row });
  const local = { peerView: () => null };
  assert.deepEqual(ids(railRowActions(item, local)), { open: ['open', 'stop'], end: ['kill', 'retire'] });
  const ctx = fakeContext();
  const { open, end } = railRowActions(item, local);
  await open[1].run(ctx);
  await end[1].run(ctx);
  assert.deepEqual(ctx.calls, [['stopAgent', 'agt_ada', 'ada', ''], ['retireAgent', 'conv-ada', 'ada', '']],
    'the Groups row\'s shutdown dialog by the stable ID, and its retire by the conversation');
  assert.equal(end[1].danger, true);
});

test('a stopped agent: Resume instead of Stop, and Retire…', async (t) => {
  const { railRowActions } = await load(t);
  const item = agentItem(ada(false));
  const local = { peerView: () => null };
  assert.deepEqual(ids(railRowActions(item, local)), { open: ['open', 'resume'], end: ['retire'] });
  const ctx = fakeContext();
  await railRowActions(item, local).open[1].run(ctx);
  assert.deepEqual(ctx.calls, [['resumeAgent', 'agt_ada', 'ada', '']]);
});

test('a peer\'s agent gets each only where the peer grants it, and is named by its stable ID', async (t) => {
  const { railRowActions } = await load(t);
  const remote = (online) => agentItem(ada(online), { pane: { seed: { remote: { peer: 'inst_bob', agent: 'agt_ada' } } } });
  const none = { peerView: () => ({ agents: {} }) };
  assert.deepEqual(ids(railRowActions(remote(true), none)), { open: ['open'], end: [] }, 'nothing granted: nothing offered');
  const stopOnly = { peerView: (node) => (node === 'inst_bob' ? { agents: { agt_ada: ['groups.members.stop'] } } : null) };
  assert.deepEqual(ids(railRowActions(remote(true), stopOnly)), { open: ['open', 'stop'], end: [] });
  assert.deepEqual(ids(railRowActions(remote(false), stopOnly)), { open: ['open'], end: [] }, 'stop is not resume');
  const all = { peerView: () => ({ agents: { agt_ada: ['groups.members.resume', 'groups.members.retire'] } }) };
  const ctx = fakeContext();
  const { open, end } = railRowActions(remote(false), all);
  assert.deepEqual(ids({ open, end }), { open: ['open', 'resume'], end: ['retire'] });
  await open[1].run(ctx);
  await end[0].run(ctx);
  assert.deepEqual(ctx.calls, [['resumeAgent', 'agt_ada', 'ada', 'inst_bob'], ['retireAgent', 'agt_ada', 'ada', 'inst_bob']]);
});

test('a peer\'s agent running with nothing attached here (a background row) is gated and named by its node too', async (t) => {
  const { railRowActions } = await load(t);
  const item = agentItem(ada(true), { row: { key: 'bg-ada', tmux_session: 'tclaude-ada', node: { id: 'inst_bob', name: 'bob', color: '#888' } } });
  const seen = [];
  const env = { peerView: (node) => { seen.push(node); return { agents: { agt_ada: ['groups.members.stop', 'groups.members.retire'] } }; } };
  const ctx = fakeContext();
  const { open, end } = railRowActions(item, env);
  assert.deepEqual(ids({ open, end }), { open: ['open', 'stop'], end: ['retire'] });
  assert.deepEqual([...new Set(seen)], ['inst_bob'], 'the grant check reads the row\'s node by its id');
  await open[1].run(ctx);
  assert.deepEqual(ctx.calls, [['stopAgent', 'agt_ada', 'ada', 'inst_bob']]);
});

test('a shell or a drawing board gets none of them', async (t) => {
  const { railRowActions } = await load(t);
  const shell = { key: 'shell:tclaude', kind: 'shell', title: 'Group shell' };
  const board = { key: 'drawing_board:plan', kind: 'drawing_board', title: 'Plan', resource: { id: 'plan' } };
  for (const item of [shell, board]) {
    const { open, end } = railRowActions(item, { peerView: () => null });
    assert.equal([...open, ...end].some((a) => ['stop', 'resume', 'retire'].includes(a.id)), false, item.kind);
  }
});
