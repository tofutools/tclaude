// A textarea with a resize grip must not take `flex: 1` (flex-basis 0%) as its
// final flex value (tcl-2vkq1y). Inside a column flex, such as the task
// board's .tb-new-field, the comment composer and the comment editor, the
// flex basis decides the height, so a 0% basis overrides the height the grip
// writes: the grip shows, the cursor changes, and dragging does nothing.
// LinkeDOM has no layout, so this is a stylesheet scan: for each shared
// textarea selector, the last `flex` it gets in source order keeps basis auto.

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';

const css = (await readFile(new URL('../dashboard/dashboard.css', import.meta.url), 'utf8'))
  .replace(/\/\*[\s\S]*?\*\//g, '');

// lastFlex returns the last `flex` declaration of a rule whose selector list
// names selector exactly, in source order.
function lastFlex(selector) {
  let flex = null;
  for (const match of css.matchAll(/([^{}]+)\{([^{}]*)\}/g)) {
    const selectors = match[1].split(',').map((s) => s.trim());
    if (!selectors.includes(selector)) continue;
    const decl = match[2].match(/(?:^|;)\s*flex\s*:\s*([^;]+)/);
    if (decl) flex = decl[1].trim();
  }
  return flex;
}

test('shared form textareas keep flex-basis auto so the resize grip works', () => {
  for (const selector of ['.cron-create-row textarea', '.sbx-section textarea', '.template-agent-row textarea']) {
    const flex = lastFlex(selector);
    assert.ok(flex, `${selector} sets flex`);
    const parts = flex.split(/\s+/);
    // `flex: <n>` alone means basis 0%; `auto`, `none` or a three-part value
    // ending in auto keep the grip's height as the basis.
    const basisAuto = flex === 'auto' || flex === 'none' || (parts.length === 3 && parts[2] === 'auto');
    assert.ok(basisAuto, `${selector} ends with flex: ${flex}, whose basis is not auto`);
  }
});
