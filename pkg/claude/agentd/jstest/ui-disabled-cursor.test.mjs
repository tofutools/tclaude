// Disabled controls show a not-allowed cursor everywhere (operator rule). Old
// per-surface skins set cursor: default on :disabled at every specificity, so
// ui.css carries one !important rule over all native and ARIA controls. A
// stylesheet scan: it holds that rule in place and keeps any other sheet from
// out-ranking it or hiding the cursor with pointer-events: none.

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync } from 'node:fs';

const DASHBOARD = new URL('../dashboard/', import.meta.url);
const read = (file) => readFileSync(new URL(file, DASHBOARD), 'utf8');
const SHEETS = readdirSync(DASHBOARD).filter((name) => name.endsWith('.css')).sort();
const uncommented = (css) => css.replace(/\/\*[\s\S]*?\*\//g, '');
const DISABLED = /:disabled|\[disabled\]|aria-disabled/;

function rules() {
  const out = [];
  for (const sheet of SHEETS) {
    for (const [, selector, body] of uncommented(read(sheet)).matchAll(/([^{}]+)\{([^{}]*)\}/g)) {
      out.push({ sheet, selector: selector.trim().replace(/\s+/g, ' '), body });
    }
  }
  return out;
}

test('ui.css gives every disabled control a not-allowed cursor', () => {
  const css = uncommented(read('ui.css'));
  const rule = css.match(/:is\(button, select, input, textarea, \[role="button"\], \[role="menuitem"\], \[role="tab"\], \[role="option"\]\):is\(:disabled, \[aria-disabled="true"\]\),\s*label:has\(> input:disabled\) \{ cursor: not-allowed !important; \}/);
  assert.ok(rule, 'the global disabled-cursor rule is in ui.css');
  assert.ok(css.lastIndexOf('cursor: not-allowed !important') > css.lastIndexOf('cursor: default'), 'and it comes after any default cursor in ui.css');
  for (const part of ['.ui-btn:disabled', '.ui-select:disabled', '.ui-input:disabled', '.ui-menu-item:disabled', '.ui-switch:disabled']) {
    const own = rules().find((r) => r.sheet === 'ui.css' && r.selector.split(', ').includes(part));
    assert.ok(own, `ui.css styles ${part}`);
    assert.match(own.body, /cursor: not-allowed/, `${part} says not-allowed on its own`);
  }
});

test('no sheet hides or overrides the disabled cursor', () => {
  const bad = rules().filter((r) => DISABLED.test(r.selector)
    && (/pointer-events:\s*none/.test(r.body) || /cursor:\s*(?!not-allowed|progress)[\w-]+\s*!important/.test(r.body)));
  assert.deepEqual(bad.map((r) => `${r.sheet}: ${r.selector}`), [], [
    'A disabled control must keep its not-allowed cursor:',
    '  - no pointer-events: none on :disabled / aria-disabled (it swallows the cursor; put it on a wrapper),',
    '  - no cursor !important other than not-allowed (or progress while aria-busy).',
  ].join('\n'));
});
