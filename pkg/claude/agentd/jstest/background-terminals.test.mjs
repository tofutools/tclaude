import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const ROWS = [
  { kind: 'agent', tmux_session: 'tmux-a', key: 'window:agt_bg', label: 'old-name', agent_id: 'agt_bg', conv_id: 'conv-bg', last_activity: 1000 },
  { kind: 'shell', tmux_session: 'tclaude-term-x-current', key: 'term:agt_bg:current:', label: 'old-name', agent_id: 'agt_bg', conv_id: 'conv-bg', which: 'current' },
  { kind: 'agent', tmux_session: 'tmux-open', key: 'one', label: 'builder-1', agent_id: 'agt_one' },
  { kind: 'other', tmux_session: 'stray', label: 'stray' },
];

async function mountTabs(t) {
  const harness = await createPreactHarness(t);
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const badgeHost = harness.document.body.appendChild(harness.document.createElement('div'));
  const [{ createTerminalShellState }, { createTerminalShellActions }, { TerminalTabs }, background, { registerTerminalShellController }] = await Promise.all([
    harness.importDashboardModule('js/terminal-shell-state.js'),
    harness.importDashboardModule('js/terminal-shell-actions.js'),
    harness.importDashboardModule('js/terminal-shell-island.js'),
    harness.importDashboardModule('js/background-terminals.js'),
    harness.importDashboardModule('js/terminals-tab.js'),
  ]);
  const state = createTerminalShellState({ persistPresentation: false });
  const actions = createTerminalShellActions({
    state, fetchImpl: async () => ({ ok: true }), windowRef: harness.window, documentRef: harness.document,
  });
  const unregister = registerTerminalShellController(actions);
  const snapshot = harness.signals.signal({ agents: [{ agent_id: 'agt_bg', conv_id: 'conv-bg', title: 'renamed-worker', online: true, state: { status: 'idle' } }] });
  const widgetFactory = (options) => ({
    connect() { options.onStatus('connected'); return Promise.resolve(true); },
    copy() { return Promise.resolve(); }, fit() {}, focus() {}, setActive() {}, status() { return 'connected'; }, dispose() {},
  });
  const mounted = await harness.mount(harness.html`<${TerminalTabs} state=${state} actions=${actions} widgetFactory=${widgetFactory} snapshot=${snapshot} />`, host);
  t.after(async () => {
    await mounted.unmount();
    unregister();
    actions.dispose();
    background.backgroundTerminals.value = [];
  });
  return { harness, host, badgeHost, state, actions, background };
}

test('background terminals keep the tab, show a neutral counter and attach from the popover', async (t) => {
  const s = await mountTabs(t);
  const { document } = s.harness;
  await s.harness.act(async () => {
    s.actions.openPane({ ws: '/one', key: 'one', label: 'builder-1', agent: 'agt_one' });
    await Promise.resolve();
  });
  await s.harness.act(() => { s.background.backgroundTerminals.value = ROWS; });

  const trigger = s.host.querySelector('.mux-bg-trigger');
  assert.ok(trigger, 'the counter renders beside the strip');
  assert.ok(trigger.closest('.mux-bg-slot'), 'outside the scrolling tab strip');
  assert.equal(trigger.querySelector('.mux-bg-count').textContent, '3', 'a row whose pane is already open is not counted');
  assert.equal(trigger.textContent.trim(), '3', 'icon and count only, no label');

  await s.harness.act(() => trigger.click());
  const pop = s.host.querySelector('.mux-bg-pop');
  assert.ok(pop, 'a click pins the breakdown open');
  const names = [...pop.querySelectorAll('.mux-bg-name')].map((node) => node.textContent);
  assert.deepEqual(names, ['renamed-worker', 'shell · renamed-worker', 'stray'], 'live roster titles, grouped by kind');
  assert.equal(pop.querySelectorAll('.mux-bg-attach').length, 2, 'an unrecognised session cannot be attached');
  assert.equal(pop.querySelectorAll('button.mux-bg-name').length, 2, 'attachable titles are clickable; the stray is not');

  await s.harness.act(async () => {
    pop.querySelector('.mux-bg-attach').click();
    await new Promise((resolve) => setTimeout(resolve, 0));
  });
  assert.ok(s.state.panes.value.some((pane) => pane.key === 'window:agt_bg'), 'attach opens the agent as a normal tab');
  assert.equal(s.host.querySelector('.mux-bg-count').textContent, '2', 'the attached row leaves the count at once');

  await s.harness.act(async () => {
    s.host.querySelector('.mux-bg-pop button.mux-bg-name').click();
    await new Promise((resolve) => setTimeout(resolve, 0));
  });
  assert.ok(s.state.panes.value.some((pane) => pane.key === 'term:agt_bg:current:'), 'clicking a title attaches it too');
  assert.equal(s.host.querySelector('.mux-bg-count').textContent, '1');

  await s.harness.act(() => s.harness.fireEvent(document, 'keydown', { key: 'Escape' }));
  assert.equal(s.host.querySelector('.mux-bg-pop'), null, 'Escape closes the popover');

  await s.harness.act(async () => {
    await s.actions.closeAllPanes();
    s.background.backgroundTerminals.value = ROWS.filter((row) => row.kind === 'other');
  });
  assert.equal(document.body.classList.contains('hide-terminals'), false, 'the Terminals tab never auto-hides');
  assert.match(s.host.querySelector('.mux-bg-empty').textContent, /1 tclaude session is still running/);

  await s.harness.act(() => { s.background.backgroundTerminals.value = []; });
  assert.equal(document.body.classList.contains('hide-terminals'), false, 'not even with nothing open or in the background');
  assert.match(s.host.querySelector('.mux-bg-empty').textContent, /No terminals open/);
});

test('background terminal helpers', async (t) => {
  const harness = await createPreactHarness(t);
  const { idleText, visibleBackgroundTerminals, startBackgroundTerminalsPoll, backgroundTerminals } = await harness.importDashboardModule('js/background-terminals.js');
  assert.deepEqual(idleText(0), { text: '', stale: false });
  assert.deepEqual(idleText(1000, 1030), { text: 'just now', stale: false });
  assert.deepEqual(idleText(1000, 1000 + 12 * 60), { text: 'idle 12m', stale: false });
  assert.deepEqual(idleText(1000, 1000 + 3 * 3600), { text: 'idle 3h', stale: true });
  assert.deepEqual(idleText(1000, 1000 + 2 * 86400), { text: 'idle 2d', stale: true });
  assert.deepEqual(visibleBackgroundTerminals(ROWS, [{ key: 'one' }]).map((row) => row.tmux_session),
    ['tmux-a', 'tclaude-term-x-current', 'stray']);

  const requests = [];
  const stop = startBackgroundTerminalsPoll({
    intervalMs: 60_000,
    doc: harness.document,
    fetchImpl: async (path) => { requests.push(path); return { ok: true, json: async () => ({ terminals: ROWS.slice(0, 1) }) }; },
  });
  t.after(() => { stop(); backgroundTerminals.value = []; });
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.deepEqual(requests, ['/api/background-terminals']);
  assert.equal(backgroundTerminals.value.length, 1);
});

const BOARD_ROW = {
  kind: 'board', tmux_session: '', key: 'board:plan', board: 'plan', label: 'Release plan', killable: true, last_activity: 1000,
  agents: [{ agent_id: 'agt_bg', title: 'old-name' }, { agent_id: 'agt_gone', title: 'stored-name' }],
};

test('a board with a session lists under Boards, opens as a board tab and leaves the menu while open', async (t) => {
  const s = await mountTabs(t);
  await s.harness.act(() => { s.background.backgroundTerminals.value = [BOARD_ROW]; });
  await s.harness.act(() => s.host.querySelector('.mux-bg-trigger').click());
  const pop = s.host.querySelector('.mux-bg-pop');
  assert.deepEqual([...pop.querySelectorAll('.mux-bg-sec-head')].map((node) => node.textContent), ['Boards · 1']);
  assert.equal(pop.querySelector('.mux-bg-name').textContent, 'Release plan · renamed-worker, stored-name',
    'the title and who is on it, by live roster title where there is one');
  assert.equal(pop.querySelector('.mux-bg-attach').textContent, 'Open');
  assert.equal(pop.querySelector('.mux-bg-kill').textContent.trim(), 'End');

  await s.harness.act(async () => {
    pop.querySelector('.mux-bg-attach').click();
    await new Promise((resolve) => setTimeout(resolve, 0));
  });
  const pane = s.state.panes.value.find((candidate) => candidate.key === 'board:plan');
  assert.ok(pane, 'the row opens under the board\'s pane key');
  assert.equal(pane.seed.board, 'plan');
  assert.equal(pane.seed.label, 'Release plan', 'the tab is named after the board alone');
  assert.equal(s.host.querySelector('.mux-bg-trigger'), null, 'the row hides while the pane is open here');
  await s.harness.act(async () => { await s.actions.closeAllPanes(); });
  assert.ok(s.host.querySelector('.mux-bg-trigger'), 'closing the tab does not end the session');
});

test('a board row hides while its board floats, and is left out of a peer\'s page', async (t) => {
  const harness = await createPreactHarness(t);
  const { visibleBackgroundTerminals, localBackgroundRows } = await harness.importDashboardModule('js/background-terminals.js');
  assert.equal(visibleBackgroundTerminals([BOARD_ROW], [], null).length, 1);
  assert.equal(visibleBackgroundTerminals([BOARD_ROW], [], { id: 'other' }).length, 1);
  assert.equal(visibleBackgroundTerminals([BOARD_ROW], [], { id: 'plan' }).length, 0);
  assert.deepEqual(localBackgroundRows([ROWS[0], BOARD_ROW], ''), [ROWS[0], BOARD_ROW]);
  assert.deepEqual(localBackgroundRows([ROWS[0], BOARD_ROW], 'peer-1'), [ROWS[0]],
    'this node\'s board routes are out of reach from a peer\'s page');
});

test('ending a board row asks for the board\'s sessions and nothing else', async (t) => {
  const harness = await createPreactHarness(t);
  const { killBackgroundTerminal } = await harness.importDashboardModule('js/background-terminals.js');
  const calls = [];
  const fetchImpl = async (path, options) => { calls.push(`${options.method} ${path}`); return { ok: true }; };
  assert.equal(await killBackgroundTerminal(BOARD_ROW, { fetchImpl }), '');
  assert.deepEqual(calls, ['DELETE /api/boards/plan/sessions']);
  const refused = async () => ({ ok: false, status: 404, json: async () => ({ error: 'board not found' }) });
  assert.equal(await killBackgroundTerminal(BOARD_ROW, { fetchImpl: refused }), 'board not found');
});

test('attach all opens one terminal at a time, each taking focus as it opens', async (t) => {
  const s = await mountTabs(t);
  const order = [];
  let release = null;
  const waitForSettled = (key) => new Promise((resolve) => {
    order.push(`settle:${key}`);
    release = resolve;
  });
  const rows = ROWS.filter((row) => row.key && row.key !== 'one');
  let done = false;
  const run = s.background.attachAllBackgroundTerminals(rows, null, { waitForSettled }).then((count) => { done = count; });
  const tick = () => s.harness.act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)); });

  await tick();
  assert.deepEqual(s.state.panes.value.map((pane) => pane.key), ['window:agt_bg'], 'only the first is opened');
  assert.equal(s.state.activeKey.value, 'window:agt_bg');
  assert.deepEqual(order, ['settle:window:agt_bg']);
  assert.equal(await s.background.attachAllBackgroundTerminals(rows, null, { waitForSettled }), 0,
    'a second click while one runs is ignored');

  release(true);
  await tick();
  assert.deepEqual(s.state.panes.value.map((pane) => pane.key), ['window:agt_bg', 'term:agt_bg:current:'],
    'the next opens once the previous has settled');
  assert.equal(s.state.activeKey.value, 'term:agt_bg:current:',
    'each pane takes focus so it attaches at the real grid, not a hidden 80x24');

  release(false);
  await run;
  assert.equal(done, 2);
});

test('a fused or peer view also lists the shown peers\' unattached agents, opened as remote terminals', async (t) => {
  const harness = await createPreactHarness(t);
  const bg = await harness.importDashboardModule('js/background-terminals.js');
  const forge = { id: 'inst_forge', name: 'forge', color: '#a371f7', online: true, local: false };
  const lab = { id: 'inst_lab', name: 'lab', color: '#3fb950', online: true, local: false };
  const gone = { id: 'inst_gone', name: 'gone', color: '#888', online: false, local: false };
  const fleet = { self: { id: 'inst_self', name: 'desk', local: true, online: true }, peers: [forge, lab, gone] };
  assert.deepEqual(bg.backgroundPeers({ fleet, fused: null }), [], 'one node, this one: no peers');
  assert.deepEqual(bg.backgroundPeers({ fleet, fused: null }, 'inst_lab').map((p) => p.id), ['inst_lab'], 'a peer view reads the peer it shows');
  assert.deepEqual(bg.backgroundPeers({ fleet, fused: 'all' }).map((p) => p.id), ['inst_forge', 'inst_lab'], 'fused: the ticked online peers');

  const peerRows = bg.peerBackgroundRows([
    { kind: 'agent', tmux_session: 'a', label: 'scout', agent_id: 'agt_scout', killable: true },
    { kind: 'shell', tmux_session: 'b', agent_id: 'agt_scout', which: 'current', killable: true },
    { kind: 'group_shell', tmux_session: 'g', group: 'ops', killable: true },
    { kind: 'session', tmux_session: 'c', conv_id: 'conv-x' },
  ], forge);
  assert.deepEqual(peerRows.map((row) => [row.key, row.killable, row.node.name, row.kind]), [
    ['peer:inst_forge:agt_scout', false, 'forge', 'agent'],
    ['', true, 'forge', 'shell'],
    ['peer:inst_forge:g', true, 'forge', 'group_shell'],
  ], 'agents open as remote terminals; tclaude shells list too, killable there, attachable when they name their group; other sessions stay out');
  assert.match(peerRows[1].attachLock, /predates group shells/);
  assert.equal(bg.hasPeerBackgroundAgent(peerRows), true, "a peer's background agent keeps its status catalog polled");
  assert.equal(bg.hasPeerBackgroundAgent([...ROWS, ...peerRows.filter((row) => row.kind !== 'agent')]), false,
    "local rows and peer shells take no peer status");
  assert.deepEqual(peerRows[2].shell, { group: 'ops', shell: 'group' });
  const [dirShell] = bg.peerBackgroundRows([{ kind: 'shell', tmux_session: 'd', agent_id: 'agt_scout', which: 'worktree', group: 'ops', label: 'scout' }], forge);
  const shells = [];
  bg.attachBackgroundTerminal(dirShell, null, { openShell: (o) => { shells.push(o); return Promise.resolve({ key: 'k' }); } });
  assert.deepEqual(shells.map(({ instance, group, shell, agent, peerLabel }) => ({ instance, group, shell, agent, peerLabel })),
    [{ instance: 'inst_forge', group: 'ops', shell: 'worktree', agent: 'agt_scout', peerLabel: 'forge' }], "a peer's directory shell opens over the federation terminal stream");
  const kills = [];
  assert.equal(await bg.killBackgroundTerminal(peerRows[2], { fetchImpl: async (path, init) => { kills.push([path, JSON.parse(init.body)]); return { ok: true }; } }), '');
  assert.equal(await bg.killBackgroundTerminal({ tmux_session: 'mine' }, { fetchImpl: async (path, init) => { kills.push([path, JSON.parse(init.body)]); return { ok: true }; } }), '');
  assert.deepEqual(kills, [
    ['/api/peer/inst_forge/background-terminals/kill', { tmux_session: 'g' }],
    ['/api/background-terminals/kill', { tmux_session: 'mine' }],
  ], "a peer's shell ends through the peer proxy, this node's here");
  assert.deepEqual(bg.visibleBackgroundTerminals(peerRows, [{ key: '/api/federation/terminal?x', seed: { remote: { peer: 'inst_forge', agent: 'agt_scout' } } }]).map((row) => row.kind), ['shell', 'group_shell'],
    'an agent already open as a remote pane is not listed, whatever its mode');

  const requests = [];
  const stop = bg.startBackgroundTerminalsPoll({
    intervalMs: 60_000,
    doc: harness.document,
    peers: () => [forge, lab],
    fetchImpl: async (path) => {
      requests.push(path);
      if (path.includes('inst_lab')) return { ok: false, status: 403 };
      if (path.includes('inst_forge')) return { ok: true, json: async () => ({ terminals: [{ kind: 'agent', tmux_session: 'a', label: 'scout', agent_id: 'agt_scout' }, { kind: 'group_shell', tmux_session: 'g', group: 'ops', killable: true }] }) };
      return { ok: true, json: async () => ({ terminals: ROWS.slice(0, 1) }) };
    },
  });
  t.after(() => { stop(); bg.backgroundTerminals.value = []; });
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.deepEqual(requests, ['/api/background-terminals', '/api/peer/inst_forge/background-terminals', '/api/peer/inst_lab/background-terminals']);
  assert.deepEqual(bg.backgroundTerminals.value.map((row) => row.key || row.tmux_session), ['window:agt_bg', 'peer:inst_forge:agt_scout', 'peer:inst_forge:g'],
    'this node\'s rows, then the peer\'s; a peer that refuses adds none');
  requests.length = 0;
  bg.refreshBackgroundTerminals();
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.deepEqual(requests, ['/api/background-terminals', '/api/peer/inst_forge/background-terminals'],
    'a refusing peer is not asked again on the next read');

  const opened = [];
  await bg.attachBackgroundTerminal(bg.backgroundTerminals.value[1], null, { openRemote: async (args) => { opened.push(args); return { key: 'remote' }; } });
  assert.deepEqual(opened.map(({ instance, agent, peerLabel, label }) => ({ instance, agent, peerLabel, label })),
    [{ instance: 'inst_forge', agent: 'agt_scout', peerLabel: 'forge', label: 'scout' }]);

  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const mounted = await harness.mount(harness.html`<${bg.BackgroundTerminalsChip} rows=${bg.backgroundTerminals.value} />`, host);
  await harness.act(() => host.querySelector('.mux-bg-trigger').click());
  const heads = [...host.querySelectorAll('.mux-bg-sec-head')].map((head) => head.textContent);
  assert.deepEqual(heads, ['Agents · 1', 'forgeAgents · 1', 'forgeGroup shells · 1'], "the peer's rows sit under its node pill, by kind");
  const shellRow = [...host.querySelectorAll('.mux-bg-sec')].at(-1).querySelector('.mux-bg-row');
  assert.equal(shellRow.querySelector('.mux-bg-attach').disabled, false, "a peer's group shell attaches from here");
  assert.equal(shellRow.querySelector('.lock-why'), null);
  assert.ok(shellRow.querySelector('.mux-bg-kill'), 'it can be ended from here');
  await mounted.unmount();
});
