// The Back rule's guard (js/back-layers.js): while an overlay is open, Back
// is the overlay's and never steps the page behind it. That holds only if
// every modal overlay is a Back layer, so this fails when one is drawn
// without registering.
import test from 'node:test';
import assert from 'node:assert/strict';
import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { createPreactHarness } from './preact-harness.mjs';

const JS = fileURLToPath(new URL('../dashboard/js/', import.meta.url));

// A modal overlay's markup: aria-modal true (or bound), or the shared
// modal/manage overlay shell.
const MODAL = /(?<!\[)aria-modal=(?:"true"|\$\{)|class=\$?\{?[`"'][^`"']*\b(?:modal-overlay|manage-overlay)\b/;
// Registering: through the shared overlay (every Dialog), or by hand.
const REGISTERS = /\b(?:useBackLayer|registerBackLayer)\(|<\$\{Dialog\}/;
// Back layers of their own: history steps the router already takes back.
const OWN_STEP = new Map([
  ['phone-sheet.js', 'a phone sheet is a phone-history layer'],
  ['phone-terminal-island.js', 'the phone terminal is the routed ?terminal overlay'],
]);

test('every modal overlay is a Back layer', () => {
  const missing = [];
  for (const file of readdirSync(JS).filter((name) => name.endsWith('.js')).sort()) {
    const source = readFileSync(join(JS, file), 'utf8');
    if (!MODAL.test(source) || REGISTERS.test(source) || OWN_STEP.has(file)) continue;
    missing.push(file);
  }
  assert.deepEqual(missing, [],
    'these draw a modal overlay that Back does not close: call useBackLayer(open, close) (back-layers.js), or use Dialog');
});

test('the topmost layer closes first, and one that stays open says so', async (t) => {
  const harness = await createPreactHarness(t);
  const layers = await harness.importDashboardModule('js/back-layers.js');
  const closed = [];
  const off1 = layers.registerBackLayer({ close: () => { closed.push('form'); return true; } });
  const off2 = layers.registerBackLayer({ close: async () => { closed.push('confirm'); return false; } });
  t.after(() => { off1(); off2(); });
  assert.equal(await layers.closeTopBackLayer(), false, 'a layer that stayed open');
  assert.deepEqual(closed, ['confirm']);
  off2();
  assert.equal(await layers.closeTopBackLayer(), true);
  assert.deepEqual(closed, ['confirm', 'form']);
});
