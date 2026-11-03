// Small uppercase labels over a group of fields, a list section or a popover
// use the standard .ui-eyebrow (dashboard/ui.css; operator pick C): 11px,
// .06em, 600, muted, with .ui-eyebrow-section for settings / section heads in
// the accent. Areas used to set their own (9-12px, .04-.08em or .3-.6px,
// weight 400-700, four greys or blue). An area class keeps placement only.
//
// A rule that sets uppercase lettering with its own letter-spacing, without a
// border (chips are .ui-tag's ratchet) and not on a table header, is a
// hand-rolled eyebrow, counted against ui-eyebrow-baseline.json by selector.
// A new one fails, and a migrated one must leave the baseline. Regenerate with
//   UPDATE_UI_EYEBROW_BASELINE=1 node --test jstest/ui-eyebrow.test.mjs

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync, writeFileSync } from 'node:fs';

const DASHBOARD = new URL('../dashboard/', import.meta.url);
const BASELINE = new URL('./ui-eyebrow-baseline.json', import.meta.url);

// handRolledEyebrows lists selectors (whitespace-normalised, skins and table
// headers excluded) whose rule letters a label in spaced uppercase.
export function handRolledEyebrows(css) {
  const found = new Set();
  const source = css.replace(/\/\*[\s\S]*?\*\//g, '');
  for (const [, selector, body] of source.matchAll(/([^{};]*)\{([^{}]*)\}/g)) {
    if (!/(?:^|[;\s])text-transform\s*:\s*uppercase/.test(body)) continue;
    if (!/(?:^|[;\s])letter-spacing\s*:/.test(body) || /(?:^|[;\s])border\s*:\s*1px/.test(body)) continue;
    for (const raw of selector.split(/,(?![^()]*\))/)) {
      const part = raw.trim().replace(/\s+/g, ' ');
      if (!part || /^body\.(?:wizard|slop)\b/.test(part) || /^\.ui-eyebrow\b/.test(part)) continue;
      if (/^th\b/.test(part.split(/\s*[\s>+~]\s*/).at(-1))) continue;
      found.add(part);
    }
  }
  return found;
}

test('the scanner finds hand-rolled eyebrows', () => {
  const css = [
    '.a-head { font-size: 10px; text-transform: uppercase; letter-spacing: .04em; color: #7d8590; }',
    '.b-chip { font-size: 10px; text-transform: uppercase; letter-spacing: .04em; border: 1px solid #333; }',
    '.c-list th { font-size: 11px; text-transform: uppercase; letter-spacing: .04em; }',
    '.d-label { text-transform: uppercase; }',
    '.ui-eyebrow { font-size: 11px; text-transform: uppercase; letter-spacing: .06em; }',
    'body.wizard .e-head { text-transform: uppercase; letter-spacing: 1px; }',
  ].join('\n');
  assert.deepEqual([...handRolledEyebrows(css)], ['.a-head']);
});

test('no new hand-rolled eyebrows', () => {
  const now = {};
  for (const file of readdirSync(DASHBOARD).filter((f) => f.endsWith('.css') && f !== 'ui.css').sort()) {
    const found = [...handRolledEyebrows(readFileSync(new URL(file, DASHBOARD), 'utf8'))].sort();
    if (found.length) now[file] = found;
  }
  if (process.env.UPDATE_UI_EYEBROW_BASELINE) writeFileSync(BASELINE, `${JSON.stringify(now, null, 2)}\n`);
  const base = JSON.parse(readFileSync(BASELINE, 'utf8'));
  const added = Object.entries(now).flatMap(([f, sels]) => sels.filter((s) => !(base[f] || []).includes(s)).map((s) => `${f}: ${s}`));
  assert.deepEqual(added, [], 'section labels use .ui-eyebrow (ui.css), with .ui-eyebrow-section for section heads');
  const gone = Object.entries(base).flatMap(([f, sels]) => sels.filter((s) => !(now[f] || []).includes(s)).map((s) => `${f}: ${s}`));
  assert.deepEqual(gone, [], 'a migrated eyebrow must leave the baseline (UPDATE_UI_EYEBROW_BASELINE=1)');
});

test('ui.css defines the eyebrow, its section variant and their wizard paint', () => {
  const ui = readFileSync(new URL('ui.css', DASHBOARD), 'utf8');
  for (const sel of ['.ui-eyebrow {', '.ui-eyebrow-section {', 'body.wizard :where(.ui-eyebrow) {', 'body.wizard :where(.ui-eyebrow-section) {']) assert.ok(ui.includes(sel), sel);
});
