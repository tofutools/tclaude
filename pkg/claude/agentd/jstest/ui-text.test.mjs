// Text in page and dialog content takes its colour from the content text
// tokens (ui.css), one value per skin:
//   --ui-muted-text   grey #7d8590      wizard lavender #b39ddb
//   --ui-body-text    #c9d1d9           wizard #e7d9f5
//   --ui-strong-text  #e6edf3           wizard gilt #f3e6c0
// Content rules that used to hard-code the default value read the token, and
// the wizard skin used to repeat its value on each of those classes, once per
// dialog id. A wizard rule that only sets that value on an element a base
// rule already colours with the token is a copy and fails here.

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync } from 'node:fs';

const DASHBOARD = new URL('../dashboard/', import.meta.url);
const parts = (selector) => selector.split(/,(?![^()]*\))/).map((p) => p.trim().replace(/\s+/g, ' ')).filter(Boolean);
const subject = (part) => part.split(/\s*[\s>+~]\s*/).pop();
const classy = (s) => s.replace(/:[\w-]+\([^)]*\)/g, '').includes('.');
const classesOf = (s) => new Set(s.replace(/:[\w-]+\([^)]*\)/g, '').match(/\.[\w-]+/g) || []);
const tagOf = (s) => s.replace(/[.:#[].*$/, '');
const colourOf = (body) => body.match(/(?:^|[;\s])color:\s*([^;]+)/)?.[1].trim().toLowerCase();

// copiedTextColour lists wizard selectors that only set the wizard value on
// an element a base rule already colours with the token. "Already" means: the
// subject carries a class, every base rule colouring that subject reads the
// token, and some token-reading base selector's compounds are an ordered
// subsequence of the wizard selector's (descendant combinators only), so it
// reaches every element the wizard rule does.
export function copiedTextColour(css, wizardValue = '#b39ddb', token = '--ui-muted-text') {
  const source = css.replace(/\/\*[\s\S]*?\*\//g, '');
  const rules = [...source.matchAll(/([^{};]*)\{([^{}]*)\}/g)].map(([, sel, body]) => [sel, body]);
  const baseColours = new Map();
  const tokenParts = [];
  for (const [sel, body] of rules) {
    if (/body\.(?:wizard|slop)/.test(sel)) continue;
    const colour = colourOf(body);
    if (!colour) continue;
    for (const p of parts(sel)) {
      const s = subject(p);
      if (!baseColours.has(s)) baseColours.set(s, new Set());
      baseColours.get(s).add(colour);
      if (colour === `var(${token})` && !/[>+~]/.test(p)) tokenParts.push(p.split(' '));
    }
  }
  const covered = (part) => {
    if (/[>+~]/.test(part)) return false;
    const w = part.split(' ').slice(1);
    const s = w.at(-1);
    if (!s || !classy(s) || !baseColours.has(s)) return false;
    // A base rule on the same classes plus a modifier (.badge.online) that the
    // wizard rule used to out-rank counts too.
    const own = classesOf(s);
    const colours = new Set(baseColours.get(s));
    for (const [t, c] of baseColours) {
      const tc = classesOf(t);
      if (t !== s && tc.size > own.size && [...own].every((x) => tc.has(x)) && (tagOf(t) === '' || tagOf(t) === tagOf(s))) for (const x of c) colours.add(x);
    }
    if (colours.size !== 1 || !colours.has(`var(${token})`)) return false;
    return tokenParts.some((b) => {
      if (b.at(-1) !== s) return false;
      let i = 0;
      for (const t of b.slice(0, -1)) { while (i < w.length - 1 && w[i] !== t) i++; if (i >= w.length - 1) return false; i++; }
      return true;
    });
  };
  const found = new Set();
  for (const [sel, body] of rules) {
    if (!sel.includes('body.wizard')) continue;
    const decls = body.split(';').map((d) => d.trim().replace(/\s+/g, ' ')).filter(Boolean);
    if (decls.length !== 1 || decls[0].toLowerCase() !== `color: ${wizardValue}`) continue;
    for (const p of parts(sel)) if (p.startsWith('body.wizard ') && covered(p)) found.add(p);
  }
  return found;
}

test('the scanner finds wizard copies of a content text colour', () => {
  const css = [
    '.a-hint { color: var(--ui-muted-text); font-size: 11px; }',
    '.row .c-note { color: var(--ui-muted-text); }',
    '.d-tip { color: #91a0b3; }',
    '.e small { color: var(--ui-muted-text); }',
    'body.wizard #x-modal .a-hint, body.wizard .b-note { color: #b39ddb; }',
    'body.wizard #y-modal .a-hint { color: #b39ddb; font-style: italic; }',
    'body.wizard #z-modal .row .c-note, body.wizard #z-modal .c-note { color: #b39ddb; }',
    'body.wizard .d-tip, body.wizard #w-modal small { color: #b39ddb; }',
    '.f-badge { color: var(--ui-muted-text); }',
    '.f-badge.online { color: #58a6ff; }',
    'body.wizard #v-modal .f-badge { color: #b39ddb; }',
  ].join('\n');
  // Flagged: a copy the base rule reaches. Kept: a wider wizard scope than the
  // base reaches (.c-note outside .row), a base hard-coding another colour,
  // a bare element subject, and a class whose modifier (.f-badge.online) a
  // base rule colours differently.
  assert.deepEqual([...copiedTextColour(css)].sort(), ['body.wizard #x-modal .a-hint', 'body.wizard #z-modal .row .c-note']);
});

const TOKENS = [['--ui-muted-text', '#7d8590', '#b39ddb'], ['--ui-body-text', '#c9d1d9', '#e7d9f5'], ['--ui-strong-text', '#e6edf3', '#f3e6c0']];

test('ui.css defines the content text tokens per skin', () => {
  const ui = readFileSync(new URL('ui.css', DASHBOARD), 'utf8');
  for (const [token, plain, wizard] of TOKENS) {
    assert.match(ui, new RegExp(`:root \\{[^}]*${token}: ${plain}`));
    assert.match(ui, new RegExp(`body\\.wizard \\{[^}]*${token}: ${wizard}`));
  }
});

test('wizard skin does not repeat a content text colour per dialog', () => {
  const found = [];
  for (const file of readdirSync(DASHBOARD).filter((f) => f.endsWith('.css') && f !== 'ui.css')) {
    const css = readFileSync(new URL(file, DASHBOARD), 'utf8');
    for (const [token, , wizard] of TOKENS) for (const sel of copiedTextColour(css, wizard, token)) found.push(`${file}: ${sel} (${token})`);
  }
  assert.deepEqual(found, [], 'content text reads the --ui-*-text tokens; the wizard sets each token once');
});
