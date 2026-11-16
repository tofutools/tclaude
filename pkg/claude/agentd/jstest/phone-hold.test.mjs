// js/phone-hold.js: a touch and hold opens a menu under the finger, and the
// browser's own long-press must not act on it: no long-press menu, no text
// selection, no press on the entry under the finger as it lifts, while a
// deliberate tap afterwards works. Shared by every holdable row and card
// (agents, shells, Now's cards), so it is tested once here.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

async function setup(t) {
  const harness = await createPreactHarness(t);
  const hold = await harness.importDashboardModule('js/phone-hold.js');
  const { useState } = harness.hooks;
  const doc = harness.window.document;
  let cleared = 0;
  doc.getSelection = () => ({ removeAllRanges: () => { cleared += 1; } });
  const taps = [];
  function Row() {
    const [open, setOpen] = useState(false);
    const { holding, handlers } = hold.useHold(() => setOpen(true));
    return harness.html`<div>
      <button type="button" class=${`phone-holdable${holding ? ' phone-holding' : ''}`} data-row ...${handlers} onClick=${() => taps.push('row')}>row</button>
      ${open && harness.html`<button type="button" data-entry onClick=${() => taps.push('entry')}>Kill…</button>`}
    </div>`;
  }
  const mounted = await harness.mount(harness.html`<${Row} />`);
  const c = mounted.container;
  const fire = (el, type, extra = {}) => {
    const event = new harness.window.Event(type, { bubbles: true, cancelable: true });
    Object.assign(event, { button: 0, pointerId: 1, clientX: 5, clientY: 5, ...extra });
    el.dispatchEvent(event);
    return event;
  };
  t.mock.timers.enable({ apis: ['setTimeout'] });
  return { harness, hold, doc, c, taps, fire, cleared: () => cleared };
}

test('after a hold, until the finger lifts: no long-press menu, no selection, the page inert', async (t) => {
  const { harness, hold, doc, c, fire, cleared } = await setup(t);
  await harness.act(() => fire(c.querySelector('[data-row]'), 'pointerdown'));
  await harness.act(() => t.mock.timers.tick(hold.HOLD_MS));
  const entry = c.querySelector('[data-entry]');
  assert.ok(entry, 'the hold opened the menu');
  assert.ok(doc.documentElement.hasAttribute(hold.ARMED_ATTR), 'armed: phone.css makes the page take no pointer');
  assert.ok(cleared() >= 1, 'the selection is cleared as the menu opens');
  assert.equal(fire(entry, 'contextmenu').defaultPrevented, true, 'the browser\'s long-press menu is swallowed');
  assert.equal(fire(entry, 'selectstart').defaultPrevented, true, 'and so is a text selection');
  // The browser cancels the pointer as its own long-press starts: still held.
  await harness.act(() => fire(doc, 'pointercancel'));
  assert.ok(doc.documentElement.hasAttribute(hold.ARMED_ATTR), 'a pointercancel is not the lift');
  assert.equal(fire(entry, 'contextmenu').defaultPrevented, true);
});

test('the lift presses nothing under the finger; the next deliberate tap does', async (t) => {
  const { harness, hold, doc, c, taps, fire } = await setup(t);
  await harness.act(() => fire(c.querySelector('[data-row]'), 'pointerdown'));
  await harness.act(() => t.mock.timers.tick(hold.HOLD_MS));
  const entry = c.querySelector('[data-entry]');
  await harness.act(() => fire(entry, 'touchend'));
  assert.equal(doc.documentElement.hasAttribute(hold.ARMED_ATTR), false, 'lifted: the page takes presses again');
  assert.equal(fire(entry, 'contextmenu').defaultPrevented, false, 'a later deliberate long-press is the page\'s own');
  // The click the lift makes is swallowed at the document. A browser runs
  // that in the capture phase, before the entry's own handler; linkedom runs
  // it on the way up, so here it shows as the click cancelled (the sandbox
  // check runs it in a browser).
  let lifted;
  await harness.act(() => { lifted = fire(entry, 'click'); });
  assert.equal(lifted.defaultPrevented, true, 'the click the lift makes is swallowed, wherever it lands');
  // A deliberate tap: its press ends the swallowing at once.
  taps.length = 0;
  let tapped;
  await harness.act(() => { fire(entry, 'pointerdown'); tapped = fire(entry, 'click'); });
  assert.equal(tapped.defaultPrevented, false);
  assert.deepEqual(taps, ['entry']);
});

test('a lift that makes no click leaves nothing behind; a lost touch end does not leave the page inert', async (t) => {
  const { harness, hold, doc, c, taps, fire } = await setup(t);
  await harness.act(() => fire(c.querySelector('[data-row]'), 'pointerdown'));
  await harness.act(() => t.mock.timers.tick(hold.HOLD_MS));
  await harness.act(() => fire(doc, 'pointerup'));
  await harness.act(() => t.mock.timers.tick(hold.LIFT_MS));
  await harness.act(() => c.querySelector('[data-entry]').click());
  assert.deepEqual(taps, ['entry'], 'past LIFT_MS a click is a click');
  // Held again, and the end never arrives.
  await harness.act(() => fire(c.querySelector('[data-row]'), 'pointerdown', { pointerId: 2 }));
  await harness.act(() => t.mock.timers.tick(hold.HOLD_MS));
  assert.ok(doc.documentElement.hasAttribute(hold.ARMED_ATTR));
  await harness.act(() => t.mock.timers.tick(hold.HELD_MAX_MS));
  assert.equal(doc.documentElement.hasAttribute(hold.ARMED_ATTR), false);
});
