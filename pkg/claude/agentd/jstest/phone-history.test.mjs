// js/phone-history.js: phone sheets, pushed pages and the shell's own views
// as router overlays, so Back closes what is on top.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

async function setup(t) {
  const harness = await createPreactHarness(t);
  const [history, routes, { phonePage }, sheet] = await Promise.all([
    harness.importDashboardModule('js/phone-history.js'),
    harness.importDashboardModule('js/overlay-routes.js'),
    harness.importDashboardModule('js/phone-page.js'),
    harness.importDashboardModule('js/phone-sheet.js'),
  ]);
  const doc = harness.document;
  const announced = [];
  doc.addEventListener(routes.OVERLAY_EVENT, (event) => announced.push(event.detail));
  const view = harness.signals.signal(null);
  const uninstall = history.installPhoneHistory({ view, doc });
  t.after(() => { uninstall(); phonePage.value = null; sheet.closePhoneSheet(); });
  const tick = () => new Promise((resolve) => setTimeout(resolve, 5));
  await tick();
  announced.length = 0; // the boot correction
  return { history, routes, phonePage, sheet, view, announced, tick };
}

test('a page and a sheet over it are two steps; Back closes the sheet, then the page', async (t) => {
  const { routes, phonePage, sheet, announced } = await setup(t);
  const backs = [];
  phonePage.value = { title: 'job', onBack: () => { backs.push(1); phonePage.value = null; } };
  sheet.openPhoneSheet({ label: 'Actions', Body: () => null });
  assert.equal(routes.overlayParams().phone, '2');
  assert.deepEqual(announced.map((detail) => detail.closed), [false, false]);

  routes.restoreOverlays({ phone: '1' });
  assert.equal(sheet.phoneSheet.value, null, 'Back took the sheet down');
  assert.deepEqual(backs, [], 'and left the page');
  routes.restoreOverlays({});
  assert.deepEqual(backs, [1], 'the next Back is the page\'s own back');
  assert.equal(announced.length, 2, 'what Back closed is not announced again');
});

test('closing a layer on screen steps back off its entry', async (t) => {
  const { sheet, announced } = await setup(t);
  sheet.openPhoneSheet({ label: 'More', Body: () => null });
  sheet.closePhoneSheet();
  assert.deepEqual(announced.map((detail) => detail.closed), [false, true]);
});

test('a page with a route of its own is left to the router', async (t) => {
  const { routes, phonePage, announced } = await setup(t);
  phonePage.value = { title: 'reader', routed: true, onBack: () => {} };
  assert.equal(routes.overlayParams().phone, '');
  assert.deepEqual(announced, []);
});

test('an own view is a step forward when it comes and when it goes; Back restores it quietly', async (t) => {
  const { routes, view, announced, tick, history } = await setup(t);
  view.value = 'now';
  await tick();
  assert.deepEqual(announced, [{ closed: false, replace: false }]);
  assert.equal(routes.overlayParams().own, 'now');
  view.value = null;
  await tick();
  assert.equal(announced.length, 2);
  assert.equal(announced[1].closed, false, 'leaving Now keeps it behind for Back');

  routes.restoreOverlays({ own: 'board' });
  assert.equal(view.value, 'board');
  assert.equal(history.keptView('mail'), null, 'kept only over the tab it was restored over');
  routes.restoreOverlays({ own: 'now' });
  assert.equal(history.keptView(null), 'now', 'Back kept Now over the tab it landed on');
  assert.equal(history.keptView(null), null, 'once');
  routes.restoreOverlays({ own: 'nonsense' });
  assert.equal(view.value, null);
  await tick();
  assert.equal(announced.length, 2, 'Back and Forward record nothing');
});
