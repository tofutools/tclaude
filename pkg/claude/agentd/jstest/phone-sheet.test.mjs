// js/phone-sheet.js: the phone's bottom sheet follows a finger dragging its
// grab handle, closes past a distance or on a flick, and slides back
// otherwise.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

async function setup(t) {
  const harness = await createPreactHarness(t);
  const sheets = await harness.importDashboardModule('js/phone-sheet.js');
  t.after(() => sheets.closePhoneSheet());
  const Body = ({ close }) => harness.html`<button type="button" onClick=${close}>Done</button>`;
  const mounted = await harness.mount(harness.html`<main></main><${sheets.PhoneSheetHost} />`);
  const open = () => harness.act(() => sheets.openPhoneSheet({ label: 'Test', Body }));
  const sheet = () => mounted.container.querySelector('.phone-sheet');
  const pointer = (type, clientY, timeStamp) => {
    const event = new harness.window.Event(type, { bubbles: true });
    Object.defineProperty(event, 'clientY', { value: clientY });
    Object.defineProperty(event, 'timeStamp', { value: timeStamp });
    sheet().querySelector('.phone-sheet-grab').dispatchEvent(event);
  };
  return { harness, sheets, open, sheet, pointer };
}

test('dragCloses: far enough, or a flick', async (t) => {
  const { sheets } = await setup(t);
  assert.equal(sheets.dragCloses(59, 1000), false, 'a slow short drag');
  assert.equal(sheets.dragCloses(60, 1000), true, 'far enough');
  assert.equal(sheets.dragCloses(100, 1000, 600), false, 'a tall sheet needs a quarter of its height');
  assert.equal(sheets.dragCloses(150, 1000, 600), true);
  assert.equal(sheets.dragCloses(140, 1000, 2000), true, 'never more than 140px');
  assert.equal(sheets.dragCloses(30, 40), true, 'a flick');
  assert.equal(sheets.dragCloses(20, 10), false, 'too short for a flick');
  assert.equal(sheets.dragCloses(30, 0), false, 'no time measured is no flick');
});

test('the sheet follows the finger and slides back when let go short', async (t) => {
  const { harness, open, sheet, pointer } = await setup(t);
  await open();
  await harness.act(() => { pointer('pointerdown', 100, 0); pointer('pointermove', 140, 200); });
  assert.equal(sheet().style.transform, 'translateY(40px)', 'it follows');
  await harness.act(() => pointer('pointermove', 80, 300));
  assert.equal(sheet().style.transform, '', 'never above its place');
  await harness.act(() => { pointer('pointermove', 140, 400); pointer('pointerup', 140, 500); });
  assert.ok(sheet(), 'a slow 40px drag keeps it');
  assert.equal(sheet().style.transform, '', 'back in place');
  assert.match(sheet().style.transition, /transform/, 'sliding back');
});

test('let go far enough or flicked, the sheet closes; a cancelled drag goes back', async (t) => {
  const { harness, open, sheet, pointer } = await setup(t);
  await open();
  await harness.act(() => { pointer('pointerdown', 100, 0); pointer('pointermove', 150, 50); pointer('pointercancel', 150, 60); });
  assert.ok(sheet());
  assert.equal(sheet().style.transform, '');
  await harness.act(() => { pointer('pointerdown', 100, 0); pointer('pointermove', 180, 500); pointer('pointerup', 180, 600); });
  assert.equal(sheet(), null, 'dragged 80px');
  await open();
  await harness.act(() => { pointer('pointerdown', 100, 0); pointer('pointerup', 130, 30); });
  assert.equal(sheet(), null, 'a flick');
});

test('under reduced motion it goes back at once', async (t) => {
  const { harness, open, sheet, pointer } = await setup(t);
  harness.window.matchMedia = (query) => ({ matches: query.includes('reduce') });
  await open();
  await harness.act(() => { pointer('pointerdown', 100, 0); pointer('pointermove', 130, 300); pointer('pointerup', 130, 400); });
  assert.equal(sheet().style.transition, 'none');
  assert.equal(sheet().style.transform, '');
});
