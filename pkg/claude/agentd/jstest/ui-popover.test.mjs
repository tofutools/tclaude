// Popovers (floating panels anchored to a control: node pickers, the open-PRs
// list, filter and settings panels) use the standard .ui-popover (ui.css),
// painted from the same --ui-dialog-* tokens as a dialog box. They used to
// paint their own panel, each with its own background, edge, corner and
// shadow, and only some had a wizard skin. An area class keeps position, size
// and layout; a stylesheet rule whose subject is a *-pop / *-popover class may
// not paint a panel itself, meaning a background and a border in one rule.
// Menus of actions use .ui-menu and are not checked here.
//
// Existing ones are a ratchet: ui-popover-baseline.json lists, by selector,
// the panels not yet migrated. A new one fails, and a migrated one must leave
// the baseline. Regenerate with
//   UPDATE_UI_POPOVER_BASELINE=1 node --test jstest/ui-popover.test.mjs

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync, writeFileSync } from 'node:fs';

const DASHBOARD = new URL('../dashboard/', import.meta.url);
const BASELINE = new URL('./ui-popover-baseline.json', import.meta.url);
const PANEL = [/(?:^|[;\s])background(?:-color)?\s*:/, /(?:^|[;\s])border(?:-color)?\s*:/];

// handRolledPopovers lists the selectors (whitespace-normalised) whose subject
// is a single *-pop / *-popover class or id that the rule paints as a panel. A
// state (`:hover`, `.open`) is not a panel.
export function handRolledPopovers(css) {
  const source = css.replace(/\/\*[\s\S]*?\*\//g, '');
  const found = new Set();
  for (const [, selector, body] of source.matchAll(/([^{};]*)\{([^{}]*)\}/g)) {
    if (!PANEL.every((prop) => prop.test(body))) continue;
    for (const part of selector.split(/,(?![^()]*\))/)) {
      const subject = part.trim().split(/\s*[\s>+~]\s*/).pop();
      if (/[:[]/.test(subject)) continue;
      const names = [...subject.matchAll(/[.#]([\w-]+)/g)].map((m) => m[1]);
      if (names.length === 1 && /-(?:pop|popover)$/.test(names[0])) found.add(part.trim().replace(/\s+/g, ' '));
    }
  }
  return found;
}

test('the scanner finds hand-painted popovers', () => {
  const css = [
    '.a-pop { position: absolute; width: 300px; }',
    '.b-popover { background: #161b22; border: 1px solid #30363d; }',
    'body.wizard #x .c-pop, .other { background: violet; border-color: gold; }',
    '.d-pop.open { background: red; border-color: red; }',
    '#e-pop { background: #0d1117; border: 1px solid #2a3340; }',
    '/* .f-pop { background: red; border: 0; } */',
  ].join('\n');
  assert.deepEqual([...handRolledPopovers(css)].sort(), ['#e-pop', '.b-popover', 'body.wizard #x .c-pop']);
});

test('ui.css defines the standard popover on the dialog tokens', () => {
  const css = readFileSync(new URL('ui.css', DASHBOARD), 'utf8');
  assert.match(css, /\.ui-popover \{[^}]*background: var\(--ui-dialog-bg\)/);
});

test('no stylesheet hand-paints a new popover', () => {
  const found = new Set();
  for (const file of readdirSync(DASHBOARD).filter((f) => f.endsWith('.css') && f !== 'ui.css')) {
    for (const sel of handRolledPopovers(readFileSync(new URL(file, DASHBOARD), 'utf8'))) found.add(sel);
  }
  if (process.env.UPDATE_UI_POPOVER_BASELINE) writeFileSync(BASELINE, `${JSON.stringify([...found].sort(), null, 2)}\n`);
  const baseline = new Set(JSON.parse(readFileSync(BASELINE, 'utf8')));
  assert.deepEqual([...found].filter((c) => !baseline.has(c)).sort(), [],
    'paint popovers with .ui-popover (skin it through the --ui-dialog-* tokens); keep an area class for position, size and layout');
  assert.deepEqual([...baseline].filter((c) => !found.has(c)).sort(), [],
    'these are migrated: remove them from jstest/ui-popover-baseline.json');
});
