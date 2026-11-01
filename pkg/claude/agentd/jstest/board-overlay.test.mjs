// Tests for the board overlay (tcl-h4y60w): a board in its own floating window,
// moved there from a tab and back; the window's placement; which window is in
// front; and features.collab off. The feature islands are the real ones; only
// xterm and the daemon are faked.

import test from 'node:test';
import assert from 'node:assert/strict';
import { assertAbsent } from './assertions.mjs';
import { createPreactHarness } from './preact-harness.mjs';

const wait = (ms = 0) => new Promise((r) => setTimeout(r, ms));
const COLLAB_ON = { collab_enabled: true, agents: [], groups: [] };
const BOARDS = [{ id: 'plan', title: 'Release plan' }, { id: 'map', title: 'Team map' }];

async function setup(t, snapshotValue = COLLAB_ON) {
  const harness = await createPreactHarness(t);
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
  nav.appendChild(doc.createElement('span')).id = 'terminals-badge-root';
  const overlayHost = doc.body.appendChild(doc.createElement('div'));
  overlayHost.id = 'board-overlay-root';

  const previous = globalThis.fetch;
  globalThis.fetch = async (url) => {
    const path = String(url);
    const board = BOARDS.find((b) => b.id === /^\/api\/boards\/([^/?]+)/.exec(path)?.[1]);
    const body = path === '/api/boards' ? { boards: BOARDS } : board ? { ...board, version: 1, participants: [], elements: [] } : {};
    return { ok: true, status: 200, json: async () => body };
  };
  t.after(() => { globalThis.fetch = previous; });

  const [loader, overlayState, windows, island, { dashboardState }, { dashPrefs }, { terminalShellState }] = await Promise.all([
    harness.importDashboardModule('js/preact-loader.js'),
    harness.importDashboardModule('js/board-overlay-state.js'),
    harness.importDashboardModule('js/overlay-instances.js'),
    harness.importDashboardModule('js/board-overlay.js'),
    harness.importDashboardModule('js/snapshot-store.js'),
    harness.importDashboardModule('js/prefs.js'),
    harness.importDashboardModule('js/terminal-shell-state.js'),
  ]);
  dashboardState.publishLocalEdit(snapshotValue);
  for (const suffix of ['', '.plan', '.map']) dashPrefs.removeItem(`${island.BOARD_OVERLAY_PLACEMENT_PREF}${suffix}`);
  windows.overlayWindows.value = [];
  const cleanups = [
    await loader.mountTerminalsFeature({ fetchImpl: async () => ({ ok: true }), widgetFactory: () => { throw new Error('no terminals here'); } }),
    await loader.mountBoardOverlayFeature({ notify: () => {} }),
  ];
  await harness.act(() => wait());
  return {
    dispose() {
      windows.overlayWindows.value = [];
      for (const cleanup of cleanups) cleanup?.();
    },
    harness, host, overlayHost, overlayState, island, windows, dashboardState, dashPrefs, state: terminalShellState,
    click: (node) => harness.act(async () => { harness.fireEvent(node, 'click'); await wait(); await wait(); }),
    button: (root, text) => [...root.querySelectorAll('button')].find((b) => b.textContent.trim() === text),
  };
}

// The islands come down before the harness takes the document away, pass or fail.
const scenario = (name, ...args) => {
  const body = args.pop();
  test(name, async (t) => {
    const env = await setup(t, ...args);
    try {
      await body({ ...env, t });
    } finally {
      env.dispose();
    }
  });
};

const tabLabels = (host) => [...host.querySelectorAll('.mux-tab .mux-tab-label')].map((node) => node.textContent);

const AGENT = 'agt_0123456789abcdef0123456789abcdef';
const AGENT_ON = { ...COLLAB_ON, agents: [{ agent_id: AGENT, title: 'builder', online: true }] };

async function agentPane(harness) {
  const { openWebWindowPane } = await harness.importDashboardModule('js/terminals-tab.js');
  await harness.act(async () => { await openWebWindowPane(AGENT, 'builder'); await wait(); await wait(); });
}

async function fakeCompanion(harness, float) {
  const { registerCompanionController } = await harness.importDashboardModule('js/companion-facade.js');
  return registerCompanionController({ open: () => true, float, reveal: () => false, reclaim: () => false });
}

scenario('an agent\'s tab moves to its chat, and the operator stays on Terminals', AGENT_ON, async ({ harness, host, windows, click, button }) => {
  const floated = [];
  const stop = await fakeCompanion(harness, (agent) => {
    floated.push(agent);
    windows.openOverlayWindow({ id: `chat:${agent}`, kind: 'chat', agent, label: 'builder' });
    return true;
  });
  try {
    await agentPane(harness);
    await harness.act(async () => { harness.fireEvent(harness.document.querySelector('nav [data-tab="terminals"]'), 'click'); await wait(); });
    await click(button(host, 'Open in overlay'));
    await harness.act(() => wait(5));
    assert.deepEqual(floated, [AGENT]);
    assert.deepEqual(tabLabels(host), [], 'a terminal is in one place at a time');
    assert.ok(harness.document.getElementById('tab-terminals').classList.contains('active'), 'moving the last tab out does not send the operator to Groups');
  } finally { stop(); }
});

scenario('with Collab off an agent\'s tab offers no overlay', { ...AGENT_ON, collab_enabled: false }, async ({ harness, host, button }) => {
  const stop = await fakeCompanion(harness, () => true);
  try {
    await agentPane(harness);
    assert.equal(button(host, 'Open in overlay'), undefined);
  } finally { stop(); }
});

scenario('with no companion to float it, an agent\'s tab offers no overlay', AGENT_ON, async ({ harness, host, button }) => {
  await agentPane(harness);
  assert.equal(button(host, 'Open in overlay'), undefined);
});

// A tab pulled off the strip and let go at (x, y). The fake strip has no size.
async function dragOut(harness, host, label, x = 400, y = 300) {
  const tab = [...host.querySelectorAll('.mux-tab')].find((node) => node.textContent.includes(label));
  await harness.act(async () => {
    harness.fireEvent(tab, 'dragstart', { dataTransfer: { setData() {} } });
    harness.fireEvent(tab, 'dragend', { clientX: x, clientY: y, screenX: x, screenY: y });
    await wait(5);
    await wait(5);
  });
}

function fakeOpen(t, harness, target = (urls) => ({ location: { replace: (url) => urls.push(url) } })) {
  const urls = [];
  const previous = [globalThis.open, harness.window.open];
  globalThis.open = harness.window.open = () => target(urls);
  t.after(() => { [globalThis.open, harness.window.open] = previous; });
  return urls;
}

const seedOf = (url) => JSON.parse(decodeURIComponent(url.split('#open=')[1]));

// What a separate window sends back to the page that opened it.
async function handOff(harness, seed) {
  const { receiveSeparateWindow } = await harness.importDashboardModule('js/terminal-shell-island.js');
  const { createTerminalShellActions } = await harness.importDashboardModule('js/terminal-shell-actions.js');
  const { terminalShellState: state } = await harness.importDashboardModule('js/terminal-shell-state.js');
  const { dashboardState } = await harness.importDashboardModule('js/snapshot-store.js');
  let accepted;
  await harness.act(async () => {
    accepted = await receiveSeparateWindow({ ...seed, initialRetry: true }, { actions: createTerminalShellActions({ state }), snapshot: dashboardState.snapshot });
    await wait(5);
  });
  return accepted;
}

scenario('a board dragged off the strip floats where it was dropped', async ({ t, harness, host, overlayHost, overlayState, island, dashPrefs }) => {
  const urls = fakeOpen(t, harness);
  await harness.act(async () => { await overlayState.openBoardPane('plan', 'Release plan'); await wait(); await wait(); });
  await dragOut(harness, host, 'Release plan', 400, 300);
  assert.deepEqual(tabLabels(host), []);
  assert.ok(overlayHost.querySelector('.board-overlay'), 'it floats');
  assert.deepEqual(urls, [], 'a browser window is a deliberate click');
  const saved = JSON.parse(dashPrefs.getItem(`${island.BOARD_OVERLAY_PLACEMENT_PREF}.plan`));
  assert.deepEqual([saved.x, saved.y, saved.snap], [280, 286, '']);
});

scenario('an agent\'s tab dragged off the strip floats its chat at the drop point', AGENT_ON, async ({ t, harness, host }) => {
  const urls = fakeOpen(t, harness);
  const floated = [];
  const stop = await fakeCompanion(harness, (agent, at) => { floated.push([agent, at]); return true; });
  try {
    await agentPane(harness);
    await dragOut(harness, host, 'builder', 400, 300);
    assert.deepEqual(floated, [[AGENT, { x: 280, y: 286 }]]);
    assert.deepEqual(urls, []);
  } finally { stop(); }
});

scenario('what cannot float still pops out when dragged', { ...AGENT_ON, collab_enabled: false }, async ({ t, harness, host }) => {
  const urls = fakeOpen(t, harness);
  const stop = await fakeCompanion(harness, () => assert.fail('it cannot float'));
  try {
    await agentPane(harness);
    await dragOut(harness, host, 'builder');
    assert.equal(urls.length, 1);
    assert.equal(seedOf(urls[0]).floats, undefined, 'the window does not offer an overlay it cannot get');
    assert.deepEqual(tabLabels(host), []);
  } finally { stop(); }
});

scenario('a tab\'s separate window asks for a real window and may come back to an overlay', async ({ t, harness, host, overlayState, click, button }) => {
  const features = [];
  const urls = [];
  const previous = harness.window.open;
  harness.window.open = (_url, _name, feature) => {
    features.push(feature);
    return { location: { replace: (url) => urls.push(url) } };
  };
  t.after(() => { harness.window.open = previous; });
  await harness.act(async () => { await overlayState.openBoardPane('plan', 'Release plan'); await wait(); await wait(); });
  host.querySelector('.mux-panes').getBoundingClientRect = () => ({ left: 0, top: 0, width: 800, height: 500 });
  const screen = harness.window.screen;
  harness.window.screen = { availWidth: 1920, availHeight: 1080 };
  t.after(() => { harness.window.screen = screen; });
  await click(button(host, 'Open in separate window'));
  await harness.act(() => wait(5));
  assert.equal(seedOf(urls[0]).board, 'plan');
  assert.equal(seedOf(urls[0]).floats, true);
  assert.match(features[0], /popup=yes,width=800/, 'no features would be a plain browser tab');
});

scenario('a floating board moves to a separate window, and stays if pop-ups are blocked', async ({ t, harness, overlayHost, overlayState, windows, click, button }) => {
  let blocked = true;
  const urls = [];
  const previous = globalThis.open;
  globalThis.open = () => (blocked ? null : { location: { replace: (url) => urls.push(url) } });
  t.after(() => { globalThis.open = previous; });
  await harness.act(async () => { overlayState.openBoardOverlay('plan', 'Release plan'); await wait(); });
  await click(button(overlayHost, 'Open in separate window'));
  assert.equal(windows.overlayWindows.value.length, 1);
  blocked = false;
  await click(button(overlayHost, 'Open in separate window'));
  assert.equal(windows.overlayWindows.value.length, 0);
  assert.equal(seedOf(urls[0]).board, 'plan');
  assert.equal(seedOf(urls[0]).floats, true);
});

scenario('a separate window sent to the overlay floats there, with no tab', async ({ harness, host, overlayHost }) => {
  assert.ok(await handOff(harness, { board: 'plan', label: 'Release plan', overlay: true }));
  assert.ok(overlayHost.querySelector('.board-overlay'));
  assert.deepEqual(tabLabels(host), []);
});

scenario('a separate window that can no longer float lands in a tab', { ...COLLAB_ON, collab_enabled: false }, async ({ harness, windows, state }) => {
  assert.ok(await handOff(harness, { board: 'plan', label: 'Release plan', overlay: true }));
  assert.deepEqual(windows.overlayWindows.value, []);
  assert.equal(state.panes.value.length, 1, 'a handoff never loses it');
});

scenario('Back to a tab leaves a board that has since floated where it is', async ({ harness, overlayState, windows, state }) => {
  await harness.act(async () => { overlayState.openBoardOverlay('plan', 'Release plan'); await wait(); });
  assert.ok(await handOff(harness, { board: 'plan', label: 'Release plan' }));
  assert.equal(windows.overlayWindows.value.length, 1);
  assert.deepEqual(state.panes.value, [], 'one place at a time');
});

scenario('Back to a tab leaves an agent that has since found a place there', AGENT_ON, async ({ harness, state }) => {
  const { registerCompanionController } = await harness.importDashboardModule('js/companion-facade.js');
  const shown = [];
  const stop = registerCompanionController({ open: () => true, float: () => true, reclaim: () => false, reveal: (agent) => { shown.push(agent); return true; } });
  try {
    assert.ok(await handOff(harness, { ws: `/api/open-window-ws/${AGENT}`, key: `window:${AGENT}`, agent: AGENT, hideConv: AGENT, label: 'builder' }));
    assert.deepEqual(shown, [AGENT]);
    assert.deepEqual(state.panes.value, []);
  } finally { stop(); }
});

scenario('Back to a tab from a separate window goes to the companion tab waiting on it', AGENT_ON, async ({ harness, state }) => {
  const { registerCompanionController } = await harness.importDashboardModule('js/companion-facade.js');
  const reclaimed = [];
  const stop = registerCompanionController({ open: () => true, float: () => true, reveal: () => false, reclaim: (agent) => { reclaimed.push(agent); return true; } });
  try {
    await handOff(harness, { ws: `/api/open-window-ws/${AGENT}`, key: `window:${AGENT}`, agent: AGENT, hideConv: AGENT, label: 'builder' });
    assert.deepEqual(reclaimed, [AGENT]);
    assert.deepEqual(state.panes.value, []);
  } finally { stop(); }
});

scenario('a board moves from its tab into the overlay and back', async ({ harness, host, overlayHost, overlayState, state, click, button }) => {
  await harness.act(async () => { await overlayState.openBoardPane('plan', 'Release plan'); await wait(); await wait(); });
  const announced = [];
  harness.document.addEventListener('tclaude:navigated', (event) => announced.push(event.detail));
  await click(button(host, 'Open in overlay'));
  await harness.act(() => wait(5));

  assert.deepEqual(announced, [{ location: { tab: 'terminals' }, correction: true }], 'a reload must not reopen it as a tab');
  assert.deepEqual(tabLabels(host), [], 'the tab left: a board opens in one place');
  const win = overlayHost.querySelector('.board-overlay');
  assert.ok(win.querySelector('svg.board-canvas'), 'the overlay shows the editor');
  assert.match(win.querySelector('.board-overlay-title').textContent, /Release plan/);
  assert.ok(harness.document.getElementById('tab-terminals').classList.contains('active'),
    'moving the last tab out does not send the operator to Groups');

  // It floats over the other views too.
  await harness.act(async () => { harness.fireEvent(harness.document.querySelector('nav [data-tab="groups"]'), 'click'); await wait(); });
  assert.ok(overlayHost.querySelector('.board-overlay'));

  await click(button(overlayHost, 'Back to a tab'));
  assertAbsent(overlayHost.querySelector('.board-overlay'));
  assert.equal(overlayState.boardOverlays.value.length, 0);
  assert.deepEqual(tabLabels(host), ['Release plan']);
  assert.equal(state.activeKey.value, 'board:plan');
  assert.ok(harness.document.getElementById('tab-terminals').classList.contains('active'), 'the tab is shown');
});

scenario('a URL naming the board the overlay shows opens no tab and corrects itself', async ({ harness, host, overlayState }) => {
  await harness.act(async () => { overlayState.openBoardOverlay('plan', 'Release plan'); await wait(); });
  const announced = [];
  harness.document.addEventListener('tclaude:navigated', (event) => announced.push(event.detail));
  await harness.act(async () => {
    harness.document.dispatchEvent(new harness.window.CustomEvent('tclaude:restore-location', { detail: { location: { tab: 'boards', selection: 'plan' } } }));
    await wait();
    await wait();
  });
  assert.deepEqual(tabLabels(host), []);
  assert.deepEqual(announced, [{ location: { tab: 'terminals' }, correction: true }]);
});

scenario('the tab context menu offers the overlay for a board', async ({ harness, host, overlayHost, overlayState, click, button }) => {
  await harness.act(async () => { await overlayState.openBoardPane('plan', 'Release plan'); await wait(); });
  await harness.act(async () => { harness.fireEvent(host.querySelector('.mux-tab'), 'contextmenu'); await wait(); });
  await click(button(host, 'Open in overlay'));
  assert.ok(overlayHost.querySelector('.board-overlay'));
});

scenario('several boards float at once, each in its own window', async ({ harness, host, overlayHost, overlayState }) => {
  await harness.act(async () => { overlayState.openBoardOverlay('plan', 'Release plan'); await wait(); });
  await harness.act(async () => { overlayState.openBoardOverlay('map', 'Team map'); await wait(); await wait(); });
  const [first, second] = overlayHost.querySelectorAll('.board-overlay');
  assert.deepEqual([...overlayHost.querySelectorAll('.board-overlay-title')].map((title) => title.textContent.trim()), ['✎ Release plan', '✎ Team map']);
  assert.notEqual(first.style.left, second.style.left, 'stepped clear of each other');
  assert.deepEqual(tabLabels(host), []);
  assert.ok(second.classList.contains('overlay-window-front'));
  assert.ok(Number(first.style.getPropertyValue('--overlay-level')) < Number(second.style.getPropertyValue('--overlay-level')));
});

scenario('a board minimizes to the dock and comes back', async ({ harness, overlayHost, overlayState }) => {
  await harness.act(async () => { overlayState.openBoardOverlay('plan', 'Release plan'); await wait(); });
  await harness.act(async () => { overlayHost.querySelector('.overlay-wc-minimize').click(); await wait(); });
  assertAbsent(overlayHost.querySelector('.board-overlay'));
  const item = overlayHost.querySelector('.overlay-dock-item');
  assert.match(item.textContent, /Release plan/);
  await harness.act(async () => { item.click(); await wait(); });
  assert.ok(overlayHost.querySelector('.board-overlay'));
  assertAbsent(overlayHost.querySelector('.overlay-dock'));
});

scenario('opening the board the overlay shows brings the window forward, not a second editor', async ({ harness, host, overlayHost, overlayState }) => {
  const { raiseOverlayWindow } = await harness.importDashboardModule('js/overlay-window.js');
  await harness.act(async () => { overlayState.openBoardOverlay('plan', 'Release plan'); await wait(); });
  await harness.act(async () => { raiseOverlayWindow(''); await wait(); });
  await harness.act(async () => { assert.equal(await overlayState.openBoardPane('plan'), null); await wait(); });
  assert.deepEqual(tabLabels(host), []);
  assert.ok(overlayHost.querySelector('.board-overlay').classList.contains('overlay-window-front'));
});

scenario('clicking the overlay or the companion brings that one forward', async ({ harness, overlayHost, overlayState }) => {
  const doc = harness.document;
  const { OverlayWindow, useOverlayWindow } = await harness.importDashboardModule('js/overlay-window.js');
  const Other = () => {
    const overlay = useOverlayWindow({ win: harness.window, defaultPlacement: () => ({ x: 0, y: 0, w: 400, h: 300, snap: '' }) });
    return harness.html`<${OverlayWindow} id="companion" window=${overlay} className="companion-window"
      frameClass="companion-resize-frame" titlebarClass="companion-titlebar" label="Companion" />`;
  };
  const other = await harness.mount(harness.html`<${Other} />`);
  await harness.act(async () => { overlayState.openBoardOverlay('plan', 'Release plan'); await wait(); });
  const companion = other.container.querySelector('.companion-window');
  const board = () => overlayHost.querySelector('.board-overlay');
  const press = (node) => harness.act(async () => { node.dispatchEvent(new harness.window.Event('pointerdown', { bubbles: true })); await wait(); });
  await press(companion);
  assert.equal(board().classList.contains('overlay-window-front'), false);
  await press(overlayHost.querySelector('.board-overlay-titlebar'));
  assert.equal(board().classList.contains('overlay-window-front'), true);
  await press(overlayHost.querySelector('.board-overlay-resize-frame .window-resize-zone.e'));
  assert.equal(board().classList.contains('overlay-window-front'), true);
  await press(doc.querySelector('main'));
  assert.equal(board().classList.contains('overlay-window-front'), true, 'the page behind changes nothing');
  await other.unmount();
});

test('the overlay placement stays reachable and resizing holds the opposite corner', async (t) => {
  const harness = await createPreactHarness(t);
  const { defaultOverlayPlacement } = await harness.importDashboardModule('js/board-overlay.js');
  const { resizedPlacement } = await harness.importDashboardModule('js/overlay-window-model.js');
  const { defaultPlacement } = await harness.importDashboardModule('js/companion-model.js');
  const viewport = { width: 1600, height: 900 };
  const placed = defaultOverlayPlacement(viewport);
  assert.ok(placed.x + placed.w < defaultPlacement(viewport).x + 400, 'it does not open on top of the companion');
  const small = defaultOverlayPlacement({ width: 500, height: 400 });
  assert.ok(small.x + small.w > 0 && small.y < 400, 'it opens on screen in a small window');

  const start = { x: 300, y: 200, w: 600, h: 400, snap: '' };
  const se = resizedPlacement(start, 'se', 50, 30, viewport);
  assert.deepEqual([se.x, se.y, se.w, se.h], [300, 200, 650, 430]);
  const nw = resizedPlacement(start, 'nw', -50, -30, viewport);
  assert.deepEqual([nw.x, nw.y, nw.w, nw.h], [250, 170, 650, 430]);
  assert.equal(nw.x + nw.w, start.x + start.w, 'the far corner stays put');
  const tiny = resizedPlacement(start, 'sw', 5000, -5000, viewport);
  assert.equal(tiny.x + tiny.w, start.x + start.w, 'and past the minimum size too');
  assert.ok(tiny.w >= 360 && tiny.h >= 220);
  const top = resizedPlacement(start, 'ne', 0, -5000, viewport);
  assert.equal(top.y, 0);
  assert.equal(top.y + top.h, start.y + start.h, 'the top edge stops at the viewport');
});

test('a window opens where it was left if that fits, moved in first, and shrunk last', async (t) => {
  const harness = await createPreactHarness(t);
  const { OVERLAY_FOOTER_ROOM, fitPlacement } = await harness.importDashboardModule('js/overlay-window-model.js');
  const box = ({ x, y, w, h }) => [x, y, w, h];
  const viewport = { width: 1000, height: 700 };
  const saved = { x: 100, y: 80, w: 600, h: 400, snap: '' };
  assert.deepEqual(box(fitPlacement(saved, viewport)), [100, 80, 600, 400], 'a place that fits is untouched');
  assert.deepEqual(box(fitPlacement({ ...saved, x: 900, y: 650 }, viewport)), [400, 700 - OVERLAY_FOOTER_ROOM - 400, 600, 400],
    'one past the corner moves in at full size');
  assert.deepEqual(box(fitPlacement({ ...saved, x: -300, y: -50 }, viewport)), [0, 0, 600, 400], 'and from the other side');
  const big = fitPlacement({ ...saved, w: 1800, h: 1200 }, viewport);
  assert.ok(big.x >= 0 && big.x + big.w <= 1000, 'one too wide fits after shrinking');
  assert.ok(big.y >= 0 && big.y + big.h <= 700 - OVERLAY_FOOTER_ROOM, 'one too tall too');
  const tiny = fitPlacement(saved, { width: 300, height: 150 });
  assert.deepEqual([tiny.w, tiny.h], [360, 220], 'the minimum size holds');
  assert.equal(tiny.x + tiny.w, 300, 'with the title bar controls still on screen');
  assert.equal(tiny.y, 0);
});

test('an edge resizes one side only and holds the opposite edge', async (t) => {
  const harness = await createPreactHarness(t);
  const { OVERLAY_FOOTER_ROOM, resizedPlacement } = await harness.importDashboardModule('js/overlay-window-model.js');
  const viewport = { width: 1600, height: 900 };
  const start = { x: 300, y: 200, w: 600, h: 400, snap: '' };
  const box = ({ x, y, w, h }) => [x, y, w, h];
  assert.deepEqual(box(resizedPlacement(start, 'e', 50, 30, viewport)), [300, 200, 650, 400]);
  assert.deepEqual(box(resizedPlacement(start, 's', 50, 30, viewport)), [300, 200, 600, 430]);
  assert.deepEqual(box(resizedPlacement(start, 'w', -50, 30, viewport)), [250, 200, 650, 400]);
  assert.deepEqual(box(resizedPlacement(start, 'n', 50, -30, viewport)), [300, 170, 600, 430]);

  const narrow = resizedPlacement(start, 'w', 5000, 0, viewport);
  assert.equal(narrow.w, 360, 'the minimum width stops a left edge');
  assert.equal(narrow.x + narrow.w, 900, 'and the right edge stays');
  const short = resizedPlacement(start, 'n', 0, 5000, viewport);
  assert.equal(short.h, 220, 'the minimum height stops a top edge');
  assert.equal(short.y + short.h, 600, 'and the bottom edge stays');

  assert.deepEqual(box(resizedPlacement(start, 'w', -5000, 0, viewport)), [0, 200, 900, 400], 'the viewport stops a left edge');
  assert.equal(resizedPlacement(start, 'e', 5000, 0, viewport).w, 1300, 'and a right edge');
  const low = resizedPlacement(start, 's', 0, 5000, viewport);
  assert.equal(low.y + low.h, 900 - OVERLAY_FOOTER_ROOM, 'and a bottom edge, above the footer');

  const past = { x: -100, y: 200, w: 600, h: 400, snap: '' };
  assert.deepEqual(box(resizedPlacement(past, 'w', 10, 0, viewport)), [-90, 200, 590, 400], 'a window past the limit shrinks without jumping');
  assert.deepEqual(box(resizedPlacement(past, 'w', -10, 0, viewport)), [-100, 200, 600, 400], 'and does not grow further past it');
});

scenario('a smaller browser fits the window without losing its saved size', async ({ harness, overlayHost, overlayState, dashPrefs }) => {
  const key = 'tclaude.dash.board-overlay.placement.plan';
  harness.window.innerWidth = 1600;
  harness.window.innerHeight = 900;
  dashPrefs.setItem(key, JSON.stringify({ x: 800, y: 300, w: 760, h: 500, snap: '' }));
  await harness.act(async () => { overlayState.openBoardOverlay('plan', 'plan'); await wait(); });
  const frame = overlayHost.querySelector('.board-overlay');
  const box = () => ['left', 'top', 'width', 'height'].map((key) => parseFloat(frame.style[key]));
  const resize = (w, h) => harness.act(async () => {
    harness.window.innerWidth = w;
    harness.window.innerHeight = h;
    harness.window.dispatchEvent(new harness.window.Event('resize'));
    await wait();
  });
  assert.deepEqual(box(), [800, 300, 760, 500]);
  await resize(1000, 600);
  const [x, y, w, h] = box();
  assert.equal(w, 760, 'it moves in before it shrinks');
  assert.equal(h, 500);
  assert.ok(x + w <= 1000 && y + h <= 600);
  await resize(600, 400);
  assert.ok(box()[2] < 760, 'and shrinks when it has to');
  const before = dashPrefs.getItem(key);
  const title = overlayHost.querySelector('.board-overlay-titlebar');
  const event = (type, x, y) => Object.assign(new harness.window.Event(type, { bubbles: true }), { button: 0, pointerId: 1, clientX: x, clientY: y });
  await harness.act(async () => {
    harness.fireEvent(title, 'pointerdown', { button: 0, pointerId: 1, clientX: 100, clientY: 10 });
    if (!('onpointerdown' in title)) harness.fireEvent(title, 'PointerDown', { button: 0, pointerId: 1, clientX: 100, clientY: 10 });
    title.dispatchEvent(event('pointermove', 90, 15));
    title.dispatchEvent(event('pointerup', 90, 15));
    await wait();
  });
  const moved = JSON.parse(dashPrefs.getItem(key));
  assert.deepEqual([moved.w, moved.h], [760, 500], 'a move keeps the saved size');
  assert.notEqual(dashPrefs.getItem(key), before);
  await resize(1600, 900);
  assert.deepEqual(box().slice(2), [760, 500], 'a larger browser brings it back');

  await resize(600, 400);
  const press = async (node, dx) => harness.act(async () => {
    harness.fireEvent(node, 'pointerdown', { button: 0, pointerId: 1, clientX: 100, clientY: 10 });
    if (!('onpointerdown' in node)) harness.fireEvent(node, 'PointerDown', { button: 0, pointerId: 1, clientX: 100, clientY: 10 });
    if (dx) node.dispatchEvent(event('pointermove', 100 + dx, 10));
    node.dispatchEvent(event('pointerup', 100 + dx, 10));
    await wait();
  });
  const held = dashPrefs.getItem(key);
  await press(title, 0);
  await press(overlayHost.querySelector('.window-resize-zone.e'), 0);
  assert.equal(dashPrefs.getItem(key), held, 'a click saves nothing');
  await press(overlayHost.querySelector('.window-resize-zone.e'), -20);
  assert.equal(JSON.parse(dashPrefs.getItem(key)).h, 500, 'a side drag keeps the saved height');
});

scenario('dragging the title bar moves the window and saves its place', async ({ harness, overlayHost, overlayState, dashPrefs, island }) => {
  harness.window.innerWidth = 1600;
  harness.window.innerHeight = 900;
  await harness.act(async () => { overlayState.openBoardOverlay('plan', 'Release plan'); await wait(); });
  const bar = overlayHost.querySelector('.board-overlay-titlebar');
  const before = overlayHost.querySelector('.board-overlay').getAttribute('style');
  const fire = (type, x, y) => {
    const event = new harness.window.Event(type, { bubbles: true, cancelable: true });
    Object.assign(event, { button: 0, pointerId: 1, clientX: x, clientY: y });
    bar.dispatchEvent(event);
  };
  await harness.act(async () => {
    harness.fireEvent(bar, 'pointerdown', { button: 0, pointerId: 1, clientX: 100, clientY: 100 });
    if (!('onpointerdown' in bar)) harness.fireEvent(bar, 'PointerDown', { button: 0, pointerId: 1, clientX: 100, clientY: 100 });
    fire('pointermove', 180, 60);
    fire('pointerup', 180, 60);
    await wait();
  });
  const saved = JSON.parse(dashPrefs.getItem(`${island.BOARD_OVERLAY_PLACEMENT_PREF}.plan`));
  assert.ok(saved, 'the place is saved');
  assert.notEqual(overlayHost.querySelector('.board-overlay').getAttribute('style'), before);
  assert.equal(overlayHost.querySelector('.board-overlay').style.left, `${saved.x}px`);
});

scenario('with features.collab off the overlay does not show, and comes back with the flag', { collab_enabled: false }, async ({ harness, overlayHost, overlayState, dashboardState }) => {
  await harness.act(async () => { overlayState.openBoardOverlay('plan', 'Release plan'); await wait(); });
  assertAbsent(overlayHost.querySelector('.board-overlay'));
  await harness.act(async () => { dashboardState.publishLocalEdit(COLLAB_ON); await wait(); });
  assert.ok(overlayHost.querySelector('.board-overlay'));
});

const dragTo = (harness, node, from, to) => harness.act(async () => {
  const event = (type, [clientX, clientY]) => Object.assign(new harness.window.Event(type, { bubbles: true, cancelable: true }), { button: 0, pointerId: 1, clientX, clientY });
  node.dispatchEvent(event('pointerdown', from));
  node.dispatchEvent(event('pointermove', to));
  node.dispatchEvent(event('pointerup', to));
  await wait();
});

scenario('the overlay snaps to edges and corners, keeps a resized panel, and maximizes', async ({ harness, overlayHost, overlayState, island, dashPrefs, dashboardState }) => {
  dashboardState.publishLocalEdit({ ...COLLAB_ON, window_controls: 'windows' });
  await harness.act(async () => { overlayState.openBoardOverlay('plan', 'Release plan'); await wait(); });
  const board = () => overlayHost.querySelector('.board-overlay');
  const bar = () => overlayHost.querySelector('.board-overlay-titlebar');
  const saved = () => JSON.parse(dashPrefs.getItem(`${island.BOARD_OVERLAY_PLACEMENT_PREF}.plan`));
  const width = harness.document.documentElement.clientWidth || harness.window.innerWidth || 1280;
  await dragTo(harness, bar(), [300, 400], [width - 2, 400]);
  assert.ok(board().classList.contains('snapped-right'));
  assert.equal(saved().snap, 'right');
  assert.equal(overlayHost.querySelector('.board-overlay-resize-frame'), null, 'a snapped window resizes along its free edge');
  await dragTo(harness, overlayHost.querySelector('.overlay-window-edge.w'), [800, 300], [760, 300]);
  const panel = board().style.width;
  assert.equal(saved().panels.right.w, parseInt(panel, 10), 'the panel width is saved');
  await harness.act(async () => { overlayHost.querySelector('.overlay-wc-maximize').click(); await wait(); });
  assert.ok(board().classList.contains('snapped-max'));
  await harness.act(async () => { overlayHost.querySelector('.overlay-wc-maximize').click(); await wait(); });
  assert.ok(board().classList.contains('snapped-right'));
  assert.equal(board().style.width, panel, 'restore brings the panel back at its size');
  await dragTo(harness, bar(), [900, 100], [2, 2]);
  assert.ok(board().classList.contains('snapped-top-left'), 'a corner');
  assert.deepEqual([...overlayHost.querySelectorAll('.overlay-window-edge')].map((node) => node.className.split(' ').pop()), ['e', 's', 'se']);
});

scenario('the overlay has the native controls, and classic keeps its ×', async ({ harness, overlayHost, overlayState, dashboardState }) => {
  dashboardState.publishLocalEdit({ ...COLLAB_ON, window_controls: 'linux' });
  await harness.act(async () => { overlayState.openBoardOverlay('plan', 'Release plan'); await wait(); });
  assert.ok(overlayHost.querySelector('.overlay-wc-minimize'), 'it minimizes to the dock');
  assert.ok(overlayHost.querySelector('.overlay-wc-maximize'));
  assert.equal(overlayHost.querySelector('.board-overlay-close'), null);
  await harness.act(async () => { dashboardState.publishLocalEdit({ ...COLLAB_ON, window_controls: 'classic' }); await wait(); });
  assert.equal(overlayHost.querySelector('.overlay-window-controls'), null);
  assert.ok(overlayHost.querySelector('.board-overlay-close'));
  await harness.act(async () => { dashboardState.publishLocalEdit({ ...COLLAB_ON, window_controls: 'linux' }); await wait(); });
  await harness.act(async () => { overlayHost.querySelector('.overlay-wc-close').click(); await wait(); });
  assert.equal(overlayState.boardOverlays.value.length, 0);
});

scenario('Ctrl+wheel over the title bar goes to the board, not the page', async ({ harness, overlayHost, overlayState }) => {
  await harness.act(async () => { overlayState.openBoardOverlay('plan', 'Release plan'); await wait(); });
  const bar = overlayHost.querySelector('.board-overlay-titlebar');
  const editor = overlayHost.querySelector('.board-editor');
  const heard = [];
  editor.addEventListener('wheel', (event) => heard.push(event.deltaY));
  // This DOM has no WheelEvent, and no onwheel, so Preact listens for 'Wheel'.
  harness.window.WheelEvent ??= class extends harness.window.Event {
    constructor(type, { deltaY, ctrlKey, metaKey, ...init }) {
      super(type, init);
      Object.assign(this, { deltaY, ctrlKey, metaKey });
    }
  };
  const type = 'onwheel' in bar ? 'wheel' : 'Wheel';
  const wheel = (init) => Object.assign(new harness.window.Event(type, { bubbles: true, cancelable: true }), { deltaY: -120, ...init });
  const plain = wheel({});
  bar.dispatchEvent(plain);
  assert.equal(plain.defaultPrevented, false, 'a plain wheel is not the board\'s');
  assert.deepEqual(heard, []);
  const zoom = wheel({ ctrlKey: true });
  bar.dispatchEvent(zoom);
  assert.equal(zoom.defaultPrevented, true, 'the page does not zoom');
  assert.deepEqual(heard, [-120]);
});
