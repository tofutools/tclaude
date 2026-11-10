// Count badges use the standard .ui-badge (dashboard/ui.css): one size and
// shape, with -accent / -danger / -warn tones and a -corner bubble. Areas used
// to hand-roll their own pill per count, each with its own padding, radius,
// size and grey. An area may keep a *-count / *-badge / *-unread class for
// placement, but a stylesheet rule whose subject is such a class may not draw
// a pill itself: border-radius plus background plus font-size in one rule.
//
// Existing ones are a ratchet: ui-badge-baseline.json lists the pills not yet
// migrated. Most are domain chips (CI, review, sandbox, owner), which are not
// counts and stay; a few sit in areas another migration owns. A new one
// fails, and a migrated one must leave the baseline. Regenerate with
//   UPDATE_UI_BADGE_BASELINE=1 node --test jstest/ui-badge.test.mjs

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync, writeFileSync } from 'node:fs';

const DASHBOARD = new URL('../dashboard/', import.meta.url);
const BASELINE = new URL('./ui-badge-baseline.json', import.meta.url);
const PILL = [/(?:^|[;\s])border-radius\s*:/, /(?:^|[;\s])background(?:-color)?\s*:/, /(?:^|[;\s])font-size\s*:/];

// handRolledPills lists the *-count / *-badge / *-unread classes a rule draws
// as a pill when they are the rule's subject.
export function handRolledPills(css) {
  const source = css.replace(/\/\*[\s\S]*?\*\//g, '');
  const found = new Set();
  for (const [, selector, body] of source.matchAll(/([^{};]*)\{([^{}]*)\}/g)) {
    if (!PILL.every((prop) => prop.test(body))) continue;
    for (const part of selector.split(/,(?![^()]*\))/)) {
      const subject = part.trim().split(/\s*[\s>+~]\s*/).pop().replace(/::?[\w-]+(\([^)]*\))?/g, '');
      for (const [, cls] of subject.matchAll(/\.([\w-]+-(?:count|badge|unread))(?![\w-])/g)) found.add(cls);
    }
  }
  return found;
}

test('the scanner finds hand-rolled count pills', () => {
  const css = [
    '.a-count { margin-left: 4px; }',
    '.b-count { padding: 0 5px; border-radius: 8px; background: #333; font-size: 10px; }',
    'body.wizard .c-badge { border-radius: 8px; background: gold; font-size: 9px; }',
    '.d-count.active { background: blue; }',
    '/* .e-count { border-radius: 8px; background: red; font-size: 9px; } */',
    '.f-badge span { border-radius: 8px; background: red; font-size: 9px; }',
    '.g-unread:hover { border-radius: 8px; background: red; font-size: 9px; }',
  ].join('\n');
  assert.deepEqual([...handRolledPills(css)].sort(), ['b-count', 'c-badge', 'g-unread']);
});

test('ui.css defines the standard count badge', () => {
  const css = readFileSync(new URL('ui.css', DASHBOARD), 'utf8');
  for (const cls of ['ui-badge', 'ui-badge-corner', 'ui-badge-accent', 'ui-badge-danger', 'ui-badge-warn']) {
    assert.match(css, new RegExp(`\\.${cls} \\{`), `ui.css defines .${cls}`);
  }
});

test('no stylesheet hand-rolls a new count pill', () => {
  const found = new Set();
  for (const file of readdirSync(DASHBOARD).filter((f) => f.endsWith('.css') && f !== 'ui.css')) {
    for (const cls of handRolledPills(readFileSync(new URL(file, DASHBOARD), 'utf8'))) found.add(cls);
  }
  if (process.env.UPDATE_UI_BADGE_BASELINE) writeFileSync(BASELINE, `${JSON.stringify([...found].sort(), null, 2)}\n`);
  const baseline = new Set(JSON.parse(readFileSync(BASELINE, 'utf8')));
  assert.deepEqual([...found].filter((c) => !baseline.has(c)).sort(), [],
    'draw counts with .ui-badge (and a tone or -corner); keep an area class for placement only');
  assert.deepEqual([...baseline].filter((c) => !found.has(c)).sort(), [],
    'these are migrated: remove them from jstest/ui-badge-baseline.json');
});
