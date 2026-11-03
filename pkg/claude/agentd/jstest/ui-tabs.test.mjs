// Tab rows use Tabs (js/ui-tabs.js), or the .ui-tabs / .ui-tab classes in
// static markup. A section's sub-navigation used to be hand-written per area
// (access, usage, automations, diagnostics, groups, fleet admin), each with
// its own copy of the framed-pill CSS, its own wizard copy, and its own
// click / Space / modified-click handling; the groups and fleet-admin rows
// had drifted to an underline look.
//
// Two ratchets, regenerated with
//   UPDATE_UI_TABS_BASELINE=1 node --test jstest/ui-tabs.test.mjs
// - markup: a role="tablist" element that is not .ui-tabs, counted per file;
// - css: a rule painting (background or colour) a *-subnav / *-subtab(s)
//   class, which is a per-area tab skin.
// A file may not exceed its baseline and a migrated one must lower it.

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync, writeFileSync } from 'node:fs';
import { createPreactHarness } from './preact-harness.mjs';

const DASHBOARD = new URL('../dashboard/', import.meta.url);
const BASELINE = new URL('./ui-tabs-baseline.json', import.meta.url);

// handRolledTablists counts opening tags with role="tablist" whose class does
// not name ui-tabs.
export function handRolledTablists(source) {
  let count = 0;
  for (const [tag] of source.matchAll(/<[a-z]+\b[^<>]*?role=["']tablist["'][^<>]*>/g)) {
    if (!/\bui-tabs\b/.test(tag)) count += 1;
  }
  return count;
}

// tabSkins lists selectors (whitespace-normalised) whose subject is a
// *-subnav / *-subtab(s) class and whose rule sets a background or colour.
export function tabSkins(css) {
  const found = new Set();
  const source = css.replace(/\/\*[\s\S]*?\*\//g, '');
  for (const [, selector, body] of source.matchAll(/([^{};]*)\{([^{}]*)\}/g)) {
    if (!/(?:^|[;\s])(?:background(?:-color)?|color)\s*:/.test(body)) continue;
    for (const part of selector.split(/,(?![^()]*\))/)) {
      const subject = part.trim().split(/\s*[\s>+~]\s*/).pop();
      if (/\.[\w-]+-sub(?:nav|tabs?)\b/.test(subject)) found.add(part.trim().replace(/\s+/g, ' '));
    }
  }
  return found;
}

test('the scanners find hand-rolled tab rows and per-area tab skins', () => {
  assert.equal(handRolledTablists('<div class="x-subnav" role="tablist"></div><div class="ui-tabs y" role="tablist"></div><div role="tablist" class="z">'), 2);
  const css = '.a-subtab { color: red; }\n.b-subnav { margin: 0; }\nbody.wizard .c-subtab.active, .d { background: x; }\n.ui-tab { color: grey; }';
  assert.deepEqual([...tabSkins(css)].sort(), ['.a-subtab', 'body.wizard .c-subtab.active']);
});

function scan() {
  const markup = {};
  const skins = {};
  const files = ['dashboard.html', 'terminals.html', ...readdirSync(new URL('js/', DASHBOARD)).filter((f) => f.endsWith('.js') && f !== 'ui-tabs.js').map((f) => `js/${f}`)];
  for (const file of files.sort()) {
    const n = handRolledTablists(readFileSync(new URL(file, DASHBOARD), 'utf8'));
    if (n) markup[file] = n;
  }
  for (const file of readdirSync(DASHBOARD).filter((f) => f.endsWith('.css')).sort()) {
    const found = [...tabSkins(readFileSync(new URL(file, DASHBOARD), 'utf8'))].sort();
    if (found.length) skins[file] = found;
  }
  return { markup, skins };
}

test('no new hand-rolled tab rows or per-area tab skins', () => {
  const now = scan();
  if (process.env.UPDATE_UI_TABS_BASELINE) writeFileSync(BASELINE, `${JSON.stringify(now, null, 2)}\n`);
  const base = JSON.parse(readFileSync(BASELINE, 'utf8'));
  const over = Object.entries(now.markup).filter(([f, n]) => n > (base.markup[f] ?? 0)).map(([f, n]) => `${f}: ${n}`);
  assert.deepEqual(over, [], 'render tab rows with Tabs from js/ui-tabs.js (or .ui-tabs / .ui-tab in static markup)');
  const stale = Object.entries(base.markup).filter(([f, n]) => (now.markup[f] ?? 0) < n).map(([f]) => f);
  assert.deepEqual(stale, [], 'a migrated file must bring its baseline down (UPDATE_UI_TABS_BASELINE=1)');
  const newSkins = Object.entries(now.skins).flatMap(([f, sels]) => sels.filter((s) => !(base.skins[f] || []).includes(s)).map((s) => `${f}: ${s}`));
  assert.deepEqual(newSkins, [], 'tabs take their paint from .ui-tabs / .ui-tab in ui.css');
  const goneSkins = Object.entries(base.skins).flatMap(([f, sels]) => sels.filter((s) => !(now.skins[f] || []).includes(s)).map((s) => `${f}: ${s}`));
  assert.deepEqual(goneSkins, [], 'a removed tab skin must leave the baseline (UPDATE_UI_TABS_BASELINE=1)');
});

async function setup(t) {
  const harness = await createPreactHarness(t);
  const { Tabs } = await harness.importDashboardModule('js/ui-tabs.js');
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  return { harness, Tabs, host };
}

test('Tabs renders an accessible tablist with a roving tab stop', async (t) => {
  const { harness, Tabs, host } = await setup(t);
  const items = [
    { key: 'a', label: 'Alpha', href: '/x/a', class: 'area-tab', attrs: { 'data-sub': 'a' } },
    { key: 'b', label: 'Beta', href: '/x/b' },
    { key: 'c', label: 'Gamma', disabled: true },
  ];
  await harness.mount(harness.html`<${Tabs} class="area-nav" label="Views" value="b" items=${items} onSelect=${() => {}} />`, host);
  const list = host.querySelector('[role="tablist"]');
  assert.equal(list.className, 'ui-tabs area-nav');
  assert.equal(list.getAttribute('aria-label'), 'Views');
  const tabs = [...list.querySelectorAll('[role="tab"]')];
  assert.deepEqual(tabs.map((tab) => tab.tagName), ['A', 'A', 'BUTTON'], 'href tabs are links, the rest buttons');
  assert.deepEqual(tabs.map((tab) => tab.getAttribute('aria-selected')), ['false', 'true', 'false']);
  assert.deepEqual(tabs.map((tab) => tab.getAttribute('tabindex')), ['-1', '0', '-1']);
  assert.equal(tabs[0].className, 'ui-tab area-tab');
  assert.equal(tabs[1].className, 'ui-tab active');
  assert.equal(tabs[0].dataset.sub, 'a', 'attrs reach the tab');
  assert.equal(tabs[2].disabled, true);
});

test('Tabs: a keyboard pick reaches a click-only handler without the key event', async (t) => {
  const { harness, Tabs, host } = await setup(t);
  const events = [];
  const items = [{ key: 'a', label: 'A', href: '/a' }, { key: 'b', label: 'B', href: '/b' }];
  await harness.mount(harness.html`<${Tabs} value="a" items=${items} onSelect=${(key, event) => events.push([key, event?.type])} />`, host);
  const [a, b] = host.querySelectorAll('[role="tab"]');
  await harness.act(() => harness.fireEvent(b, 'keydown', { key: ' ' }));
  await harness.act(() => harness.fireEvent(a, 'keydown', { key: 'ArrowRight' }));
  await harness.act(() => harness.fireEvent(b, 'click', { button: 0 }));
  assert.deepEqual(events, [['b', undefined], ['b', undefined], ['b', 'click']]);
});

test('Tabs: plain clicks select in place, modified clicks and disabled tabs do not', async (t) => {
  const { harness, Tabs, host } = await setup(t);
  const picked = [];
  const items = [{ key: 'a', label: 'A', href: '/a' }, { key: 'b', label: 'B', href: '/b' }];
  await harness.mount(harness.html`<${Tabs} value="a" items=${items} onSelect=${(key) => picked.push(key)} />`, host);
  const [, b] = host.querySelectorAll('[role="tab"]');
  await harness.act(() => harness.fireEvent(b, 'click', { button: 0 }));
  await harness.act(() => harness.fireEvent(b, 'click', { button: 0, ctrlKey: true }));
  await harness.act(() => harness.fireEvent(b, 'keydown', { key: ' ' }));
  assert.deepEqual(picked, ['b', 'b'], 'click and Space pick; a modified click is left to the browser');
  await harness.act(() => harness.fireEvent(b, 'keydown', { key: 'ArrowLeft', altKey: true }));
  assert.deepEqual(picked, ['b', 'b'], 'Alt+Arrow (browser Back/Forward) is left alone');
});

test('Tabs: arrow keys move focus and selection, wrapping and skipping disabled tabs', async (t) => {
  const { harness, Tabs, host } = await setup(t);
  const picked = [];
  const items = [{ key: 'a', label: 'A' }, { key: 'b', label: 'B', disabled: true }, { key: 'c', label: 'C' }];
  await harness.mount(harness.html`<${Tabs} value="a" items=${items} onSelect=${(key) => picked.push(key)} />`, host);
  const [a, , c] = host.querySelectorAll('[role="tab"]');
  await harness.act(() => harness.fireEvent(a, 'keydown', { key: 'ArrowRight' }));
  await harness.act(() => harness.fireEvent(a, 'keydown', { key: 'ArrowLeft' }));
  await harness.act(() => harness.fireEvent(c, 'keydown', { key: 'Home' }));
  assert.deepEqual(picked, ['c', 'c'], 'Right skips the disabled tab, Left wraps, Home on the first selects nothing new');
  assert.equal(harness.document.activeElement, a);
});

test('Tabs: variant="seg" kind="radio" is a joined radiogroup', async (t) => {
  const { harness, Tabs, host } = await setup(t);
  const picked = [];
  const items = [{ key: 'local', label: 'Here' }, { key: 'peer', label: 'Peer', disabled: true }];
  await harness.mount(harness.html`<${Tabs} variant="seg" kind="radio" label="Target" value="local" items=${items} onSelect=${(key) => picked.push(key)} />`, host);
  const group = host.querySelector('[role="radiogroup"]');
  assert.equal(group.className, 'ui-tabs ui-tabs-seg');
  const radios = [...group.querySelectorAll('[role="radio"]')];
  assert.deepEqual(radios.map((r) => r.getAttribute('aria-checked')), ['true', 'false']);
  await harness.act(() => harness.fireEvent(radios[0], 'keydown', { key: 'ArrowRight' }));
  await harness.act(() => harness.fireEvent(radios[1], 'click', { button: 0 }));
  assert.deepEqual(picked, [], 'the disabled radio is skipped and not pickable');
});
