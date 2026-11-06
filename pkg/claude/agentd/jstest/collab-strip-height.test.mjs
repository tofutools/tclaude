import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';

// tcl-w313wy: the Collab tab strip is the drop target for an overlay window's
// tab, so with no tab open it keeps one tab row's height instead of collapsing
// to its padding. LinkeDOM lays nothing out, so this pins the rule's
// arithmetic against the tab's own box: the strip's min-height is its top
// padding and bottom border plus a tab's top border, vertical padding and one
// line (real Chrome: 36px empty and with one tab).

const css = await readFile(new URL('../dashboard/mux.css', import.meta.url), 'utf8');

function rule(selector) {
  const at = css.search(new RegExp(`(^|\\n)${selector.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}\\s*\\{`));
  assert.ok(at >= 0, `mux.css has ${selector}`);
  return css.slice(css.indexOf('{', at) + 1, css.indexOf('}', at));
}

const px = (value) => Number(/^(-?\d+(?:\.\d+)?)px$/.exec(value.trim())?.[1] ?? NaN);
const prop = (body, name) => new RegExp(`(?:^|[;\\s])${name}\\s*:\\s*([^;]+);`).exec(body.replace(/\/\*[\s\S]*?\*\//g, ''))?.[1].trim();

test('the empty Collab strip keeps one tab row of height', () => {
  const strip = rule('.mux-tabs');
  const tab = rule('.mux-tab');
  const [stripTop] = prop(strip, 'padding').split(/\s+/).map(px);
  const stripBorder = px(prop(strip, 'border-bottom').split(/\s+/)[0]);
  const [tabV] = prop(tab, 'padding').split(/\s+/).map(px);
  const tabBorder = px(prop(tab, 'border').split(/\s+/)[0]);
  assert.equal(prop(tab, 'border-bottom'), 'none', 'a tab has no bottom border: the strip\'s is the row\'s');
  const fixed = stripTop + stripBorder + tabBorder + 2 * tabV;
  assert.ok(Number.isFinite(fixed), 'fixture: the boxes parse');
  assert.equal(prop(strip, 'min-height'), `calc(${fixed}px + 1lh)`, 'min-height is a tab row: its boxes plus one line');
});
