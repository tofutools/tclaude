// A popover or menu that a press outside (or Escape) closes takes that rule
// from popover-watch.js: watchPopover, or the useDismissablePopover hook. The
// shared rule also waits behind a dialog that was already up, closes for a
// dialog opened over it, and gives Escape only to the popover opened last.
// Hand-rolled document listeners skipped all of that and drifted apart.
//
// So a document- or window-level pointerdown / mousedown / touchstart
// listener is allowed only in the files below, each of which uses the press
// for something other than closing a popover. A new one fails: use the shared rule, or add the file here with
// the reason it is not a popover.

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync } from 'node:fs';

const JS = new URL('../dashboard/js/', import.meta.url);

const ALLOWED = {
  'popover-watch.js': 'the shared rule itself',
  'drawing-board-island.js': 'a press elsewhere ends the drawing board\'s claim on page keys',
  'process-editor-place.js': 'tracks which process editor the operator touched last',
  'overlay-snap-assist.js': 'window snapping (overlay-window area): the press picks or ends a snap',
  'row-actions.js': 'a new gesture makes a context-menu activation stale',
  'row-drag.js': 'a press on a control in a draggable table row (agent, resource, task) turns that row\'s drag off for the press, so the control clicks; it closes nothing',
  'side-panel-reveal.js': 'a press takes a side-panel peek back, so a click on the scrollbar does not slide the panel in under the drag; it closes nothing',
  'vegas.js': 'the first press unmutes audio',
};

const PRESS = /\b(?:document|doc|documentRef|ownerDocument|owner|window|win|globalThis|body|documentElement)\??\.addEventListener\??\.?\(\s*['"`](?:pointerdown|mousedown|touchstart)['"`]/;

export function pressListenerFiles(sources) {
  return Object.entries(sources).filter(([, src]) => PRESS.test(src)).map(([file]) => file).sort();
}

test('the scanner finds document press listeners', () => {
  assert.deepEqual(pressListenerFiles({
    'a.js': "document.addEventListener('pointerdown', away, true);",
    'b.js': "doc?.addEventListener?.('mousedown', away);",
    'c.js': "button.addEventListener('pointerdown', press);",
    'e.js': "window.addEventListener(`touchstart`, away);",
    'd.js': "document.addEventListener('click', delegate);",
  }), ['a.js', 'b.js', 'e.js']);
});

test('outside-press dismissal goes through the shared popover rule', () => {
  const sources = {};
  for (const file of readdirSync(JS).filter((f) => f.endsWith('.js'))) sources[file] = readFileSync(new URL(file, JS), 'utf8');
  const found = pressListenerFiles(sources);
  assert.deepEqual(found.filter((f) => !ALLOWED[f]), [], 'close a popover with watchPopover / useDismissablePopover (hover-popover.js)');
  assert.deepEqual(Object.keys(ALLOWED).filter((f) => !found.includes(f)).sort(), [], 'drop a file that no longer listens from ALLOWED');
});
