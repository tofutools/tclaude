// Label chips use the standard .ui-tag (dashboard/ui.css; operator pick B):
// one round outlined pill, 11px, coloured by -accent / -warn / -own. Areas
// used to draw their own (about forty rules mixing radius 8/9/10/12/999px,
// 0-2px x 5-9px padding and 10-11.5px text). An area class keeps placement,
// and a state modifier on it may still set color / border-color (two
// classes out-rank .ui-tag; the wizard paint sits in :where() so it does too).
//
// A rule that draws a pill itself (a 1px border, a radius of 8px or more,
// text of 11.5px or less and 0-2px vertical padding) is a hand-rolled chip,
// counted against ui-tag-baseline.json by selector. A new one fails, and a
// migrated one must leave the baseline. Regenerate with
//   UPDATE_UI_TAG_BASELINE=1 node --test jstest/ui-tag.test.mjs

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync, writeFileSync } from 'node:fs';

const DASHBOARD = new URL('../dashboard/', import.meta.url);
const BASELINE = new URL('./ui-tag-baseline.json', import.meta.url);

// handRolledChips lists selectors (whitespace-normalised, skins excluded)
// whose rule draws a small outlined pill.
export function handRolledChips(css) {
  const found = new Set();
  const source = css.replace(/\/\*[\s\S]*?\*\//g, '');
  for (const [, selector, body] of source.matchAll(/([^{};]*)\{([^{}]*)\}/g)) {
    const radius = body.match(/(?:^|[;\s])border-radius\s*:\s*(\d+)px/);
    const size = body.match(/(?:^|[;\s])font-size\s*:\s*([\d.]+)px/);
    const pad = body.match(/(?:^|[;\s])padding\s*:\s*(\d+)(?:px)?\s+\d+px/);
    if (!radius || !size || !pad || !/(?:^|[;\s])border\s*:\s*1px solid/.test(body)) continue;
    if (Number(radius[1]) < 8 || Number(size[1]) > 11.5 || Number(pad[1]) > 2) continue;
    for (const raw of selector.split(/,(?![^()]*\))/)) {
      const part = raw.trim().replace(/\s+/g, ' ');
      if (part && !/^body\.(?:wizard|slop)\b/.test(part) && !/^\.ui-(?:tag|badge)\b/.test(part)) found.add(part);
    }
  }
  return found;
}

test('the scanner finds hand-rolled chips', () => {
  const css = [
    '.a-chip { font-size: 11px; padding: 0 7px; border-radius: 10px; border: 1px solid #30363d; }',
    '.b-chip.on { color: red; border-color: red; }',
    '.c-box { font-size: 12px; padding: 6px 10px; border-radius: 8px; border: 1px solid #333; }',
    '.ui-tag { font-size: 11px; padding: 0 7px; border-radius: 999px; border: 1px solid #30363d; }',
    'body.wizard .d-chip { font-size: 11px; padding: 0 7px; border-radius: 10px; border: 1px solid violet; }',
  ].join('\n');
  assert.deepEqual([...handRolledChips(css)], ['.a-chip']);
});

test('no new hand-rolled chips', () => {
  const now = {};
  for (const file of readdirSync(DASHBOARD).filter((f) => f.endsWith('.css') && f !== 'ui.css').sort()) {
    const found = [...handRolledChips(readFileSync(new URL(file, DASHBOARD), 'utf8'))].sort();
    if (found.length) now[file] = found;
  }
  if (process.env.UPDATE_UI_TAG_BASELINE) writeFileSync(BASELINE, `${JSON.stringify(now, null, 2)}\n`);
  const base = JSON.parse(readFileSync(BASELINE, 'utf8'));
  const added = Object.entries(now).flatMap(([f, sels]) => sels.filter((s) => !(base[f] || []).includes(s)).map((s) => `${f}: ${s}`));
  assert.deepEqual(added, [], 'label chips use .ui-tag (ui.css) with a variant, not their own pill');
  const gone = Object.entries(base).flatMap(([f, sels]) => sels.filter((s) => !(now[f] || []).includes(s)).map((s) => `${f}: ${s}`));
  assert.deepEqual(gone, [], 'a migrated chip must leave the baseline (UPDATE_UI_TAG_BASELINE=1)');
});

test('ui.css defines the tag and its variants', () => {
  const ui = readFileSync(new URL('ui.css', DASHBOARD), 'utf8');
  for (const sel of ['.ui-tag {', '.ui-tag-accent {', '.ui-tag-warn {', '.ui-tag-own {', 'body.wizard :where(.ui-tag) {']) assert.ok(ui.includes(sel), sel);
});
