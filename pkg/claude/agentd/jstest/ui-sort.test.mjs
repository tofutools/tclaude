// Sortable column headers render through SortTh (js/ui-components.js), which
// gives every table the same look (sortable / sort-active / sort-arrow in
// dashboard.css), the same keyboard handling (Tab, Enter, Space) and
// aria-sort. Tables used to hand-roll the header, several times over, with
// their own classes and arrows; this keeps it to one.

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync } from 'node:fs';

const DASHBOARD = new URL('../dashboard/', import.meta.url);
const JS = new URL('js/', DASHBOARD);

// handRolledSort lists the markers of a sortable header built outside SortTh.
export function handRolledSort(source) {
  const found = [];
  for (const [pattern, what] of [
    [/aria-sort\s*=/, 'aria-sort'],
    [/class="sort-arrow"|['"`]sort-arrow['"`]/, 'sort-arrow'],
    [/['"`]sortable[\s'"`]|sortable\$\{|['"`]sort-active['"`]/, 'sortable class'],
  ]) if (pattern.test(source)) found.push(what);
  return found;
}

// sortClassRules lists th sort-state classes a stylesheet styles other than
// the house sortable / sort-active.
export function sortClassRules(css) {
  const source = css.replace(/\/\*[\s\S]*?\*\//g, '');
  const found = new Set();
  for (const [, cls] of source.matchAll(/\bth\.([\w-]*sort[\w-]*)/g)) {
    if (cls !== 'sortable' && cls !== 'sort-active') found.add(cls);
  }
  return found;
}

test('the scanners find hand-rolled sortable headers', () => {
  assert.deepEqual(handRolledSort('html`<th class=${on ? "sortable sort-active" : "sortable"} aria-sort=${x}>`'), ['aria-sort', 'sortable class']);
  assert.deepEqual(handRolledSort('<span class="sort-arrow">▲</span>'), ['sort-arrow']);
  assert.deepEqual(handRolledSort('html`<${SortTh} active=${on}>Name<//>`'), []);
  assert.deepEqual([...sortClassRules('.a th.audit-sort:hover { color: red } table th.sortable { } /* th.x-sort */')], ['audit-sort']);
});

test('sortable headers render only through SortTh', () => {
  const offenders = [];
  for (const file of readdirSync(JS).filter((f) => f.endsWith('.js') && f !== 'ui-components.js')) {
    const found = handRolledSort(readFileSync(new URL(file, JS), 'utf8'));
    if (found.length) offenders.push(`${file}: ${found.join(', ')}`);
  }
  assert.deepEqual(offenders, [], 'render sortable column headers with SortTh from ui-components.js');
});

test('stylesheets style only the house sort classes', () => {
  const offenders = [];
  for (const file of readdirSync(DASHBOARD).filter((f) => f.endsWith('.css'))) {
    for (const cls of sortClassRules(readFileSync(new URL(file, DASHBOARD), 'utf8'))) offenders.push(`${file}: th.${cls}`);
  }
  assert.deepEqual(offenders, [], 'sortable headers use th.sortable / th.sort-active');
});
