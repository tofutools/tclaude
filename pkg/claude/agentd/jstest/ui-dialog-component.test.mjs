// Dialogs use the Dialog component (js/ui-dialog.js): it renders the title the
// overlay is labelled by, the error slot (always role="alert"), the
// .modal-buttons footer and the guarded close on top of ManagementOverlay.
// Dialogs used to hand-write those around the overlay, and drifted: about a
// third of the error slots never announced themselves.
//
// Rendering ManagementOverlay directly is a ratchet: ui-dialog-component-
// baseline.json records each file's count of <${ManagementOverlay}> tags
// (under any import alias). A file may not exceed it, a new file may not
// start, and a migrated file must bring its count down. Regenerate with
//   UPDATE_UI_DIALOG_COMPONENT_BASELINE=1 node --test jstest/ui-dialog-component.test.mjs

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync, writeFileSync } from 'node:fs';
import { createPreactHarness } from './preact-harness.mjs';

const JS = new URL('../dashboard/js/', import.meta.url);
const BASELINE = new URL('./ui-dialog-component-baseline.json', import.meta.url);
const OWNERS = new Set(['management-overlay.js', 'ui-dialog.js']);

// rawOverlays counts the <${…}> tags in source that render ManagementOverlay,
// following `ManagementOverlay as X` import aliases.
export function rawOverlays(source) {
  const names = new Set(['ManagementOverlay']);
  for (const [, alias] of source.matchAll(/\bManagementOverlay\s+as\s+(\w+)/g)) names.add(alias);
  let count = 0;
  for (const name of names) count += source.match(new RegExp(`<\\$\\{${name}\\}`, 'g'))?.length ?? 0;
  return count;
}

test('the scanner counts direct overlays under any alias', () => {
  assert.equal(rawOverlays("import { ManagementOverlay as Overlay } from './management-overlay.js';\nhtml`<${Overlay} id=a></${Overlay}>`; html`<${Overlay}>`"), 2);
  assert.equal(rawOverlays('html`<${ManagementOverlay} id=a>`'), 1);
  assert.equal(rawOverlays("import { Dialog } from './ui-dialog.js';\nhtml`<${Dialog} id=a>`"), 0);
});

test('no file renders ManagementOverlay directly beyond its baseline', () => {
  const counts = {};
  for (const name of readdirSync(JS).filter((f) => f.endsWith('.js') && !OWNERS.has(f)).sort()) {
    const n = rawOverlays(readFileSync(new URL(name, JS), 'utf8'));
    if (n) counts[`js/${name}`] = n;
  }
  if (process.env.UPDATE_UI_DIALOG_COMPONENT_BASELINE) writeFileSync(BASELINE, `${JSON.stringify(counts, null, 2)}\n`);
  const baseline = JSON.parse(readFileSync(BASELINE, 'utf8'));
  const over = Object.entries(counts).filter(([file, n]) => n > (baseline[file] ?? 0)).map(([file, n]) => `${file}: ${n} (baseline ${baseline[file] ?? 0})`);
  assert.deepEqual(over, [], 'render dialogs with Dialog from js/ui-dialog.js, not ManagementOverlay directly');
  const stale = Object.entries(baseline).filter(([file, n]) => (counts[file] ?? 0) < n).map(([file]) => file);
  assert.deepEqual(stale, [], 'a migrated file must bring its baseline down (UPDATE_UI_DIALOG_COMPONENT_BASELINE=1)');
});

async function setup(t) {
  const harness = await createPreactHarness(t);
  const dialog = await harness.importDashboardModule('js/ui-dialog.js');
  const ui = await harness.importDashboardModule('js/ui-components.js');
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  return { harness, dialog, ui, host };
}

test('Dialog derives its ids, labels itself and always announces errors', async (t) => {
  const { harness, dialog, host } = await setup(t);
  await harness.mount(harness.html`<${dialog.Dialog} id="thing-modal" title="Make a thing" error="" cancel="Cancel"
    onClose=${() => {}} footer=${harness.html`<button class="ui-btn" id="thing-submit">Make</button>`}
    after=${harness.html`<div id="thing-note">note</div>`}>
    <p id="thing-body">body</p>
  <//>`, host);
  const box = host.querySelector('[role="dialog"]');
  assert.equal(box.getAttribute('aria-labelledby'), 'thing-title');
  assert.equal(host.querySelector('#thing-title').tagName, 'H3');
  assert.equal(host.querySelector('#thing-title').textContent, 'Make a thing');
  const error = host.querySelector('#thing-error');
  assert.equal(error.getAttribute('role'), 'alert', 'the slot is a live region before any error arrives');
  assert.equal(error.className, 'cron-create-error');
  const footer = host.querySelector('.modal-buttons');
  assert.deepEqual([...footer.children].map((el) => el.id || el.className), ['thing-cancel', 'spacer', 'thing-submit']);
  assert.ok(error.compareDocumentPosition(footer) & 4, 'the error sits above the footer');
  assert.equal(footer.nextElementSibling.id, 'thing-note', 'after follows the footer');
});

test('Dialog without error, title or footer props renders none of them', async (t) => {
  const { harness, dialog, host } = await setup(t);
  await harness.mount(harness.html`<${dialog.Dialog} id="bare-modal" labelledby="own-title" onClose=${() => {}}>
    <h3 id="own-title">Own</h3>
  <//>`, host);
  assert.equal(host.querySelector('[role="dialog"]').getAttribute('aria-labelledby'), 'own-title');
  assert.equal(host.querySelector('.cron-create-error'), null);
  assert.equal(host.querySelector('.modal-buttons'), null);
});

test('DialogClose and useDialogClose go through the guarded close', async (t) => {
  const { harness, dialog, host } = await setup(t);
  let closed = 0;
  let close = null;
  function Body() {
    close = dialog.useDialogClose();
    return null;
  }
  const view = (busy) => harness.html`<${dialog.Dialog} id="g-modal" title="G" cancel="Cancel" busy=${busy}
    onClose=${() => { closed += 1; }}><${Body} /><//>`;
  await harness.mount(view(true), host);
  const cancel = host.querySelector('#g-cancel');
  assert.equal(cancel.disabled, true, 'busy disables cancel');
  assert.equal(await close(), false, 'busy blocks the close');
  assert.equal(closed, 0);
  await harness.mount(view(false), host);
  host.querySelector('#g-cancel').click();
  await harness.act(() => Promise.resolve());
  assert.equal(closed, 1);
});
