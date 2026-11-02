// Dead CSS: a class selector in a dashboard stylesheet that nothing renders.
// Markup moves to Preact and areas get rewritten, and their old selectors stay
// behind, so this holds the stylesheets to the markup: every class a sheet
// styles must appear in the page shells or a dashboard module.
//
// A static scan cannot see every class. One built at runtime from a literal
// prefix (`state-${s}`, 'chip-' + kind) is recognised by that prefix. One
// built any other way (`${prefix}-pos` in the pager, `c${n}` colours) cannot
// be, and goes in css-dead-classes-allowlist.json with the reason it is live,
// after a review. The allowlist is a ratchet: an entry that the scan no
// longer flags must be removed, so it only shrinks. A newly dead class fails:
// delete its rules, or, when it is built at runtime, allowlist it with why.

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync } from 'node:fs';

const DASHBOARD = new URL('../dashboard/', import.meta.url);
const ALLOWLIST = new URL('./css-dead-classes-allowlist.json', import.meta.url);
const read = (file) => readFileSync(new URL(file, DASHBOARD), 'utf8');

// cssClasses maps each class selector in a stylesheet to its first line.
export function cssClasses(css) {
  const source = css.replace(/\/\*[\s\S]*?\*\//g, (c) => c.replace(/[^\n]/g, ' '));
  const found = new Map();
  // Selectors only: the text before each `{`, after the previous `{`, `}` or `;`.
  for (const match of source.matchAll(/([^{};]*)\{/g)) {
    const selector = match[1];
    if (/^\s*@/.test(selector)) continue;
    for (const cls of selector.matchAll(/\.(-?[A-Za-z_][\w-]*)/g)) {
      // A number such as `.5s` in a leftover value is not a class.
      if (!found.has(cls[1])) found.set(cls[1], source.slice(0, match.index + cls.index).split('\n').length);
    }
  }
  return found;
}

// runtimePrefixes lists the literal prefixes the sources build class names
// from: `name-${…}` in a template and 'name-' + … in a concatenation.
export function runtimePrefixes(sources) {
  const prefixes = new Set();
  for (const m of sources.matchAll(/([A-Za-z][\w-]*-)(?:\$\{|['"`]\s*\+)/g)) prefixes.add(m[1]);
  return prefixes;
}

// deadClasses lists the classes in sheets (file → css) that the sources never
// name, as `class (file:line)`, sorted by class.
export function deadClasses(sheets, sources) {
  const words = new Set(sources.match(/[\w-]+/g));
  const prefixes = [...runtimePrefixes(sources)];
  const dead = new Map();
  for (const [file, css] of Object.entries(sheets)) {
    for (const [cls, line] of cssClasses(css)) {
      if (words.has(cls) || dead.has(cls) || prefixes.some((p) => cls.startsWith(p))) continue;
      dead.set(cls, `${file}:${line}`);
    }
  }
  return new Map([...dead].sort(([a], [b]) => a.localeCompare(b)));
}

test('the scan tells live, runtime-built and dead classes apart', () => {
  const sheets = {
    'a.css': [
      '.live, .gone { color: red; }',
      '/* .commented { } */',
      '.state-idle:hover, .chip-x > .dead-child { transition: all .5s; }',
      '@media (max-width: 640px) { .narrow { display: none; } }',
    ].join('\n'),
  };
  const sources = "html`<div class=\"live narrow\"></div><i class=${`state-${s}`}></i>`; el.className = 'chip-' + kind;";
  assert.deepEqual([...deadClasses(sheets, sources)], [['dead-child', 'a.css:3'], ['gone', 'a.css:1']]);
});

test('every class a dashboard stylesheet styles is rendered somewhere', () => {
  const sheets = Object.fromEntries(readdirSync(DASHBOARD).filter((f) => f.endsWith('.css')).sort().map((f) => [f, read(f)]));
  assert.ok(sheets['dashboard.css'], 'the scan found the dashboard stylesheets');
  const sources = ['dashboard.html', 'terminals.html', ...readdirSync(new URL('js/', DASHBOARD)).filter((f) => f.endsWith('.js')).map((f) => `js/${f}`)]
    .map(read).join('\n');
  const dead = deadClasses(sheets, sources);
  const allow = JSON.parse(readFileSync(ALLOWLIST, 'utf8'));
  const unexpected = [...dead].filter(([cls]) => !(cls in allow)).map(([cls, at]) => `.${cls} (${at})`);
  assert.deepEqual(unexpected, [],
    'these classes are styled but never rendered: delete their rules, or allowlist one that is built at runtime, with the reason');
  const stale = Object.keys(allow).filter((cls) => !dead.has(cls));
  assert.deepEqual(stale, [], 'these allowlist entries are no longer flagged: remove them from css-dead-classes-allowlist.json');
});
