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
  assert.ok(lineLevel() < 1090, 'under the snap guide, in the window layer');
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
  assert.ok(level('c') < 1090, 'under the snap guide, in the window layer');
  assert.ok(level('a') >= overlay.OVERLAY_BOTTOM_LEVEL);
});

test('closing a window leaves its cell empty, and the last one ends the split', async (t) => {
  const { harness, instances, overlay, prefs } = await setup(t, PAIR);
  await harness.act(() => instances.openOverlayWindow({ id: 'a', kind: 'k', label: 'A' }));
  await harness.act(() => instances.closeOverlayTab('a'));
  assert.deepEqual(JSON.parse(prefs.getItem(overlay.OVERLAY_SPLIT_PREF)), { ...PAIR, a: { gap: true } });
  assert.equal(instances.overlayWindows.value.length, 0);
  await harness.act(() => instances.openOverlayWindow({ id: 'b', kind: 'k', label: 'B' }));
  await harness.act(() => instances.closeOverlayTab('b'));
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
  const saved = JSON.parse(store.get(instances.OVERLAY_WINDOWS_KEY)).windows;
  assert.deepEqual(saved.map((window) => window.id), ['x', 'z']);
  assert.equal(saved[1].at, undefined, 'a drop point is not a place to reopen at');
  assert.ok(harness);
});

test('a window closed with its ✕ is not saved for a reload, and a minimized tab is', async (t) => {
  const { instances } = await setup(t);
  const store = new Map();
  const win = { sessionStorage: { getItem: (key) => store.get(key) ?? null, setItem: (key, value) => store.set(key, value) } };
  const stop = instances.keepOverlayWindows(win, () => true);
  t.after(() => { stop(); instances.overlayWindows.value = []; instances.minimizedOverlayTabs.value = []; });
  instances.overlayWindows.value = [{ id: 'w', tabs: [{ id: 'p', kind: 'k' }, { id: 'q', kind: 'k' }, { id: 'r', kind: 'k' }], active: 'p', minimized: false, place: '' }];
  instances.minimizeOverlayTab('r');
  const saved = () => JSON.parse(store.get(instances.OVERLAY_WINDOWS_KEY));
  assert.deepEqual(saved().windows.map((window) => window.tabs.map((tab) => tab.id)), [['p', 'q']]);
  await instances.closeEveryTab('w');
  assert.deepEqual(saved().windows, [], 'a reload brings the closed window back nowhere');
  assert.deepEqual(saved().minimizedTabs.map(({ tab }) => tab.id), ['r'], 'the tab minimized before still waits on the dock');
});

test('windows saved before tabs come back in the split and the stack they had', async (t) => {
  const { harness, instances, overlay, prefs, level, mounted } = await setup(t, PAIR);
  const store = new Map([[instances.OVERLAY_WINDOWS_KEY, JSON.stringify([
    { id: 'a', kind: 'k', label: 'A', minimized: false },
    { id: 'b', kind: 'k', label: 'B', minimized: true },
  ])]]);
  const win = { sessionStorage: { getItem: (key) => store.get(key) ?? null, setItem: (key, value) => store.set(key, value) } };
  t.after(instances.keepOverlayWindows(win, (tab) => tab.kind === 'k'));
  assert.deepEqual(instances.overlayWindows.value.map(({ id, active }) => [id, active]), [['a', 'a'], ['b', 'b']]);
  assert.deepEqual(instances.overlayWindows.value.map((window) => window.tabs.map((tab) => tab.label)), [['A'], ['B']]);
  assert.deepEqual(instances.overlayTabs.value.map(({ id, minimized }) => [id, minimized]), [['a', false], ['b', true]]);
  await harness.act(() => overlay.raiseOverlayWindow('a'));
  assert.ok(mounted.container.querySelector('.overlay-split-line'), 'the split still holds them, by the same ids');
  assert.ok(level('a') > level('b'));
  assert.deepEqual(JSON.parse(prefs.getItem(overlay.OVERLAY_SPLIT_PREF)), PAIR);
  // What this page writes, it reads back the same.
  const again = JSON.parse(store.get(instances.OVERLAY_WINDOWS_KEY));
  assert.deepEqual(again.windows.map((window) => window.id), ['a', 'b']);
  assert.deepEqual(again.minimizedTabs, []);
});

test('every way into a window finds it by its tab', async (t) => {
  const { instances } = await setup(t);
  instances.overlayWindows.value = [{ id: 'w', tabs: [{ id: 'p', kind: 'k', label: 'P' }, { id: 'q', kind: 'k', label: 'Q' }], active: 'p', minimized: true }];
  t.after(() => { instances.overlayWindows.value = []; });
  instances.openOverlayWindow({ id: 'q', kind: 'k', label: 'Q' });
  assert.equal(instances.overlayWindows.value.length, 1, 'a tab opens in one place at a time');
  assert.deepEqual([instances.overlayWindows.value[0].minimized, instances.overlayWindows.value[0].active], [false, 'q']);
  instances.labelOverlayWindow('q', 'Renamed');
  assert.deepEqual(instances.overlayTabs.value.map((tab) => tab.label), ['P', 'Renamed']);
  instances.focusOverlayWindow('p');
  assert.equal(instances.overlayWindows.value[0].focusAsked, 1);
  instances.minimizeOverlayWindow('p');
  assert.equal(instances.overlayWindows.value[0].minimized, true, 'minimizing takes the whole window');
  instances.closeOverlayTab('q');
  assert.deepEqual(instances.overlayTabs.value.map((tab) => tab.id), ['p'], 'taking a tab off leaves the rest');
  instances.closeOverlayTab('p');
  assert.deepEqual(instances.overlayWindows.value, []);
});

test('a tab on the dock keeps its one place, and opening it brings it back', async (t) => {
  const { instances, overlay } = await setup(t);
  t.after(() => { instances.overlayWindows.value = []; instances.minimizedOverlayTabs.value = []; });
  instances.openOverlayWindow({ id: 'x', kind: 'k', label: 'X' });
  instances.minimizeOverlayTab('x');
  assert.deepEqual(instances.overlayWindows.value, [], 'its last tab takes the window');
  assert.deepEqual(instances.overlayTabs.value.map(({ id, windowId, minimized }) => [id, windowId, minimized]), [['x', '', true]]);
  instances.labelOverlayWindow('x', 'Renamed');
  instances.openOverlayWindow({ id: 'x', kind: 'k', label: 'stale' });
  assert.deepEqual(instances.minimizedOverlayTabs.value, []);
  assert.deepEqual(instances.overlayTabs.value.map(({ label, minimized }) => [label, minimized]), [['Renamed', false]]);
  assert.equal(overlay.frontOverlayWindow.value, 'x');
});

test('the last tab to leave a window gives up its cell, as closing it does', async (t) => {
  const { instances, overlay, prefs, harness } = await setup(t, PAIR);
  await harness.act(() => instances.openOverlayWindow({ id: 'a', kind: 'k', label: 'A' }));
  await harness.act(() => instances.openOverlayWindow({ id: 'b', kind: 'k', label: 'B' }));
  t.after(() => { instances.overlayWindows.value = []; instances.minimizedOverlayTabs.value = []; });
  await harness.act(() => instances.moveOverlayTab('a', 'b', 0));
  assert.deepEqual(JSON.parse(prefs.getItem(overlay.OVERLAY_SPLIT_PREF)), { ...PAIR, a: { gap: true } });
  assert.deepEqual(instances.overlayWindows.value.map((window) => window.tabs.map((tab) => tab.id)), [['a', 'b']]);
  await harness.act(() => instances.tearOverlayTabOut('a', { x: 5, y: 6 }));
  assert.deepEqual(JSON.parse(prefs.getItem(overlay.OVERLAY_SPLIT_PREF)), { ...PAIR, a: { gap: true } }, 'one tab leaving leaves the cell in place');
  assert.deepEqual(instances.overlayWindows.value.map((window) => [window.id, window.tabs.map((tab) => tab.id)]), [['b', ['b']], ['a', ['a']]]);
});

test('closing a window takes each tab off, and a question goes to the tab that asks', async (t) => {
  const { instances } = await setup(t);
  t.after(() => { instances.overlayWindows.value = []; instances.minimizedOverlayTabs.value = []; });
  instances.overlayWindows.value = [{ id: 'w', tabs: [{ id: 'p', kind: 'k' }, { id: 'q', kind: 'k' }], active: 'p', minimized: false, place: '' }];
  instances.askOverlayClose('q');
  assert.deepEqual(instances.overlayTabs.value.find((tab) => tab.id === 'q').closeAsked, 1);
  assert.equal(instances.overlayWindows.value[0].active, 'q', 'it asks where it can be seen');
  instances.overlayCloseHeard('q');
  assert.equal(instances.overlayTabs.value[0].closeAsked, undefined, 'and only once');
  instances.closeEveryTab('w', { asking: 'q' });
  assert.deepEqual(instances.overlayWindows.value[0].tabs.map((tab) => tab.id), ['q'], 'the tab that asks stays to ask');
  assert.deepEqual(instances.minimizedOverlayTabs.value, [], 'the other leaves, with nothing on the dock');
  instances.closeEveryTab('w');
  assert.deepEqual(instances.overlayWindows.value, []);
  assert.deepEqual(instances.minimizedOverlayTabs.value, []);

  instances.overlayWindows.value = [{ id: 'w', tabs: [{ id: 'p', kind: 'k' }, { id: 'r', kind: 'k' }], active: 'p', minimized: false, place: '' }];
  const closed = [];
  instances.closeEveryTab('w', { closeOf: (tab) => (tab.id === 'r' ? () => closed.push(tab.id) : null) });
  assert.deepEqual(closed, ['r'], 'a kind closes its own tab its own way');
  assert.deepEqual(instances.overlayWindows.value.map((window) => window.tabs.map((tab) => tab.id)), [['r']], 'and what it keeps stays where it was');
  assert.deepEqual(instances.minimizedOverlayTabs.value, []);
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

// A drag by the title bar, as a hand does it: down, a few moves, up.
async function dragBar(harness, mounted, id, from, to, during = () => {}) {
  const bar = mounted.container.querySelector(`.w-${id} .t`);
  const event = (type, { x, y }) => Object.assign(new harness.window.Event(type, { bubbles: true, cancelable: true }), { button: 0, pointerId: 1, clientX: x, clientY: y });
  await harness.act(() => bar.dispatchEvent(event('pointerdown', from)));
  for (let i = 1; i <= 4; i++) {
    await harness.act(() => bar.dispatchEvent(event('pointermove', { x: from.x + (to.x - from.x) * i / 4, y: from.y + (to.y - from.y) * i / 4 })));
  }
  during();
  await harness.act(() => bar.dispatchEvent(event('pointerup', to)));
}

test('a window torn out of the split floats where it is let go off a target, even in the cell it left', async (t) => {
  const { harness, overlay, prefs, mounted } = await setup(t, PAIR);
  const frame = (id) => mounted.container.querySelector(`.w-${id}`);
  assert.ok(frame('a').classList.contains('snapped'), 'a starts in the left cell');
  let previewed = false;
  await dragBar(harness, mounted, 'a', { x: 100, y: 10 }, { x: 300, y: 700 }, () => {
    previewed = frame('a').classList.contains('overlay-window-floating');
  });
  assert.ok(previewed, 'while held, it shows that it will float');
  assert.equal(frame('a').classList.contains('snapped'), false, 'let go in its old cell but off its box, it floats');
  assert.equal(frame('a').classList.contains('overlay-window-floating'), false, 'and the preview ends with the drag');
  assert.deepEqual(JSON.parse(prefs.getItem(overlay.OVERLAY_SPLIT_PREF)), { ...PAIR, a: { gap: true } }, 'its cell stays empty');
  assert.ok(frame('b').classList.contains('snapped'), 'b keeps its cell');
});

test('Escape calls a move off: a window torn out of the split goes back into its cell', async (t) => {
  const { harness, overlay, prefs, mounted } = await setup(t, PAIR);
  const frame = (id) => mounted.container.querySelector(`.w-${id}`);
  const box = () => ['left', 'top', 'width', 'height'].map((side) => frame('a').style[side]);
  const was = box();
  await dragBar(harness, mounted, 'a', { x: 100, y: 10 }, { x: 900, y: 400 }, () => {
    assert.deepEqual(JSON.parse(prefs.getItem(overlay.OVERLAY_SPLIT_PREF)), { ...PAIR, a: { gap: true } }, 'torn out while held');
    harness.window.dispatchEvent(Object.assign(new harness.window.Event('keydown', { cancelable: true }), { key: 'Escape' }));
  });
  assert.deepEqual(JSON.parse(prefs.getItem(overlay.OVERLAY_SPLIT_PREF)), PAIR, 'its cell is its own again');
  assert.ok(frame('a').classList.contains('snapped'), 'and it is back in it');
  assert.deepEqual(box(), was, 'where it was');
  assert.equal(mounted.container.querySelector('.overlay-snap-guide'), null);
  assert.equal(JSON.parse(prefs.getItem('p.a')).snap, 'left', 'its placement is as it was');
});

test('a window minimized to the dock keeps its cell, and closing it there gives the cell up', async (t) => {
  const { harness, instances, overlay, prefs } = await setup(t, PAIR);
  const saved = () => JSON.parse(prefs.getItem(overlay.OVERLAY_SPLIT_PREF));
  await harness.act(() => instances.openOverlayWindow({ id: 'a', kind: 'k', label: 'A' }));
  await harness.act(() => instances.minimizeOverlayTab('a'));
  assert.equal(instances.overlayWindows.value.length, 0, 'its one tab took the window to the dock');
  assert.deepEqual(saved(), PAIR, 'the cell waits for it');
  await harness.act(() => instances.openOverlayWindow({ id: 'a', kind: 'k', label: 'A' }));
  assert.deepEqual(instances.overlayWindows.value.map(({ id }) => id), ['a'], 'it comes back as the same window');
  assert.deepEqual(saved(), PAIR, 'to its cell');
  await harness.act(() => instances.minimizeOverlayTab('a'));
  await harness.act(() => instances.closeOverlayTab('a'));
  assert.deepEqual(saved(), { ...PAIR, a: { gap: true } }, 'closed for good, it leaves the cell empty');
});

test('no new window takes the id of one on the dock, whose cell waits for it', async (t) => {
  const { harness, instances, overlay, prefs } = await setup(t, PAIR);
  const ids = () => instances.overlayWindows.value.map(({ id }) => id);
  await harness.act(() => instances.openOverlayWindow({ id: 'a', kind: 'k', label: 'A' }));
  await harness.act(() => instances.openTabInWindow('a', 'p.a', { id: 'x', kind: 'k', label: 'X' }));
  await harness.act(() => instances.openOverlayWindow({ id: 'c', kind: 'k', label: 'C' }));
  await harness.act(() => instances.minimizeOverlayTab('x'));
  await harness.act(() => instances.moveOverlayTab('a', 'c', 0));
  assert.deepEqual(ids(), ['c'], 'window a is gone, and x waits on the dock to bring it back');
  assert.deepEqual(JSON.parse(prefs.getItem(overlay.OVERLAY_SPLIT_PREF)), PAIR, 'so its cell waits too');
  await harness.act(() => instances.tearOverlayTabOut('a', { x: 300, y: 300 }));
  assert.equal(ids().length, 2);
  assert.ok(!ids().includes('a'), `tab a torn out takes another id: ${ids()}`);
  await harness.act(() => instances.openOverlayWindow({ id: 'x', kind: 'k', label: 'X' }));
  assert.ok(ids().includes('a'), 'x comes back as window a, to its cell');
});

test('a floating window dropped on an empty cell\'s box later takes the cell', async (t) => {
  const { harness, overlay, prefs, mounted } = await setup(t, PAIR);
  await dragBar(harness, mounted, 'a', { x: 100, y: 10 }, { x: 300, y: 400 });
  assert.equal(mounted.container.querySelector('.w-a').classList.contains('snapped'), false);
  const held = await hold(harness, mounted, 'a', { x: 20, y: 5 });
  await held.move({ x: 600, y: 300 });
  const box = held.centre('.overlay-snap-target.gap');
  await held.move(box);
  await held.up(box);
  assert.ok(mounted.container.querySelector('.w-a').classList.contains('snapped'), 'a new drag goes back into the cell');
  assert.deepEqual(JSON.parse(prefs.getItem(overlay.OVERLAY_SPLIT_PREF)), PAIR);
});

// A drag held by the title bar, step by step, to look at the targets it shows.
async function hold(harness, mounted, id, from) {
  const bar = mounted.container.querySelector(`.w-${id} .t`);
  const event = (type, { x, y }) => Object.assign(new harness.window.Event(type, { bubbles: true, cancelable: true }), { button: 0, pointerId: 1, clientX: x, clientY: y });
  await harness.act(() => bar.dispatchEvent(event('pointerdown', from)));
  const all = (selector) => [...mounted.container.querySelectorAll(selector)];
  const px = (node, side) => Number.parseFloat(node.style[side]);
  return {
    move: (to) => harness.act(() => bar.dispatchEvent(event('pointermove', to))),
    up: (to) => harness.act(() => bar.dispatchEvent(event('pointerup', to))),
    all,
    hot: () => all('.overlay-snap-targets .hot').map((node) => node.className),
    guide: () => mounted.container.querySelector('.overlay-snap-guide'),
    centre: (selector) => {
      const node = mounted.container.querySelector(selector);
      return { x: px(node, 'left') + px(node, 'width') / 2, y: px(node, 'top') + px(node, 'height') / 2 };
    },
  };
}

const LEFT_AND_GAP = { axis: 'x', at: 0.5, a: { id: 'a' }, b: { gap: true } };
const snappedIn = (mounted, id) => mounted.container.querySelector(`.w-${id}`).classList.contains('snapped');

test('a drag shows every target for the whole drag, and over an empty cell away from its box nothing fills (tcl-7uk9l6)', async (t) => {
  const { harness, overlay, prefs, mounted } = await setup(t, LEFT_AND_GAP);
  const held = await hold(harness, mounted, 'c', { x: 20, y: 5 });
  await held.move({ x: 700, y: 120 });
  assert.equal(held.all('.overlay-snap-edge').length > 0, true, 'the room\'s edge strips show');
  assert.deepEqual(held.all('.overlay-snap-edge').map((node) => node.dataset.snapZone).filter((zone, i, all) => all.indexOf(zone) === i).sort(),
    ['bottom', 'bottom-left', 'bottom-right', 'left', 'right', 'top-left', 'top-right']);
  assert.equal(held.all('.overlay-snap-target.gap').length, 1, 'the empty cell has its box');
  assert.equal(held.guide(), null, 'over the empty cell, but not its box: no fill preview');
  assert.deepEqual(held.hot(), []);
  await held.move({ x: 900, y: 600 });
  assert.equal(held.guide(), null, 'anywhere in it');
  await held.up({ x: 900, y: 600 });
  assert.equal(snappedIn(mounted, 'c'), false, 'let go there, it floats');
  assert.deepEqual(JSON.parse(prefs.getItem(overlay.OVERLAY_SPLIT_PREF)), LEFT_AND_GAP, 'and the split is as it was');
  assert.equal(mounted.container.querySelector('.overlay-snap-targets'), null, 'the targets go with the drag');
});

test('over an empty cell\'s box the box lights, the fill shows, and the drop fills the cell', async (t) => {
  const { harness, overlay, prefs, mounted } = await setup(t, LEFT_AND_GAP);
  const held = await hold(harness, mounted, 'c', { x: 20, y: 5 });
  await held.move({ x: 700, y: 120 });
  const box = held.centre('.overlay-snap-target.gap');
  await held.move(box);
  assert.deepEqual(held.hot(), ['overlay-snap-target gap hot']);
  const cell = held.all('.overlay-snap-target.gap')[0];
  assert.ok(held.guide(), 'the fill preview shows');
  assert.equal(held.guide().style.left, `${Number.parseFloat(cell.style.left) + Number.parseFloat(cell.style.width) / 2 - Number.parseFloat(held.guide().style.width) / 2}px`, 'over the cell the box is centred in');
  await held.up(box);
  assert.ok(snappedIn(mounted, 'c'));
  assert.deepEqual(JSON.parse(prefs.getItem(overlay.OVERLAY_SPLIT_PREF)), { ...LEFT_AND_GAP, b: { id: 'c' } }, 'the whole cell');
});

test('a tiled window splits only over its marker, and floats a drop anywhere else on it', async (t) => {
  const { harness, overlay, prefs, mounted } = await setup(t, PAIR);
  const held = await hold(harness, mounted, 'c', { x: 20, y: 5 });
  await held.move({ x: 320, y: 300 });
  const sides = (id) => held.all(`.overlay-snap-target.split[data-snap-window="${id}"]`).map((node) => node.dataset.snapSide).sort();
  assert.deepEqual(sides('a'), ['right', 'top'], 'a\'s free edges have markers; its left and bottom are the room\'s edge strips');
  assert.deepEqual(sides('b'), ['left', 'top']);
  assert.equal(sides('c').length, 0, 'the window dragged has none');
  assert.equal(held.guide(), null, 'over a window, off its markers: no split');
  const marker = held.centre('.overlay-snap-target.split[data-snap-window="a"][data-snap-side="top"]');
  await held.move(marker);
  assert.deepEqual(held.hot(), ['overlay-snap-target split top hot']);
  assert.ok(held.guide(), 'the split shows');
  await held.move({ x: 320, y: 300 });
  assert.equal(held.guide(), null, 'leaving the marker takes it away');
  await held.up({ x: 320, y: 300 });
  assert.equal(snappedIn(mounted, 'c'), false, 'let go off a marker, it floats');
  assert.deepEqual(JSON.parse(prefs.getItem(overlay.OVERLAY_SPLIT_PREF)), PAIR);
  const again = await hold(harness, mounted, 'c', { x: 20, y: 5 });
  await again.move({ x: 320, y: 300 });
  const top = again.centre('.overlay-snap-target.split[data-snap-window="a"][data-snap-side="top"]');
  await again.move(top);
  await again.up(top);
  assert.ok(snappedIn(mounted, 'c'));
  assert.deepEqual(JSON.parse(prefs.getItem(overlay.OVERLAY_SPLIT_PREF)), { ...PAIR, a: { axis: 'y', at: 0.5, a: { id: 'c' }, b: { id: 'a' } } }, 'over a, in a\'s cell');
});

test('an edge strip lights under the pointer and snaps there as before', async (t) => {
  const { harness, prefs, mounted } = await setup(t, LEFT_AND_GAP);
  const held = await hold(harness, mounted, 'c', { x: 20, y: 5 });
  await held.move({ x: 700, y: 120 });
  const strip = held.centre('.overlay-snap-edge[data-snap-zone="right"]');
  await held.move(strip);
  assert.deepEqual(held.hot(), ['overlay-snap-edge hot']);
  assert.ok(held.guide(), 'the snap shows');
  await held.up(strip);
  assert.ok(snappedIn(mounted, 'c'));
  assert.equal(JSON.parse(prefs.getItem('p.c')).snap, 'right');
});

test('untile all floats every tiled window where it shows and ends the split', async (t) => {
  const { harness, overlay, prefs, mounted } = await setup(t, PAIR);
  const rect = (id) => {
    const style = mounted.container.querySelector(`.w-${id}`).getAttribute('style');
    return ['left', 'top', 'width', 'height'].map((side) => style.match(new RegExp(`${side}:([-\\d.]+)px`))[1]);
  };
  const before = { a: rect('a'), b: rect('b') };
  assert.equal(overlay.tiledOverlayWindowCount.value, 2);
  await harness.act(() => overlay.untileOverlayWindows());
  for (const id of ['a', 'b']) {
    assert.equal(mounted.container.querySelector(`.w-${id}`).classList.contains('snapped'), false, `${id} floats`);
    assert.deepEqual(rect(id), before[id], `${id} does not move on screen`);
    assert.equal(JSON.parse(prefs.getItem(`p.${id}`)).snap, '', `${id} stays floating after a reload`);
  }
  assert.equal(prefs.getItem(overlay.OVERLAY_SPLIT_PREF), '', 'the split is gone');
  assert.equal(overlay.tiledOverlayWindowCount.value, 0);
});

test('untile all leaves the cell of a window that is away, for when it comes back', async (t) => {
  const { harness, overlay, prefs } = await setup(t, { axis: 'x', at: 0.5, a: PAIR, b: { id: 'd' } });
  await harness.act(() => overlay.untileOverlayWindows());
  assert.deepEqual(JSON.parse(prefs.getItem(overlay.OVERLAY_SPLIT_PREF)), { axis: 'x', at: 0.5, a: { gap: true }, b: { id: 'd' } });
});

test('minimize all sends every window to the dock, and restore all brings them back', async (t) => {
  const { harness, instances } = await setup(t);
  const bulk = await harness.importDashboardModule('js/overlay-window-bulk.js');
  const keys = () => bulk.overlayBulkActions.value.map(({ key }) => key);
  // The fixture's a and b are snapped to an edge, so untiling is offered; no
  // window is open yet, so there is nothing to minimize or restore.
  assert.deepEqual(keys(), ['untile']);
  await harness.act(() => {
    instances.openOverlayWindow({ id: 'x', kind: 'k', label: 'X' });
    instances.openOverlayWindow({ id: 'y', kind: 'k', label: 'Y' });
  });
  assert.ok(keys().includes('minimize'));
  assert.ok(!keys().includes('restore'));
  await harness.act(() => bulk.minimizeAllOverlayWindows());
  assert.deepEqual(instances.overlayWindows.value.map((window) => !!window.minimized), [true, true]);
  assert.deepEqual(keys().filter((key) => key !== 'untile'), ['restore']);
  await harness.act(() => bulk.restoreAllOverlayWindows());
  assert.deepEqual(instances.overlayWindows.value.map((window) => !!window.minimized), [false, false]);
  const commands = bulk.buildOverlayBulkCommands();
  assert.deepEqual(commands.map(({ label }) => label), bulk.overlayBulkActions.value.map(({ label }) => label), 'the palette says what the dock says');
  assert.ok(commands.every(({ label }) => Object.values(bulk.OVERLAY_BULK_ACTIONS).some((action) => action.label === label)));
});

// Three floating windows, for Snap Assist: none tiled yet.
async function floating(t) {
  const harness = await createPreactHarness(t);
  const [overlay, instances, assist] = await Promise.all([
    harness.importDashboardModule('js/overlay-window.js'),
    harness.importDashboardModule('js/overlay-instances.js'),
    harness.importDashboardModule('js/overlay-snap-assist.js'),
  ]);
  const prefs = memoryPrefs();
  for (const id of ['a', 'b', 'c']) prefs.setItem(`p.${id}`, JSON.stringify({ x: 300, y: 200, w: 400, h: 300, snap: '' }));
  instances.overlayWindows.value = ['a', 'b', 'c'].map((id) => ({ id, place: `p.${id}`, active: id, tabs: [{ id, kind: 'board', label: `Board ${id}` }] }));
  overlay.setSnapAssistEnabled(true);
  overlay.endSnapAssist();
  function Win({ id }) {
    const window = overlay.useOverlayWindow({ id, win: harness.window, prefs, prefKey: `p.${id}`, defaultPlacement: () => ({ x: 300, y: 200, w: 400, h: 300, snap: '' }), snaps: true });
    return harness.html`<${overlay.OverlayWindow} id=${id} window=${window} className=${`w-${id}`} frameClass="f" titlebarClass="t" label=${id} title="x">body<//>`;
  }
  const viewOf = (tab) => ({ label: tab.label, lead: '▣', title: tab.label });
  const { h } = harness.preact;
  const mounted = await harness.mount(h('div', null, ...['a', 'b', 'c'].map((id) => h(Win, { key: id, id })), h(assist.SnapAssist, { win: harness.window, viewOf })));
  t.after(() => { instances.overlayWindows.value = []; overlay.endSnapAssist(); });
  const snapped = (id) => mounted.container.querySelector(`.w-${id}`).classList.contains('snapped');
  const cards = () => [...mounted.container.querySelectorAll('.snap-assist-card')].map((card) => card.querySelector('.snap-assist-title').textContent);
  return { harness, overlay, instances, prefs, mounted, snapped, cards };
}

test('with nothing tiled, a drag shows only the room\'s edge strips, and a drop off them floats', async (t) => {
  const s = await floating(t);
  const held = await hold(s.harness, s.mounted, 'a', { x: 400, y: 210 });
  await held.move({ x: 600, y: 400 });
  assert.ok(held.all('.overlay-snap-edge').length > 0);
  assert.equal(held.all('.overlay-snap-target').length, 0, 'no boxes and no markers');
  await held.up({ x: 600, y: 400 });
  assert.equal(s.snapped('a'), false);
});

test('beside a window snapped alone, the space it leaves has a box, and only the box fills it', async (t) => {
  const s = await floating(t);
  await dragBar(s.harness, s.mounted, 'a', { x: 400, y: 210 }, { x: 1, y: 400 });
  await s.harness.act(() => s.harness.document.dispatchEvent(Object.assign(new s.harness.window.Event('keydown', { cancelable: true }), { key: 'Escape' })));
  const held = await hold(s.harness, s.mounted, 'b', { x: 400, y: 210 });
  await held.move({ x: 1000, y: 200 });
  assert.equal(held.all('.overlay-snap-target.gap').length, 1);
  assert.equal(held.guide(), null, 'near the space is not on its box');
  const box = held.centre('.overlay-snap-target.gap');
  await held.move(box);
  assert.ok(held.guide());
  await held.up(box);
  assert.ok(s.snapped('b'), 'b fills the space');
  assert.deepEqual(JSON.parse(s.prefs.getItem(s.overlay.OVERLAY_SPLIT_PREF)), { axis: 'x', at: 0.5, a: { id: 'a' }, b: { id: 'b' } });
});

test('snapping a window offers the others for the space it left, and a pick fills it', async (t) => {
  const s = await floating(t);
  await dragBar(s.harness, s.mounted, 'a', { x: 400, y: 210 }, { x: 1, y: 400 });
  assert.ok(s.snapped('a'), 'a snapped to the left');
  assert.deepEqual(s.cards(), ['Board b', 'Board c'], 'the right half offers the rest');
  assert.ok(!s.prefs.getItem(s.overlay.OVERLAY_SPLIT_PREF), 'nothing is saved before a pick');
  await s.harness.act(() => s.mounted.container.querySelector('.snap-assist-card').click());
  assert.ok(s.snapped('b'), 'b took the space');
  assert.deepEqual(JSON.parse(s.prefs.getItem(s.overlay.OVERLAY_SPLIT_PREF)), { axis: 'x', at: 0.5, a: { id: 'a' }, b: { id: 'b' } });
  assert.deepEqual(s.cards(), [], 'with no space left it is done');
});

test('a corner snap asks for each space left, the largest first, until a press elsewhere ends it', async (t) => {
  const s = await floating(t);
  await dragBar(s.harness, s.mounted, 'a', { x: 400, y: 210 }, { x: 1, y: 60 });
  assert.ok(s.snapped('a'));
  assert.equal(s.cards().length, 2);
  await s.harness.act(() => s.mounted.container.querySelector('.snap-assist-card').click());
  assert.deepEqual(s.cards(), ['Board c'], 'the next space asks');
  await s.harness.act(() => s.harness.document.body.dispatchEvent(new s.harness.window.Event('pointerdown', { bubbles: true })));
  assert.deepEqual(s.cards(), [], 'a press elsewhere ends it');
  assert.ok(!s.snapped('c'));
});

test('Escape ends Snap Assist, and with it off nothing asks', async (t) => {
  const s = await floating(t);
  await dragBar(s.harness, s.mounted, 'a', { x: 400, y: 210 }, { x: 1, y: 400 });
  assert.equal(s.cards().length, 2);
  await s.harness.act(() => s.harness.document.dispatchEvent(Object.assign(new s.harness.window.Event('keydown', { cancelable: true }), { key: 'Escape' })));
  assert.deepEqual(s.cards(), []);
  s.overlay.setSnapAssistEnabled(false);
  await dragBar(s.harness, s.mounted, 'b', { x: 400, y: 210 }, { x: s.harness.window.innerWidth - 1, y: 400 });
  assert.ok(s.snapped('b'));
  assert.deepEqual(s.cards(), [], 'Config turned it off');
});

test('Escape gives the keyboard back to the window just snapped (tcl-1bzjw3)', async (t) => {
  const s = await floating(t);
  await dragBar(s.harness, s.mounted, 'a', { x: 400, y: 210 }, { x: 1, y: 400 });
  await s.harness.act(() => new Promise((resolve) => setTimeout(resolve, 5)));
  assert.ok(s.mounted.container.querySelector('.snap-assist').contains(s.harness.document.activeElement), 'the cards have the keyboard while it asks');
  await s.harness.act(() => s.harness.document.dispatchEvent(Object.assign(new s.harness.window.Event('keydown', { cancelable: true }), { key: 'Escape' })));
  assert.deepEqual(s.cards(), []);
  assert.equal(s.harness.document.activeElement, s.mounted.container.querySelector('.w-a'), 'a has it back');
  assert.ok(s.instances.overlayWindows.value.find((window) => window.id === 'a').focusAsked > 0, 'and its tab is asked to take it, as a terminal does');
});

test('a lone snap beside another window snapped alone asks nothing, rather than offer its space', async (t) => {
  const s = await floating(t);
  await dragBar(s.harness, s.mounted, 'a', { x: 400, y: 210 }, { x: 1, y: 400 });
  await s.harness.act(() => s.harness.document.dispatchEvent(Object.assign(new s.harness.window.Event('keydown', { cancelable: true }), { key: 'Escape' })));
  await dragBar(s.harness, s.mounted, 'b', { x: 400, y: 210 }, { x: 1, y: 60 });
  assert.ok(s.snapped('b'), 'b snapped to the corner');
  assert.deepEqual(s.cards(), [], 'a has the left half');
});

// Stateless drop targets: a window dragged out of the split leaves the layout
// for the drag, and its targets are the layout's without it, the cell it left
// included. Let go on a target, the target applies, its own old spot too.
const targetsShown = (held) => held.all('.overlay-snap-targets [data-snap-gap], .overlay-snap-targets [data-snap-window], .overlay-snap-edge')
  .map((node) => [node.dataset.snapGap ?? '', node.dataset.snapWindow ?? '', node.dataset.snapSide ?? '', node.dataset.snapZone ?? '',
    node.style.left, node.style.top, node.style.width, node.style.height].join('|')).sort();

test('a window torn out of the split sees the targets of the layout without it, its old cell\'s box included', async (t) => {
  const { harness, overlay, prefs, mounted } = await setup(t, PAIR);
  const out = await hold(harness, mounted, 'a', { x: 100, y: 10 });
  await out.move({ x: 300, y: 300 });
  await out.move({ x: 600, y: 300 });
  assert.equal(out.all('.overlay-snap-target.gap').length, 1, 'the cell it left has its box');
  assert.equal(out.all('.overlay-snap-target[data-snap-window="a"]').length, 0, 'and it has no markers of its own');
  const torn = targetsShown(out);
  await out.up({ x: 600, y: 300 });
  assert.deepEqual(JSON.parse(prefs.getItem(overlay.OVERLAY_SPLIT_PREF)), { ...PAIR, a: { gap: true } }, 'let go off a target, it floats');
  const floating = await hold(harness, mounted, 'c', { x: 20, y: 5 });
  await floating.move({ x: 600, y: 300 });
  assert.deepEqual(torn, targetsShown(floating), 'the same targets a floating window sees over that layout');
  await floating.up({ x: 600, y: 300 });
});

test('a window torn out of the split and let go on its old cell\'s box goes back into it', async (t) => {
  const { harness, overlay, prefs, mounted } = await setup(t, PAIR);
  const held = await hold(harness, mounted, 'a', { x: 100, y: 10 });
  await held.move({ x: 300, y: 300 });
  const box = held.centre('.overlay-snap-target.gap');
  await held.move(box);
  assert.deepEqual(held.hot(), ['overlay-snap-target gap hot']);
  assert.ok(held.guide(), 'the fill shows');
  await held.up(box);
  assert.ok(snappedIn(mounted, 'a'), 'it is back in its cell');
  assert.deepEqual(JSON.parse(prefs.getItem(overlay.OVERLAY_SPLIT_PREF)), PAIR);
});

test('a window torn out of the split and let go on the room\'s edge over its old cell snaps there', async (t) => {
  const { harness, overlay, prefs, mounted } = await setup(t, PAIR);
  const held = await hold(harness, mounted, 'a', { x: 100, y: 10 });
  await held.move({ x: 300, y: 300 });
  const strip = held.centre('.overlay-snap-edge[data-snap-zone="left"]');
  await held.move(strip);
  assert.ok(held.guide(), 'the snap shows');
  await held.up(strip);
  assert.ok(snappedIn(mounted, 'a'));
  assert.deepEqual(JSON.parse(prefs.getItem(overlay.OVERLAY_SPLIT_PREF)), PAIR, 'into the cell it left');
});
