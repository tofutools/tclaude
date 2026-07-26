// cog-menu-filter-preact.test.mjs — the per-group / per-agent ⚙ cogs' filter box
// wired through the real ActionMenu component.
//
// menu-filter.test.mjs covers the matching rules; these cover the integration
// risks that only appear once Preact owns the menu: that the box is focused and
// empty on open, that Enter reaches the item's own handler (so the keyboard adds
// no second way to invoke an action), that the 2s snapshot re-render does not
// wipe the applied filter, and that closing leaves nothing filtered behind.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

async function mountCog(t, { onRun = () => {} } = {}) {
  const harness = await createPreactHarness(t);
  const [{ ActionMenu, GroupsInteractionProvider }, menuFilter] = await Promise.all([
    harness.importDashboardModule('js/groups-interactions.js'),
    harness.importDashboardModule('js/menu-filter.js'),
  ]);
  const { html } = harness;

  // Stand-ins shaped like the production items: a themed twin-span label, an
  // explanatory title, and one conditional row that comes and goes with the
  // snapshot the way an online/offline item does.
  const items = (extra) => html`
    <button role="menuitem" data-act="add-member" title="Add an existing conversation to this group"
      ><span class="theme-copy-regular">+ add member</span><span class="theme-copy-wizard">+ add familiar</span></button>
    <button role="menuitem" data-act="clone-group" title="Clone this group"
      onClick=${() => onRun('clone-group')}
      ><span class="theme-copy-regular">⧉ clone…</span><span class="theme-copy-wizard">⧉ mirror party…</span></button>
    ${extra ? html`<button role="menuitem" data-act="group-web-term" title="Open a web terminal here">🖥 open web terminal</button>` : null}
    <button role="menuitem" data-act="delete-group" class="danger" title="Delete this group"
      ><span class="theme-copy-regular">delete group</span><span class="theme-copy-wizard">disband party</span></button>
  `;
  const tree = (extra) => html`
    <${GroupsInteractionProvider}>
      <${ActionMenu} menuKey="group:tclaude" kind="group-menu" wrapperClass="group-actions group-header-cog"
        >${items(extra)}<//>
    <//>`;

  const mounted = await harness.mount(tree(false));
  const root = mounted.container;
  const view = {
    harness,
    menuFilter,
    root,
    rerenderWith: (extra) => mounted.rerender(tree(extra)),
    cog: () => root.querySelector('.cog-btn'),
    menu: () => root.querySelector('.action-menu'),
    filter: () => root.querySelector('.action-menu-filter'),
    visible: () => [...root.querySelectorAll('[role="menuitem"]')]
      .filter((item) => !item.hasAttribute(menuFilter.FILTERED_OUT_ATTR))
      .map((item) => item.querySelector('.theme-copy-regular')?.textContent
        ?? item.textContent.trim()),
    open: async () => harness.act(() => view.cog().click()),
    type: async (value) => harness.input(view.filter(), value),
    press: async (key) => harness.act(() => harness.fireEvent(view.filter(), 'keydown', { key })),
  };
  return view;
}

test('the cog menu carries a filter box above its untouched items', async (t) => {
  const view = await mountCog(t);
  await view.open();

  const filter = view.filter();
  assert.ok(filter, 'the menu renders a filter box');
  assert.equal(filter.getAttribute('aria-label'), 'Filter actions');
  // The filter must not be mistaken for an item by the menu's own routing.
  assert.equal(filter.getAttribute('role'), null);
  assert.equal(view.menu().classList.contains('open'), true);
  assert.deepEqual(view.visible(), ['+ add member', '⧉ clone…', 'delete group']);
});

test('opening a cog focuses the filter box so it is typeable at once', async (t) => {
  const view = await mountCog(t);
  await view.open();
  // The component focuses in a microtask, matching InlineEditor's pattern.
  await new Promise((resolve) => queueMicrotask(resolve));
  assert.equal(view.harness.document.activeElement, view.filter());
});

test('typing narrows the menu and clicking the filter box does not dismiss it', async (t) => {
  const view = await mountCog(t);
  await view.open();

  await view.type('clone');
  assert.deepEqual(view.visible(), ['⧉ clone…']);

  // ActionMenu closes on any button click inside the menu; the box is not one.
  await view.harness.act(() => view.filter().click());
  assert.equal(view.menu().classList.contains('open'), true, 'still open');
  assert.deepEqual(view.visible(), ['⧉ clone…'], 'and still filtered');
});

test('Enter runs the matched item through its own click handler', async (t) => {
  const runs = [];
  const view = await mountCog(t, { onRun: (act) => runs.push(act) });
  await view.open();

  await view.type('clone');
  await view.press('Enter');

  assert.deepEqual(runs, ['clone-group']);
  // Running an item closes the menu, exactly as clicking it does.
  assert.equal(view.menu().classList.contains('open'), false);
});

test('a snapshot re-render keeps the live filter applied', async (t) => {
  const view = await mountCog(t);
  await view.open();
  await view.type('term');
  assert.deepEqual(view.visible(), [], 'the conditional item is absent for now');

  // The poll publishes a snapshot in which the group gained a default dir, so
  // its "open web terminal" item appears. Preact re-renders the children under
  // the open menu; the filter has to be re-applied to the new node rather than
  // leaving it unfiltered.
  await view.rerenderWith(true);
  assert.deepEqual(view.visible(), ['🖥 open web terminal']);
  assert.equal(view.filter().value, 'term', 'and the query survives the render');
});

test('Escape clears the query before it closes the menu', async (t) => {
  const view = await mountCog(t);
  await view.open();
  await view.type('clone');

  await view.press('Escape');
  assert.equal(view.filter().value, '', 'the query is cleared');
  assert.equal(view.menu().classList.contains('open'), true, 'the menu stays open');
  assert.deepEqual(view.visible(), ['+ add member', '⧉ clone…', 'delete group']);

  await view.press('Escape');
  assert.equal(view.menu().classList.contains('open'), false, 'a second Escape closes');
});

test('closing leaves no filtered state behind for the next open', async (t) => {
  const view = await mountCog(t);
  await view.open();
  await view.type('clone');

  await view.harness.act(() => view.cog().click());
  assert.equal(view.menu().classList.contains('open'), false);
  assert.equal(view.filter().value, '', 'the box is emptied on close');
  assert.deepEqual(view.visible(), ['+ add member', '⧉ clone…', 'delete group'],
    'and every item is showing again');
  assert.equal(view.menuFilter.menuActiveItem(view.menu()), null);
});
