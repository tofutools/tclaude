// Tests for boards as tabs of the terminal shell (tcl-0ghg18): a board opening
// beside terminals, its tab coming back after a reload, the /boards routes, a
// detached board, and features.collab off. The shell is the real mounted
// feature; only xterm and the daemon are faked.

import test from 'node:test';
import assert from 'node:assert/strict';
import { assertAbsent } from './assertions.mjs';
import { createPreactHarness } from './preact-harness.mjs';

const wait = (ms = 0) => new Promise((r) => setTimeout(r, ms));
const COLLAB_ON = { collab_enabled: true, agents: [], groups: [] };
const BOARD_LIST = [{ id: 'plan', title: 'Release plan' }, { id: 'map', title: 'Team map' }];

function installHosts(harness) {
  const doc = harness.document;
  const nav = doc.body.appendChild(doc.createElement('nav'));
  const main = doc.body.appendChild(doc.createElement('main'));
  for (const name of ['groups', 'terminals']) {
    const button = nav.appendChild(doc.createElement('a'));
    button.dataset.tab = name;
    button.addEventListener('click', (event) => {
      event.preventDefault();
      for (const item of nav.querySelectorAll('[data-tab]')) item.classList.toggle('active', item === button);
      for (const section of main.querySelectorAll('section')) section.classList.toggle('active', section.id === `tab-${name}`);
    });
    main.appendChild(doc.createElement('section')).id = `tab-${name}`;
  }
  const host = doc.getElementById('tab-terminals').appendChild(doc.createElement('div'));
  host.id = 'terminals-root';
  const badgeHost = nav.appendChild(doc.createElement('span'));
  badgeHost.id = 'terminals-badge-root';
  return { host, badgeHost };
}

function fakeWidgets(harness) {
  const widgets = [];
  const factory = (options) => {
    const child = harness.document.createElement('textarea');
    options.host.append(child);
    const widget = {
      options, connect: () => Promise.resolve(true), copy: () => Promise.resolve(), fit() {}, focus() {},
      setActive() {}, status: () => 'connected', dispose() {},
    };
    widgets.push(widget);
    return widget;
  };
  return { factory, widgets };
}

// A daemon holding the given boards, recording what the page asked it.
function fakeDaemon(t, boards = BOARD_LIST) {
  const requests = [];
  const previous = globalThis.fetch;
  globalThis.fetch = async (url, init = {}) => {
    const path = String(url);
    requests.push(`${init.method || 'GET'} ${path}`);
    const id = /^\/api\/boards\/([^/?]+)/.exec(path)?.[1];
    const board = boards.find((b) => b.id === id);
    let body = {};
    if (path === '/api/boards') body = { boards };
    else if (board) body = { ...board, version: 1, participants: [], elements: [] };
    return { ok: true, status: 200, json: async () => body };
  };
  t.after(() => { globalThis.fetch = previous; });
  return requests;
}

async function mountShell(t, snapshotValue) {
  const harness = await createPreactHarness(t);
  const hosts = installHosts(harness);
  const fake = fakeWidgets(harness);
  const [{ mountTerminalsFeature }, controller, { dashboardState }, { dashPrefs }, { terminalShellState }, tabs] = await Promise.all([
    harness.importDashboardModule('js/preact-loader.js'),
    harness.importDashboardModule('js/terminals-tab.js'),
    harness.importDashboardModule('js/snapshot-store.js'),
    harness.importDashboardModule('js/prefs.js'),
    harness.importDashboardModule('js/terminal-shell-state.js'),
    harness.importDashboardModule('js/terminal-board-tabs.js'),
  ]);
  const setSnapshot = (value) => dashboardState.publishLocalEdit(value);
  setSnapshot(snapshotValue);
  dashPrefs.removeItem(tabs.BOARD_TABS_PREF);
  return { t, harness, ...hosts, fake, mountTerminalsFeature, controller, dashboardState, dashPrefs, state: terminalShellState, tabs, setSnapshot };
}

async function start(env) {
  const cleanup = await env.mountTerminalsFeature({ widgetFactory: env.fake.factory, fetchImpl: async () => ({ ok: true }) });
  await env.harness.act(() => wait());
  // A failed assertion must not leave a board's poll keeping the process alive.
  let done = false;
  const once = () => {
    if (done) return;
    done = true;
    cleanup();
  };
  env.t.after(once);
  return once;
}

const tabLabels = (host) => [...host.querySelectorAll('.mux-tab .mux-tab-label')].map((node) => node.textContent);

test('a board opens as a tab beside a terminal, with its editor and no terminal chrome', async (t) => {
  const requests = fakeDaemon(t);
  const env = await mountShell(t, COLLAB_ON);
  const cleanup = await start(env);
  const { harness, host, controller, fake, badgeHost } = env;
  await harness.act(async () => {
    await controller.openTerminalPane({ ws: '/one', key: 'one', label: 'one', agent: 'agt_one' });
    await controller.openBoardPane('plan', 'Release plan');
    await wait();
  });
  assert.deepEqual(tabLabels(host), ['one', 'Release plan']);
  assert.equal(fake.widgets.length, 1, 'a board pane mounts no terminal widget');
  const pane = host.querySelector('.mux-pane-board.active');
  assert.ok(pane.querySelector('svg.board-canvas'), 'the active tab shows the board editor');
  assertAbsent(pane.querySelector('.mux-pane-header'), 'a board pane has no terminal header');
  assert.ok(requests.includes('GET /api/boards/plan'));
  assert.equal(badgeHost.querySelector('#terminals-badge').textContent, '1', 'the nav badge counts terminals');
  assert.equal(host.querySelector('.mux-tab[data-agent-status="board"] .mux-tab-close').title, 'Close this board');

  // Switching back leaves the editor mounted but idle, like a background terminal.
  await harness.act(async () => { env.state.activatePane('one'); await wait(); });
  assert.ok(host.querySelector('.mux-pane-board:not(.active) svg.board-canvas'), 'the editor survives a tab switch');
  assert.equal(await controller.waitForTerminalSettled('board:plan'), true, 'a board has no attach to wait for');
  // Everything that reorders or closes tabs treats it as any other tab.
  await harness.act(async () => { env.state.movePaneByOffset('board:plan', -1); await wait(); });
  assert.deepEqual(tabLabels(host), ['Release plan', 'one']);
  const group = env.state.createGroup({ keys: ['board:plan', 'one'] });
  assert.ok(group, 'a board joins a tab group');
  await harness.act(async () => {
    harness.fireEvent(host.querySelector('.mux-tab[data-agent-status="board"] .mux-tab-close'), 'click');
    await wait();
  });
  assert.deepEqual(tabLabels(host), ['one']);
  assert.ok(!requests.some((r) => r.includes('/api/hide/')), 'closing a board detaches nothing');
  cleanup();
});

test('a crafted seed cannot give a board a socket or a colliding key', async (t) => {
  const harness = await createPreactHarness(t);
  const { normalizeSeed } = await harness.importDashboardModule('js/terminals-core.js');
  assert.equal(normalizeSeed({ board: '../etc', ws: '/x' }), null);
  assert.equal(normalizeSeed({ board: '' }), null);
  const seed = normalizeSeed({ board: 'plan', ws: 'wss://evil', key: 'window:agt_x' });
  assert.equal(seed.ws, undefined);
  assert.equal(seed.key, 'board:plan');
});

test('the + menu lists boards and opens one in a tab', async (t) => {
  const requests = fakeDaemon(t);
  const env = await mountShell(t, COLLAB_ON);
  const cleanup = await start(env);
  const { harness, host } = env;
  await harness.act(async () => { harness.fireEvent(host.querySelector('.mux-board-add'), 'click'); await wait(); });
  await harness.act(() => wait());
  const cards = host.querySelectorAll('.mux-board-menu .board-card');
  assert.equal(cards.length, 2);
  assert.ok(requests.includes('GET /api/boards'));
  await harness.act(async () => { harness.fireEvent(cards[1], 'click'); await wait(); });
  assert.deepEqual(tabLabels(host), ['Team map'], 'the tab carries the board title');
  assertAbsent(host.querySelector('.mux-board-menu'), 'picking a board closes the menu');
  assert.ok(harness.document.getElementById('tab-terminals').classList.contains('active'));
  cleanup();
});

test('board tabs come back after a reload, minus boards deleted since', async (t) => {
  fakeDaemon(t);
  const env = await mountShell(t, COLLAB_ON);
  const { harness, host, dashPrefs, tabs } = env;
  dashPrefs.setItem(tabs.BOARD_TABS_PREF, JSON.stringify([
    { id: 'plan', label: 'Old title' }, { id: 'map', label: 'Team map' }, { id: 'gone', label: 'Deleted' }, { id: 'BAD ID', label: 'x' },
  ]));
  const announced = [];
  harness.document.addEventListener('tclaude:navigated', (event) => announced.push(event.detail));
  const groups = harness.document.querySelector('nav [data-tab="groups"]');
  harness.fireEvent(groups, 'click');
  const cleanup = await start(env);
  await harness.act(() => wait());
  assert.deepEqual(tabLabels(host), ['Release plan', 'Team map'], 'the daemon has the current title, and only live boards return');
  assert.ok(harness.document.getElementById('tab-groups').classList.contains('active'), 'restoring does not pull the operator here');
  assert.deepEqual(announced, [], 'a restore is no navigation');
  assert.equal(host.querySelectorAll('svg.board-canvas').length, 1, 'a tab behind the active one loads when it is looked at');
  await harness.act(async () => { env.state.activatePane('board:map'); await wait(); });
  assert.equal(host.querySelectorAll('svg.board-canvas').length, 2);
  assert.deepEqual(JSON.parse(dashPrefs.getItem(tabs.BOARD_TABS_PREF)).map((tab) => tab.id), ['plan', 'map'], 'the deleted board is forgotten');
  cleanup();
  assert.equal(JSON.parse(dashPrefs.getItem(tabs.BOARD_TABS_PREF)).length, 2, 'unmounting the page forgets nothing');
});

test('a tab title follows the board', async (t) => {
  fakeDaemon(t, [{ id: 'plan', title: 'Renamed plan' }]);
  const env = await mountShell(t, COLLAB_ON);
  const cleanup = await start(env);
  await env.harness.act(async () => { await env.controller.openBoardPane('plan', 'Old'); await wait(); });
  await env.harness.act(() => wait());
  assert.deepEqual(tabLabels(env.host), ['Renamed plan']);
  cleanup();
});

test('/boards/<id> opens that tab; bare /boards offers the list and heals to what shows', async (t) => {
  fakeDaemon(t);
  const env = await mountShell(t, COLLAB_ON);
  const cleanup = await start(env);
  const { harness, host, state } = env;
  const announced = [];
  harness.document.addEventListener('tclaude:navigated', (event) => announced.push(event.detail));
  const restore = (location) => harness.act(async () => {
    harness.document.dispatchEvent(new harness.window.CustomEvent('tclaude:restore-location', { detail: { location } }));
    await wait();
  });
  await restore({ tab: 'boards', selection: 'plan' });
  assert.equal(state.activeKey.value, 'board:plan');
  assert.deepEqual(state.location.value, { tab: 'boards', selection: 'plan' }, 'the URL keeps naming the board');
  assert.deepEqual(announced, [], 'a restore does not announce itself back');

  await restore({ tab: 'boards' });
  assert.ok(host.querySelector('.mux-board-menu'), 'a bare /boards opens the list');
  assert.deepEqual(announced.at(-1), { location: { tab: 'boards', selection: 'plan' }, correction: true });

  await restore({ tab: 'boards', selection: 'Not A Board' });
  assert.equal(state.panes.value.length, 1, 'an invalid id opens nothing');

  // A board tab the operator switches to is a navigation, under its own route.
  await harness.act(async () => { await env.controller.openBoardPane('map'); await wait(); });
  harness.fireEvent(harness.document.querySelector('nav [data-tab="terminals"]'), 'click');
  await harness.act(async () => { state.activatePane('board:plan'); await wait(); });
  assert.deepEqual(announced.at(-1), { location: { tab: 'boards', selection: 'plan' }, correction: false });
  cleanup();
});

test('the router keeps the /boards routes and shows them in the Terminals section', async (t) => {
  const harness = await createPreactHarness(t);
  const core = await harness.importDashboardModule('js/nav-history-core.js');
  assert.deepEqual(core.fromPath('/boards/plan', ''), { tab: 'boards', selection: 'plan' });
  assert.equal(core.toPath({ tab: 'boards', selection: 'plan' }), '/boards/plan');
});

test('with features.collab off there is no board menu, nothing restored, and no board tab stays', async (t) => {
  const requests = fakeDaemon(t);
  const env = await mountShell(t, { collab_enabled: false, agents: [] });
  const { harness, host, dashPrefs, tabs, setSnapshot } = env;
  dashPrefs.setItem(tabs.BOARD_TABS_PREF, JSON.stringify([{ id: 'plan', label: 'Release plan' }]));
  const cleanup = await start(env);
  assertAbsent(host.querySelector('.mux-board-add'), 'no board menu');
  assert.deepEqual(tabLabels(host), [], 'no board panes restored');
  assert.deepEqual(requests.filter((r) => r.includes('/api/boards')), [], 'the daemon is not asked for what it refuses');

  // A handoff can still carry a board seed in: it does not stay.
  await harness.act(async () => { await env.controller.openBoardPane('map', 'Team map'); await wait(); });
  assertAbsent(host.querySelector('svg.board-canvas'), 'the editor stays out');
  await harness.act(async () => { setSnapshot({ collab_enabled: false, agents: [], generated_at: 'x' }); await wait(); });
  assert.deepEqual(tabLabels(host), []);
  assert.equal(JSON.parse(dashPrefs.getItem(tabs.BOARD_TABS_PREF)).length, 1, 'the remembered tabs wait for the flag');

  // Turning it on needs no reload; turning it off takes the tabs away again.
  await harness.act(async () => { setSnapshot(COLLAB_ON); await wait(); });
  assert.deepEqual(tabLabels(host), ['Release plan']);
  assert.ok(host.querySelector('.mux-board-add'));
  await harness.act(async () => { setSnapshot({ collab_enabled: false }); await wait(); });
  assert.deepEqual(tabLabels(host), []);
  assert.equal(JSON.parse(dashPrefs.getItem(tabs.BOARD_TABS_PREF)).length, 1);
  cleanup();
});

test('detaching a board carries it to the solo page, which renders it and sends it back', async (t) => {
  fakeDaemon(t);
  const harness = await createPreactHarness(t);
  const { createTerminalShellState } = await harness.importDashboardModule('js/terminal-shell-state.js');
  const { createTerminalShellActions } = await harness.importDashboardModule('js/terminal-shell-actions.js');
  const { decodeTerminalOpenHash, createStandaloneTerminalsPage } = await harness.importDashboardModule('js/terminal-standalone.js');
  const state = createTerminalShellState({ persistPresentation: false });
  let opened = null;
  const target = { location: { replace(url) { opened = url; } } };
  const windowRef = { open: () => target, screen: {} };
  const actions = createTerminalShellActions({ state, windowRef, documentRef: harness.document, fetchImpl: async () => ({ ok: true }) });
  actions.openPane({ board: 'plan', key: 'board:plan', label: 'Release plan' });
  assert.equal(await actions.popOutPane('board:plan'), true);
  assert.equal(state.panes.value.length, 0, 'the tab left');
  assert.ok(opened.startsWith('/terminals?solo=1#open='));
  const seed = decodeTerminalOpenHash(opened.slice(opened.indexOf('#')));
  assert.equal(seed.board, 'plan');
  assert.equal(seed.ws, undefined);

  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const location = { hash: opened.slice(opened.indexOf('#')), pathname: '/terminals', search: '?solo=1', origin: 'http://x' };
  const requests = [];
  const page = createStandaloneTerminalsPage({
    host, initPrefs: async () => {}, initThemeSync: () => {},
    widgetFactory: () => { throw new Error('a board mounts no widget'); },
    fetchImpl: async (url) => { requests.push(String(url)); return { ok: true, json: async () => COLLAB_ON }; },
    locationRef: location, historyRef: { replaceState() {} },
    mountMessageDialogs: async () => () => {},
  });
  await harness.act(async () => { await page.start(); await wait(); });
  assert.ok(host.querySelector('svg.board-canvas'), 'the solo page renders the board');
  assert.ok([...host.querySelectorAll('button')].some((b) => b.textContent.includes('↩ dashboard')));
  assert.ok(requests.includes('/api/snapshot'), 'it reads the roster, to name the agents on the board');
  page.dispose();
});
