// Tests for overlay window instances (tcl-ppvf8g): any number of windows, each
// with its own id, and what stacking and a split do per instance.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const memoryPrefs = () => {
  const values = new Map();
  return { getItem: (key) => values.get(key) ?? null, setItem: (key, value) => values.set(key, String(value)) };
};

async function setup(t, tree = null) {
  const harness = await createPreactHarness(t);
  const [overlay, instances] = await Promise.all([
    harness.importDashboardModule('js/overlay-window.js'),
    harness.importDashboardModule('js/overlay-instances.js'),
  ]);
  const prefs = memoryPrefs();
  if (tree) prefs.setItem(overlay.OVERLAY_SPLIT_PREF, JSON.stringify(tree));
  for (const id of ['a', 'b', 'c']) prefs.setItem(`p.${id}`, JSON.stringify({ x: 0, y: 0, w: 400, h: 300, snap: id === 'c' ? '' : 'left' }));
  const refocused = [];
  function Win({ id }) {
    const window = overlay.useOverlayWindow({ id, onRefocus: () => refocused.push(id), win: harness.window, prefs, prefKey: `p.${id}`, defaultPlacement: () => ({ x: 0, y: 0, w: 400, h: 300, snap: '' }), snaps: true });
    return harness.html`<${overlay.OverlayWindow} id=${id} window=${window} className=${`w-${id}`} frameClass="f" titlebarClass="t" label=${id} title="x">body<//>`;
  }
  const { h } = harness.preact;
  const mounted = await harness.mount(h('div', null, ...['a', 'b', 'c'].map((id) => h(Win, { key: id, id }))));
  const level = (id) => Number(mounted.container.querySelector(`.w-${id}`).style.getPropertyValue('--overlay-level'));
  const lineLevel = () => Number(mounted.container.querySelector('.overlay-split-line').style.zIndex);
  return { harness, overlay, instances, prefs, mounted, level, lineLevel, refocused };
}

const PAIR = { axis: 'x', at: 0.5, a: { id: 'a' }, b: { id: 'b' } };

test('a window raised over a split sits above the split and its lines', async (t) => {
  const { harness, overlay, level, lineLevel } = await setup(t, PAIR);
  await harness.act(() => overlay.raiseOverlayWindow('a'));
  assert.equal(level('a'), level('b') + 1, 'a split moves as one, its front window one above the rest');
  assert.equal(lineLevel(), level('a') + 1, 'its lines sit just above it');
  assert.ok(lineLevel() < 950, 'under agent marks');
  await harness.act(() => overlay.raiseOverlayWindow('c'));
  assert.ok(level('c') > lineLevel(), 'so a window raised over it covers them');
  await harness.act(() => overlay.raiseOverlayWindow('b'));
  assert.ok(level('c') < level('a'), 'and clicking the split brings all of it back');
  assert.ok(level('c') < lineLevel());
});

test('a click raises one of two windows in a split, for when an unlocked line lets them overlap', async (t) => {
  const { harness, overlay, level } = await setup(t, PAIR);
  await harness.act(() => overlay.raiseOverlayWindow('b'));
  assert.ok(level('b') > level('a'));
  await harness.act(() => overlay.raiseOverlayWindow('a'));
  assert.ok(level('a') > level('b'));
});

test('every window has its own level from the start, so a reload keeps lines under what is over them', async (t) => {
  const { overlay, level, lineLevel } = await setup(t, PAIR);
  assert.ok(level('c') > lineLevel(), 'c mounted last, over the split');
  assert.equal(overlay.frontOverlayWindow.value, 'c');
  assert.ok(level('c') < 950, 'under agent marks');
  assert.ok(level('a') >= overlay.OVERLAY_BOTTOM_LEVEL);
});

test('closing a window leaves its cell empty, and the last one ends the split', async (t) => {
  const { harness, instances, overlay, prefs } = await setup(t, PAIR);
  await harness.act(() => instances.openOverlayWindow({ id: 'a', kind: 'k', label: 'A' }));
  await harness.act(() => instances.closeOverlayWindow('a'));
  assert.deepEqual(JSON.parse(prefs.getItem(overlay.OVERLAY_SPLIT_PREF)), { ...PAIR, a: { gap: true } });
  assert.equal(instances.overlayWindows.value.length, 0);
  await harness.act(() => instances.openOverlayWindow({ id: 'b', kind: 'k', label: 'B' }));
  await harness.act(() => instances.closeOverlayWindow('b'));
  assert.equal(prefs.getItem(overlay.OVERLAY_SPLIT_PREF), '');
});

test('a click on a title bar brings focus to the window, and a control keeps its own', async (t) => {
  const { harness, mounted } = await setup(t, PAIR);
  const section = mounted.container.querySelector('.w-a');
  const title = section.querySelector('.t');
  const click = (node) => harness.act(() => node.dispatchEvent(new harness.window.Event('click', { bubbles: true })));
  await click(title);
  assert.ok(harness.document.activeElement === section, 'the border shows on this window');
  const other = mounted.container.querySelector('.w-b');
  await click(other.querySelector('.t'));
  assert.ok(harness.document.activeElement === other, 'and moves with the next click, from another window');
});

test('a click that left focus on the window itself hands it to the content', async (t) => {
  const { harness, mounted, refocused } = await setup(t, PAIR);
  const section = mounted.container.querySelector('.w-a');
  section.focus();
  await harness.act(() => section.querySelector('.t').dispatchEvent(new harness.window.Event('click', { bubbles: true })));
  assert.deepEqual(refocused, ['a'], 'as a browser leaves it after a press on content that takes no focus');
});

test('a window opens once, and comes back rather than opening twice', async (t) => {
  const { instances } = await setup(t);
  instances.openOverlayWindow({ id: 'x', kind: 'k', label: 'X' });
  instances.minimizeOverlayWindow('x');
  assert.equal(instances.overlayWindows.value[0].minimized, true);
  instances.openOverlayWindow({ id: 'x', kind: 'k', label: 'X' });
  assert.equal(instances.overlayWindows.value.length, 1);
  assert.equal(instances.overlayWindows.value[0].minimized, false);
});

test('the open windows survive a reload, and invalid ones do not', async (t) => {
  const { harness, instances } = await setup(t);
  const store = new Map([[instances.OVERLAY_WINDOWS_KEY, JSON.stringify([{ id: 'x', kind: 'k' }, { id: 'y', kind: 'bad' }, 'junk'])]]);
  const win = { sessionStorage: { getItem: (key) => store.get(key) ?? null, setItem: (key, value) => store.set(key, value) } };
  const stop = instances.keepOverlayWindows(win, (window) => window.kind === 'k');
  t.after(stop);
  assert.deepEqual(instances.overlayWindows.value.map((window) => window.id), ['x']);
  instances.openOverlayWindow({ id: 'z', kind: 'k', label: 'Z', at: { x: 1, y: 2 } });
  const saved = JSON.parse(store.get(instances.OVERLAY_WINDOWS_KEY));
  assert.deepEqual(saved.map((window) => window.id), ['x', 'z']);
  assert.equal(saved[1].at, undefined, 'a drop point is not a place to reopen at');
  assert.ok(harness);
});

// A pointer on an unlocked edge of the window, in the room's coordinates.
function edgeDrag(harness, container, overlayClass, side) {
  const handle = container.querySelector(`.${overlayClass} .overlay-window-edge.${side}`);
  const fire = (type, x) => harness.act(() => handle.dispatchEvent(Object.assign(new harness.window.Event(type, { bubbles: true, cancelable: true }), { button: 0, pointerId: 1, clientX: x, clientY: 300 })));
  return { down: (x) => fire('pointerdown', x), move: (x) => fire('pointermove', x), up: (x) => fire('pointerup', x), cancel: (x) => fire('pointercancel', x) };
}

async function spacedPair(t) {
  const s = await setup(t, { axis: 'x', at: 0.25, free: 0.75, a: { id: 'a' }, b: { id: 'b' } });
  s.harness.window.innerWidth = 1600;
  s.harness.window.innerHeight = 900;
  await s.harness.act(() => s.harness.window.dispatchEvent(new s.harness.window.Event('resize')));
  s.guide = () => s.mounted.container.querySelector('.overlay-snap-guide');
  s.saved = () => JSON.parse(s.prefs.getItem(s.overlay.OVERLAY_SPLIT_PREF));
  s.line = () => s.mounted.container.querySelector('.overlay-split-line');
  s.drag = edgeDrag(s.harness, s.mounted.container, 'w-a', 'e');
  return s;
}

test('an edge dragged near its partner shows the snap, and released there the line is shared again', async (t) => {
  const s = await spacedPair(t);
  await s.drag.down(400);
  await s.drag.move(900);
  assert.equal(s.guide(), null, 'still far off');
  await s.drag.move(1170);
  assert.equal(s.guide().style.width, '1200px', 'the preview shows where the window lands');
  await s.drag.up(1170);
  assert.equal(s.guide(), null);
  assert.equal(s.saved().free, undefined);
  assert.equal(s.saved().at, 0.75);
  assert.ok(s.line(), 'a shared line again');
});

test('released outside the reach an edge stays unlocked where it was let go', async (t) => {
  const s = await spacedPair(t);
  await s.drag.down(400);
  await s.drag.move(1170);
  await s.drag.move(800);
  assert.equal(s.guide(), null, 'leaving the reach takes the preview away');
  await s.drag.up(800);
  assert.deepEqual([s.saved().at, s.saved().free], [0.5, 0.75]);
  assert.equal(s.line(), null);
});

for (const [name, end] of [['Escape', (s) => s.harness.act(() => s.harness.window.dispatchEvent(Object.assign(new s.harness.window.Event('keydown', { cancelable: true }), { key: 'Escape' })))], ['a cancelled pointer', (s) => s.drag.cancel(1170)]]) {
  test(`${name} leaves an edge as it was, even inside the preview`, async (t) => {
    const s = await spacedPair(t);
    const before = s.prefs.getItem(s.overlay.OVERLAY_SPLIT_PREF);
    await s.drag.down(400);
    await s.drag.move(1170);
    await end(s);
    assert.equal(s.guide(), null);
    assert.equal(s.mounted.container.querySelector('.w-a').style.width, '400px', 'the window is back');
    await s.drag.move(1000);
    await s.drag.up(1000);
    assert.equal(s.mounted.container.querySelector('.w-a').style.width, '400px', 'and the drag is over');
    assert.equal(s.prefs.getItem(s.overlay.OVERLAY_SPLIT_PREF), before);
  });
}
