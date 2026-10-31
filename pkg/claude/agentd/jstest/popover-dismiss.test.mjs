// The close rule of the popovers a click opens (watchPopover): a press outside,
// Escape, and the pointer staying away. Text, or a field in use, survives
// hover-away only. And a filter in a popover has the cursor when it opens.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

async function watched(t, markup = '<button id="trigger"></button><div id="panel"></div>', options = {}) {
  const harness = await createPreactHarness(t);
  const { CLICK_CLOSE_MS, watchPopover } = await harness.importDashboardModule('js/hover-popover.js');
  const root = harness.document.body.appendChild(harness.document.createElement('div'));
  root.innerHTML = markup;
  const closed = [];
  const stop = watchPopover({ roots: () => root, close: (why) => closed.push(why), ...options });
  t.after(() => stop());
  const leave = () => root.dispatchEvent(new harness.window.Event('mouseleave'));
  const enter = () => root.dispatchEvent(new harness.window.Event('mouseenter'));
  const wait = () => new Promise((resolve) => setTimeout(resolve, CLICK_CLOSE_MS + 40));
  return { harness, root, closed, stop, leave, enter, wait };
}

test('a press outside closes at once, a press inside does not', async (t) => {
  const { harness, root, closed } = await watched(t);
  harness.fireEvent(root.querySelector('#panel'), 'pointerdown');
  assert.deepEqual(closed, []);
  harness.fireEvent(harness.document.body, 'pointerdown');
  assert.deepEqual(closed, ['outside']);
});

test('Escape closes only the newest popover', async (t) => {
  const { harness, closed } = await watched(t);
  const { watchPopover } = await harness.importDashboardModule('js/hover-popover.js');
  const inner = [];
  const stop = watchPopover({ roots: () => [], close: (why) => inner.push(why) });
  harness.fireEvent(harness.document.body, 'keydown', { key: 'Escape' });
  assert.deepEqual([inner, closed], [['escape'], []]);
  stop();
  harness.fireEvent(harness.document.body, 'keydown', { key: 'Escape' });
  assert.deepEqual(closed, ['escape']);
});

test('the pointer staying away closes, coming back in time keeps it', async (t) => {
  const { closed, leave, enter, wait } = await watched(t);
  leave();
  enter();
  await wait();
  assert.deepEqual(closed, []);
  leave();
  await wait();
  assert.deepEqual(closed, ['away']);
});

let hovering = false;
test('a fading popover closes after the fade, unless the pointer comes back', async (t) => {
  const fades = [];
  const { closed, leave, enter, wait } = await watched(t, undefined, { fadeMs: 60, fading: (on) => fades.push(on) });
  leave();
  await wait();
  assert.deepEqual([fades, closed], [[true], []]);
  enter();
  await wait();
  assert.deepEqual([fades, closed], [[true, false], []]);
  leave();
  await wait();
  await new Promise((resolve) => setTimeout(resolve, 100));
  assert.deepEqual([fades, closed], [[true, false, true], ['away']]);
});

test('a click-opened popover outlasts the hover delay, a hover-opened one does not', async (t) => {
  const { harness, closed, leave } = await watched(t, undefined, { hoverOpened: () => hovering });
  const { HOVER_CLOSE_MS, CLICK_CLOSE_MS } = await harness.importDashboardModule('js/hover-popover.js');
  assert.ok(CLICK_CLOSE_MS > HOVER_CLOSE_MS);
  const pause = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
  leave();
  await pause(HOVER_CLOSE_MS + 40);
  assert.deepEqual(closed, [], 'a slipped pointer keeps a clicked menu');
  await pause(CLICK_CLOSE_MS - HOVER_CLOSE_MS + 40);
  assert.deepEqual(closed, ['away']);

  hovering = true;
  closed.length = 0;
  leave();
  await pause(HOVER_CLOSE_MS + 40);
  assert.deepEqual(closed, ['away']);
});

test('a filter focused by opening does not hold it, a used field or text does', async (t) => {
  const { harness, root, closed, leave, enter, wait } = await watched(t, '<input id="field" type="text">');
  const field = root.querySelector('#field');
  field.focus();
  leave();
  await wait();
  assert.deepEqual(closed, ['away'], 'nothing is lost: focus from opening does not count');

  closed.length = 0;
  enter();
  harness.fireEvent(field, 'pointerdown');
  harness.fireEvent(field, 'pointerup');
  leave();
  await wait();
  assert.deepEqual(closed, [], 'the operator is in the field');

  field.blur();
  field.value = 'half a thought';
  await wait();
  assert.deepEqual(closed, [], 'a draft holds it after focus has gone');

  harness.fireEvent(harness.document.body, 'keydown', { key: 'Escape' });
  assert.deepEqual(closed, ['escape'], 'hover-away alone is all a draft stops');

  closed.length = 0;
  field.value = '';
  await wait();
  assert.deepEqual(closed, ['away'], 'the check repeats, so it goes once the draft has');
});

test('a drag that leaves the popover waits for the release', async (t) => {
  const { harness, root, closed, leave, wait } = await watched(t, '<input id="range" type="range">');
  harness.fireEvent(root.querySelector('#range'), 'pointerdown');
  leave();
  await wait();
  assert.deepEqual(closed, []);
  harness.fireEvent(harness.document.body, 'pointerup');
  await wait();
  assert.deepEqual(closed, ['away']);
});

test('hold keeps it through hover-away and ends with the reason', async (t) => {
  let holding = true;
  const { closed, leave, wait } = await watched(t, '<div></div>', { hold: () => holding });
  leave();
  await wait();
  assert.deepEqual(closed, []);
  holding = false;
  await wait();
  assert.deepEqual(closed, ['away']);
});

test('a shown dialog above owns the page, a dialog around the popover does not', async (t) => {
  const { harness, root, closed, leave, wait } = await watched(t);
  const dialog = harness.document.body.appendChild(harness.document.createElement('div'));
  dialog.className = 'modal-overlay show';
  leave();
  await wait();
  harness.fireEvent(dialog, 'pointerdown');
  const escape = harness.fireEvent(dialog, 'keydown', { key: 'Escape' });
  assert.deepEqual(closed, [], 'it waits behind the dialog');
  assert.equal(escape.defaultPrevented, false, 'Escape is the dialog\'s');

  dialog.appendChild(root);
  harness.fireEvent(harness.document.body, 'keydown', { key: 'Escape' });
  assert.deepEqual(closed, ['escape'], 'a popover inside the dialog closes first');
});

test('stopping cancels a pending close', async (t) => {
  const { closed, stop, leave, wait } = await watched(t);
  leave();
  stop();
  await wait();
  assert.deepEqual(closed, []);
});

