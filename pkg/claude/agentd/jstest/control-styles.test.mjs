// A button with no rule of its own renders as the browser's white default on
// the dark dashboard (tcl-okc0iu). A stylesheet scan, so each control named
// here keeps a rule that sets its colours.

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';

const css = await readFile(new URL('../dashboard/dashboard.css', import.meta.url), 'utf8');

function ruleFor(selector) {
  const at = css.indexOf(`${selector} {`);
  assert.notEqual(at, -1, `${selector} has a rule`);
  return css.slice(at, css.indexOf('}', at));
}

test('the Config row buttons and the import Reveal button set their own chrome', () => {
  for (const selector of ['.cfg-row-ctl button.cfg-inline', '.import-confirm-reveal']) {
    const rule = ruleFor(selector);
    assert.match(rule, /background:/, selector);
    assert.match(rule, /border:/, selector);
    assert.match(rule, /color:/, selector);
  }
});

test('the conversation search close button shares the filter bar clear button', () => {
  assert.match(ruleFor(':is(.filter-bar, .content-search-bar) button.clear-filter'), /background: transparent/);
});

test('bare checkboxes and radios render dark at zero specificity', () => {
  const rule = ruleFor(':where(input[type="checkbox"], input[type="radio"])');
  assert.match(rule, /color-scheme: dark/);
  assert.match(rule, /accent-color:/);
});
