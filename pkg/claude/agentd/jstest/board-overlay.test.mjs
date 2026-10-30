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

  const [loader, overlayState, island, { dashboardState }, { dashPrefs }, { terminalShellState }, { BOARD_TABS_PREF }] = await Promise.all([
    harness.importDashboardModule('js/preact-loader.js'),
    harness.importDashboardModule('js/board-overlay-state.js'),
    harness.importDashboardModule('js/board-overlay.js'),
    harness.importDashboardModule('js/snapshot-store.js'),
    harness.importDashboardModule('js/prefs.js'),
    harness.importDashboardModule('js/terminal-shell-state.js'),
    harness.importDashboardModule('js/terminal-board-tabs.js'),
  ]);
  dashboardState.publishLocalEdit(snapshotValue);
  dashPrefs.removeItem(BOARD_TABS_PREF);
  dashPrefs.removeItem(island.BOARD_OVERLAY_PLACEMENT_PREF);
  overlayState.boardOverlay.value = null;
  const cleanups = [
    await loader.mountTerminalsFeature({ fetchImpl: async () => ({ ok: true }), widgetFactory: () => { throw new Error('no terminals here'); } }),
    await loader.mountBoardOverlayFeature({ notify: () => {} }),
  ];
  await harness.act(() => wait());
  return {
    dispose() {
      overlayState.boardOverlay.value = null;
      for (const cleanup of cleanups) cleanup?.();
    },
    harness, host, overlayHost, overlayState, island, dashboardState, dashPrefs, state: terminalShellState,
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
      await body(env);
    } finally {
      env.dispose();
    }
  });
};

const tabLabels = (host) => [...host.querySelectorAll('.mux-tab .mux-tab-label')].map((node) => node.textContent);

scenario('a board moves from its tab into the overlay and back', async ({ harness, host, overlayHost, overlayState, state, click, button }) => {
  await harness.act(async () => { await overlayState.openBoardPane('plan', 'Release plan'); await wait(); await wait(); });
  await click(button(host, 'Open in overlay'));

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
  assert.equal(overlayState.boardOverlay.value, null);
  assert.deepEqual(tabLabels(host), ['Release plan']);
  assert.equal(state.activeKey.value, 'board:plan');
  assert.ok(harness.document.getElementById('tab-terminals').classList.contains('active'), 'the tab is shown');
});

scenario('the tab context menu offers the overlay for a board', async ({ harness, host, overlayHost, overlayState, click, button }) => {
  await harness.act(async () => { await overlayState.openBoardPane('plan', 'Release plan'); await wait(); });
  await harness.act(async () => { harness.fireEvent(host.querySelector('.mux-tab'), 'contextmenu'); await wait(); });
  await click(button(host, 'Open in overlay'));
  assert.ok(overlayHost.querySelector('.board-overlay'));
});

scenario('one board floats at a time: the one displaced goes back to a tab', async ({ harness, host, overlayHost, overlayState }) => {
  await harness.act(async () => { overlayState.openBoardOverlay('plan', 'Release plan'); await wait(); });
  await harness.act(async () => { overlayState.openBoardOverlay('map', 'Team map'); await wait(); await wait(); });
  assert.equal(overlayHost.querySelectorAll('.board-overlay').length, 1);
  assert.match(overlayHost.querySelector('.board-overlay-title').textContent, /Team map/);
  assert.deepEqual(tabLabels(host), ['Release plan']);
});

scenario('opening the board the overlay shows brings the window forward, not a second editor', async ({ harness, host, overlayState }) => {
  const body = harness.document.body;
  await harness.act(async () => { overlayState.openBoardOverlay('plan', 'Release plan'); await wait(); });
  body.classList.remove('board-overlay-front');
  await harness.act(async () => { assert.equal(await overlayState.openBoardPane('plan'), null); await wait(); });
  assert.deepEqual(tabLabels(host), []);
  assert.ok(body.classList.contains('board-overlay-front'));
});

scenario('clicking the overlay or the companion brings that one forward', async ({ harness, overlayHost, overlayState }) => {
  const doc = harness.document;
  const body = doc.body;
  const companion = body.appendChild(doc.createElement('div'));
  companion.className = 'companion-window';
  await harness.act(async () => { overlayState.openBoardOverlay('plan', 'Release plan'); await wait(); });
  const press = (node) => node.dispatchEvent(new harness.window.Event('pointerdown', { bubbles: true }));
  press(companion);
  assert.equal(body.classList.contains('board-overlay-front'), false);
  press(overlayHost.querySelector('.board-overlay-titlebar'));
  assert.equal(body.classList.contains('board-overlay-front'), true);
  press(doc.querySelector('main'));
  assert.equal(body.classList.contains('board-overlay-front'), true, 'the page behind changes nothing');
});

test('the overlay placement stays reachable and resizing holds the opposite corner', async (t) => {
  const harness = await createPreactHarness(t);
  const { defaultOverlayPlacement, resizedPlacement } = await harness.importDashboardModule('js/board-overlay.js');
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
  const saved = JSON.parse(dashPrefs.getItem(island.BOARD_OVERLAY_PLACEMENT_PREF));
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
