// The phone layout must not change the desktop page. Desktop mode stays
// unchanged by construction: js/layout-boot.js adds the viewport meta, the
// data-layout mark and phone.css only in phone mode, and every phone rule is
// scoped under :root[data-layout="phone"]. This holds those pieces in place:
//   - every selector in phone.css starts with that scope, and it imports no
//     other sheet;
//   - no other stylesheet targets data-layout (phone rules live in phone.css);
//   - dashboard.html links neither a viewport meta nor phone.css statically;
//   - layout-boot.js loads as a plain blocking script in <head>, after the
//     last stylesheet, so it decides before the first paint and phone.css
//     lands after ui.css.

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync } from 'node:fs';

const DASHBOARD = new URL('../dashboard/', import.meta.url);
const read = (file) => readFileSync(new URL(file, DASHBOARD), 'utf8');
const SCOPE = ':root[data-layout="phone"]';

// cssSelectors returns every style-rule selector in a stylesheet, one entry
// per comma-separated selector, skipping at-rule preludes (@media …) and
// keyframe steps.
export function cssSelectors(css) {
  const source = css.replace(/\/\*[\s\S]*?\*\//g, ' ');
  const selectors = [];
  const stack = []; // per open block: 'keyframes' | 'at' | 'rule'
  let start = 0;
  for (let i = 0; i < source.length; i++) {
    const ch = source[i];
    if (ch === ';') { start = i + 1; continue; }
    if (ch === '}') { stack.pop(); start = i + 1; continue; }
    if (ch !== '{') continue;
    const prelude = source.slice(start, i).trim();
    start = i + 1;
    if (prelude.startsWith('@')) {
      stack.push(/^@(-\w+-)?keyframes\b/.test(prelude) ? 'keyframes' : 'at');
      continue;
    }
    const inKeyframes = stack.includes('keyframes');
    stack.push('rule');
    if (inKeyframes) continue;
    selectors.push(...splitSelectorList(prelude));
  }
  return selectors;
}

function splitSelectorList(list) {
  const parts = [];
  let depth = 0;
  let from = 0;
  for (let i = 0; i < list.length; i++) {
    const ch = list[i];
    if (ch === '(' || ch === '[') depth++;
    else if (ch === ')' || ch === ']') depth--;
    else if (ch === ',' && depth === 0) { parts.push(list.slice(from, i).trim()); from = i + 1; }
  }
  parts.push(list.slice(from).trim());
  return parts.filter(Boolean).map((part) => part.replace(/\s+/g, ' '));
}

test('cssSelectors sees nested and listed selectors, not at-rules or keyframe steps', () => {
  const css = `/* a { } */ :root[data-layout="phone"] .a, .b:is(.c, .d) { x: 1; }
    @media (min-width: 560px) { .e > .f { y: 2; } }
    @keyframes spin { from { a: 1 } 50% { a: 2 } }
    @supports (display: grid) { :root .g { z: 3; } }`;
  assert.deepEqual(cssSelectors(css), [':root[data-layout="phone"] .a', '.b:is(.c, .d)', '.e > .f', ':root .g']);
});

test('every phone.css rule is scoped under :root[data-layout="phone"]', () => {
  const selectors = cssSelectors(read('phone.css'));
  assert.ok(selectors.length > 0, 'phone.css has rules');
  const unscoped = selectors.filter((selector) => !(selector === SCOPE || selector.startsWith(`${SCOPE} `)
    || selector.startsWith(`${SCOPE}:`) || selector.startsWith(`${SCOPE}.`) || selector.startsWith(`${SCOPE}[`)));
  assert.deepEqual(unscoped, [], `phone.css selectors must start with ${SCOPE}`);
  const source = read('phone.css').replace(/\/\*[\s\S]*?\*\//g, ' ');
  assert.doesNotMatch(source, /@import\b/, 'an imported sheet would bring unscoped rules past this check');
});

test('phone rules live only in phone.css', () => {
  const sheets = readdirSync(DASHBOARD).filter((name) => name.endsWith('.css') && name !== 'phone.css');
  const offenders = sheets.filter((name) => /data-layout/.test(read(name)));
  assert.deepEqual(offenders, [], 'style the phone layout in phone.css, not in the desktop sheets');
});

test('the desktop page carries no viewport meta and no phone sheet', () => {
  for (const page of ['dashboard.html']) {
    const html = read(page).replace(/<!--[\s\S]*?-->/g, '');
    assert.doesNotMatch(html, /<meta[^>]+name=["']?viewport/i, `${page}: the viewport meta is phone-only (layout-boot.js adds it)`);
    assert.doesNotMatch(html, /phone\.css/, `${page}: phone.css is linked by layout-boot.js in phone mode only`);
    assert.doesNotMatch(html, /data-layout/, `${page}: data-layout is set by layout-boot.js in phone mode only`);
  }
});

test('layout-boot.js is a blocking head script after the last stylesheet', () => {
  const html = read('dashboard.html').replace(/<!--[\s\S]*?-->/g, '');
  const tag = html.match(/<script\b[^>]*src="\/static\/js\/layout-boot\.js"[^>]*>/);
  assert.ok(tag, 'dashboard.html loads layout-boot.js');
  assert.doesNotMatch(tag[0], /\b(type=|defer|async)/, 'a classic, blocking script');
  const at = tag.index;
  const lastSheet = [...html.matchAll(/<link\b[^>]*rel="stylesheet"/g)].at(-1).index;
  assert.ok(at > lastSheet, 'after every stylesheet link, so phone.css follows them in the cascade');
  assert.ok(at < html.indexOf('</head>'), 'inside <head>, before the first paint');
});
