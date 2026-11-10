// Every scroller on the dashboard gets the one house scrollbar from ui.css: a
// thin bar with a quiet thumb that brightens on hover, coloured by the
// --ui-scroll-* tokens each skin sets on the root. Areas used to hand-copy
// their own @supports + ::-webkit-scrollbar recipe (and a wizard and slop copy
// of it), which drifted into several looks and left most scrollers on the
// browser's light default. This holds the line: no stylesheet or page module
// but ui.css styles a scrollbar. The one exception is hiding one outright, for
// a surface whose scrolling something else owns (an xterm viewport under tmux):
// `scrollbar-width: none` and `::-webkit-scrollbar { display: none; }`.

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync } from 'node:fs';

const DASHBOARD = new URL('../dashboard/', import.meta.url);
const read = (file) => readFileSync(new URL(file, DASHBOARD), 'utf8');
const stripComments = (css) => css.replace(/\/\*[\s\S]*?\*\//g, (c) => c.replace(/[^\n]/g, ' '));
const lineOf = (source, index) => source.slice(0, index).split('\n').length;

// scrollbarStyling lists the places in a stylesheet that style a scrollbar
// rather than hide it.
export function scrollbarStyling(source) {
  const css = stripComments(source);
  const found = [];
  for (const match of css.matchAll(/([^{};]*)\{([^{}]*)\}/g)) {
    const [, selector, body] = match;
    const at = match.index + selector.length;
    if (/::-webkit-scrollbar/.test(selector)) {
      if (!/^\s*display:\s*none;?\s*$/.test(body)) found.push(lineOf(css, at));
      continue;
    }
    for (const decl of body.matchAll(/(?:^|;)\s*(scrollbar-(?:width|color)|--[\w-]*scrollbar[\w-]*)\s*:\s*([^;]*)/g)) {
      if (decl[1] === 'scrollbar-width' && decl[2].trim() === 'none') continue;
      found.push(lineOf(css, at + 1 + decl.index));
    }
  }
  return [...new Set(found)].sort((a, b) => a - b);
}

test('the scanner tells styled scrollbars from hidden ones', () => {
  const source = [
    '.a { overflow: auto; scrollbar-width: none; }',
    '.a::-webkit-scrollbar { display: none; }',
    '/* .b { scrollbar-color: red blue; } */',
    '.c { scrollbar-width: thin; }',
    '.d { color: red; scrollbar-color: #111 #222; }',
    '.e::-webkit-scrollbar-thumb { background: #333; }',
    '.f { --pane-scrollbar-thumb: #444; }',
    '@supports not selector(::-webkit-scrollbar) {',
    '  .g { scrollbar-gutter: stable; }',
    '}',
  ].join('\n');
  assert.deepEqual(scrollbarStyling(source), [4, 5, 6, 7]);
});

test('only ui.css styles scrollbars', () => {
  const sheets = readdirSync(DASHBOARD).filter((name) => name.endsWith('.css') && name !== 'ui.css');
  assert.ok(sheets.includes('dashboard.css'), 'the scan found the dashboard stylesheets');
  const over = sheets.flatMap((file) => scrollbarStyling(read(file)).map((line) => `${file}:${line}`));
  assert.deepEqual(over, [], 'style scrollbars only through the --ui-scroll-* tokens in ui.css');

  const modules = ['dashboard.html', 'terminals.html', ...readdirSync(new URL('js/', DASHBOARD)).filter((name) => name.endsWith('.js')).map((name) => `js/${name}`)];
  const inline = modules.filter((file) => /scrollbar-color|scrollbarColor|scrollbarWidth|scrollbar-width:\s*(?!none)|::-webkit-scrollbar(?![\w-]*\s*\{\s*display:\s*none)/.test(read(file)));
  assert.deepEqual(inline, [], 'no page or module carries its own scrollbar styling');
});

test('ui.css defines the house scrollbar for every skin and both engines', () => {
  const css = read('ui.css');
  for (const want of [
    ':root {\n  --ui-scroll-thumb:',
    ':root:has(> body.wizard) {\n  --ui-scroll-thumb:',
    ':root:has(> body.slop) {\n  --ui-scroll-thumb:',
    // Firefox: the standardized properties, kept out of Chromium, where a
    // non-auto scrollbar-color would switch off the ::-webkit- hover state.
    '@supports not selector(::-webkit-scrollbar) {\n  * { scrollbar-width: thin; scrollbar-color: var(--ui-scroll-thumb) transparent; }',
    '::-webkit-scrollbar { width: 10px; height: 10px; }',
    '::-webkit-scrollbar-thumb:hover { background-color: var(--ui-scroll-thumb-hover); }',
    '@media (forced-colors: active) {',
  ]) {
    assert.ok(css.includes(want), `ui.css is missing ${JSON.stringify(want)}`);
  }
});
