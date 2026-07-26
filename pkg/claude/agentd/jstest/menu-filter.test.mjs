// menu-filter.test.mjs — the shared type-to-filter core behind the ⚙ cog menus.
//
// These cover the rules that are easy to regress and invisible in a screenshot:
// that both theme vocabularies stay searchable without inventing a fused phrase
// across the two label spans, that the descriptive `title` is search fodder,
// that order is preserved rather than ranked, and that disabled items are listed
// but never become the Enter target.

import test from 'node:test';
import assert from 'node:assert/strict';
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

test('menu filter preserves menu order instead of ranking', async (t) => {
  const { harness, ...module } = await core(t);
  const { menu } = mountMenu(harness);

  // "delete group" is the strongest match here — a word-start hit on the label
  // (score 80) — while the other three match only as a phrase inside their
  // title (50). Ranked, it would jump to the top; it must stay last, where the
  // operator has always found it.
  module.applyMenuFilter(menu, 'group');
  assert.deepEqual(shown(module, menu),
    ['+ add member', '🧹 cleanup worktrees…', '⧉ clone…', 'delete group']);
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
});

test('an empty query restores the untouched menu, highlight included', async (t) => {
  const { harness, ...module } = await core(t);
  const { menu, input } = mountMenu(harness);

  module.applyMenuFilter(menu, 'clone', { input });
  assert.ok(module.menuActiveItem(menu), 'a live query pre-selects its top match');

  module.applyMenuFilter(menu, '', { input });
  assert.equal(module.menuActiveItem(menu), null);
  assert.equal(input.hasAttribute('aria-activedescendant'), false);
  assert.equal(menu.hasAttribute(module.EMPTY_ATTR), false);
  assert.deepEqual(shown(module, menu), ['+ add member', '🧹 cleanup worktrees…',
    'summary…', '⧉ clone…', 'delete group']);
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
  assert.deepEqual(runs, ['add-member'], 'the topmost match ran');
});

test('disabled items are listed but skipped by the keyboard', async (t) => {
  const { harness, ...module } = await core(t);
  const { menu, input } = mountMenu(harness);

  // "summary…" matches its own title but is disabled while the agent is offline.
  module.applyMenuFilter(menu, 'export needs', { input });
  assert.deepEqual(shown(module, menu), ['summary…'],
    'a disabled item stays visible so its title can explain why');
  assert.equal(module.menuActiveItem(menu), null, 'but it is never the Enter target');

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

test('the focused filter box points at the keyboard cursor for screen readers', async (t) => {
  const { harness, ...module } = await core(t);
  const { menu, input } = mountMenu(harness);

  module.applyMenuFilter(menu, 'clone', { input });
  const active = module.menuActiveItem(menu);
  assert.ok(active.id, 'an id is minted for the selected item');
  assert.equal(input.getAttribute('aria-activedescendant'), active.id);
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

  assert.equal(escape(true), true, 'a mistyped query is consumed, not the menu');
  assert.equal(cleared, 1);
  assert.equal(stopped, 1, 'and kept from the document handler that would close');

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
  assert.equal(module.menuActiveItem(menu), clone);
  assert.equal(input.getAttribute('aria-activedescendant'), clone.id);

  // A disabled row must not steal the cursor from a runnable one.
  harness.fireEvent(menu.querySelector('[data-act="export-summary"]'), 'mouseover');
  assert.equal(module.menuActiveItem(menu), clone);

  unbind();
  harness.fireEvent(menu.querySelector('[data-act="delete-group"]'), 'mouseover');
  assert.equal(module.menuActiveItem(menu), clone, 'unbinding stops tracking');
});
