// The close rule of the popovers a click opens (watchPopover): a press outside,
// Escape, and the pointer staying away. Text, or a field in use, survives
// hover-away only. And a filter in a popover has the cursor when it opens.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

async function watched(t, markup = '<button id="trigger"></button><div id="panel"></div>', options = {}) {
  const harness = await createPreactHarness(t);
  const { HOVER_CLOSE_MS, watchPopover } = await harness.importDashboardModule('js/hover-popover.js');
  const root = harness.document.body.appendChild(harness.document.createElement('div'));
  root.innerHTML = markup;
  const closed = [];
  const stop = watchPopover({ roots: () => root, close: (why) => closed.push(why), ...options });
  t.after(() => stop());
  const leave = () => root.dispatchEvent(new harness.window.Event('mouseleave'));
  const enter = () => root.dispatchEvent(new harness.window.Event('mouseenter'));
  const wait = () => new Promise((resolve) => setTimeout(resolve, HOVER_CLOSE_MS + 40));
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

test('hold and keep leave the popover to what sits above it', async (t) => {
  let holding = true;
  const { harness, closed, leave, wait } = await watched(t, '<div></div>', {
    hold: () => holding,
    keep: (event) => event.key === 'Escape' && holding,
  });
  leave();
  await wait();
  harness.fireEvent(harness.document.body, 'keydown', { key: 'Escape' });
  assert.deepEqual(closed, []);
  holding = false;
  await wait();
  assert.deepEqual(closed, ['away']);
});

test('stopping cancels a pending close', async (t) => {
  const { closed, stop, leave, wait } = await watched(t);
  leave();
  stop();
  await wait();
  assert.deepEqual(closed, []);
});

