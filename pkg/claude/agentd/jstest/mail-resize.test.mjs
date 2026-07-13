import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('Messages resize restores columns and tears down active drag plus gutter listeners', async (t) => {
  const harness = await createPreactHarness(t);
  const { initMailResize } = await harness.importDashboardModule('js/mail-resize.js');
  const client = harness.document.body.appendChild(harness.document.createElement('div'));
  client.className = 'mail-client';
  client.innerHTML = '<div class="mail-sidebar-col"></div><div class="mail-list-col"></div><div class="mail-reader"></div>'
    + '<div class="mail-gutter" data-boundary="sidebar-list"></div><div class="mail-gutter" data-boundary="list-reader"></div>';
  Object.defineProperty(client, 'clientWidth', { value: 1200 });
  for (const [selector, width] of [['.mail-sidebar-col', 240], ['.mail-list-col', 400], ['.mail-reader', 540]]) {
    Object.defineProperty(client.querySelector(selector), 'offsetWidth', { value: width });
  }
  const gutter = client.querySelector('.mail-gutter');
  let captured = false;
  gutter.setPointerCapture = () => { captured = true; };
  gutter.hasPointerCapture = () => captured;
  gutter.releasePointerCapture = () => { captured = false; };

  const cleanup = initMailResize(client);
  assert.match(client.style.gridTemplateColumns, /^240px 10px/);
  harness.fireEvent(gutter, 'pointerdown', { button: 0, pointerId: 7, clientX: 100 });
  harness.fireEvent(gutter, 'pointermove', { pointerId: 7, clientX: 150 });
  assert.equal(harness.document.body.classList.contains('mail-col-resizing'), true);
  assert.match(client.style.gridTemplateColumns, /^290px 10px/);
  cleanup();
  assert.equal(harness.document.body.classList.contains('mail-col-resizing'), false);
  const afterCleanup = client.style.gridTemplateColumns;
  harness.fireEvent(gutter, 'dblclick');
  assert.equal(client.style.gridTemplateColumns, afterCleanup);
});
