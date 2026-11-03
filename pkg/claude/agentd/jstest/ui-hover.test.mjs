// One hover style for every button (operator rule): hovering brightens the
// background a step and never brightens the border. Danger and warn brighten
// within their own colour; keyboard focus keeps its outline ring, which is not
// a hover look. A stylesheet scan: it holds the standard button's hover rules
// and keeps any sheet from giving a button a bright border on hover again.

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync } from 'node:fs';

const DASHBOARD = new URL('../dashboard/', import.meta.url);
const read = (file) => readFileSync(new URL(file, DASHBOARD), 'utf8');
const SHEETS = readdirSync(DASHBOARD).filter((name) => name.endsWith('.css')).sort();
const uncommented = (css) => css.replace(/\/\*[\s\S]*?\*\//g, '');
// Classes the shared Button renders with (class="…" on <${Button}>): area
// skins name buttons by these too, not only by button/btn.
const BUTTON_CLASSES = new Set();
for (const name of readdirSync(new URL('js/', DASHBOARD)).filter((file) => file.endsWith('.js'))) {
  for (const [, tag] of read(`js/${name}`).matchAll(/<\$\{Button\}([^>]*)>/g)) {
    for (const [, list] of tag.matchAll(/\bclass="([^"$]+)"/g)) list.split(/\s+/).forEach((c) => c && BUTTON_CLASSES.add(c));
  }
}
const namesButton = (selector) => /button|btn|\.tool\b/.test(selector)
  || [...selector.matchAll(/\.([\w-]+)/g)].some(([, c]) => BUTTON_CLASSES.has(c));
// A border declaration that paints a colour; taking the border away on hover
// (0, none, transparent) is not a highlight.
const BORDER = /(?:^|[;\s])border(?:-color)?\s*:(?!\s*(?:0|none|transparent)\s*(?:!important\s*)?(?:;|$))/;

function rules() {
  const out = [];
  for (const sheet of SHEETS) {
    for (const [, selector, body] of uncommented(read(sheet)).matchAll(/([^{}]+)\{([^{}]*)\}/g)) {
      out.push({ sheet, selector: selector.trim().replace(/\s+/g, ' '), body });
    }
  }
  return out;
}

test('the standard button hovers by background, never by border', () => {
  const hovers = rules().filter((r) => r.sheet === 'ui.css' && /\.ui-btn[\w-]*(?:\.[\w-]+)*:hover/.test(r.selector));
  assert.ok(hovers.length >= 6, 'ui.css styles the button variants on hover');
  for (const rule of hovers) {
    assert.doesNotMatch(rule.body, BORDER, `${rule.selector} keeps its border on hover`);
  }
  const base = hovers.find((r) => r.selector === '.ui-btn:hover:not(:disabled)');
  assert.match(base.body, /background: var\(--ui-bg-hover\)/, 'the default button brightens its background');
});

test('no sheet brightens a button border on hover', () => {
  const bad = rules().filter((r) => namesButton(r.selector)
    && r.selector.split(/,(?![^()]*\))/).some((part) => part.includes(':hover'))
    && BORDER.test(r.body));
  assert.deepEqual(bad.map((r) => `${r.sheet}: ${r.selector}`), [], [
    'A hovered button brightens its background, not its border:',
    '  - drop the border / border-color from the :hover rule,',
    '  - give it a background a step brighter instead (var(--ui-bg-hover), or a tint of its danger/warn colour).',
    'Keyboard focus is the outline ring; put a :focus-visible border in its own rule.',
  ].join('\n'));
});

test('every selector closes its parentheses and nothing is glued to one', () => {
  // A mechanical selector edit that drops a ")" makes the browser discard the
  // rule (and can swallow the next one), which a text scan would miss.
  const balanced = (selector) => {
    let depth = 0;
    for (const ch of selector) {
      if (ch === '(') depth++;
      else if (ch === ')' && --depth < 0) return false;
    }
    return depth === 0;
  };
  // A fence pasted inside a class name (`.foo-button:where(…)s`) leaves a
  // word glued to the closing paren and the selector silently stops matching.
  const bad = rules().filter((r) => !balanced(r.selector) || /\)[\w-]/.test(r.selector));
  assert.deepEqual(bad.map((r) => `${r.sheet}: ${r.selector.slice(0, 120)}`), []);
});
