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
