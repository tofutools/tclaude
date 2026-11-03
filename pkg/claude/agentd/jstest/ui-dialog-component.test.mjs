// Dialogs use the Dialog component (js/ui-dialog.js): it renders the title the
// overlay is labelled by, the error slot (always role="alert"), the
// .modal-buttons footer and the guarded close on top of ManagementOverlay.
// Dialogs used to hand-write those around the overlay, and drifted: about a
// third of the error slots never announced themselves.
//
// Every dialog now goes through Dialog, so rendering ManagementOverlay
// directly (under any import alias) outside its owners fails. A dialog whose
// title sits deeper in its own markup passes labelledby and keeps its heading.

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync } from 'node:fs';
import { createPreactHarness } from './preact-harness.mjs';

const JS = new URL('../dashboard/js/', import.meta.url);
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

// Dialogs used to build their own .modal-overlay around a role="dialog" box
// and trap focus with useDialogFocus, each with its own backdrop click, error
// line and footer. The plugin, daemon-profile, process rename/create, board
// delete, config diff and sandbox diff dialogs were the last of those. What is
// left is not a form dialog: the media and Markdown previews, the command
// palette (a popover on the overlay backdrop) and the shell confirm, which
// every other dialog stacks on.
const OWN_OVERLAY = new Set(['image-preview-overlay.js', 'markdown-attachment.js', 'palette-island.js', 'shell-island.js']);
const OWN_FOCUS = new Set(['dialog-focus.js', 'dialog-resize.js', 'management-overlay.js', 'image-preview-overlay.js', 'markdown-attachment.js']);

test('no new dialog builds its own overlay or focus trap', () => {
  const files = readdirSync(JS).filter((f) => f.endsWith('.js')).sort();
  const source = (name) => readFileSync(new URL(name, JS), 'utf8');
  const overlays = files.filter((name) => !OWN_OVERLAY.has(name) && /class=["'`$]\{?`?[^"'`]*\bmodal-overlay\b/.test(source(name)));
  assert.deepEqual(overlays, [], 'render dialogs with Dialog from js/ui-dialog.js; it builds the .modal-overlay');
  const traps = files.filter((name) => !OWN_FOCUS.has(name) && /\buseDialogFocus\b/.test(source(name)));
  assert.deepEqual(traps, [], 'Dialog traps focus and handles Escape; do not call useDialogFocus for a dialog');
});

test('no file renders ManagementOverlay directly', () => {
  // Any mention (an import, an htm tag, an h() call) outside the owners
  // counts, so neither an alias nor hyperscript slips past.
  const raw = readdirSync(JS).filter((f) => f.endsWith('.js') && !OWNERS.has(f)).sort()
    .filter((name) => /\bManagementOverlay\b/.test(readFileSync(new URL(name, JS), 'utf8')));
  assert.deepEqual(raw, [], 'render dialogs with Dialog from js/ui-dialog.js, not ManagementOverlay directly');
  // Dialog registers its own guarded close; a dialog that owns one passes it
  // as guard=${{ requestClose, registerClose }}. A registerClose prop would be
  // replaced silently and its Cancel would stop closing.
  const passthrough = readdirSync(JS).filter((f) => f.endsWith('.js') && !OWNERS.has(f)).sort()
    .filter((name) => /\bregisterClose(?:=\$\{|\s*:|,\s*$)/m.test(readFileSync(new URL(name, JS), 'utf8')));
  assert.deepEqual(passthrough, [], 'pass a dialog-owned close to Dialog as guard, not registerClose');
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
