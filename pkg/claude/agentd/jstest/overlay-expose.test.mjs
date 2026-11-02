// Tests for showing every overlay window at once (tcl-vg45q5): what the view
// lists, how it is entered and left, and that leaving changes nothing.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

async function setup(t, { host = true } = {}) {
  const harness = await createPreactHarness(t);
  const [instances, expose, bulk] = await Promise.all([
    harness.importDashboardModule('js/overlay-instances.js'),
    harness.importDashboardModule('js/overlay-expose.js'),
    harness.importDashboardModule('js/overlay-window-bulk.js'),
  ]);
  instances.overlayHostUp.value = host;
  const mounted = await harness.mount(harness.html`<${expose.OverlayExpose} win=${harness.window} />`);
  const stopKey = expose.installOverlayExposeKey(harness.window);
  t.after(stopKey);
  const view = () => mounted.container.querySelector('.overlay-expose');
  const cards = () => [...mounted.container.querySelectorAll('.overlay-expose-card')].map((card) => card.querySelector('.overlay-expose-title').textContent.trim());
  const key = (init, target = harness.window.document.body) => harness.act(() => harness.fireEvent(target, 'keydown', init));
  return { harness, instances, expose, bulk, mounted, view, cards, key };
}

const chat = (id, label) => ({ id, kind: 'chat', agent: id, label });

async function threeWindows({ harness, instances }) {
  await harness.act(() => {
    instances.openOverlayWindow(chat('a', 'Alpha'));
    instances.openOverlayWindow(chat('b', 'Bravo'));
    instances.openTabInWindow(instances.overlayWindows.peek().find((window) => window.active === 'b').id, null, { id: 'b2', kind: 'board', label: 'Board two' });
    instances.openOverlayWindow(chat('c', 'Charlie'));
    instances.minimizeOverlayWindow('a');
  });
}

test('the view and every trigger stay away with fewer than two windows, or with Collab off', async (t) => {
  const s = await setup(t);
  const offered = () => s.bulk.overlayBulkActions.value.some(({ key }) => key === 'expose');
  await s.harness.act(() => s.instances.openOverlayWindow(chat('a', 'Alpha')));
  assert.equal(offered(), false, 'one window: nothing to choose between');
  await s.key({ key: ',', ctrlKey: true });
  assert.equal(s.view(), null, 'Ctrl+, does nothing');
  await s.harness.act(() => s.instances.openOverlayWindow(chat('b', 'Bravo')));
  assert.equal(offered(), true);
  await s.harness.act(() => { s.instances.overlayHostUp.value = false; });
  assert.equal(offered(), false, 'Collab off');
  await s.key({ key: ',', ctrlKey: true });
  assert.equal(s.view(), null);
});

test('Ctrl+, shows every window, the front first and minimized ones after, with kind and tabs', async (t) => {
  const s = await setup(t);
  await threeWindows(s);
  await s.key({ key: ',', ctrlKey: true });
  assert.ok(s.view(), 'the view shows');
  assert.deepEqual(s.cards(), ['Charlie', 'Board two', 'Alpha'], 'front first, the minimized window last');
  const [front, tabbed, away] = s.mounted.container.querySelectorAll('.overlay-expose-card');
  assert.match(front.querySelector('.overlay-expose-kind').textContent, /^Chat$/);
  assert.equal(tabbed.querySelector('.overlay-expose-count').textContent, '2', 'a window says how many tabs it holds');
  assert.match(tabbed.querySelector('.overlay-expose-tabs').textContent, /Bravo/, 'and names the others');
  assert.match(away.querySelector('.overlay-expose-kind').textContent, /minimized/);
  assert.equal(s.harness.window.document.activeElement, s.mounted.container.querySelector('.overlay-expose-filter'), 'typing goes to the filter');
  await s.key({ key: ',', ctrlKey: true });
  assert.equal(s.view(), null, 'Ctrl+, again leaves');
});

test('typing filters by any tab title, and Enter raises the chosen window and leaves', async (t) => {
  const s = await setup(t);
  await threeWindows(s);
  await s.harness.act(() => s.expose.showAllOverlayWindows());
  const filter = s.mounted.container.querySelector('.overlay-expose-filter');
  await s.harness.input(filter, 'alp');
  assert.deepEqual(s.cards(), ['Alpha']);
  await s.harness.input(filter, 'bravo');
  assert.deepEqual(s.cards(), ['Board two'], 'a window matches by a tab that is not active');
  await s.harness.input(filter, 'nothing like it');
  assert.match(s.mounted.container.querySelector('.overlay-expose-empty').textContent, /No window matches/);
  await s.harness.input(filter, 'alp');
  await s.key({ key: 'Enter' }, filter);
  assert.equal(s.view(), null, 'choosing leaves');
  const alpha = s.instances.overlayWindows.value.find((window) => window.active === 'a');
  assert.equal(alpha.minimized, false, 'a minimized window comes back');
  assert.ok(alpha.focusAsked > 0, 'and takes focus');
});

test('a click on a card raises it; Escape or a click outside leaves with nothing changed', async (t) => {
  const s = await setup(t);
  await threeWindows(s);
  const before = JSON.stringify(s.instances.overlayWindows.value);
  await s.harness.act(() => s.expose.showAllOverlayWindows());
  await s.key({ key: 'ArrowRight' }, s.mounted.container.querySelector('.overlay-expose-filter'));
  assert.ok(s.mounted.container.querySelectorAll('.overlay-expose-card')[1].classList.contains('chosen'), 'arrows move the choice');
  await s.key({ key: 'Escape' }, s.mounted.container.querySelector('.overlay-expose-filter'));
  assert.equal(s.view(), null);
  assert.equal(JSON.stringify(s.instances.overlayWindows.value), before, 'Escape changes nothing');
  await s.harness.act(() => s.expose.showAllOverlayWindows());
  await s.harness.act(() => s.harness.fireEvent(s.view(), 'pointerdown'));
  assert.equal(s.view(), null);
  assert.equal(JSON.stringify(s.instances.overlayWindows.value), before, 'a click outside changes nothing');
  await s.harness.act(() => s.expose.showAllOverlayWindows());
  await s.harness.act(() => s.mounted.container.querySelectorAll('.overlay-expose-card')[1].click());
  assert.equal(s.view(), null);
  assert.ok(s.instances.overlayWindows.value.find((window) => window.active === 'b2').focusAsked > 0, 'the clicked window takes focus');
});

test('the palette and the dock menu offer it as "Show all windows"', async (t) => {
  const s = await setup(t);
  await threeWindows(s);
  const command = s.bulk.buildOverlayBulkCommands().find(({ id }) => id === 'overlay.expose-all');
  assert.equal(command?.label, 'Show all windows');
  assert.equal(s.bulk.overlayBulkActions.value[0].key, 'expose', 'first on the dock menu');
  await s.harness.act(() => command.run());
  assert.ok(s.view());
});

const tick = () => new Promise((resolve) => setTimeout(resolve, 5));

test('leaving by Ctrl+, puts focus back where it was, and picking leaves it with the window', async (t) => {
  const s = await setup(t);
  await threeWindows(s);
  const terminal = s.harness.window.document.body.appendChild(s.harness.window.document.createElement('textarea'));
  t.after(() => terminal.remove());
  terminal.focus();
  await s.key({ key: ',', ctrlKey: true }, terminal);
  assert.notEqual(s.harness.window.document.activeElement, terminal, 'the filter takes focus');
  const filter = s.mounted.container.querySelector('.overlay-expose-filter');
  assert.equal(filter.getAttribute('aria-activedescendant'), s.mounted.container.querySelector('.overlay-expose-card.chosen').id, 'the chosen card is announced');
  await s.key({ key: ',', ctrlKey: true }, filter);
  await tick();
  assert.equal(s.harness.window.document.activeElement, terminal, 'back to the terminal');
  await s.key({ key: ',', ctrlKey: true }, terminal);
  await s.key({ key: 'Enter' }, s.mounted.container.querySelector('.overlay-expose-filter'));
  await tick();
  assert.notEqual(s.harness.window.document.activeElement, terminal, 'a picked window keeps the focus it was given');
});

test('Ctrl+, does not open over a dialog', async (t) => {
  const s = await setup(t);
  await threeWindows(s);
  const dialog = s.harness.window.document.body.appendChild(s.harness.window.document.createElement('div'));
  dialog.className = 'modal-overlay show';
  t.after(() => dialog.remove());
  await s.key({ key: ',', ctrlKey: true });
  assert.equal(s.view(), null);
});

test('the view going away with its host does not leave it open to come back unasked', async (t) => {
  const s = await setup(t);
  await threeWindows(s);
  await s.harness.act(() => s.expose.showAllOverlayWindows());
  await s.mounted.unmount();
  assert.equal(s.expose.overlayExposeOpen.value, false);
  await s.harness.act(() => s.expose.showAllOverlayWindows());
  await s.harness.act(() => { s.instances.overlayHostUp.value = false; });
  assert.equal(s.expose.overlayExposeOpen.value, true, 'not mounted here, so the flag alone');
  const again = await s.harness.mount(s.harness.html`<${s.expose.OverlayExpose} win=${s.harness.window} />`);
  assert.equal(again.container.querySelector('.overlay-expose'), null, 'with Collab off it does not show');
});

test('model: the spread lays windows out without overlap, scaled down to fit, never up', async (t) => {
  const harness = await createPreactHarness(t);
  const { spreadLayout } = await harness.importDashboardModule('js/overlay-expose.js');
  const area = { x: 0, y: 100, width: 1000, height: 600 };
  const spots = spreadLayout([{ w: 800, h: 600 }, { w: 800, h: 600 }, { w: 400, h: 300 }], area, 20);
  const rects = spots.map(({ x, y, scale }, index) => {
    const [w, h] = [[800, 600], [800, 600], [400, 300]][index].map((side) => side * scale);
    return { left: x - w / 2, right: x + w / 2, top: y - h / 2, bottom: y + h / 2 };
  });
  for (const rect of rects) {
    assert.ok(rect.left >= area.x - 0.01 && rect.right <= area.x + area.width + 0.01 && rect.top >= area.y - 0.01 && rect.bottom <= area.y + area.height + 0.01, 'inside the stage');
  }
  for (let i = 0; i < rects.length; i++) {
    for (let j = i + 1; j < rects.length; j++) {
      const [a, b] = [rects[i], rects[j]];
      assert.ok(a.right <= b.left + 0.01 || b.right <= a.left + 0.01 || a.bottom <= b.top + 0.01 || b.bottom <= a.top + 0.01, `${i} and ${j} don't overlap`);
    }
  }
  assert.deepEqual(spreadLayout([{ w: 200, h: 100 }], area), [{ x: 500, y: 400, scale: 1 }], 'a small window is not blown up');
});

// Windows on the page, as overlay-window.js draws them, with the layout boxes a
// browser would give them, and a stage the spread has 1000 × 600 of.
function liveWindows(s, boxes) {
  const doc = s.harness.window.document;
  const nodes = Object.entries(boxes).map(([id, [x, y, w, h]]) => {
    const node = doc.body.appendChild(doc.createElement('section'));
    node.dataset.overlayWindow = id;
    for (const [key, value] of Object.entries({ offsetLeft: x, offsetTop: y, offsetWidth: w, offsetHeight: h })) Object.defineProperty(node, key, { value });
    return node;
  });
  const proto = s.harness.window.Element.prototype;
  const original = proto.getBoundingClientRect;
  proto.getBoundingClientRect = function rect() {
    return this.classList.contains('overlay-expose-stage') ? { left: 0, top: 80, right: 1000, bottom: 680, width: 1000, height: 600, x: 0, y: 80 } : original.call(this);
  };
  return () => { proto.getBoundingClientRect = original; for (const node of nodes) node.remove(); };
}

const sheet = (s) => s.harness.window.document.head.querySelector('style[data-overlay-expose]')?.textContent || '';

test('the open windows themselves spread out, live, and the minimized ones are cards beneath', async (t) => {
  const s = await setup(t);
  await threeWindows(s);
  const ids = Object.fromEntries(s.instances.overlayWindows.peek().map((window) => [window.active, window.id]));
  t.after(liveWindows(s, { [ids.b2]: [40, 60, 700, 500], [ids.c]: [500, 200, 600, 400] }));
  const before = JSON.stringify(s.instances.overlayWindows.value);
  await s.key({ key: ',', ctrlKey: true });
  const targets = () => [...s.mounted.container.querySelectorAll('.overlay-expose-live')].map((target) => target.querySelector('.overlay-expose-live-label').textContent.trim());
  assert.deepEqual(targets(), ['✎ Board two 2', '💬 Charlie'], 'a target over each spread window, in reading order, with its tab count');
  assert.deepEqual(s.cards(), ['Alpha'], 'the minimized window is a card');
  assert.ok(s.mounted.container.querySelector('.overlay-expose-grid.row'), 'in a row along the bottom');
  assert.match(sheet(s), new RegExp(`\\[data-overlay-window="${ids.b2}"\\] \\{ transform: translate\\(`), 'each window moves by a transform');
  assert.match(sheet(s), /pointer-events: none/, 'and takes no input while spread');
  assert.equal(JSON.stringify(s.instances.overlayWindows.value), before, 'nothing about the windows changes');

  const filter = s.mounted.container.querySelector('.overlay-expose-filter');
  await s.harness.input(filter, 'charl');
  assert.match(sheet(s), new RegExp(`\\[data-overlay-window="${ids.b2}"\\][^}]*opacity: \\.25`), 'a window that does not match dims');
  assert.deepEqual(targets(), ['💬 Charlie'], 'and cannot be picked');
  await s.harness.input(filter, '');

  await s.key({ key: 'Escape' }, filter);
  assert.equal(s.view(), null);
  assert.doesNotMatch(sheet(s), /transform: translate/, 'leaving takes the transforms away, so the windows glide back');
  assert.equal(JSON.stringify(s.instances.overlayWindows.value), before, 'Escape changes nothing');

  await s.key({ key: ',', ctrlKey: true });
  await s.harness.act(() => s.mounted.container.querySelectorAll('.overlay-expose-live')[1].click());
  assert.equal(s.view(), null);
  assert.ok(s.instances.overlayWindows.value.find((window) => window.active === 'c').focusAsked > 0, 'a click on a spread window picks it');
});

test('with reduced motion the windows jump to the same spread, and back', async (t) => {
  const s = await setup(t);
  await threeWindows(s);
  const ids = Object.fromEntries(s.instances.overlayWindows.peek().map((window) => [window.active, window.id]));
  t.after(liveWindows(s, { [ids.b2]: [40, 60, 700, 500], [ids.c]: [500, 200, 600, 400] }));
  const previous = s.harness.window.matchMedia;
  s.harness.window.matchMedia = (query) => ({ matches: query.includes('reduce'), addEventListener() {}, removeEventListener() {} });
  t.after(() => { s.harness.window.matchMedia = previous; });
  await s.key({ key: ',', ctrlKey: true });
  assert.match(sheet(s), /transform: translate/);
  assert.doesNotMatch(sheet(s), /transition/);
  await s.key({ key: 'Escape' }, s.mounted.container.querySelector('.overlay-expose-filter'));
  assert.equal(s.harness.window.document.head.querySelector('style[data-overlay-expose]'), null, 'gone at once');
});
