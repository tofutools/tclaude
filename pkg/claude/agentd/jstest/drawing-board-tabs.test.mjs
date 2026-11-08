// Tests for drawing boards as tabs of the terminal shell (tcl-0ghg18): a drawing board opening
// beside terminals, its tab coming back after a reload, the /drawing-boards routes, a
// detached drawing board. The shell is the real mounted
// feature; only xterm and the daemon are faked.

import test from 'node:test';
import assert from 'node:assert/strict';
import { assertAbsent } from './assertions.mjs';
import { createPreactHarness } from './preact-harness.mjs';

const wait = (ms = 0) => new Promise((r) => setTimeout(r, ms));
const COLLAB_ON = { agents: [], groups: [] };
const DRAWING_BOARD_LIST = [{ id: 'plan', title: 'Release plan' }, { id: 'map', title: 'Team map' }];

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

// A daemon holding the given drawing boards, recording what the page asked it.
function fakeDaemon(t, drawingBoards = structuredClone(DRAWING_BOARD_LIST)) {
  const requests = [];
  const previous = globalThis.fetch;
  globalThis.fetch = async (url, init = {}) => {
    const path = String(url);
    requests.push(`${init.method || 'GET'} ${path}`);
    const id = /^\/api\/drawing-boards\/([^/?]+)/.exec(path)?.[1];
    const drawingBoard = drawingBoards.find((b) => b.id === id);
    if (id && !drawingBoard) return { ok: false, status: 404, json: async () => ({}) };
    // A rename takes, as the daemon's does, so a later read has the new title.
    if (drawingBoard && init.method === 'POST' && path.endsWith('/title')) Object.assign(drawingBoard, { title: JSON.parse(init.body).title });
    let body = {};
    if (path === '/api/drawing-boards') body = { drawingBoards };
    else if (drawingBoard) body = { ...drawingBoard, version: 1, participants: [], elements: [] };
    return { ok: true, status: 200, json: async () => body };
  };
  t.after(() => { globalThis.fetch = previous; });
  return requests;
}

async function mountShell(t, snapshotValue) {
  const harness = await createPreactHarness(t);
  const hosts = installHosts(harness);
  const fake = fakeWidgets(harness);
  const [{ mountTerminalsFeature }, controller, { openDrawingBoardPane }, { dashboardState }, { terminalShellState }, closeNav] = await Promise.all([
    harness.importDashboardModule('js/preact-loader.js'),
    harness.importDashboardModule('js/terminals-tab.js'),
    harness.importDashboardModule('js/drawing-board-overlay-state.js'),
    harness.importDashboardModule('js/snapshot-store.js'),
    harness.importDashboardModule('js/terminal-shell-state.js'),
    harness.importDashboardModule('js/terminal-close-nav.js'),
  ]);
  const setSnapshot = (value) => dashboardState.publishLocalEdit(value);
  setSnapshot(snapshotValue);
  closeNav.setStayOnTerminalsAfterLastClose(false);
  return { t, harness, ...hosts, fake, mountTerminalsFeature, controller, openDrawingBoardPane, dashboardState, state: terminalShellState, closeNav, setSnapshot };
}

async function start(env) {
  const cleanup = await env.mountTerminalsFeature({ widgetFactory: env.fake.factory, fetchImpl: async () => ({ ok: true }) });
  await env.harness.act(() => wait());
  // A failed assertion must not leave a drawing board's poll keeping the process alive.
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

test('a drawing board opens as a tab beside a terminal, with its editor and no terminal chrome', async (t) => {
  const requests = fakeDaemon(t);
  const env = await mountShell(t, COLLAB_ON);
  const cleanup = await start(env);
  const { harness, host, controller, openDrawingBoardPane, fake, badgeHost } = env;
  await harness.act(async () => {
    await controller.openTerminalPane({ ws: '/one', key: 'one', label: 'one', agent: 'agt_one' });
    await openDrawingBoardPane('plan', 'Release plan');
    await wait();
  });
  assert.deepEqual(tabLabels(host), ['one', 'Release plan']);
  assert.equal(fake.widgets.length, 1, 'a drawing board pane mounts no terminal widget');
  const pane = host.querySelector('.mux-pane-drawing-board.active');
  assert.ok(pane.querySelector('svg.drawing-board-canvas'), 'the active tab shows the drawing board editor');
  assertAbsent(pane.querySelector('.mux-pane-header'), 'a drawing board pane has no terminal header');
  assert.ok(requests.includes('GET /api/drawing-boards/plan'));
  assert.equal(badgeHost.querySelector('#terminals-badge').textContent, '1', 'the nav badge counts terminals');
  assert.equal(host.querySelector('.mux-tab[data-agent-status="drawing_board"] .mux-tab-close').title, 'Close this drawing board');

  // Switching back leaves the editor mounted but idle, like a background terminal.
  await harness.act(async () => { env.state.activatePane('one'); await wait(); });
  assert.ok(host.querySelector('.mux-pane-drawing-board:not(.active) svg.drawing-board-canvas'), 'the editor survives a tab switch');
  assert.equal(await controller.waitForTerminalSettled('drawing_board:plan'), true, 'a drawing board has no attach to wait for');
  // Everything that reorders or closes tabs treats it as any other tab.
  await harness.act(async () => { env.state.movePaneByOffset('drawing_board:plan', -1); await wait(); });
  assert.deepEqual(tabLabels(host), ['Release plan', 'one']);
  const group = env.state.createGroup({ keys: ['drawing_board:plan', 'one'] });
  assert.ok(group, 'a drawing board joins a tab group');
  await harness.act(async () => {
    harness.fireEvent(host.querySelector('.mux-tab[data-agent-status="drawing_board"] .mux-tab-close'), 'click');
    await wait();
  });
  assert.deepEqual(tabLabels(host), ['one']);
  assert.ok(!requests.some((r) => r.includes('/api/hide/')), 'closing a drawing board detaches nothing');
  cleanup();
});

test('a crafted seed cannot give a drawing board a socket or a colliding key', async (t) => {
  const harness = await createPreactHarness(t);
  const { normalizeSeed } = await harness.importDashboardModule('js/terminals-core.js');
  assert.equal(normalizeSeed({ drawingBoard: '../etc', ws: '/x' }), null);
  assert.equal(normalizeSeed({ drawingBoard: '' }), null);
  const seed = normalizeSeed({ drawingBoard: 'plan', ws: 'wss://evil', key: 'window:agt_x' });
  assert.equal(seed.ws, undefined);
  assert.equal(seed.key, 'drawing_board:plan');
});

test('the panel\'s ＋ New lists drawing boards and opens one in a tab', async (t) => {
  const requests = fakeDaemon(t);
  const env = await mountShell(t, COLLAB_ON);
  const cleanup = await start(env);
  const { harness, host } = env;
  assertAbsent(host.querySelector('.mux-drawing-board-add'), 'the strip no longer carries a ＋');
  await harness.act(async () => { harness.fireEvent(host.querySelector('.cr-new'), 'click'); await wait(); });
  await harness.act(async () => { harness.fireEvent(host.querySelector('[data-new-kind="drawing_board"]'), 'click'); await wait(); });
  await harness.act(() => wait());
  const cards = host.querySelectorAll('.mux-drawing-board-menu .drawing-board-row-open');
  assert.equal(cards.length, 2);
  assert.ok(requests.includes('GET /api/drawing-boards'));
  await harness.act(async () => { harness.fireEvent(cards[1], 'click'); await wait(); });
  assert.deepEqual(tabLabels(host), ['Team map'], 'the tab carries the drawing board title');
  assertAbsent(host.querySelector('.mux-drawing-board-menu'), 'picking a drawing board closes the menu');
  assert.ok(harness.document.getElementById('tab-terminals').classList.contains('active'));
  cleanup();
});

test('deleting a drawing board from its tab asks, then closes the tab', async (t) => {
  const drawingBoards = [...DRAWING_BOARD_LIST];
  const requests = fakeDaemon(t, drawingBoards);
  const env = await mountShell(t, COLLAB_ON);
  const cleanup = await start(env);
  const { harness, host, state } = env;
  const { openDrawingBoardPane } = await harness.importDashboardModule('js/drawing-board-overlay-state.js');
  await harness.act(async () => { await openDrawingBoardPane('plan', 'Release plan'); await wait(); });
  assert.deepEqual(tabLabels(host), ['Release plan']);
  const tab = host.querySelector('.mux-tab');
  await harness.act(() => harness.fireEvent(tab, 'contextmenu', { clientX: 8, clientY: 8 }));
  const item = [...harness.document.querySelectorAll('.mux-tab-menu-item')].find((el) => el.textContent === 'Delete drawing board…');
  await harness.act(async () => { harness.fireEvent(item, 'click'); await wait(); });
  await harness.act(() => wait());
  const dialog = harness.document.querySelector('.drawing-board-delete');
  assert.match(dialog.textContent, /Delete “Release plan”\?/);
  assert.equal(requests.filter((r) => r.startsWith('DELETE')).length, 0, 'nothing is deleted before the operator says so');
  await harness.act(async () => { harness.fireEvent(dialog.querySelector('.drawing-board-delete-confirm'), 'click'); await wait(); await wait(); });
  assert.ok(requests.includes('DELETE /api/drawing-boards/plan'));
  assert.deepEqual(tabLabels(host), [], 'the tab of a deleted drawing board closes');
  assertAbsent(harness.document.querySelector('.drawing-board-delete'), 'the dialog closes');
  assert.equal(state.panes.value.length, 0);
  cleanup();
});

test('a new page opens only the drawing board its URL names', async (t) => {
  const requests = fakeDaemon(t);
  const env = await mountShell(t, COLLAB_ON);
  const cleanup = await start(env);
  const { harness, host, state } = env;
  await harness.act(() => wait());
  assert.deepEqual(tabLabels(host), [], 'no saved list: nothing opens by itself');
  assert.deepEqual(requests.filter((r) => r.includes('/api/drawing-boards')), []);
  await harness.act(async () => {
    harness.document.dispatchEvent(new harness.window.CustomEvent('tclaude:restore-location', { detail: { location: { tab: 'drawing-boards', selection: 'plan' } } }));
    await wait();
  });
  await harness.act(() => wait());
  assert.deepEqual(tabLabels(host), ['Release plan']);
  assert.equal(state.activeKey.value, 'drawing_board:plan');
  cleanup();
});

test('the URL of a deleted drawing board opens nothing and corrects itself to the bare tab', async (t) => {
  fakeDaemon(t);
  const env = await mountShell(t, COLLAB_ON);
  const cleanup = await start(env);
  const { harness, host } = env;
  const announced = [];
  harness.document.addEventListener('tclaude:navigated', (event) => announced.push(event.detail));
  await harness.act(async () => {
    harness.document.dispatchEvent(new harness.window.CustomEvent('tclaude:restore-location', { detail: { location: { tab: 'drawing-boards', selection: 'gone' } } }));
    await wait();
  });
  await harness.act(() => wait());
  assert.deepEqual(tabLabels(host), []);
  assert.deepEqual(announced, [{ location: { tab: 'terminals' }, correction: true }]);
  cleanup();
});

// The strip's own close, on the tab showing, and what the router is told.
async function closeLastDrawingBoard(env) {
  const announced = [];
  env.harness.document.addEventListener('tclaude:navigated', (event) => announced.push(event.detail));
  await env.harness.act(async () => {
    env.harness.fireEvent(env.host.querySelector('.mux-tab[data-agent-status="drawing_board"] .mux-tab-close'), 'click');
    await wait();
  });
  await env.harness.act(() => wait(5));
  return announced;
}

async function openDrawingBoardTab(env) {
  await env.harness.act(async () => { await env.openDrawingBoardPane('plan', 'Release plan'); await wait(); });
  env.harness.fireEvent(env.harness.document.querySelector('nav [data-tab="terminals"]'), 'click');
}

test('closing the last drawing board leaves for Groups, with the entry behind it on the bare tab', async (t) => {
  fakeDaemon(t);
  const env = await mountShell(t, COLLAB_ON);
  const cleanup = await start(env);
  await openDrawingBoardTab(env);
  const announced = await closeLastDrawingBoard(env);
  assert.deepEqual(announced, [{ location: { tab: 'terminals' }, correction: true }]);
  assert.ok(env.harness.document.getElementById('tab-groups').classList.contains('active'));
  cleanup();
});

test('configured to stay, closing the last drawing board stays and the URL stops naming it', async (t) => {
  fakeDaemon(t);
  const env = await mountShell(t, COLLAB_ON);
  env.closeNav.setStayOnTerminalsAfterLastClose(true);
  t.after(() => env.closeNav.setStayOnTerminalsAfterLastClose(false));
  const cleanup = await start(env);
  await openDrawingBoardTab(env);
  const announced = await closeLastDrawingBoard(env);
  assert.deepEqual(announced, [{ location: { tab: 'terminals' }, correction: true }]);
  assert.ok(env.harness.document.getElementById('tab-terminals').classList.contains('active'));
  cleanup();
});

test('a last drawing board that goes while another section shows leaves the URL alone', async (t) => {
  fakeDaemon(t);
  const env = await mountShell(t, COLLAB_ON);
  const cleanup = await start(env);
  await openDrawingBoardTab(env);
  const announced = [];
  env.harness.document.addEventListener('tclaude:navigated', (event) => announced.push(event.detail));
  env.harness.fireEvent(env.harness.document.querySelector('nav [data-tab="groups"]'), 'click');
  await env.harness.act(async () => { env.state.removePanes(['drawing_board:plan']); await wait(5); });
  assert.deepEqual(announced, [], 'the URL already names the section on screen');
  assert.ok(env.harness.document.getElementById('tab-groups').classList.contains('active'));
  cleanup();
});

test('a tab title follows the drawing board', async (t) => {
  fakeDaemon(t, [{ id: 'plan', title: 'Renamed plan' }]);
  const env = await mountShell(t, COLLAB_ON);
  const cleanup = await start(env);
  await env.harness.act(async () => { await env.openDrawingBoardPane('plan', 'Old'); await wait(); });
  await env.harness.act(() => wait());
  assert.deepEqual(tabLabels(env.host), ['Renamed plan']);
  cleanup();
});

test('/drawing-boards/<id> opens that tab; bare /drawing-boards offers the list and heals to what shows', async (t) => {
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
  await restore({ tab: 'drawing-boards', selection: 'plan' });
  assert.equal(state.activeKey.value, 'drawing_board:plan');
  assert.deepEqual(state.location.value, { tab: 'drawing-boards', selection: 'plan' }, 'the URL keeps naming the drawing board');
  assert.deepEqual(announced, [], 'a restore does not announce itself back');

  await restore({ tab: 'drawing-boards' });
  assert.ok(host.querySelector('.mux-drawing-board-menu'), 'a bare /drawing-boards opens the list');
  assert.deepEqual(announced.at(-1), { location: { tab: 'drawing-boards', selection: 'plan' }, correction: true });

  await restore({ tab: 'drawing-boards', selection: 'Not A Drawing board' });
  assert.equal(state.panes.value.length, 1, 'an invalid id opens nothing');

  // A drawing board tab the operator switches to is a navigation, under its own route.
  await harness.act(async () => { await env.openDrawingBoardPane('map'); await wait(); });
  harness.fireEvent(harness.document.querySelector('nav [data-tab="terminals"]'), 'click');
  await harness.act(async () => { state.activatePane('drawing_board:plan'); await wait(); });
  assert.deepEqual(announced.at(-1), { location: { tab: 'drawing-boards', selection: 'plan' }, correction: false });
  cleanup();
});

test('/drawing-boards/<id>?crop=…&elements=… opens the tab on that part, as a task drawing on a linked node links it', async (t) => {
  fakeDaemon(t);
  const env = await mountShell(t, COLLAB_ON);
  const cleanup = await start(env);
  const { harness, state } = env;
  const { drawingBoardFocus } = await harness.importDashboardModule('js/drawing-board-overlay-state.js');
  const asked = [];
  const stop = drawingBoardFocus.subscribe((focus) => { if (focus) asked.push(focus); });
  const restore = (location) => harness.act(async () => {
    harness.document.dispatchEvent(new harness.window.CustomEvent('tclaude:restore-location', { detail: { location } }));
    await wait();
  });
  const core = await harness.importDashboardModule('js/nav-history-core.js');
  await restore(core.fromPath('/drawing-boards/plan', '?node=inst_bob1234&crop=10,-20,300.5,200&elements=api,db'));
  assert.equal(state.activeKey.value, 'drawing_board:plan', 'the tab opens');
  assert.deepEqual(asked, [{ drawingBoard: 'plan', elements: ['api', 'db'], box: { x: 10, y: -20, width: 300.5, height: 200 } }],
    'and the board is asked to show the part the link names');

  // Open already: the tab comes forward and still shows that part.
  await restore(core.fromPath('/drawing-boards/plan', '?crop=0,0,40,30'));
  assert.deepEqual(asked.at(-1), { drawingBoard: 'plan', elements: [], box: { x: 0, y: 0, width: 40, height: 30 } });
  // A plain link asks for nothing.
  const before = asked.length;
  await restore(core.fromPath('/drawing-boards/plan', ''));
  assert.equal(asked.length, before);
  stop();
  cleanup();
});

test('the router keeps the /drawing-boards routes and shows them in the Terminals section', async (t) => {
  const harness = await createPreactHarness(t);
  const core = await harness.importDashboardModule('js/nav-history-core.js');
  assert.deepEqual(core.fromPath('/drawing-boards/plan', ''), { tab: 'drawing-boards', selection: 'plan' });
  assert.equal(core.toPath({ tab: 'drawing-boards', selection: 'plan' }), '/drawing-boards/plan');
  // The part to show rides as params; anything malformed is dropped.
  const loc = core.fromPath('/drawing-boards/plan', '?crop=1,2,3,4&elements=a,b_c');
  assert.deepEqual(loc, { tab: 'drawing-boards', selection: 'plan', params: { crop: '1,2,3,4', elements: 'a,b_c' } });
  assert.equal(core.toQuery(loc), 'crop=1%2C2%2C3%2C4&elements=a%2Cb_c');
  assert.deepEqual(core.fromPath('/drawing-boards/plan', '?crop=1,2,x,4&elements=a,<b>'), { tab: 'drawing-boards', selection: 'plan' });
  assert.deepEqual(core.fromPath('/drawing-boards/plan', `?elements=${Array.from({ length: 201 }, (_, i) => `e${i}`).join(',')}`),
    { tab: 'drawing-boards', selection: 'plan' }, 'more than 200 ids is no focus');
});

test('detaching a drawing board carries it to the solo page, which renders it and sends it back', async (t) => {
  fakeDaemon(t);
  const harness = await createPreactHarness(t);
  const { createTerminalShellState } = await harness.importDashboardModule('js/terminal-shell-state.js');
  const { createTerminalShellActions } = await harness.importDashboardModule('js/terminal-shell-actions.js');
  const { decodeTerminalOpenHash, createStandaloneTerminalsPage } = await harness.importDashboardModule('js/terminal-standalone.js');
  const state = createTerminalShellState();
  let opened = null;
  const target = { location: { replace(url) { opened = url; } } };
  const windowRef = { open: () => target, screen: {} };
  const actions = createTerminalShellActions({ state, windowRef, documentRef: harness.document, fetchImpl: async () => ({ ok: true }) });
  actions.openPane({ drawingBoard: 'plan', key: 'drawing_board:plan', label: 'Release plan' });
  assert.equal(await actions.popOutPane('drawing_board:plan'), true);
  assert.equal(state.panes.value.length, 0, 'the tab left');
  assert.ok(opened.startsWith('/terminals?solo=1#open='));
  const seed = decodeTerminalOpenHash(opened.slice(opened.indexOf('#')));
  assert.equal(seed.drawingBoard, 'plan');
  assert.equal(seed.ws, undefined);

  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const location = { hash: opened.slice(opened.indexOf('#')), pathname: '/terminals', search: '?solo=1', origin: 'http://x' };
  const requests = [];
  const page = createStandaloneTerminalsPage({
    host, initPrefs: async () => {}, initThemeSync: () => {},
    widgetFactory: () => { throw new Error('a drawing board mounts no widget'); },
    fetchImpl: async (url) => { requests.push(String(url)); return { ok: true, json: async () => COLLAB_ON }; },
    locationRef: location, historyRef: { replaceState() {} },
    mountMessageDialogs: async () => () => {},
  });
  await harness.act(async () => { await page.start(); await wait(); });
  assert.ok(host.querySelector('svg.drawing-board-canvas'), 'the solo page renders the drawing board');
  await harness.act(async () => { harness.fireEvent(host.querySelector('[data-editor-menu="view"]'), 'click'); await wait(); });
  assert.ok([...host.querySelectorAll('.reh-menu button')].some((b) => b.textContent.includes('Open in the Collab tab')), 'View offers the way back');
  assert.ok(requests.includes('/api/snapshot?standalone=1'), 'it reads the roster, to name the agents on the drawing board');
  page.dispose();
});

// A pop-out with one drawing board on it, a shape drawn and its save still to go.
async function soloWithEdit(t, patchAnswer) {
  fakeDaemon(t);
  const daemon = globalThis.fetch;
  globalThis.fetch = async (url, init = {}) => {
    if (init.method === 'PATCH') {
      await patchAnswer();
      return { ok: true, status: 200, json: async () => ({ version: 2, accepted: 1 }) };
    }
    return daemon(url, init);
  };
  const harness = await createPreactHarness(t);
  const { createStandaloneTerminalsPage } = await harness.importDashboardModule('js/terminal-standalone.js');
  const { encodeTerminalOpenHash } = await harness.importDashboardModule('js/terminal-handoff.js');
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const replaced = [];
  const location = {
    hash: encodeTerminalOpenHash({ drawingBoard: 'plan', label: 'Release plan' }), pathname: '/terminals', search: '?solo=1', origin: 'http://x',
    replace(url) { replaced.push(url); },
  };
  const page = createStandaloneTerminalsPage({
    host, initPrefs: async () => {}, initThemeSync: () => {},
    widgetFactory: () => { throw new Error('a drawing board mounts no widget'); },
    fetchImpl: async () => ({ ok: true, json: async () => COLLAB_ON }),
    locationRef: location, historyRef: { replaceState() {} },
    mountMessageDialogs: async () => () => {},
  });
  await harness.act(async () => { await page.start(); await wait(); });
  await harness.act(() => wait());
  await harness.act(() => harness.getByRole(host, 'button', { name: 'Rectangle' }).click());
  const svg = host.querySelector('svg.drawing-board-canvas');
  const pointer = (type, x, y) => harness.act(() => harness.fireEvent(svg, `on${type.toLowerCase()}` in svg ? type.toLowerCase() : type, { button: 0, pointerId: 1, clientX: x, clientY: y }));
  await pointer('PointerDown', 100, 100);
  await pointer('PointerMove', 220, 160);
  await pointer('PointerUp', 220, 160);
  await harness.act(async () => { harness.fireEvent(host.querySelector('[data-editor-menu="view"]'), 'click'); await wait(); });
  const back = [...host.querySelectorAll('.reh-menu button')].find((b) => b.textContent.includes('Open in the Collab tab'));
  return { harness, page, replaced, back };
}

test('a pop-out going back to the dashboard waits for the drawing board to be saved', async (t) => {
  let release = null;
  const { harness, page, replaced, back } = await soloWithEdit(t, () => new Promise((resolve) => { release = resolve; }));
  try {
    await harness.act(async () => { harness.fireEvent(back, 'click'); await wait(5); });
    assert.ok(release, 'the save is out, without waiting for the debounce');
    assert.deepEqual(replaced, [], 'the window stays while it is');
    assert.equal(page.state.panes.value.length, 1);
    await harness.act(async () => { release(); await wait(5); await wait(5); });
    assert.equal(replaced.length, 1, 'then it goes');
    assert.equal(page.state.panes.value.length, 0);
  } finally {
    page.dispose();
  }
});

test('a pop-out whose drawing board could not be saved stays open', async (t) => {
  const { harness, page, replaced, back } = await soloWithEdit(t, async () => { throw new TypeError('network'); });
  try {
    await harness.act(async () => { harness.fireEvent(back, 'click'); await wait(5); await wait(5); });
    assert.deepEqual(replaced, []);
    assert.equal(page.state.panes.value.length, 1, 'the edit still exists here');
  } finally {
    page.dispose();
  }
});

test('a lock toggled in the Collab panel shows on the drawing board tab name and in its menu', async (t) => {
  const requests = fakeDaemon(t);
  const snap = (locked) => ({
    ...COLLAB_ON, groups: [{ id: 1, name: 'tclaude' }],
    group_resources: [{ kind: 'drawing_board', id: 'plan', group_id: 1, title: 'Release plan', state: 'closed', locked }],
  });
  const env = await mountShell(t, snap(false));
  const cleanup = await start(env);
  const { harness, host, openDrawingBoardPane, setSnapshot } = env;
  const lock = await harness.importDashboardModule('js/resource-lock.js');
  // The re-read after a toggle brings the daemon's new row, as refresh.js would.
  lock.lockRefresh.run = async () => setSnapshot(snap(true));
  await harness.act(async () => { await openDrawingBoardPane('plan', 'Release plan'); await wait(); });
  await harness.act(() => wait());
  const tab = () => host.querySelector('.mux-tab');
  assertAbsent(tab().querySelector('[data-lock-badge]'), 'an unlocked drawing board tab wears no lock');
  const menuItems = async () => {
    await harness.act(() => harness.fireEvent(tab(), 'contextmenu', { clientX: 8, clientY: 8 }));
    const labels = [...harness.document.querySelectorAll('.mux-tab-menu-item')].map((el) => el.textContent);
    await harness.act(() => harness.fireEvent(harness.document, 'keydown', { key: 'Escape' }));
    return labels;
  };
  assert.ok((await menuItems()).includes('Lock for agents'));

  const railLock = host.querySelector('[data-rail-item="drawing_board:plan"] [data-rail-lock="drawing_board:plan"]');
  assert.ok(railLock, 'the panel row has the lock toggle');
  await harness.act(async () => { harness.fireEvent(railLock, 'click'); await wait(); await wait(); });
  assert.ok(requests.includes('POST /api/resource-locks'));
  assert.ok(host.querySelector('[data-rail-lock="drawing_board:plan"]').classList.contains('is-locked'), 'the panel row follows');
  assert.ok(tab().querySelector('[data-lock-badge="drawing_board"]'), 'the tab name wears the lock');
  assert.ok((await menuItems()).includes('Unlock for agents'), 'the tab menu offers the unlock');
  cleanup();
});

// A drawing board's or a page's tab renames what it shows, in place: a
// double-click or F2 turns its label into the name field (Enter saves, Escape
// keeps it). A terminal's tab has no such field.
const tabOf = (host, key) => host.querySelector(`.mux-tab[data-pane-key="${key}"]`);
const renameField = (host, key) => tabOf(host, key)?.querySelector('input.mux-tab-rename');

async function waitUntil(harness, condition, message, timeoutMs = 3000) {
  const deadline = Date.now() + timeoutMs;
  while (!condition()) {
    if (Date.now() > deadline) assert.fail(message);
    await harness.act(() => wait(5));
  }
}

const PAGE_ID = 'pg_00112233445566aa';

async function threeTabs(t) {
  const requests = fakeDaemon(t);
  const env = await mountShell(t, COLLAB_ON);
  const cleanup = await start(env);
  const { harness, controller, openDrawingBoardPane } = env;
  const { openPagePane } = await harness.importDashboardModule('js/page-place.js');
  await harness.act(async () => {
    await controller.openTerminalPane({ ws: '/one', key: 'one', label: 'one', agent: 'agt_one' });
    await openDrawingBoardPane('plan', 'Release plan');
    await openPagePane(PAGE_ID, 'sketch');
    await wait();
  });
  const pageKey = [...env.host.querySelectorAll('.mux-tab')].map((tab) => tab.dataset.paneKey).find((key) => key.includes(PAGE_ID));
  return { ...env, requests, cleanup, pageKey };
}

test('a double-click on a drawing board\'s tab renames the drawing board', async (t) => {
  const { harness, host, requests, cleanup } = await threeTabs(t);
  const key = 'drawing_board:plan';
  await harness.act(() => harness.fireEvent(tabOf(host, key), 'dblclick'));
  const field = renameField(host, key);
  assert.ok(field, 'the tab\'s label becomes the name field');
  assert.equal(field.value, 'Release plan');
  assert.notEqual(tabOf(host, key).getAttribute('draggable'), 'true', 'the tab does not drag while its name is edited');
  // What is typed stays in the field: a Space does not activate the tab.
  const space = harness.fireEvent(field, 'keydown', { key: ' ' });
  assert.equal(space.defaultPrevented, false);
  field.value = 'Launch plan';
  await harness.act(() => harness.fireEvent(field, 'keydown', { key: 'Enter' }));
  await waitUntil(harness, () => requests.includes('POST /api/drawing-boards/plan/title'), 'the rename reaches the daemon');
  await waitUntil(harness, () => tabLabels(host).includes('Launch plan'), 'the tab carries the new name');
  assert.equal(renameField(host, key), null, 'the field is gone');
  cleanup();
});

test('F2 on a page\'s tab renames the page; Escape keeps its name', async (t) => {
  const { harness, host, requests, cleanup, pageKey } = await threeTabs(t);
  assert.ok(pageKey, 'the page has a tab');
  await harness.act(() => harness.fireEvent(tabOf(host, pageKey), 'keydown', { key: 'F2' }));
  let field = renameField(host, pageKey);
  assert.ok(field, 'F2 opens the name field');
  field.value = 'never saved';
  await harness.act(() => harness.fireEvent(field, 'keydown', { key: 'Escape' }));
  assert.equal(renameField(host, pageKey), null, 'Escape ends the edit');
  assert.ok(!requests.some((r) => r.startsWith('POST /api/pages/')), 'and saves nothing');

  await harness.act(() => harness.fireEvent(tabOf(host, pageKey), 'keydown', { key: 'F2' }));
  field = renameField(host, pageKey);
  field.value = 'Checkout mock';
  await harness.act(() => harness.fireEvent(field, 'keydown', { key: 'Enter' }));
  await waitUntil(harness, () => requests.includes(`POST /api/pages/${PAGE_ID}/title`), 'the rename reaches the daemon');
  await waitUntil(harness, () => tabLabels(host).includes('Checkout mock'), 'the tab carries the new name');
  cleanup();
});

test('a terminal\'s tab has no name field to open', async (t) => {
  const { harness, host, requests, cleanup } = await threeTabs(t);
  await harness.act(() => harness.fireEvent(tabOf(host, 'one'), 'dblclick'));
  await harness.act(() => harness.fireEvent(tabOf(host, 'one'), 'keydown', { key: 'F2' }));
  assertAbsent(renameField(host, 'one'), 'a terminal is not renamed from its tab');
  assertAbsent(host.querySelector('input.mux-tab-rename'), 'nor is any other tab');
  assert.ok(!requests.some((r) => r.endsWith('/title')));
  cleanup();
});

test('a page renamed from its tab keeps the new name while the snapshot still has the old title', async (t) => {
  const requests = fakeDaemon(t);
  const page = { id: PAGE_ID, name: 'sketch', title: 'sketch', owner: '', state: 'open', kind: 'scratch', groupOwned: true };
  const env = await mountShell(t, { ...COLLAB_ON, live_pages: [page] });
  const cleanup = await start(env);
  const { harness, host, setSnapshot } = env;
  const { openPagePane } = await harness.importDashboardModule('js/page-place.js');
  await harness.act(async () => { await openPagePane(PAGE_ID, 'sketch'); await wait(); });
  const key = [...host.querySelectorAll('.mux-tab')].map((tab) => tab.dataset.paneKey).find((k) => k.includes(PAGE_ID));
  await harness.act(() => harness.fireEvent(tabOf(host, key), 'keydown', { key: 'F2' }));
  const field = renameField(host, key);
  field.value = 'Checkout mock';
  await harness.act(() => harness.fireEvent(field, 'keydown', { key: 'Enter' }));
  await waitUntil(harness, () => requests.includes(`POST /api/pages/${PAGE_ID}/title`), 'the rename reaches the daemon');
  await waitUntil(harness, () => tabLabels(host).includes('Checkout mock'), 'the tab carries the new name');
  // A poll before the daemon's answer shows up still has the old title.
  await harness.act(async () => { setSnapshot({ ...COLLAB_ON, live_pages: [{ ...page }] }); await wait(); });
  assert.deepEqual(tabLabels(host), ['Checkout mock'], 'the old title is not written back');
  // Once the snapshot has a new title, the tab follows it.
  await harness.act(async () => { setSnapshot({ ...COLLAB_ON, live_pages: [{ ...page, title: 'Checkout v2' }] }); await wait(); });
  await waitUntil(harness, () => tabLabels(host).includes('Checkout v2'), 'a later title from the snapshot still reaches the tab');
  cleanup();
});
