// Moving the minimized chips between snap spots (tcl-avebwq): a press that
// travels drags the row, a short one stays a click, Esc cancels, and the spot
// survives a reload and places the window that opens from it. The release's
// click is swallowed in the capture phase, which linkedom doesn't run, so that
// part is checked in a real browser.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const CHIP_SPOT = 'tclaude.dash.companion.chip-spot';
const WINDOW_SPOT = 'tclaude.dash.companion.window-spot';

function memoryPrefs(initial = {}) {
  const store = new Map(Object.entries(initial));
  return { getItem: (k) => (store.has(k) ? store.get(k) : null), setItem: (k, v) => store.set(k, String(v)), store };
}

async function setup(t, prefs = memoryPrefs()) {
  const harness = await createPreactHarness(t);
  const { CompanionApp } = await harness.importDashboardModule('js/companion-island.js');
  const snapshot = harness.signals.signal({ groups: [] });
  const activeTab = harness.signals.signal('groups');
  const view = await harness.mount(harness.html`<${CompanionApp} snapshot=${snapshot} activeTab=${activeTab} prefs=${prefs}
    sessionPrefs=${memoryPrefs()} widgetFactory=${() => ({ connect: async () => {}, dispose() {}, setActive() {}, focus() {} })}
    registerController=${() => () => {}} />`);
  const row = () => view.container.querySelector('.companion-chips');
  const chip = () => view.container.querySelector('.companion-chip');
  const pointerEvent = (type, x, y) => Object.assign(new harness.window.Event(type, { bubbles: true, cancelable: true }), { pointerId: 1, button: 0, clientX: x, clientY: y });
  const pointer = (type, x, y, target = harness.window) => harness.act(() => target.dispatchEvent(pointerEvent(type, x, y)));
  // linkedom has no layout: lay the outlines out on a 1000x600 screen.
  const layOutSlots = () => {
    const at = { 'top-left': [0, 0], 'top-right': [900, 0], 'bottom-left': [0, 560], 'bottom-center': [450, 560], 'bottom-right': [900, 560] };
    for (const slot of view.container.querySelectorAll('[data-chip-spot]')) {
      const [left, top] = at[slot.dataset.chipSpot] || [-5000, -5000];
      slot.getBoundingClientRect = () => ({ left, top, width: 100, height: 40, right: left + 100, bottom: top + 40 });
    }
  };
  row().getBoundingClientRect = () => ({ left: 900, top: 560, width: 100, height: 40, right: 1000, bottom: 600 });
  return { harness, view, prefs, row, chip, pointer, layOutSlots };
}

test('a short press stays a click and opens the window', async (t) => {
  const { view, chip, pointer, row } = await setup(t);
  await pointer('pointerdown', 950, 580, chip());
  await pointer('pointermove', 952, 581);
  assert.equal(view.container.querySelector('[data-chip-spot]'), null, 'no outlines for a nudge');
  await pointer('pointerup', 952, 581);
  chip().click();
  await pointer('pointermove', 0, 0);
  assert.equal(row(), null, 'the click opened the window');
});

test('a drag lights the nearest spot and lands there', async (t) => {
  const { view, chip, row, pointer, layOutSlots, prefs } = await setup(t);
  await pointer('pointerdown', 950, 580, chip());
  await pointer('pointermove', 900, 580);
  assert.equal(view.container.querySelectorAll('[data-chip-spot]').length, 16, 'an outline at every spot');
  assert.match(row().className, /dragging/);
  layOutSlots();
  await pointer('pointermove', 60, 30);
  assert.equal(view.container.querySelector('.companion-chip-slot.near').dataset.chipSpot, 'top-left');
  await pointer('pointerup', 60, 30);
  assert.match(row().className, /companion-spot-top-left/);
  assert.doesNotMatch(row().className, /dragging/);
  assert.equal(row().style.translate, '', 'the row is back in the layout');
  assert.equal(prefs.store.get(CHIP_SPOT), 'top-left');
  assert.equal(prefs.store.get(WINDOW_SPOT), 'top-left');
  assert.equal(view.container.querySelector('[data-chip-spot]'), null);
});

test('Esc during a drag cancels', async (t) => {
  const { harness, view, chip, row, pointer, layOutSlots, prefs } = await setup(t);
  await pointer('pointerdown', 950, 580, chip());
  await pointer('pointermove', 900, 580);
  layOutSlots();
  await pointer('pointermove', 60, 30);
  await harness.act(() => harness.window.dispatchEvent(Object.assign(new harness.window.Event('keydown', { bubbles: true, cancelable: true }), { key: 'Escape' })));
  assert.equal(view.container.querySelector('[data-chip-spot]'), null);
  await pointer('pointerup', 60, 30);
  assert.match(row().className, /companion-spot-bottom-right/);
  assert.equal(prefs.store.has(CHIP_SPOT), false, 'nothing saved');
});

test('a row wider than half the room still reaches a quarter point', async (t) => {
  const { view, row, chip, pointer, layOutSlots, prefs } = await setup(t);
  row().getBoundingClientRect = () => ({ left: 300, top: 560, width: 700, height: 40, right: 1000, bottom: 600 });
  await pointer('pointerdown', 950, 580, chip());
  await pointer('pointermove', 940, 580);
  layOutSlots();
  for (const slot of view.container.querySelectorAll('[data-chip-spot]')) {
    if (slot.dataset.chipSpot === 'bottom-q1') slot.getBoundingClientRect = () => ({ left: 76, top: 560, width: 348, height: 40, right: 424, bottom: 600 });
    if (slot.dataset.chipSpot === 'bottom-left') slot.getBoundingClientRect = () => ({ left: 16, top: 560, width: 700, height: 40, right: 716, bottom: 600 });
  }
  // The row is held at x 0 here, its centre at 350, nearer bottom-left's 366.
  await pointer('pointermove', 500, 580);
  await pointer('pointerup', 500, 580);
  assert.equal(prefs.getItem(CHIP_SPOT), 'bottom-q1');
});

test('the spot survives a reload', async (t) => {
  const { row } = await setup(t, memoryPrefs({ [CHIP_SPOT]: 'bottom-center' }));
  assert.match(row().className, /companion-spot-bottom-center/);
});

test('every spot saved before there were sixteen still means the same place', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/companion-model.js');
  assert.equal(m.CHIP_SPOTS.length, 16);
  for (const spot of ['top-left', 'top-right', 'bottom-left', 'bottom-center', 'bottom-right']) assert.equal(m.parseChipSpot(spot), spot);
  assert.equal(m.DEFAULT_CHIP_SPOT, 'bottom-right');
});

test('a nonsense saved spot falls back', async (t) => {
  const { row } = await setup(t, memoryPrefs({ [CHIP_SPOT]: 'ceiling' }));
  assert.match(row().className, /companion-spot-bottom-right/);
});

test('model: a window at a spot stays on screen, the peek opens on the free side', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/companion-model.js');
  const o = await harness.importDashboardModule('js/overlay-window-model.js');
  const vp = { width: 1000, height: 600 };
  const p = { x: 300, y: 100, w: 640, h: 420, snap: 'left' };
  assert.deepEqual(m.spotPlacement('top-left', p, vp, 90), { x: 16, y: 102, w: 640, h: 420, snap: '' });
  assert.deepEqual(m.spotPlacement('bottom-right', p, vp, 90), { x: 344, y: 139, w: 640, h: 420, snap: '' });
  assert.equal(m.spotPlacement('bottom-center', p, vp).x, 180);
  const tiny = m.spotPlacement('top-right', p, { width: 500, height: 400 }, 300);
  assert.ok(tiny.x >= 0 && tiny.x + tiny.w <= 500 && tiny.y + tiny.h <= 400 - o.OVERLAY_FOOTER_ROOM);
  const rect = { left: 16, right: 150, top: 100, bottom: 134 };
  assert.deepEqual(m.peekAnchor('top-left', rect, vp), { top: 142, left: 16 });
  assert.deepEqual(m.peekAnchor('bottom-right', rect, vp), { bottom: 508, right: 850 });
  assert.deepEqual(m.peekAnchor('left-q1', rect, vp), { top: 142, left: 16 });
  assert.deepEqual(m.peekAnchor('left-q3', rect, vp), { bottom: 508, left: 16 });
  assert.deepEqual(m.peekAnchor('right-center', rect, vp), { bottom: 508, right: 850 });
  assert.deepEqual(m.peekAnchor('top-q3', rect, vp), { top: 142, right: 850 });
  for (const viewport of [vp, { width: 1600, height: 900 }, { width: 500, height: 400 }]) {
    for (const spot of m.CHIP_SPOTS) {
      const { x, y, w, h } = m.spotPlacement(spot, p, viewport, 90);
      const small = viewport.width < 700;
      assert.ok(x >= 0 && y >= 0 && (small || (x + w <= viewport.width && y + h <= viewport.height - o.OVERLAY_FOOTER_ROOM)), `${spot} at ${viewport.width}`);
    }
  }
  assert.equal(m.spotPlacement('left-center', p, { width: 1600, height: 900 }, 90).x, 16);
  assert.equal(m.spotPlacement('top-q1', p, { width: 1600, height: 900 }, 90).x, 16 + (1600 - 640 - 32) / 4);
});
