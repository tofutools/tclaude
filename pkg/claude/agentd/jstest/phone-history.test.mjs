// js/phone-history.js: phone sheets, pushed pages and the shell's own views
// as router overlays, so Back closes what is on top.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

async function setup(t) {
  return setupWith(t, await createPreactHarness(t));
}

// browser gives the harness window a history that can step Forward (forward()
// counts the steps; land() is the popstate the step lands as) and, with
// canGoForward, the Navigation API's word on whether anything is ahead.
function browser(harness, { canGoForward } = {}) {
  const win = harness.document.defaultView;
  const steps = [];
  win.location = { href: 'http://localhost/?terminal=a' };
  win.history = { forward: () => steps.push('forward') };
  if (canGoForward !== undefined) win.navigation = { canGoForward };
  const land = () => win.dispatchEvent(new win.Event('popstate'));
  return { steps, land };
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

test('a sheet opened over a dialog is on top: Back closes the sheet, then the dialog', async (t) => {
  const harness = await createPreactHarness(t);
  const { routes, sheet, tick } = await setupWith(t, harness);
  const { open } = await mountDialog(t, harness);
  await tick();
  sheet.openPhoneSheet({ label: 'Link', Body: () => null });
  assert.equal(routes.overlayParams().phone, '2');
  await harness.act(async () => { routes.restoreOverlays({ phone: '1' }); await tick(); });
  assert.equal(sheet.phoneSheet.value, null, 'Back took the sheet on top');
  assert.equal(open.value, true, 'and left the dialog under it');
  await harness.act(async () => { routes.restoreOverlays({}); await tick(); });
  assert.equal(open.value, false);
});

test('a layer that is a location of its own (routed) is no extra step', async (t) => {
  const harness = await createPreactHarness(t);
  const { routes, announced, tick } = await setupWith(t, harness);
  const layers = await harness.importDashboardModule('js/back-layers.js');
  const off = layers.registerBackLayer({ close: () => true, routed: true });
  t.after(off);
  await tick();
  assert.equal(routes.overlayParams().phone, '', 'its own route carries it');
  assert.deepEqual(announced, [], 'so it takes no history entry of the phone\'s');
});

// GUARDS is every way a phone form guards a draft, each as a form holding
// one: a phone sheet's useSheetGuard (the message sheet: Mail's Reply and
// Write, ✉ Message), and a Dialog's guarded close whose question is a layer
// of its own (shellConfirmDiscard: All options… and every desktop form a
// phone reaches). Each opens over a page with a route of its own (Mail's
// reader, a terminal), whose onBack must never run.
const GUARDS = [
  {
    name: 'a phone sheet (useSheetGuard)',
    async mount(t, harness, sheet) {
      const { useSheetGuard, PhoneSheetHost } = await harness.importDashboardModule('js/phone-sheet.js');
      const asking = harness.signals.signal(false);
      // As the message sheet: a close asks; asked again, it means Keep.
      const Body = () => {
        useSheetGuard(() => { asking.value = !asking.peek(); return true; });
        return harness.html`<textarea>draft</textarea>`;
      };
      sheet.openPhoneSheet({ label: 'Message lead', Body });
      await harness.mount(harness.html`<${PhoneSheetHost} />`);
      return {
        open: () => !!sheet.phoneSheet.value,
        asking: () => asking.value,
        keep: () => { asking.value = false; },
        discard: () => { asking.value = false; sheet.closePhoneSheet(); },
      };
    },
  },
  {
    name: 'a Dialog whose question is a layer of its own (shellConfirmDiscard)',
    async mount(t, harness) {
      const { registerBackLayer } = await harness.importDashboardModule('js/back-layers.js');
      let answer = null;
      let off = null;
      // The shell's confirmation: a modal of its own, which Back cancels. It
      // registers as it renders, a microtask after it is asked for.
      const confirmDiscard = () => new Promise((resolve) => {
        answer = (yes) => { off?.(); off = null; answer = null; resolve(yes); };
        queueMicrotask(() => { if (answer) off = registerBackLayer({ close: () => { answer?.(false); return true; } }); });
      });
      const { open } = await mountDialog(t, harness, { dirty: true, confirmDiscard });
      return {
        open: () => open.value,
        asking: () => !!answer,
        keep: () => answer(false),
        discard: () => answer(true),
      };
    },
  },
];

for (const guard of GUARDS) {
  test(`Back over a draft in ${guard.name}: asks, keeps its step, and never moves the page behind`, async (t) => {
    const harness = await createPreactHarness(t);
    const { routes, phonePage, sheet, announced, tick } = await setupWith(t, harness);
    const behind = [];
    phonePage.value = { title: 'reader', routed: true, onBack: () => behind.push('back') };
    const form = await guard.mount(t, harness, sheet);
    await tick();
    assert.equal(routes.overlayParams().phone, '1', 'the form is a step');
    announced.length = 0;

    // Back: the router restores the entry below, then reads what is on
    // screen at once to correct that entry. The form, asking, must not be on
    // it yet, or it would take the page's entry for its own.
    const back = async () => {
      let read;
      await harness.act(async () => { routes.restoreOverlays({}); read = routes.overlayParams().phone; await tick(); });
      return read;
    };
    assert.equal(await back(), '', 'Back leaves the entry the page behind is on as it was');
    assert.equal(form.asking(), true, 'Back asks');
    assert.equal(form.open(), true);
    assert.equal(routes.overlayParams().phone, '1', 'and the form takes its step back');
    assert.deepEqual(announced.map((detail) => detail.closed), [false], 'as a step of its own');

    await harness.act(async () => { form.keep(); await tick(); });
    assert.equal(form.open(), true, 'kept, it stays');
    assert.equal(routes.overlayParams().phone, '1', 'with its step');

    announced.length = 0;
    assert.equal(await back(), '');
    assert.equal(form.asking(), true, 'the next Back asks again');
    assert.deepEqual(announced.map((detail) => detail.closed), [false]);

    await harness.act(async () => { form.discard(); await tick(); });
    assert.equal(form.open(), false, 'discarded, it closes');
    assert.deepEqual(behind, [], 'and nothing ever stepped the page behind back');
  });
}

// Android's Back skips every entry of a page that added one after a Back
// without a fresh tap (Chrome's history-manipulation intervention), so it left
// the app from a kept draft (tk_3739e29d). A held Back steps Forward onto the
// form's own entry instead: no new entry, ever.
{
  const guard = GUARDS[0];
  test(`a kept draft in ${guard.name} steps Forward onto its own entry, never adds one`, async (t) => {
    const harness = await createPreactHarness(t);
    const { steps, land } = browser(harness);
    const { routes, phonePage, sheet, announced, tick } = await setupWith(t, harness);
    const behind = [];
    phonePage.value = { title: 'reader', routed: true, onBack: () => behind.push('back') };
    const form = await guard.mount(t, harness, sheet);
    await tick();
    announced.length = 0;

    const back = () => harness.act(async () => { routes.restoreOverlays({}); await tick(); });
    await back();
    assert.equal(form.asking(), true, 'Back asks');
    // Kept (a sheet's at once, a dialog's once its question is answered),
    // the form steps Forward onto its own entry.
    await harness.act(async () => { form.keep(); await tick(); });
    assert.deepEqual(steps, ['forward'], 'kept, it steps Forward onto the form\'s entry');
    assert.deepEqual(announced, [], 'recording no entry of its own');
    await harness.act(async () => { land(); routes.restoreOverlays({ phone: '1' }); await tick(); });
    await new Promise((resolve) => setTimeout(resolve, 450));
    assert.deepEqual(announced, [], 'landed, nothing is recorded later either');

    await back();
    assert.equal(form.asking(), true, 'the next Back asks again');
    assert.deepEqual(announced, []);
    await harness.act(async () => { form.discard(); await tick(); });
    assert.equal(form.open(), false);
    assert.deepEqual(behind, [], 'and the page behind never moved');
  });
}

test('a step Forward that never lands (nothing ahead) records the step as before', async (t) => {
  const harness = await createPreactHarness(t);
  const { steps } = browser(harness);
  const { routes, sheet, announced, tick } = await setupWith(t, harness);
  const form = await GUARDS[0].mount(t, harness, sheet);
  await tick();
  announced.length = 0;
  await harness.act(async () => { routes.restoreOverlays({}); await tick(); });
  assert.deepEqual(steps, ['forward']);
  assert.deepEqual(announced, []);
  await new Promise((resolve) => setTimeout(resolve, 450));
  assert.equal(form.open(), true);
  assert.deepEqual(announced.map((detail) => detail.closed), [false], 'so the form still has its step');
});

test('with nothing ahead (the Navigation API says so) the step is recorded at once', async (t) => {
  const harness = await createPreactHarness(t);
  const { steps } = browser(harness, { canGoForward: false });
  const { routes, sheet, announced, tick } = await setupWith(t, harness);
  await GUARDS[0].mount(t, harness, sheet);
  await tick();
  announced.length = 0;
  await harness.act(async () => { routes.restoreOverlays({}); await tick(); });
  assert.deepEqual(steps, []);
  assert.deepEqual(announced.map((detail) => detail.closed), [false]);
});
