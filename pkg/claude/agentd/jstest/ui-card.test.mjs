// Cards (bordered surfaces inside a page or dialog: list entries, choice
// tiles, framed notes) use the standard .ui-card, or .ui-card.ui-inset for the
// darker recessed surface (ui.css), painted from the --ui-card-* tokens. They
// used to paint their own surface, each with a slightly different edge and
// corner, and the wizard skin repeated its violet surface once per dialog id.
// An area class keeps size, padding, layout and state edges; a stylesheet rule
// whose subject is a *-card / *-tile / *-panel class, or one of the classes
// already moved onto .ui-card, may not paint a surface itself, meaning a
// background and a border in one rule.
//
// Existing ones are a ratchet: ui-card-baseline.json lists, by selector, the
// surfaces not yet migrated. A new one fails, and a migrated one must leave
// the baseline. Regenerate with
//   UPDATE_UI_CARD_BASELINE=1 node --test jstest/ui-card.test.mjs

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync, writeFileSync } from 'node:fs';

const DASHBOARD = new URL('../dashboard/', import.meta.url);
const BASELINE = new URL('./ui-card-baseline.json', import.meta.url);
const SURFACE = [/(?:^|[;\s])background(?:-color)?\s*:/, /(?:^|[;\s])border(?:-color)?\s*:/];
// Classes whose markup carries .ui-card; their names do not say "card".
const MIGRATED = new Set([
  'starter-row', 'cleanup-tier', 'window-direction', 'reincarnate-mode', 'deploy-mode',
  'group-force-block', 'sandbox-import-profile', 'profile-transfer-row', 'paf-redact',
  'paf-box', 'snap-panel',
]);

// handRolledCards lists the selectors (whitespace-normalised) whose subject is
// a single card-like class or id that the rule paints as a surface. A state
// (`:hover`, `.selected`) is not a surface.
export function handRolledCards(css) {
  const source = css.replace(/\/\*[\s\S]*?\*\//g, '');
  const found = new Set();
  for (const [, selector, body] of source.matchAll(/([^{};]*)\{([^{}]*)\}/g)) {
    if (!SURFACE.every((prop) => prop.test(body))) continue;
    for (const part of selector.split(/,(?![^()]*\))/)) {
      const subject = part.trim().split(/\s*[\s>+~]\s*/).pop();
      if (/[:[]/.test(subject)) continue;
      const names = [...subject.matchAll(/[.#]([\w-]+)/g)].map((m) => m[1]);
      if (names.length === 1 && (/-(?:card|tile|panel)$/.test(names[0]) || MIGRATED.has(names[0]))) {
        found.add(part.trim().replace(/\s+/g, ' '));
      }
    }
  }
  return found;
}

test('the scanner finds hand-painted cards', () => {
  const css = [
    '.a-card { padding: 10px; }',
    '.b-card { background: #161b22; border: 1px solid #2a3340; }',
    'body.wizard #x .cleanup-tier, .other { background: #140f28; border-color: violet; }',
    '.c-card.selected { background: red; border-color: red; }',
    '#d-panel { background: #0d1117; border: 1px solid #2a3340; }',
    '/* .e-tile { background: red; border: 0; } */',
  ].join('\n');
  assert.deepEqual([...handRolledCards(css)].sort(), ['#d-panel', '.b-card', 'body.wizard #x .cleanup-tier']);
});

test('ui.css defines the standard card on the card tokens', () => {
  const css = readFileSync(new URL('ui.css', DASHBOARD), 'utf8');
  assert.match(css, /\.ui-card \{[^}]*background: var\(--ui-card-bg\)/);
  assert.match(css, /\.ui-card\.ui-inset \{[^}]*background: var\(--ui-card-inset-bg\)/);
});

test('no stylesheet hand-paints a new card', () => {
  const found = new Set();
  for (const file of readdirSync(DASHBOARD).filter((f) => f.endsWith('.css') && f !== 'ui.css')) {
    for (const sel of handRolledCards(readFileSync(new URL(file, DASHBOARD), 'utf8'))) found.add(sel);
  }
  if (process.env.UPDATE_UI_CARD_BASELINE) writeFileSync(BASELINE, `${JSON.stringify([...found].sort(), null, 2)}\n`);
  const baseline = new Set(JSON.parse(readFileSync(BASELINE, 'utf8')));
  assert.deepEqual([...found].filter((c) => !baseline.has(c)).sort(), [],
    'paint cards with .ui-card (+ .ui-inset), skinned through the --ui-card-* tokens; keep an area class for size, padding and layout');
  assert.deepEqual([...baseline].filter((c) => !found.has(c)).sort(), [],
    'these are migrated: remove them from jstest/ui-card-baseline.json');
});

test('migrated card classes keep .ui-card in their markup', () => {
  const classes = [...MIGRATED, 'template-card', 'plugin-card', 'usage-rule-card'];
  const JS = new URL('js/', DASHBOARD);
  const bare = [];
  for (const file of readdirSync(JS).filter((f) => f.endsWith('.js'))) {
    const source = readFileSync(new URL(file, JS), 'utf8');
    for (const m of source.matchAll(/class=(?:"([^"]*)"|\$\{`([^`]*)`\})/g)) {
      const value = m[1] ?? m[2];
      const tokens = value.split(/[\s${}'?:]+/);
      for (const cls of classes) if (tokens.includes(cls) && !tokens.includes('ui-card')) bare.push(`${file}: ${value}`);
    }
  }
  assert.deepEqual(bare, [], 'card markup carries ui-card so every skin paints it');
});
