import test from 'node:test'; import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const ROWS = [
  { instance: 'inst_forge7', agent: 'agt_ada1', name: 'ada', peer: 'forge', watch: true, attach: true },
  { instance: 'inst_forge7', agent: 'agt_bob2', name: 'bob', peer: 'forge', watch: true, attach: false },
  { instance: 'inst_forge7', agent: 'agt_cy3x', name: 'cy', peer: 'forge', watch: false, attach: false },
];

test('a remote terminal opens interactive or watch-only as the peer shares it, and says why it cannot', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/remote-terminal.js');
  const fetches = []; const opened = []; const toasts = [];
  const fetchImpl = async (url) => { fetches.push(url); return { ok: true, json: async () => ROWS }; };
  const go = (agent) => m.openRemoteTerminal({ instance: 'inst_forge7', agent, peerLabel: 'forge', fetchImpl, toast: (x, err) => toasts.push([x, err]), open: (o) => { opened.push(o); return o; } });
  m.resetRemoteSessionsCache();
  await go('agt_ada1');
  assert.deepEqual(opened.at(-1), { wsPath: '/api/federation/terminal?peer=inst_forge7&agent=agt_ada1&mode=interactive', label: 'ada @ forge', remote: { peer: 'inst_forge7', peerLabel: 'forge', agent: 'agt_ada1' } });
  await go('agt_bob2');
  assert.match(opened.at(-1).wsPath, /mode=watch$/);
  await go('agt_cy3x');
  assert.equal(opened.length, 2);
  assert.match(toasts.at(-1)[0], /forge does not share this agent's terminal.*sessions\.watch.*sessions\.attach/);
  await go('agt_gone9');
  assert.match(toasts.at(-1)[0], /has not listed this agent's session.*tclaude federation attach agt_gone9@inst_forge7/);
  assert.deepEqual(fetches, ['/api/federation/sessions'], 'the catalog is cached between clicks');
  assert.equal(await go('agt_x;rm -rf'), null);
  assert.equal(m.nativeAttachCommand('agt_x;rm', 'inst_forge7'), '');
  assert.equal(m.remoteTerminalMode({ watch: true }), 'watch');
  assert.equal(m.remoteTerminalMode(null), '');
});

test('a failed catalog read is reported, not opened', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/remote-terminal.js');
  m.resetRemoteSessionsCache();
  const toasts = [];
  const r = await m.openRemoteTerminal({ instance: 'inst_forge7', agent: 'agt_ada1', peerLabel: 'forge', fetchImpl: async () => ({ ok: false, status: 503 }), toast: (x) => toasts.push(x), open: () => assert.fail('opened') });
  assert.equal(r, null);
  assert.match(toasts.at(-1), /Could not read forge's sessions: sessions: HTTP 503/);
});

test('the map\'s terminal picker lists one peer\'s sessions with how each opens, plus the CLI command', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/remote-terminal.js');
  const opened = []; const copied = []; let closed = 0;
  const rows = [...ROWS, { instance: 'inst_lab22', agent: 'agt_zed9', name: 'zed', watch: true }];
  const mounted = await harness.mount(harness.html`<${m.RemoteSessionsDialog} node=${{ id: 'inst_forge7', name: 'forge' }} onClose=${() => { closed++; }}
    fetchImpl=${async () => ({ ok: true, json: async () => rows })} toast=${() => {}} open=${(o) => opened.push(o)} copy=${async (x) => { copied.push(x); }} />`);
  await harness.act(() => new Promise((r) => setTimeout(r, 10)));
  const q = (s) => mounted.container.querySelector(s) || harness.document.querySelector(s);
  const trs = [...harness.document.querySelectorAll('#remote-sessions tr')];
  assert.deepEqual(trs.map((tr) => tr.dataset.agent), ['agt_ada1', 'agt_bob2', 'agt_cy3x'], 'only this peer');
  assert.equal(q('[data-agent="agt_ada1"] [data-open]').dataset.open, 'interactive');
  assert.equal(q('[data-agent="agt_bob2"] [data-open]').dataset.open, 'watch');
  assert.equal(q('[data-agent="agt_cy3x"] [data-open]'), null);
  await harness.act(() => q('[data-agent="agt_cy3x"] [data-copy-cli]').click());
  await harness.act(() => new Promise((r) => setTimeout(r, 0)));
  assert.deepEqual(copied, ['tclaude federation attach agt_cy3x@inst_forge7']);
  await harness.act(() => q('[data-agent="agt_bob2"] [data-open]').click());
  assert.equal(closed, 1);
  assert.equal(opened[0].wsPath, '/api/federation/terminal?peer=inst_forge7&agent=agt_bob2&mode=watch');
  assert.equal(opened[0].label, 'bob @ forge');
  await mounted.unmount();
});

test('by default a remote terminal opens as a Terminals tab pane, not the overlay', async (t) => {
  const harness = await createPreactHarness(t);
  const tab = await harness.importDashboardModule('js/terminals-tab.js');
  const m = await harness.importDashboardModule('js/remote-terminal.js');
  const panes = []; const modals = [];
  const release = tab.registerTerminalShellController({
    openPane: (seed, opts) => { panes.push([seed, opts]); return { key: seed.ws }; },
    openModal: (d) => { modals.push(d); return d; },
  });
  t.after(release);
  const fetchImpl = async () => ({ ok: true, json: async () => ROWS });
  const opened = await m.openRemoteTerminal({ instance: 'inst_forge7', agent: 'agt_ada1', peerLabel: 'forge', fetchImpl, toast: () => {} });
  assert.ok(opened);
  assert.equal(modals.length, 0, 'no overlay terminal');
  assert.equal(panes.length, 1);
  const [seed, opts] = panes[0];
  assert.equal(seed.ws, '/api/federation/terminal?peer=inst_forge7&agent=agt_ada1&mode=interactive');
  assert.equal(seed.label, 'ada @ forge');
  assert.deepEqual({ ...seed.remote }, { peer: 'inst_forge7', peerLabel: 'forge', agent: 'agt_ada1' });
  assert.equal(seed.agent, undefined, 'no local agent actions on a peer agent');
  assert.deepEqual(opts, { reveal: true });
});
