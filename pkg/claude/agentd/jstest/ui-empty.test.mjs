// Empty states use the standard .ui-empty / .ui-empty-block (dashboard/ui.css):
// the muted line a list, panel or popover shows when it has nothing to show.
// Areas used to hand-roll one each (*-empty classes with their own colour,
// size and italics, plus a wizard copy), which drifted into a dozen looks.
// An area may keep a *-empty class for placement, but its text style comes
// from ui.css: a stylesheet rule on a *-empty class (or .empty) may not set
// color, font-size or font-style.
//
// Existing hand-rolled ones are a ratchet: ui-empty-baseline.json lists the
// classes not yet migrated (several sit in areas another migration owns). A
// new one fails; a migrated one must leave the baseline.
// Regenerate after a migration with
//   UPDATE_UI_EMPTY_BASELINE=1 node --test jstest/ui-empty.test.mjs

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync, writeFileSync } from 'node:fs';

const DASHBOARD = new URL('../dashboard/', import.meta.url);
const BASELINE = new URL('./ui-empty-baseline.json', import.meta.url);

// handRolledEmpties lists the *-empty classes whose rules set text style.
export function handRolledEmpties(css) {
  const source = css.replace(/\/\*[\s\S]*?\*\//g, '');
  const found = new Set();
  for (const [, selector, body] of source.matchAll(/([^{};]*)\{([^{}]*)\}/g)) {
    if (!/(?:^|[;\s])(?:color|font-size|font-style)\s*:/.test(body)) continue;
    for (const part of selector.split(',')) {
      // The subject is the last compound selector; a pseudo-element or a
      // descendant such as `.x-empty strong` styles something else.
      const subject = part.trim().split(/\s*[\s>+~]\s*/).pop();
      if (/::/.test(subject)) continue;
      for (const [, cls] of subject.matchAll(/\.((?:[\w-]+-)?empty)(?![\w-])/g)) found.add(cls);
    }
  }
  return found;
}

test('the scanner finds hand-rolled empty states', () => {
  const css = [
    '.a-empty { padding: 4px; }',
    '.b-empty { color: #888; font-size: 11px; }',
    'body.wizard #m .c-empty, .other { color: red; }',
    '.d-empty strong { color: #fff; }',
    '/* .e-empty { color: red; } */',
    '.list.empty { font-style: italic; }',
    '.f-empty::after { color: red; }',
    '.g-emptyish { color: red; }',
  ].join('\n');
  assert.deepEqual([...handRolledEmpties(css)].sort(), ['b-empty', 'c-empty', 'empty']);
});

test('ui.css defines the standard empty states', () => {
  const css = readFileSync(new URL('ui.css', DASHBOARD), 'utf8');
  assert.match(css, /\.ui-empty, \.ui-empty-block \{[^}]*color: var\(--ui-muted\)/);
  assert.match(css, /\.ui-empty-block \{[^}]*text-align: center/);
});

test('no stylesheet hand-rolls a new empty state', () => {
  const found = new Set();
  for (const file of readdirSync(DASHBOARD).filter((f) => f.endsWith('.css') && f !== 'ui.css')) {
    for (const cls of handRolledEmpties(readFileSync(new URL(file, DASHBOARD), 'utf8'))) found.add(cls);
  }
  if (process.env.UPDATE_UI_EMPTY_BASELINE) writeFileSync(BASELINE, `${JSON.stringify([...found].sort(), null, 2)}\n`);
  const baseline = new Set(JSON.parse(readFileSync(BASELINE, 'utf8')));
  assert.deepEqual([...found].filter((c) => !baseline.has(c)).sort(), [],
    'style these with .ui-empty / .ui-empty-block instead; keep an area class for placement only');
  assert.deepEqual([...baseline].filter((c) => !found.has(c)).sort(), [],
    'these are migrated: remove them from jstest/ui-empty-baseline.json');
});
