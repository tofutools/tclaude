// Tests for the one minimize and restore animation (tcl-2e15pj): flights start
// and land, turn round mid-air, stagger, play nothing under reduced motion, and
// never show a black terminal: content not live yet flies covered by its last
// frame, or a placeholder, until it is up.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

async function setup(t, { reduced = false } = {}) {
  const harness = await createPreactHarness(t);
  const motion = await harness.importDashboardModule('js/overlay-window-motion.js');
  motionModule = motion;
  motion.resetOverlayMotionForTest();
  const doc = harness.document;
  const frames = [];
  const animations = [];
  const win = {
    document: doc,
    innerWidth: 1200,
    innerHeight: 800,
    matchMedia: (query) => ({ matches: reduced && query.includes('reduce') }),
    requestAnimationFrame: (run) => frames.push(run),
    setTimeout: (run) => { frames.push(run); return 0; },
    getComputedStyle: () => ({ opacity: '0.6' }),
  };
  const flush = () => { while (frames.length) frames.shift()(); };
  const rects = new Map();
  const rectFor = (node) => rects.get(node) || { left: 0, top: 0, width: 0, height: 0 };
  // A node laid out at rect, which animate() records.
  const place = (node, rect) => {
    rects.set(node, rect);
    node.getBoundingClientRect = () => rectFor(node);
    node.animate = (keyframes, options) => {
      const animation = {
        node, keyframes, options, currentTime: 0, cancelled: false, onfinish: null,
        cancel() { this.cancelled = true; },
        finish() { this.onfinish?.(); },
      };
      animations.push(animation);
      return animation;
    };
    return node;
  };
  const windowEl = (id, rect) => {
    const node = doc.createElement('section');
    node.setAttribute('data-overlay-window', id);
    doc.body.appendChild(node);
    return place(node, rect);
  };
  const pill = (id, rect) => {
    const node = doc.createElement('span');
    node.setAttribute('data-dock-window', id);
    doc.body.appendChild(node);
    return place(node, rect);
  };
  const windows = harness.signals.signal([]);
  const stack = [];
  const set = (list) => { windows.value = list; };
  set([]);
  const stop = motion.installOverlayWindowMotion(win, { windows, stack: () => stack });
  t.after(() => { stop(); motion.resetOverlayMotionForTest(); });
  const state = (id) => motion.overlayWindowMotion.peek().get(id) || '';
  return { harness, motion, doc, win, flush, windows, set, stack, windowEl, pill, place, animations, state };
}

const W = { left: 100, top: 100, width: 400, height: 300 };
const P = { left: 600, top: 770, width: 100, height: 20 };

test('a minimize flies the live window into its pill, then lands', async (t) => {
  const s = await setup(t);
  s.windowEl('a', W);
  s.pill('a', P);
  s.set([{ id: 'a', tabs: [], minimized: false }]);
  s.set([{ id: 'a', tabs: [], minimized: true }]);
  assert.equal(s.state('a'), 'minimize', 'minimized at once, shown while it flies');
  assert.equal(s.motion.overlayWindowLeaving('a'), true);
  s.flush();
  assert.equal(s.animations.length, 1);
  const [animation] = s.animations;
  assert.equal(animation.keyframes[0].transform, 'none', 'it starts where it is');
  assert.equal(animation.keyframes.at(-1).transform, s.motion.flipTransform(W, P), 'and ends on the pill');
  assert.ok(animation.keyframes.at(-1).opacity < 1, 'fading near the end');
  assert.equal(animation.keyframes[1].opacity, 1, 'but not before');
  assert.ok(animation.options.duration >= 200 && animation.options.duration <= 240);
  animation.finish();
  assert.equal(s.state('a'), '', 'landed: put away');
  assert.equal(animation.cancelled, true, 'its transform goes with it');
});

test('a restore grows the window out of its pill', async (t) => {
  const s = await setup(t);
  s.windowEl('a', W);
  s.pill('a', P);
  s.set([{ id: 'a', tabs: [], minimized: true }]);
  s.set([{ id: 'a', tabs: [], minimized: false }]);
  assert.equal(s.state('a'), 'waiting', 'out of sight until the flight starts');
  s.flush();
  assert.equal(s.state('a'), 'restore');
  const [animation] = s.animations;
  assert.equal(animation.keyframes[0].transform, s.motion.flipTransform(W, P));
  assert.equal(animation.keyframes.at(-1).transform, 'none');
  animation.finish();
  assert.equal(s.state('a'), '');
});

test('a second press mid-flight turns the window round from where it is', async (t) => {
  const s = await setup(t);
  const node = s.windowEl('a', W);
  s.pill('a', P);
  s.set([{ id: 'a', tabs: [], minimized: false }]);
  s.set([{ id: 'a', tabs: [], minimized: true }]);
  s.flush();
  const [down] = s.animations;
  down.currentTime = 100;
  const midway = { left: 300, top: 400, width: 200, height: 150 };
  s.place(node, midway);
  s.set([{ id: 'a', tabs: [], minimized: false }]);
  assert.equal(down.cancelled, true, 'the minimize stops');
  assert.equal(s.state('a'), 'restore', 'shown at once, no wait');
  s.place(node, W);
  s.flush();
  const up = s.animations[1];
  assert.equal(up.keyframes[0].transform, s.motion.flipTransform(W, midway), 'from where it was on screen');
  assert.equal(up.keyframes[0].opacity, 0.6, 'as faint as it was');
  assert.equal(up.keyframes.at(-1).transform, 'none');
  assert.equal(up.options.duration, 100, 'as long as it had flown');
  assert.equal(up.options.delay, 0);
  down.finish();
  assert.equal(s.state('a'), 'restore', 'the old flight landing changes nothing');
  up.finish();
  assert.equal(s.state('a'), '', 'no half state left');
});

test('a restore pressed away before it started is instant', async (t) => {
  const s = await setup(t);
  s.windowEl('a', W);
  s.pill('a', P);
  s.set([{ id: 'a', tabs: [], minimized: true }]);
  s.set([{ id: 'a', tabs: [], minimized: false }]);
  s.set([{ id: 'a', tabs: [], minimized: true }]);
  assert.equal(s.state('a'), '');
  s.flush();
  assert.equal(s.animations.length, 0);
});

test('reduced motion plays nothing', async (t) => {
  const s = await setup(t, { reduced: true });
  s.windowEl('a', W);
  s.pill('a', P);
  s.set([{ id: 'a', tabs: [], minimized: false }]);
  s.set([{ id: 'a', tabs: [], minimized: true }]);
  s.flush();
  assert.equal(s.state('a'), '');
  assert.equal(s.animations.length, 0);
});

test('windows moving together stagger, front first out and back first in, the whole capped', async (t) => {
  const s = await setup(t);
  for (const id of ['a', 'b', 'c']) { s.windowEl(id, W); s.pill(id, P); }
  s.stack.push('a', 'b', 'c');
  s.set(['a', 'b', 'c'].map((id) => ({ id, tabs: [], minimized: false })));
  s.set(['a', 'b', 'c'].map((id) => ({ id, tabs: [], minimized: true })));
  s.flush();
  const order = (list) => list.map((animation) => [animation.node.getAttribute('data-overlay-window'), animation.options.delay]);
  assert.deepEqual(order(s.animations), [['c', 0], ['b', 25], ['a', 50]], 'the front window leaves first');
  for (const animation of [...s.animations]) animation.finish();
  s.animations.length = 0;
  s.set(['a', 'b', 'c'].map((id) => ({ id, tabs: [], minimized: false })));
  s.flush();
  assert.deepEqual(order(s.animations), [['a', 0], ['b', 25], ['c', 50]], 'the back window returns first');

  const delays = s.motion.staggerDelays(20);
  assert.ok(delays.at(-1) + s.motion.MOTION_MS <= s.motion.MOTION_TOTAL_MS, 'many windows still end in time');
  assert.deepEqual(s.motion.staggerDelays(1), [0]);
});

test('a window without a pill flies to what stands for it, or the dock\'s menu', async (t) => {
  const s = await setup(t);
  s.windowEl('steward', W);
  const chip = s.place(s.doc.createElement('button'), { left: 1100, top: 760, width: 30, height: 30 });
  s.doc.body.appendChild(chip);
  t.after(s.motion.addOverlayMotionTarget((window) => (window.id === 'steward' ? chip : null)));
  assert.deepEqual(s.motion.dockRectOf(s.win, { id: 'steward', tabs: [] }), { left: 1100, top: 760, width: 30, height: 30 });
  const menu = s.place(s.doc.createElement('button'), { left: 20, top: 770, width: 24, height: 20 });
  menu.className = 'overlay-dock-menu-button';
  s.doc.body.appendChild(menu);
  assert.deepEqual(s.motion.dockRectOf(s.win, { id: 'other', tabs: [] }), { left: 20, top: 770, width: 24, height: 20 }, 'a folded pill: its ☰');
});

// A terminal window's content: a section holding a host whose terminal is or
// is not attached yet.
function terminalWindow(s, id) {
  const node = s.windowEl(id, W);
  const host = s.doc.createElement('div');
  host.className = 'companion-term-host';
  node.appendChild(host);
  s.place(host, { left: 106, top: 130, width: 388, height: 264 });
  const draw = (text) => {
    host.innerHTML = `<div class="xterm"><div class="xterm-screen"><div class="xterm-rows"><div>${text}</div></div></div></div>`;
  };
  let attach;
  const attached = new Promise((resolve) => { attach = resolve; });
  let live = false;
  const provider = {
    tabId: `${id}-tab`,
    host: () => host,
    live: () => live,
    ready: () => attached,
    capture: () => (live ? s.motion.captureTerminal(host) : null),
  };
  const unregister = s.motion.registerLiveContent(id, provider);
  return { node, host, draw, unregister, attach: (text) => { draw(text); live = true; attach(true); }, drop: () => { live = false; host.innerHTML = ''; } };
}

// The cover a window draws over its content: { kind, text }, or null.
let motionModule = null;
const cover = (node) => {
  const entry = motionModule.overlayLiveCovers.peek().get(node.getAttribute('data-overlay-window'));
  return entry ? { kind: entry.frame ? 'last' : 'placeholder', text: entry.frame?.textContent || '' } : null;
};
const settle = () => new Promise((resolve) => setImmediate(resolve));

test('a terminal not attached flies at once, behind a placeholder, then shows live when it attaches', async (t) => {
  const s = await setup(t);
  const term = terminalWindow(s, 'a');
  s.pill('a', P);
  s.set([{ id: 'a', tabs: [], minimized: true }]);
  s.set([{ id: 'a', tabs: [], minimized: false }]);
  s.flush();
  assert.equal(s.animations.length, 1, 'the flight starts at once, attached or not');
  assert.equal(cover(term.node)?.kind, 'placeholder', 'nothing kept yet: the placeholder, never black');
  term.attach('$ live');
  await settle();
  assert.equal(cover(term.node), null, 'live content swaps in, mid-flight');
});

test('a terminal not attached yet flies behind its last good frame', async (t) => {
  const s = await setup(t);
  const term = terminalWindow(s, 'a');
  s.pill('a', P);
  term.attach('$ before');
  // Minimizing while live keeps the frame.
  s.set([{ id: 'a', tabs: [], minimized: false }]);
  s.set([{ id: 'a', tabs: [], minimized: true }]);
  s.flush();
  assert.equal(cover(term.node), null, 'live: no cover');
  s.animations[0].finish();
  // Its terminal went while it was away; restore.
  term.drop();
  s.set([{ id: 'a', tabs: [], minimized: false }]);
  s.flush();
  const shown = cover(term.node);
  assert.equal(shown?.kind, 'last');
  assert.match(shown.text, /\$ before/, 'what it showed last');
  assert.equal(s.animations.length, 2, 'and it does not wait');
});

test('the last frame survives the terminal going away, for its tab only', async (t) => {
  const s = await setup(t);
  const term = terminalWindow(s, 'a');
  term.attach('$ kept');
  term.unregister();
  let attach;
  s.motion.registerLiveContent('a', { tabId: 'a-tab', host: () => term.host, live: () => false, ready: () => new Promise((resolve) => { attach = resolve; }), capture: () => null });
  term.host.innerHTML = '';
  const done = s.motion.ensureLiveContent({ id: 'a' });
  assert.match(cover(term.node)?.text || '', /\$ kept/);
  await settle();
  attach(true);
  assert.equal(await done, true);
  assert.equal(cover(term.node), null);

  s.motion.registerLiveContent('a', { tabId: 'another-tab', host: () => term.host, live: () => false, ready: () => Promise.resolve(false), capture: () => null });
  const other = s.motion.ensureLiveContent({ id: 'a' });
  assert.equal(cover(term.node)?.kind, 'placeholder', 'another tab never shows this one\'s frame');
  assert.equal(await other, false, 'content that gives up');
  assert.equal(cover(term.node), null, 'leaves no cover behind');
});

test('terminalReady waits for the attach and its first output', async (t) => {
  const s = await setup(t);
  const widgetRef = { current: null };
  const statusRef = { current: 'connecting…' };
  const timers = [];
  const win = { setTimeout: (run) => timers.push(run) };
  let resolved = null;
  s.motion.terminalReady(widgetRef, statusRef, { win }).then((ok) => { resolved = ok; });
  await settle();
  assert.equal(resolved, null, 'no widget yet');
  widgetRef.current = { settled: async () => true };
  statusRef.current = 'connected';
  timers.shift()();
  await settle();
  assert.equal(resolved, true);
});

test('the window draws its cover: the kept screen handed to its host, or the placeholder', async (t) => {
  const s = await setup(t);
  const { html } = s.harness;
  const frame = s.doc.createElement('div');
  frame.textContent = '$ kept screen';
  const rect = { left: 6, top: 30, width: 300, height: 200 };
  const view = await s.harness.mount(html`<${s.motion.LiveCover} cover=${{ frame, rect, token: 1 }} />`);
  const drawn = view.container.querySelector('[data-overlay-live-frame]');
  assert.equal(drawn.getAttribute('data-overlay-live-frame'), 'last');
  assert.equal(drawn.querySelector('.overlay-live-frame-holder').firstChild, frame, 'the copy itself, under a Preact-owned host');
  assert.match(drawn.getAttribute('style'), /left:6px;top:30px;width:300px;height:200px/);
  await view.rerender(html`<${s.motion.LiveCover} cover=${{ frame: null, rect, token: 2 }} />`);
  const placeholder = view.container.querySelector('[data-overlay-live-frame]');
  assert.equal(placeholder.getAttribute('data-overlay-live-frame'), 'placeholder');
  assert.equal(placeholder.textContent.trim(), 'Connecting…');
  await view.rerender(html`<${s.motion.LiveCover} cover=${undefined} />`);
  assert.equal(view.container.querySelector('[data-overlay-live-frame]'), null, 'gone when live');
});
