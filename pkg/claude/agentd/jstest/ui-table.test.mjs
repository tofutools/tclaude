// Tables come in two densities (operator pick A): the global table rule in
// dashboard.css is the roomy default for main lists, and .ui-compact (ui.css)
// packs a table inside a dialog, panel or dense view. Header and row lines
// are set once there. Areas used to give each table its own cell padding and
// line colour (five paddings, four line greys).
//
// A rule that sets cell padding or a row line on an area's th / td is a
// per-table density, counted against ui-table-baseline.json by selector. A
// new one fails, and a removed one must leave the baseline. Regenerate with
//   UPDATE_UI_TABLE_BASELINE=1 node --test jstest/ui-table.test.mjs

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync, writeFileSync } from 'node:fs';

const DASHBOARD = new URL('../dashboard/', import.meta.url);
const BASELINE = new URL('./ui-table-baseline.json', import.meta.url);
const DENSITY = /(?:^|[;\s])(?:padding(?:-[a-z]+)?|border-bottom(?:-color)?)\s*:/;

// cellDensities lists selectors (whitespace-normalised, skins excluded) whose
// subject is a th or td under some class or id and whose rule sets cell
// padding or a bottom line.
export function cellDensities(css) {
  const found = new Set();
  const source = css.replace(/\/\*[\s\S]*?\*\//g, '');
  for (const [, selector, body] of source.matchAll(/([^{};]*)\{([^{}]*)\}/g)) {
    if (!DENSITY.test(body)) continue;
    for (const raw of selector.split(/,(?![^()]*\))/)) {
      const part = raw.trim().replace(/\s+/g, ' ');
      if (!part || /^body\.(?:wizard|slop)\b/.test(part)) continue;
      const steps = part.split(/\s*[\s>+~]\s*/);
      const subject = steps.at(-1);
      if (!/^(?:th|td)\b/.test(subject) && !/^:is\((?:th|td)/.test(subject)) continue;
      if (part === 'table.ui-compact :is(th, td)') continue;
      if (steps.slice(0, -1).some((step) => /[.#]/.test(step))) found.add(part);
    }
  }
  return found;
}

test('the scanner finds per-table cell densities', () => {
  const css = [
    'table th, table td { padding: 8px 12px; }',
    'table.ui-compact :is(th, td) { padding: 4px 8px; }',
    '.a-table td { padding: 3px 6px; }',
    '.b-table th, .c { border-bottom: 1px solid #333; }',
    '.d-table td { vertical-align: middle; }',
    '.f-table.ui-compact td { padding: 1px; }',
    'body.wizard .e-table td { border-bottom-color: violet; }',
  ].join('\n');
  assert.deepEqual([...cellDensities(css)].sort(), ['.a-table td', '.b-table th', '.f-table.ui-compact td']);
});

test('no new per-table cell density', () => {
  const now = {};
  for (const file of readdirSync(DASHBOARD).filter((f) => f.endsWith('.css')).sort()) {
    const found = [...cellDensities(readFileSync(new URL(file, DASHBOARD), 'utf8'))].sort();
    if (found.length) now[file] = found;
  }
  if (process.env.UPDATE_UI_TABLE_BASELINE) writeFileSync(BASELINE, `${JSON.stringify(now, null, 2)}\n`);
  const base = JSON.parse(readFileSync(BASELINE, 'utf8'));
  const added = Object.entries(now).flatMap(([f, sels]) => sels.filter((s) => !(base[f] || []).includes(s)).map((s) => `${f}: ${s}`));
  assert.deepEqual(added, [], 'tables use the default density or .ui-compact, not their own cell padding / row lines');
  const gone = Object.entries(base).flatMap(([f, sels]) => sels.filter((s) => !(now[f] || []).includes(s)).map((s) => `${f}: ${s}`));
  assert.deepEqual(gone, [], 'a removed density must leave the baseline (UPDATE_UI_TABLE_BASELINE=1)');
});

test('ui.css defines the compact density', () => {
  assert.match(readFileSync(new URL('ui.css', DASHBOARD), 'utf8'), /table\.ui-compact :is\(th, td\) \{ padding: 4px 8px; \}/);
});
