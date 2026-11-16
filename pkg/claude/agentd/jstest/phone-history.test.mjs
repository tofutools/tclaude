// js/phone-history.js: phone sheets, pushed pages and the shell's own views
// as router overlays, so Back closes what is on top.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

async function setup(t) {
  return setupWith(t, await createPreactHarness(t));
}

async function setupWith(t, harness) {
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

// The rule: while a dialog or form is open, Back is the dialog's. It closes
// it through its own guarded close (a dirty form asks first), and never steps
// the page behind it (back-layers.js).
async function mountDialog(t, harness, { dirty = false, confirmDiscard = async () => true } = {}) {
  const { Dialog } = await harness.importDashboardModule('js/ui-dialog.js');
  const open = harness.signals.signal(true);
  const asked = [];
  const Form = () => open.value && harness.html`<${Dialog} id="phone-back-form-modal" title="New agent"
    onClose=${() => { open.value = false; }} dirty=${dirty}
    confirmDiscard=${async () => { asked.push(1); return confirmDiscard(); }}><input name="what" /><//>`;
  await harness.mount(harness.html`<${Form} />`);
  return { open, asked };
}

test('Back over a dialog closes the dialog, and only the dialog', async (t) => {
  const harness = await createPreactHarness(t);
  const { routes, announced, tick } = await setupWith(t, harness);
  const { open } = await mountDialog(t, harness);
  await tick();
  assert.equal(routes.overlayParams().phone, '1', 'an open dialog is a step');
  assert.deepEqual(announced.map((detail) => detail.closed), [false]);
  await harness.act(async () => { routes.restoreOverlays({}); await tick(); });
  assert.equal(open.value, false, 'Back closed it');
  assert.deepEqual(announced.map((detail) => detail.closed), [false],
    'and announced no close of its own, so nothing steps the page behind back');
});

test('Back over a dirty form asks to discard; kept, it stays and takes its step back', async (t) => {
  const harness = await createPreactHarness(t);
  const { routes, announced, tick } = await setupWith(t, harness);
  let answer = false;
  const { open, asked } = await mountDialog(t, harness, { dirty: true, confirmDiscard: async () => answer });
  await tick();
  announced.length = 0;
  await harness.act(async () => { routes.restoreOverlays({}); await tick(); });
  assert.equal(asked.length, 1, 'Back asked');
  assert.equal(open.value, true, 'kept, the form stays');
  assert.deepEqual(announced.map((detail) => detail.closed), [false], 'and takes its entry back, so the next Back asks again');
  answer = true;
  await harness.act(async () => { routes.restoreOverlays({}); await tick(); });
  assert.equal(asked.length, 2);
  assert.equal(open.value, false, 'discarded, the form closes');
  assert.deepEqual(announced.map((detail) => detail.closed), [false], 'with no step of its own');
});

test('a sheet that hands over to a dialog ("All options…") leaves Back with the dialog', async (t) => {
  const harness = await createPreactHarness(t);
  const { routes, sheet, announced, tick } = await setupWith(t, harness);
  sheet.openPhoneSheet({ label: 'New agent', Body: () => null });
  sheet.closePhoneSheet();
  const { open } = await mountDialog(t, harness);
  await tick();
  assert.deepEqual(announced.map((detail) => detail.closed), [false, true, false],
    'the sheet steps off its entry and the dialog takes one');
  assert.equal(routes.overlayParams().phone, '1');
  await harness.act(async () => { routes.restoreOverlays({}); await tick(); });
  assert.equal(open.value, false, 'Back closes the dialog, not the page behind it');
});

test('while Back\'s close of a form is still asking, the address has already left it', async (t) => {
  const harness = await createPreactHarness(t);
  const { routes, tick } = await setupWith(t, harness);
  let answer;
  const { open } = await mountDialog(t, harness, { dirty: true, confirmDiscard: () => new Promise((resolve) => { answer = resolve; }) });
  await tick();
  await harness.act(async () => { routes.restoreOverlays({}); await tick(); });
  assert.equal(open.value, true, 'the discard prompt is up');
  assert.equal(routes.overlayParams().phone, '', 'so the router does not put the step back while it asks');
  await harness.act(async () => { answer(true); await tick(); });
  assert.equal(open.value, false);
  assert.equal(routes.overlayParams().phone, '');
});
