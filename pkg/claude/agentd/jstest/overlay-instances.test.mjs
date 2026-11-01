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
  function Win({ id }) {
    const window = overlay.useOverlayWindow({ id, win: harness.window, prefs, prefKey: `p.${id}`, defaultPlacement: () => ({ x: 0, y: 0, w: 400, h: 300, snap: '' }), snaps: true });
    return harness.html`<${overlay.OverlayWindow} id=${id} window=${window} className=${`w-${id}`} frameClass="f" titlebarClass="t" label=${id} title="x">body<//>`;
  }
  const { h } = harness.preact;
  const mounted = await harness.mount(h('div', null, ...['a', 'b', 'c'].map((id) => h(Win, { key: id, id }))));
  const level = (id) => Number(mounted.container.querySelector(`.w-${id}`).style.getPropertyValue('--overlay-level'));
  const lineLevel = () => Number(mounted.container.querySelector('.overlay-split-line').style.zIndex);
  return { harness, overlay, instances, prefs, mounted, level, lineLevel };
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

test('closing a window gives its place in a split to its sibling', async (t) => {
  const { harness, instances, overlay, prefs } = await setup(t, PAIR);
  await harness.act(() => instances.openOverlayWindow({ id: 'a', kind: 'k', label: 'A' }));
  await harness.act(() => instances.closeOverlayWindow('a'));
  assert.equal(prefs.getItem(overlay.OVERLAY_SPLIT_PREF), '', 'one window is not a split');
  assert.equal(instances.overlayWindows.value.length, 0);
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
