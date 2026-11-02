// New buttons and selects use the standard ui-* controls (dashboard/ui.css,
// tcl-32qwip), so they are styled by default instead of by whichever container
// they land in. A source scan of the page shells and every dashboard module:
// a <button> or <select> tag counts as standard when it carries a ui-* class
// (static or inside a ${…} expression) or is marked data-ui-exempt, the opt-out
// for deliberate one-offs such as window controls.
//
// Existing markup is a ratchet: ui-controls-baseline.json records each file's
// count of non-standard tags. A file may not exceed it, and once an area is
// migrated the baseline must come down with it so the count cannot creep back.
// Regenerate after a migration with
//   UPDATE_UI_CONTROLS_BASELINE=1 node --test jstest/ui-controls.test.mjs

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync, writeFileSync } from 'node:fs';

const DASHBOARD = new URL('../dashboard/', import.meta.url);
const BASELINE = new URL('./ui-controls-baseline.json', import.meta.url);

// tagEnd returns the index of the `>` closing the tag that opens at `start`,
// skipping `>` inside quoted attribute values and htm ${…} expressions.
function tagEnd(source, start) {
  let depth = 0;
  let quote = '';
  for (let i = start; i < source.length; i++) {
    const c = source[i];
    if (depth === 0 && quote) {
      if (c === quote) quote = '';
    } else if (c === '$' && source[i + 1] === '{') {
      depth++;
      i++;
    } else if (depth > 0) {
      if (c === '{') depth++;
      else if (c === '}') depth--;
    } else if (c === '"' || c === "'") {
      quote = c;
    } else if (c === '>') {
      return i;
    }
  }
  return source.length;
}

// nonStandardTags lists the line numbers of <button>/<select> tags in source
// that carry neither a ui-* class nor data-ui-exempt.
export function nonStandardTags(source) {
  const lines = [];
  for (const match of source.matchAll(/<(button|select)(?=[\s>/])/g)) {
    const lineStart = source.lastIndexOf('\n', match.index) + 1;
    if (/^\s*(\/\/|\/\*|\*)/.test(source.slice(lineStart, match.index))) continue; // a comment naming the tag
    const tag = source.slice(match.index, tagEnd(source, match.index) + 1);
    if (/(^|[\s"'`])ui-[a-z]/.test(tag) || /\bdata-ui-exempt\b/.test(tag)) continue;
    lines.push(source.slice(0, match.index).split('\n').length);
  }
  return lines;
}

function scan() {
  const files = ['dashboard.html', 'terminals.html', ...readdirSync(new URL('js/', DASHBOARD)).filter((name) => name.endsWith('.js')).sort().map((name) => `js/${name}`)];
  const found = {};
  for (const file of files) {
    const lines = nonStandardTags(readFileSync(new URL(file, DASHBOARD), 'utf8'));
    if (lines.length) found[file] = lines;
  }
  return found;
}

test('the scanner tells standard, exempt and bare controls apart', () => {
  const source = [
    '<button class="ui-btn ui-btn-primary">Save</button>',
    '<button class=${`ui-btn ${busy ? "is-busy" : ""}`} onClick=${() => a > b && go()}>Go</button>',
    '<select class="ui-select ui-sm"></select>',
    '<button data-ui-exempt class="overlay-close">×</button>',
    '<button class="tool">Old</button>',
    '<button onClick=${() => x > 1}>Bare</button>',
    '<select id="pick"></select>',
    '<buttonish>not a tag</buttonish>',
    '// a click does not focus a <button> on every platform',
    ' * repopulates a descendant <select>, whereas',
  ].join('\n');
  assert.deepEqual(nonStandardTags(source), [5, 6, 7]);
});

test('no file gains a button or select without a standard ui-* class', () => {
  const found = scan();
  if (process.env.UPDATE_UI_CONTROLS_BASELINE) {
    const counts = Object.fromEntries(Object.entries(found).map(([file, lines]) => [file, lines.length]));
    writeFileSync(BASELINE, `${JSON.stringify(counts, null, 2)}\n`);
  }
  const baseline = JSON.parse(readFileSync(BASELINE, 'utf8'));
  const over = [];
  const stale = [];
  for (const [file, lines] of Object.entries(found)) {
    const allowed = baseline[file] ?? 0;
    if (lines.length > allowed) {
      over.push(`${file}: ${lines.length} non-standard (baseline ${allowed}), lines ${lines.join(', ')}`);
    }
  }
  for (const [file, allowed] of Object.entries(baseline)) {
    const count = found[file]?.length ?? 0;
    if (count < allowed) stale.push(`${file}: ${count} (baseline ${allowed})`);
  }
  assert.deepEqual(over, [],
    'use the ui-* classes from dashboard/ui.css (ui-btn, ui-select, ui-menu-item, …) or mark a deliberate one-off data-ui-exempt');
  assert.deepEqual(stale, [],
    'fewer non-standard controls than the baseline: lower it with UPDATE_UI_CONTROLS_BASELINE=1 so the count cannot creep back');
});
