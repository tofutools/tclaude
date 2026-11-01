import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const terminal = {
  id: 7, from_agent: 'agt_a', subject: 'Broken build', body: 'look', read: false,
  action: { kind: 'terminal', label: 'Open a terminal here', detail: '/repo/wt', note: 'Runs as you, outside the agent\'s sandbox.' },
};

async function click(harness, host) {
  await harness.act(() => host.querySelector('button').click());
  for (let i = 0; i < 5; i += 1) await harness.act(() => Promise.resolve());
}

async function load(t) {
  const harness = await createPreactHarness(t);
  return { harness, ...(await harness.importDashboardModule('js/message-action.js')) };
}

function recorder() {
  const calls = [];
  const note = (name) => (target) => { calls.push([name, target]); return true; };
  return { calls, openers: { terminal: note('terminal'), board: note('board'), url: note('url'), path: note('path') } };
}

test('what the daemon resolved is what runs, and nothing else', async (t) => {
  const { runMessageAction } = await load(t);
  const { calls, openers } = recorder();
  assert.equal(await runMessageAction({ kind: 'terminal', agent: 'agt_a', group: 'g' }, openers), true);
  assert.equal(await runMessageAction({ kind: 'board', board: 'brd_1', title: 'Plan' }, openers), true);
  assert.equal(await runMessageAction({ kind: 'pr', url: 'https://github.com/o/r/pull/1' }, openers), true);
  assert.equal(await runMessageAction({ kind: 'tab', path: '/usage' }, openers), true);
  assert.deepEqual(calls.map(([name]) => name), ['terminal', 'board', 'url', 'path']);

  calls.length = 0;
  assert.equal(await runMessageAction({ kind: 'pr', url: 'javascript:alert(1)' }, openers), false);
  assert.equal(await runMessageAction({ kind: 'tab', path: 'https://example.com' }, openers), false);
  assert.equal(await runMessageAction({ kind: 'other' }, openers), false);
  assert.equal(await runMessageAction(null, openers), false);
  assert.deepEqual(calls, []);
});

test('a terminal that does not open is reported', async (t) => {
  const { runMessageAction } = await load(t);
  assert.equal(await runMessageAction({ kind: 'terminal' }, { terminal: () => null }), false);
});

test('the button says what it does and where, and only a click asks the daemon', async (t) => {
  const { harness, MessageAction } = await load(t);
  const { h } = harness.preact;
  const fetches = [];
  const { calls, openers } = recorder();
  let done = 0;
  const fetchImpl = async (url, init) => {
    fetches.push([url, JSON.parse(init.body)]);
    return { ok: true, json: async () => ({ kind: 'terminal', agent: 'agt_a', group: 'g' }) };
  };
  const { container: host } = await harness.mount(h(MessageAction, { message: terminal, fetchImpl, openers, onDone: () => { done += 1; } }));
  assert.match(host.textContent, /Open a terminal here/);
  assert.match(host.textContent, /\/repo\/wt/);
  assert.match(host.textContent, /outside the agent's sandbox/);
  assert.deepEqual(fetches, []);

  await click(harness, host);
  assert.deepEqual(fetches, [['/api/human-messages/action', { id: 7 }]]);
  assert.equal(calls.length, 1);
  assert.equal(done, 1);
});

test('a refused click says why and runs nothing', async (t) => {
  const { harness, MessageAction } = await load(t);
  const { h } = harness.preact;
  const { calls, openers } = recorder();
  const toasts = [];
  const fetchImpl = async () => ({ ok: false, status: 409, json: async () => ({ error: 'That board is gone.' }) });
  const { container: host } = await harness.mount(h(MessageAction, { message: terminal, fetchImpl, openers, toast: (text, error) => toasts.push([text, error]) }));
  await click(harness, host);
  assert.deepEqual(calls, []);
  assert.deepEqual(toasts, [['That action did not run: That board is gone.', true]]);
});

test('an unavailable action is off and says why; a peer page shows none', async (t) => {
  const { harness, MessageAction } = await load(t);
  const { h } = harness.preact;
  const off = { ...terminal, action: { ...terminal.action, unavailable: 'That agent is gone.' } };
  const { container: host } = await harness.mount(h(MessageAction, { message: off }));
  assert.equal(host.querySelector('button').disabled, true);
  assert.match(host.textContent, /That agent is gone\./);

  const { container: peer } = await harness.mount(h(MessageAction, { message: terminal, remote: true }));
  assert.equal(peer.querySelector('button'), null);
});

test('an action rides in chat only while unread, for its own agent', async (t) => {
  const { harness } = await load(t);
  const { actionsFor } = await harness.importDashboardModule('js/companion-model.js');
  const agent = { agentId: 'agt_a', convId: 'conv-a' };
  const later = { ...terminal, id: 8 };
  const snapshot = { messages: [later, terminal, { ...terminal, id: 9, read: true }, { ...terminal, id: 10, from_agent: 'agt_b' }, { id: 11, from_agent: 'agt_a' }] };
  assert.deepEqual(actionsFor(snapshot, agent).map((m) => m.id), [7, 8]);
  assert.deepEqual(actionsFor(snapshot, null), []);
});
