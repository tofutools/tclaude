// Help and error text use the standard .ui-hint / .ui-error / .ui-error-box
// (dashboard/ui.css). Areas used to hand-roll their own: a *-hint, *-help or
// *-error class with its own grey or red, size and line height, plus a wizard
// copy per dialog. An area may keep such a class for placement, but its text
// style comes from ui.css: a stylesheet rule whose subject is a bare *-hint,
// *-help or *-error class may not set color, font-size, font-style or
// line-height. A state variant (`.x-hint.warn`, `.x-error:hover`) may still
// recolour.
//
// Existing hand-rolled ones are a ratchet: ui-hint-baseline.json lists the
// classes not yet migrated (several sit in areas another migration owns, and
// some are not text at all, such as a badge). A new one fails; a migrated one
// must leave the baseline. Regenerate after a migration with
//   UPDATE_UI_HINT_BASELINE=1 node --test jstest/ui-hint.test.mjs

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync, writeFileSync } from 'node:fs';

const DASHBOARD = new URL('../dashboard/', import.meta.url);
const BASELINE = new URL('./ui-hint-baseline.json', import.meta.url);

// handRolledText lists the *-hint, *-help and *-error classes that a rule
// gives its own text style as the bare subject.
export function handRolledText(css) {
  const source = css.replace(/\/\*[\s\S]*?\*\//g, '');
  const found = new Set();
  for (const [, selector, body] of source.matchAll(/([^{};]*)\{([^{}]*)\}/g)) {
    if (!/(?:^|[;\s])(?:color|font-size|font-style|line-height)\s*:/.test(body)) continue;
    for (const part of selector.split(/,(?![^()]*\))/)) {
      const subject = part.trim().split(/\s*[\s>+~]\s*/).pop();
      const bare = subject.match(/^\.([\w-]+-(?:hint|help|error))$/);
      if (bare) found.add(bare[1]);
    }
  }
  return found;
}

test('the scanner finds hand-rolled help and error text', () => {
  const css = [
    '.a-hint { margin: 4px 0; }',
    '.b-hint { color: #888; font-size: 11px; }',
    'body.wizard #m .c-error, .other { color: red; }',
    '.d-hint.warn { color: orange; }',
    '.e-help:hover { color: #fff; }',
    '/* .f-hint { color: red; } */',
    '.g-error strong { color: red; }',
    ':is(.h-hint, .i-hint) { line-height: 1.4; }',
  ].join('\n');
  assert.deepEqual([...handRolledText(css)].sort(), ['b-hint', 'c-error']);
});

test('ui.css defines the standard help and error text', () => {
  const css = readFileSync(new URL('ui.css', DASHBOARD), 'utf8');
  assert.match(css, /\.ui-hint \{[^}]*color: var\(--ui-muted\)/);
  assert.match(css, /\.ui-error \{[^}]*color: var\(--ui-danger-text\)/);
  assert.match(css, /\.ui-error-box \{[^}]*border-left: 3px solid var\(--ui-danger\)/);
});

test('no stylesheet hand-rolls new help or error text', () => {
  const found = new Set();
  for (const file of readdirSync(DASHBOARD).filter((f) => f.endsWith('.css') && f !== 'ui.css')) {
    for (const cls of handRolledText(readFileSync(new URL(file, DASHBOARD), 'utf8'))) found.add(cls);
  }
  if (process.env.UPDATE_UI_HINT_BASELINE) writeFileSync(BASELINE, `${JSON.stringify([...found].sort(), null, 2)}\n`);
  const baseline = new Set(JSON.parse(readFileSync(BASELINE, 'utf8')));
  assert.deepEqual([...found].filter((c) => !baseline.has(c)).sort(), [],
    'style these with .ui-hint / .ui-error / .ui-error-box instead; keep an area class for placement only');
  assert.deepEqual([...baseline].filter((c) => !found.has(c)).sort(), [],
    'these are migrated: remove them from jstest/ui-hint-baseline.json');
});
