// js/phone-desktop-only.js: which views a phone leaves to the desktop, and the
// note and page that say so.
import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

test('desktopOnlyView names the views a phone leaves out', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/phone-desktop-only.js');
  assert.equal(mod.desktopOnlyView('fleet-admin'), 'Fleet admin');
  assert.equal(mod.desktopOnlyView('vegas'), 'Vegas');
  assert.equal(mod.desktopOnlyView('groups', 'routes'), 'Route map');
  assert.equal(mod.desktopOnlyView('groups', 'members'), '');
  assert.equal(mod.desktopOnlyView('access'), '');
});

test('the note and the page both switch to the desktop layout', async (t) => {
  const harness = await createPreactHarness(t);
  const mod = await harness.importDashboardModule('js/phone-desktop-only.js');
  const opened = [];
  const open = () => opened.push(1);
  const note = await harness.mount(harness.html`<${mod.DesktopOnlyNote} what="Editing is" open=${open} />`);
  assert.equal(note.container.textContent.replace(/\s+/g, ' ').trim(), 'Editing is desktop only. Desktop site ›');
  await harness.act(() => note.container.querySelector('button').click());
  const page = await harness.mount(harness.html`<${mod.DesktopOnlyPage} what="Route map" open=${open} />`);
  assert.match(page.container.querySelector('h2').textContent, /^Route map is desktop only$/);
  await harness.act(() => page.container.querySelector('button').click());
  assert.equal(opened.length, 2);
});
