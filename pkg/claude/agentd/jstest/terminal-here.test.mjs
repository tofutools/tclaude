import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const AGENT = 'agt_0123456789abcdef0123456789abcdef';
const PEER = 'inst_0123456789abcdef';
const member = { agent_id: AGENT, conv_id: 'conv-1', startup_dir: '/repo', current_dir: '/repo-wt', state: { harness: 'claude' } };

async function load(t) {
  const harness = await createPreactHarness(t);
  return { harness, ...(await harness.importDashboardModule('js/terminal-here.js')) };
}

test('an agent pane has a shell here; a shell, a pop-out and a peer shell do not', async (t) => {
  const { paneShellAgent } = await load(t);
  assert.deepEqual(paneShellAgent({ key: `window:${AGENT}`, seed: { agent: AGENT } }), { agent: AGENT });
  assert.equal(paneShellAgent({ key: `term:${AGENT}:current:`, seed: { agent: AGENT } }), null);
  assert.equal(paneShellAgent({ key: `window:${AGENT}`, seed: { agent: AGENT } }, true), null);
  assert.deepEqual(paneShellAgent({ key: 'x', seed: { remote: { peer: PEER, peerLabel: 'nas', agent: AGENT } } }),
    { agent: AGENT, node: PEER, peerLabel: 'nas' });
  assert.equal(paneShellAgent({ key: 'x', seed: { remote: { peer: PEER, agent: '' } } }), null);
});

test('the shell opens where the agent is working now, in its group', async (t) => {
  const { shellTarget } = await load(t);
  const snapshot = { groups: [{ name: 'other', members: [] }, { name: 'tclaude', members: [member] }] };
  assert.deepEqual(shellTarget(snapshot, AGENT), { agent: AGENT, group: 'tclaude', dir: '/repo-wt' });
  assert.deepEqual(shellTarget(snapshot, 'conv-1'), { agent: AGENT, group: 'tclaude', dir: '/repo-wt' });
  assert.deepEqual(shellTarget({ agents: [{ ...member, current_dir: '' }] }, AGENT), { agent: AGENT, group: '', dir: '/repo' });
});

test('nothing is offered without a directory, for a plain shell, or for an unknown agent', async (t) => {
  const { shellTarget } = await load(t);
  assert.equal(shellTarget({ agents: [{ agent_id: AGENT, state: {} }] }, AGENT), null);
  assert.equal(shellTarget({ agents: [{ ...member, state: { harness: 'shell' } }] }, AGENT), null);
  assert.equal(shellTarget({ agents: [member] }, 'agt_other'), null);
  assert.equal(shellTarget(null, AGENT), null);
});

test('a peer lets this in only under sessions.shell, in the group that grants it', async (t) => {
  const { shellTarget } = await load(t);
  const groups = [{ name: 'closed', members: [member] }, { name: 'open', members: [member] }];
  assert.equal(shellTarget({ groups, peer_view: { level: 'restricted', groups: { closed: ['sessions.attach'] } } }, AGENT), null);
  assert.equal(shellTarget({ groups, peer_view: { level: 'restricted', groups: { open: ['sessions.shell'] } } }, AGENT).group, 'open');
  assert.equal(shellTarget({ groups, peer_view: { level: 'unrestricted' } }, AGENT).group, 'closed');
});

test('the button names the directory and the sandbox, and opens the local shell', async (t) => {
  const { harness, ShellHere } = await load(t);
  const { h } = harness.preact;
  const opened = [];
  const snapshot = { groups: [{ name: 'tclaude', members: [member] }] };
  const mounted = await harness.mount(h(ShellHere, { agent: AGENT, name: 'alpha', snapshot, openLocal: async (...args) => { opened.push(args); return {}; } }));
  const button = mounted.container.querySelector('.companion-shell');
  assert.match(button.title, /\/repo-wt/);
  assert.match(button.title, /outside alpha's sandbox/);
  assert.match(button.title, /can write to this directory/);
  await harness.act(() => button.click());
  assert.deepEqual(opened, [[AGENT, 'shell · alpha', 'worktree', { group: 'tclaude' }]]);
});

test('a terminal that cannot open says so', async (t) => {
  const { harness, ShellHere } = await load(t);
  const { h } = harness.preact;
  const notices = [];
  const mounted = await harness.mount(h(ShellHere, { agent: AGENT, snapshot: { agents: [member] }, notify: (text) => notices.push(text), openLocal: async () => null }));
  await harness.act(async () => { mounted.container.querySelector('.companion-shell').click(); await Promise.resolve(); });
  assert.equal(notices.length, 1);
});

test('a peer agent reads the peer snapshot, and stays hidden when it is refused', async (t) => {
  const { harness, ShellHere } = await load(t);
  const { h } = harness.preact;
  const realFetch = globalThis.fetch;
  const asked = [];
  let answer = { ok: true, json: async () => ({ groups: [{ name: 'open', members: [member] }], peer_view: { level: 'unrestricted' } }) };
  globalThis.fetch = async (url) => { asked.push(url); return answer; };
  t.after(() => { globalThis.fetch = realFetch; });
  const opened = [];
  const props = { agent: AGENT, node: PEER, peerLabel: 'nas', name: 'alpha', snapshot: {}, openPeer: async (options) => { opened.push(options); } };
  const settle = async () => { for (let i = 0; i < 5; i += 1) await harness.act(() => Promise.resolve()); };
  const mounted = await harness.mount(h(ShellHere, props));
  await settle();
  assert.deepEqual(asked, [`/api/peer/${PEER}/snapshot`]);
  const button = mounted.container.querySelector('.companion-shell');
  assert.match(button.title, /on nas in \/repo-wt/);
  await harness.act(() => button.click());
  assert.equal(opened[0].instance, PEER);
  assert.equal(opened[0].group, 'open');
  assert.equal(opened[0].shell, 'worktree');
  answer = { ok: false };
  const refused = await harness.mount(h(ShellHere, { ...props, node: 'inst_fedcba9876543210' }));
  await settle();
  assert.equal(refused.container.querySelector('.companion-shell'), null);
});
