// js/phone-draft-guard.js: a phone sheet holding a short draft (a comment, a
// note, an agent's prompt) asks before a close drops it, by the message
// sheet's rule. Shown through the Board's text sheet, multi-line (a comment)
// and one-line (a new task's title, quick to type again).
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

async function setup(t, props) {
  const harness = await createPreactHarness(t);
  const [sheet, { TextSheet }] = await Promise.all([
    harness.importDashboardModule('js/phone-sheet.js'),
    harness.importDashboardModule('js/phone-board.js'),
  ]);
  const sent = [];
  sheet.openPhoneSheet({
    label: 'Comment',
    Body: TextSheet,
    props: { title: 'Saved carts', label: 'Comment', send: 'Comment', what: 'this comment', onSubmit: async (text) => { sent.push(text); return true; }, ...props },
  });
  t.after(() => sheet.closePhoneSheet());
  await harness.mount(harness.html`<${sheet.PhoneSheetHost} />`);
  const doc = harness.document;
  const ask = () => doc.querySelector('[role="alertdialog"]');
  const button = (name) => [...doc.querySelectorAll('button')].find((b) => b.textContent.trim() === name);
  const field = () => doc.querySelector('.phone-board-field');
  const request = () => harness.act(() => sheet.requestClosePhoneSheet());
  return { harness, sheet, sent, ask, button, field, request };
}

test('with nothing written, a close just closes', async (t) => {
  const { sheet, request } = await setup(t);
  await request();
  assert.equal(sheet.phoneSheet.value, null, 'nothing written: it just closes');
});

test('the question: a requested close (Escape, the scrim) asks, again keeps, Keep and Discard do what they say', async (t) => {
  const { harness, sheet, ask, button, field, request } = await setup(t);
  await harness.input(field(), 'ship after the migration');
  await request();
  assert.ok(sheet.phoneSheet.value, 'a draft holds the close');
  assert.equal(ask()?.getAttribute('aria-label'), 'Discard this comment?');
  assert.equal(harness.document.activeElement, button('Keep'), 'the question takes the focus');
  await request();
  assert.equal(ask(), null, 'asked again while it asks: Keep');
  assert.equal(field().value, 'ship after the migration');
  assert.equal(harness.document.activeElement, field(), 'and the focus is back in the field');

  await harness.act(() => button('Cancel').click());
  assert.ok(ask(), 'Cancel is a requested close too');
  await harness.act(() => button('Keep').click());
  assert.equal(ask(), null);
  assert.equal(harness.document.activeElement, field(), 'Keep gives the focus back to the field');
  assert.ok(sheet.phoneSheet.value, 'kept, it stays');

  await request();
  await harness.act(() => button('Discard').click());
  assert.equal(sheet.phoneSheet.value, null, 'discarded, it closes');
});

test('a sent comment closes without asking', async (t) => {
  const { harness, sheet, sent, field } = await setup(t);
  await harness.input(field(), 'done');
  await harness.act(async () => {
    harness.document.querySelector('form').dispatchEvent(new harness.window.Event('submit', { cancelable: true }));
    await new Promise((resolve) => setTimeout(resolve, 0));
  });
  assert.deepEqual(sent, ['done']);
  assert.equal(sheet.phoneSheet.value, null);
});

test('a one-line title is not asked about', async (t) => {
  const { harness, sheet, field, request } = await setup(t, { multiline: false, label: 'Title', send: 'Add' });
  await harness.input(field(), 'Saved carts');
  await request();
  assert.equal(sheet.phoneSheet.value, null);
});

test('Back asks, and Back again while it asks discards: Back alone can leave a form', async (t) => {
  const { harness, sheet, ask, field } = await setup(t);
  await harness.input(field(), 'ship after the migration');
  const back = () => harness.act(() => sheet.requestClosePhoneSheet({ back: true }));
  await back();
  assert.ok(ask(), 'Back asks');
  await harness.act(() => sheet.requestClosePhoneSheet());
  assert.equal(ask(), null, 'another close while it asks keeps');
  assert.ok(sheet.phoneSheet.value);
  await back();
  assert.ok(ask());
  await back();
  assert.equal(sheet.phoneSheet.value, null, 'Back again while it asks discards');
});
