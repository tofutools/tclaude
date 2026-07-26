// toolbar-actions-menu.test.mjs — the Groups toolbar ⚙ cog.
//
// This cog is the one menu whose items are static markup rather than a Preact
// render, so it wires the shared filter core by hand. These tests pin that the
// imperative path behaves like the Preact one: focused-and-empty on open,
// type-to-narrow, Enter through the item's own handler, and reset on close.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

// The production shell's cog, trimmed to a few representative items.
const MARKUP = `
  <div class="filter-bar-cog">
    <button class="cog-btn" type="button" aria-haspopup="menu" aria-expanded="false"><span class="cog-glyph">⚙︎</span></button>
    <div class="action-menu" role="menu">
      <input class="action-menu-filter" type="text" autocomplete="off" spellcheck="false" aria-label="Filter actions" placeholder="filter actions…">
      <button role="menuitem" id="group-import-open" title="Import a group from a .zip archive"><span class="tpl-word-regular">⤒ import</span><span class="tpl-word-wizard">⤒ unseal an archive</span></button>
      <button role="menuitem" id="cleanup-all-open" title="Bulk cleanup across agents and conversations">🧹 clean up</button>
      <button role="menuitem" id="templates-manage-open" title="Manage group templates — reusable team blueprints"><span class="tpl-word-regular">⧉ templates…</span><span class="tpl-word-wizard">⧉ circles…</span></button>
      <button role="menuitem" id="links-manage-open" data-act="links-manage" title="Manage inter-group communication links">🔗 links…</button>
    </div>
  </div>`;

async function mountToolbar(t) {
  const harness = await createPreactHarness(t);
  const { document } = harness;
  document.body.innerHTML = MARKUP;
  // LinkeDOM has no layout, so the flip-up measurement needs a stand-in. Zeroes
  // mean "fits below", which is the ordinary case this suite is about.
  for (const el of document.querySelectorAll('.filter-bar-cog, .cog-btn, .action-menu')) {
    el.getBoundingClientRect = () => ({ top: 0, bottom: 0, height: 0 });
  }
  harness.window.innerHeight = 900;

  const [{ bindToolbarActionsMenu }, menuFilter] = await Promise.all([
    harness.importDashboardModule('js/toolbar-actions-menu.js'),
    harness.importDashboardModule('js/menu-filter.js'),
  ]);
  // Not registered as a t.after: the harness restores the DOM globals in its own
  // after-hook first, and this unbind needs `document`. Each test imports a
  // fresh copy of the module against a fresh document, so nothing leaks between
  // them; the handle is returned for the test that asserts unbinding.
  const cleanup = bindToolbarActionsMenu();

  const menu = document.querySelector('.action-menu');
  const filter = document.querySelector('.action-menu-filter');
  const view = {
    harness,
    menuFilter,
    cleanup,
    menu,
    filter,
    cog: document.querySelector('.cog-btn'),
    isOpen: () => menu.classList.contains('open'),
    visible: () => [...menu.querySelectorAll('[role="menuitem"]')]
      .filter((item) => !item.hasAttribute(menuFilter.FILTERED_OUT_ATTR))
      .map((item) => item.id),
    type: (value) => {
      filter.value = value;
      harness.fireEvent(filter, 'input');
    },
    press: (key) => harness.fireEvent(filter, 'keydown', { key }),
  };
  return view;
}

const ALL = ['group-import-open', 'cleanup-all-open', 'templates-manage-open', 'links-manage-open'];

test('the toolbar cog opens focused on its filter box, showing everything', async (t) => {
  const view = await mountToolbar(t);
  view.cog.click();

  assert.equal(view.isOpen(), true);
  assert.equal(view.cog.getAttribute('aria-expanded'), 'true');
  assert.equal(view.harness.document.activeElement, view.filter);
  assert.deepEqual(view.visible(), ALL);
});

test('typing narrows the toolbar menu across labels and titles', async (t) => {
  const view = await mountToolbar(t);
  view.cog.click();

  view.type('templates');
  assert.deepEqual(view.visible(), ['templates-manage-open']);

  // "blueprints" lives only in the templates title; "archive" only in import's.
  view.type('archive');
  assert.deepEqual(view.visible(), ['group-import-open']);

  view.type('circles');
  assert.deepEqual(view.visible(), ['templates-manage-open'],
    'the 🧙 vocabulary stays searchable in the plain theme');

  view.type('nothing here');
  assert.deepEqual(view.visible(), []);
  assert.equal(view.menu.getAttribute(view.menuFilter.EMPTY_ATTR), '1');
});

test('Enter activates the matched toolbar item and closes the menu', async (t) => {
  const view = await mountToolbar(t);
  const runs = [];
  for (const id of ALL) {
    view.harness.document.getElementById(id).addEventListener('click', () => runs.push(id));
  }
  view.cog.click();

  view.type('links');
  view.press('Enter');

  assert.deepEqual(runs, ['links-manage-open']);
  assert.equal(view.isOpen(), false, 'the item click dismissed the menu');
});

test('closing the toolbar cog resets its filter', async (t) => {
  const view = await mountToolbar(t);
  view.cog.click();
  view.type('links');
  assert.deepEqual(view.visible(), ['links-manage-open']);

  view.cog.click();
  assert.equal(view.isOpen(), false);
  assert.equal(view.filter.value, '');
  assert.deepEqual(view.visible(), ALL);

  // Reopening starts from the full list rather than the stale query.
  view.cog.click();
  assert.deepEqual(view.visible(), ALL);
});

test('Escape clears the toolbar query first, then closes', async (t) => {
  const view = await mountToolbar(t);
  view.cog.click();
  view.type('links');

  view.press('Escape');
  assert.equal(view.filter.value, '');
  assert.equal(view.isOpen(), true, 'the first Escape only clears the query');

  view.press('Escape');
  assert.equal(view.isOpen(), false);
});

test('a click outside closes the toolbar cog and clears its filter', async (t) => {
  const view = await mountToolbar(t);
  view.cog.click();
  view.type('links');

  const outside = view.harness.document.body.appendChild(
    view.harness.document.createElement('div'));
  outside.click();

  assert.equal(view.isOpen(), false);
  assert.equal(view.filter.value, '');
  assert.deepEqual(view.visible(), ALL);
});
