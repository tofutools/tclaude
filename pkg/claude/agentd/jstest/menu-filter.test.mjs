// menu-filter.test.mjs — the shared type-to-filter core behind the ⚙ cog menus.
//
// These cover the rules that are easy to regress and invisible in a screenshot:
// that both theme vocabularies stay searchable without inventing a fused phrase
// across the two label spans, that the descriptive `title` is search fodder,
// that name hits precede descriptive-text hits without disturbing order within
// either group, and that disabled items are listed but never become the Enter
// target.

import test from 'node:test';
import assert from 'node:assert/strict';
import { assertAbsent, assertSameNode } from './assertions.mjs';
import { createPreactHarness } from './preact-harness.mjs';

async function core(t) {
  const harness = await createPreactHarness(t);
  const module = await harness.importDashboardModule('js/menu-filter.js');
  return { harness, ...module };
}

// A menu shaped like the production ones: themed twin-span labels, explanatory
// titles, data-act slugs, a separator and a disabled item.
function menuMarkup() {
  return `
    <div class="action-menu" role="menu">
      <input class="action-menu-filter" type="text" />
      <button role="menuitem" data-act="add-member" title="Add an existing conversation to this group"
        ><span class="theme-copy-regular">+ add member</span><span class="theme-copy-wizard">+ add familiar</span></button>
      <button role="menuitem" data-act="cleanup-worktrees-group" title="Scan this group's repo for stale worktrees"
        >🧹 cleanup worktrees…</button>
      <div class="menu-sep" role="separator"></div>
      <button role="menuitem" data-act="export-summary" disabled title="Export needs a running agent"
        ><span class="theme-copy-regular">summary…</span><span class="theme-copy-wizard">inscribe scroll…</span></button>
      <button role="menuitem" data-act="clone-group" title="Clone this group — copy every setting"
        ><span class="theme-copy-regular">⧉ clone…</span><span class="theme-copy-wizard">⧉ mirror party…</span></button>
      <button role="menuitem" data-act="delete-group" class="danger" title="Delete this group"
        ><span class="theme-copy-regular">delete group</span><span class="theme-copy-wizard">disband party</span></button>
    </div>`;
}

function mountMenu(harness) {
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  host.innerHTML = menuMarkup();
  const menu = host.querySelector('.action-menu');
  return { menu, input: menu.querySelector('.action-menu-filter') };
}

function labelsOf(items) {
  return items.map((item) => item.querySelector('.theme-copy-regular')?.textContent
    ?? item.textContent.trim());
}

function shown(module, menu) {
  return labelsOf([...menu.querySelectorAll('[role="menuitem"]')]
    .filter((item) => !item.hasAttribute(module.FILTERED_OUT_ATTR)));
}

test('menu filter keeps both theme vocabularies searchable', async (t) => {
  const { harness, ...module } = await core(t);
  const { menu } = mountMenu(harness);

  module.applyMenuFilter(menu, 'delete');
  assert.deepEqual(shown(module, menu), ['delete group']);

  // The 🧙 label of the very same item, with the plain theme still active.
  module.applyMenuFilter(menu, 'disband');
  assert.deepEqual(shown(module, menu), ['delete group']);
});

test('menu filter does not fuse the two label spans into a phrase', async (t) => {
  const { harness, ...module } = await core(t);
  const { menu } = mountMenu(harness);

  // textContent would read "delete groupdisband party" and match this; the
  // per-text-node join must not invent a phrase that spans the seam.
  module.applyMenuFilter(menu, 'groupdisband');
  assert.deepEqual(shown(module, menu), []);
  assert.equal(menu.getAttribute(module.EMPTY_ATTR), '1');
});

test('menu filter searches the descriptive title and the data-act slug', async (t) => {
  const { harness, ...module } = await core(t);
  const { menu } = mountMenu(harness);

  // "conversation" appears only in the add-member title.
  module.applyMenuFilter(menu, 'conversation');
  assert.deepEqual(shown(module, menu), ['+ add member']);

  // "worktrees" reaches the item via its label; "cleanup group" only via the
  // hyphenated data-act slug opened into words.
  module.applyMenuFilter(menu, 'cleanup group');
  assert.deepEqual(shown(module, menu), ['🧹 cleanup worktrees…']);
});

test('menu filter reuses the palette synonyms', async (t) => {
  const { harness, ...module } = await core(t);
  const { menu } = mountMenu(harness);

  // "party" → "group" comes from palette-score.js's SYNONYMS, not from any
  // keyword authored on these items, and reaches the four whose label or title
  // talks about a group. "summary…" mentions neither, so it drops.
  module.applyMenuFilter(menu, 'party');
  assert.deepEqual(shown(module, menu),
    ['+ add member', '🧹 cleanup worktrees…', '⧉ clone…', 'delete group']);
});

test('menu filter puts name hits before descriptive-text hits', async (t) => {
  const { harness, ...module } = await core(t);
  const { menu, input } = mountMenu(harness);

  const { visible } = module.applyMenuFilter(menu, 'group', { input });
  assert.deepEqual(labelsOf(visible),
    ['⧉ clone…', 'delete group', '+ add member', '🧹 cleanup worktrees…']);
  assert.equal(module.menuActiveItem(menu).getAttribute('data-act'), 'clone-group',
    'Enter starts on the first visually ranked result');

  const priorities = visible.map((item) => item.getAttribute(module.MATCH_PRIORITY_ATTR));
  assert.deepEqual(priorities, ['1', '1', '2', '2']);
  assert.deepEqual(visible.map((item) => item.getAttribute('aria-posinset')),
    ['1', '2', '3', '4'], 'assistive technology gets the ranked visual positions');
  assert.deepEqual(visible.map((item) => item.getAttribute('aria-setsize')),
    ['4', '4', '4', '4']);
});

test('menu filter preserves authored order within each match group', async (t) => {
  const { harness, ...module } = await core(t);
  const { menu } = mountMenu(harness);

  const { visible } = module.applyMenuFilter(menu, 'party');
  assert.deepEqual(labelsOf(visible),
    ['⧉ clone…', 'delete group', '+ add member', '🧹 cleanup worktrees…']);
});

test('menu filter hides separators only while a query is live', async (t) => {
  const { harness, ...module } = await core(t);
  const { menu } = mountMenu(harness);
  const separator = menu.querySelector('[role="separator"]');

  module.applyMenuFilter(menu, '');
  assert.equal(separator.hasAttribute(module.FILTERED_OUT_ATTR), false);

  module.applyMenuFilter(menu, 'group');
  assert.equal(separator.hasAttribute(module.FILTERED_OUT_ATTR), true);

  module.applyMenuFilter(menu, '');
  assert.equal(separator.hasAttribute(module.FILTERED_OUT_ATTR), false);
  for (const item of module.menuItems(menu)) {
    assert.equal(item.hasAttribute('aria-posinset'), false);
    assert.equal(item.hasAttribute('aria-setsize'), false);
  }
});

test('resetActive restores the untouched menu, cursor included', async (t) => {
  const { harness, ...module } = await core(t);
  const { menu, input } = mountMenu(harness);

  module.applyMenuFilter(menu, 'clone', { input });
  assert.ok(module.menuActiveItem(menu), 'a live query pre-selects its top match');

  // The open/close edges ask for a clean slate.
  module.applyMenuFilter(menu, '', { input, resetActive: true });
  assertAbsent(module.menuActiveItem(menu));
  assert.equal(input.hasAttribute('aria-activedescendant'), false);
  assert.equal(menu.hasAttribute(module.EMPTY_ATTR), false);
  assert.deepEqual(shown(module, menu), ['+ add member', '🧹 cleanup worktrees…',
    'summary…', '⧉ clone…', 'delete group']);
});

test('a re-apply preserves the cursor instead of clearing it', async (t) => {
  const { harness, ...module } = await core(t);
  const { menu, input } = mountMenu(harness);
  module.applyMenuFilter(menu, '', { input, resetActive: true });

  // The operator arrows down with an empty box. The Preact cogs re-apply the
  // filter after every ~2s snapshot render; clearing there would undo the
  // selection out from under them and leave Enter doing nothing.
  module.moveMenuActive(menu, 1, { input });
  module.moveMenuActive(menu, 1, { input });
  const chosen = module.menuActiveItem(menu);
  assert.equal(chosen.getAttribute('data-act'), 'cleanup-worktrees-group');

  module.applyMenuFilter(menu, '', { input });
  assert.equal(module.menuActiveItem(menu), chosen, 'the cursor survives a re-apply');
  assert.equal(input.getAttribute('aria-activedescendant'), chosen.id);

  // Same for a re-apply under a live query that still includes the cursor.
  module.applyMenuFilter(menu, 'group', { input });
  assert.equal(module.menuActiveItem(menu), chosen);
});

test('a narrowing query moves the cursor off an item it filtered away', async (t) => {
  const { harness, ...module } = await core(t);
  const { menu, input } = mountMenu(harness);
  module.applyMenuFilter(menu, 'group', { input });
  assert.equal(module.menuActiveItem(menu).getAttribute('data-act'), 'clone-group');

  // "conversation" appears only in add-member's title, so the cursor moves to
  // that result; "clone" drops it, and the cursor must fall to the new top
  // match rather than pointing at a hidden row.
  module.applyMenuFilter(menu, 'conversation', { input });
  assert.equal(module.menuActiveItem(menu).getAttribute('data-act'), 'add-member');

  module.applyMenuFilter(menu, 'clone', { input });
  assert.equal(module.menuActiveItem(menu).getAttribute('data-act'), 'clone-group');
});

test('a live query pre-selects its top match so Enter runs without arrowing', async (t) => {
  const { harness, ...module } = await core(t);
  const { menu, input } = mountMenu(harness);
  const runs = [];
  for (const item of menu.querySelectorAll('[role="menuitem"]')) {
    item.addEventListener('click', () => runs.push(item.getAttribute('data-act')));
  }

  module.applyMenuFilter(menu, 'party', { input });
  const handled = module.handleMenuFilterKeyDown(menu, {
    key: 'Enter', currentTarget: input, preventDefault() {}, stopPropagation() {},
  }, { hasQuery: true });

  assert.equal(handled, true);
  assert.deepEqual(runs, ['clone-group'], 'the topmost name match ran');
});

test('disabled items are listed but skipped by the keyboard', async (t) => {
  const { harness, ...module } = await core(t);
  const { menu, input } = mountMenu(harness);

  // "summary…" matches its own title but is disabled while the agent is offline.
  module.applyMenuFilter(menu, 'export needs', { input });
  assert.deepEqual(shown(module, menu), ['summary…'],
    'a disabled item stays visible so its title can explain why');
  assertAbsent(module.menuActiveItem(menu), 'but it is never the Enter target');

  const handled = module.handleMenuFilterKeyDown(menu, {
    key: 'Enter', currentTarget: input, preventDefault() {}, stopPropagation() {},
  }, { hasQuery: true });
  assert.equal(handled, false, 'Enter is left to the browser when nothing can run');
});

test('arrow navigation wraps across the visible, enabled items', async (t) => {
  const { harness, ...module } = await core(t);
  const { menu, input } = mountMenu(harness);
  module.applyMenuFilter(menu, '', { input });

  const act = () => module.menuActiveItem(menu)?.getAttribute('data-act') ?? null;
  assert.equal(module.moveMenuActive(menu, 1, { input }) && act(), 'add-member');
  // export-summary sits next in the DOM but is disabled, so ↓ skips it.
  module.moveMenuActive(menu, 1, { input });
  assert.equal(act(), 'cleanup-worktrees-group');
  module.moveMenuActive(menu, 1, { input });
  assert.equal(act(), 'clone-group');
  module.moveMenuActive(menu, 1, { input });
  assert.equal(act(), 'delete-group');
  module.moveMenuActive(menu, 1, { input });
  assert.equal(act(), 'add-member', 'wraps to the top');
  module.moveMenuActive(menu, -1, { input });
  assert.equal(act(), 'delete-group', 'and back round the other way');
});

test('the filter box aims a RESOLVABLE aria-activedescendant at the cursor', async (t) => {
  const { harness, ...module } = await core(t);
  const { menu, input } = mountMenu(harness);
  const { document } = harness;

  module.applyMenuFilter(menu, 'clone', { input });
  const active = module.menuActiveItem(menu);
  assert.ok(active.id, 'an id is minted for the selected item');
  assert.equal(input.getAttribute('aria-activedescendant'), active.id);

  // The cursor's item is a SIBLING of the box, not a descendant, so the
  // reference only resolves through aria-controls. Without that edge the
  // attribute is inert and no AT announces the cursor — assert the whole
  // chain, not just that the id round-trips.
  const controls = input.getAttribute('aria-controls');
  assert.ok(controls, 'the box declares what it controls');
  const controlled = document.getElementById(controls);
  assertSameNode(controlled, menu, 'aria-controls resolves to the menu');
  assert.equal(controlled.contains(document.getElementById(active.id)), true,
    'and that menu contains the referenced item');

  // Re-applying must not mint a second id for the same menu.
  module.applyMenuFilter(menu, 'delete', { input });
  assert.equal(input.getAttribute('aria-controls'), controls);
});

test('Escape clears a live query first and closes only on the second press', async (t) => {
  const { harness, ...module } = await core(t);
  const { menu, input } = mountMenu(harness);
  let cleared = 0;
  let stopped = 0;
  const escape = (hasQuery) => module.handleMenuFilterKeyDown(menu, {
    key: 'Escape',
    currentTarget: input,
    preventDefault() {},
    stopPropagation() { stopped += 1; },
  }, { hasQuery, clearQuery: () => { cleared += 1; } });

  module.applyMenuFilter(menu, 'clone', { input });
  assert.ok(module.menuActiveItem(menu), 'the query left a cursor');

  assert.equal(escape(true), true, 'a mistyped query is consumed, not the menu');
  assert.equal(cleared, 1);
  assert.equal(stopped, 1, 'and kept from the document handler that would close');
  // Clearing is a start-over: the cursor went with the query that placed it.
  assertAbsent(module.menuActiveItem(menu));
  assert.equal(input.hasAttribute('aria-activedescendant'), false);

  assert.equal(escape(false), false, 'an empty box lets Escape through to close');
  assert.equal(cleared, 1);
  assert.equal(stopped, 1);
});

test('hover moves the same single cursor the keyboard uses', async (t) => {
  const { harness, ...module } = await core(t);
  const { menu, input } = mountMenu(harness);
  const unbind = module.bindMenuHover(menu, { resolveInput: () => input });
  t.after(unbind);
  module.applyMenuFilter(menu, '', { input });

  const clone = menu.querySelector('[data-act="clone-group"]');
  harness.fireEvent(clone, 'mouseover');
  assertSameNode(module.menuActiveItem(menu), clone);
  assert.equal(input.getAttribute('aria-activedescendant'), clone.id);

  // A disabled row must not steal the cursor from a runnable one.
  harness.fireEvent(menu.querySelector('[data-act="export-summary"]'), 'mouseover');
  assertSameNode(module.menuActiveItem(menu), clone);

  unbind();
  harness.fireEvent(menu.querySelector('[data-act="delete-group"]'), 'mouseover');
  assertSameNode(module.menuActiveItem(menu), clone, 'unbinding stops tracking');
});
