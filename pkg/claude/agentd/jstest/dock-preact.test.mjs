import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness, getByRole } from './preact-harness.mjs';

function section(calls) {
  return {
    key: 'profiles',
    icon: () => '⚙',
    title: () => 'Agent profiles',
    empty: () => 'no profiles yet',
    items: (snapshot) => snapshot?.profiles || [],
    name: (item) => item.name,
    chips: (item) => [{ text: item.model, more: false }],
    drag: true,
    onManageItem: (item) => calls.push(['edit', item.name]),
    onCloneItem: (item) => calls.push(['clone', item.name]),
    onDeleteItem: (item) => calls.push(['delete', item.name]),
    onManageAll: () => calls.push(['manage']),
  };
}

test('Dock keeps keyed cards, disclosure and an open menu stable across snapshot publishes', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createDockState }, { Dock }] = await Promise.all([
    harness.importDashboardModule('js/dock-state.js'),
    harness.importDashboardModule('js/dock-island.js'),
  ]);
  const state = createDockState();
  const calls = [];
  const persisted = [];
  state.publish({ profiles: [{ name: 'review', model: 'sonnet' }] });
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  host.getBoundingClientRect = () => ({ top: 0, bottom: 800 });
  const mounted = await harness.mount(harness.html`
    <${Dock}
      host=${host}
      state=${state}
      sections=${[section(calls)]}
      isSectionOpen=${() => true}
      setSectionOpen=${(key, open) => persisted.push([key, open])}
    />
  `, host);

  const details = host.querySelector('details.dock-section');
  const card = host.querySelector('.dock-card');
  const cog = getByRole(card, 'button', { name: 'Actions for review' });
  assert.equal(details.hasAttribute('open'), true);
  assert.equal(card.hasAttribute('draggable'), true);
  assert.equal(card.dataset.dockKind, 'profiles');
  assert.equal(card.dataset.dockName, 'review');
  assert.equal(details.querySelector('.dock-section-icon').textContent, '⚙', 'the category icon stays in the section heading');
  assert.equal(card.querySelector('.dock-card-icon'), null, 'cards do not repeat the category icon on every row');

  await harness.act(() => harness.fireEvent(cog, 'click'));
  const menu = getByRole(card, 'menu', { name: 'review' });
  assert.ok(menu.classList.contains('open'));
  assert.equal(cog.getAttribute('aria-expanded'), 'true');

  await harness.act(() => state.publish({ profiles: [{ name: 'review', model: 'opus' }] }));
  assert.equal(host.querySelector('.dock-card'), card, 'stable item key preserves the drag source');
  assert.equal(host.querySelector('.dock-card-menu'), menu, 'open menu node survives the poll');
  assert.ok(menu.classList.contains('open'));
  assert.equal(card.querySelector('.dock-chip').textContent, 'opus');

  const edit = getByRole(menu, 'menuitem', { name: 'Edit' });
  edit.focus();
  await harness.act(() => harness.fireEvent(harness.document.body, 'keydown', { key: 'Escape' }));
  await Promise.resolve();
  assert.equal(cog.getAttribute('aria-expanded'), 'false');
  assert.equal(harness.document.activeElement, cog, 'Escape restores focus to the owning cog');

  details.removeAttribute('open');
  await harness.act(() => harness.fireEvent(details, 'toggle'));
  assert.deepEqual(persisted.at(-1), ['profiles', false]);

  await harness.act(() => harness.fireEvent(cog, 'click'));
  await harness.act(() => harness.fireEvent(getByRole(menu, 'menuitem', { name: 'Edit' }), 'click'));
  await harness.act(() => harness.fireEvent(cog, 'click'));
  await harness.act(() => harness.fireEvent(getByRole(menu, 'menuitem', { name: 'Clone' }), 'click'));
  await harness.act(() => harness.fireEvent(cog, 'click'));
  await harness.act(() => harness.fireEvent(getByRole(menu, 'menuitem', { name: 'Delete' }), 'click'));
  await harness.act(() => harness.fireEvent(details.querySelector('.dock-section-manage'), 'click'));
  assert.deepEqual(calls, [
    ['edit', 'review'], ['clone', 'review'], ['delete', 'review'], ['manage'],
  ]);

  await harness.act(() => harness.fireEvent(cog, 'click'));
  assert.ok(menu.classList.contains('open'));
  getByRole(menu, 'menuitem', { name: 'Edit' }).focus();
  await harness.act(() => harness.fireEvent(harness.document.body, 'click'));
  await Promise.resolve();
  assert.equal(cog.getAttribute('aria-expanded'), 'false', 'outside click closes the menu');
  assert.equal(harness.document.activeElement, cog, 'outside click restores focus when it remained in the hidden menu');

  await mounted.unmount();
  host.remove();
});

test('Profile cards show complete details in a non-reflowing tooltip', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createDockState }, { Dock }] = await Promise.all([
    harness.importDashboardModule('js/dock-state.js'),
    harness.importDashboardModule('js/dock-island.js'),
  ]);
  const state = createDockState();
  state.publish({ profiles: [
    { name: 'review', model: 'sonnet' },
    { name: 'foo bar', model: 'sonnet' },
    { name: 'foo_20bar', model: 'sonnet' },
  ] });
  const rich = section([]);
  rich.chips = () => [
    { text: 'aka codex-reviewer', more: false },
    { text: '+3', more: true },
  ];
  rich.fullChips = () => [
    { text: 'aka codex-reviewer', more: false },
    { text: 'aka cold-reviewer', more: false },
    { text: 'sonnet', more: false },
    { text: 'effort high', more: false },
    { text: 'sandbox on', more: false },
  ];
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const mounted = await harness.mount(harness.html`
    <${Dock}
      host=${host}
      state=${state}
      sections=${[rich]}
      isSectionOpen=${() => true}
      setSectionOpen=${() => {}}
    />
  `, host);

  const card = host.querySelector('[data-dock-name="review"]');
  const compact = card.querySelector('.dock-chips-compact');
  const tooltip = getByRole(card, 'region', { name: 'Full details for review' });
  const full = tooltip.querySelector('.dock-chips-full');
  const cog = getByRole(card, 'button', { name: 'Actions for review' });
  assert.ok(card.classList.contains('dock-card-has-details'));
  assert.equal(card.getAttribute('title') || null, null, 'the rich panel replaces the card native tooltip');
  assert.equal(card.querySelector('.dock-grip').getAttribute('title') || null, null,
    'the profile drag grip does not add a second native tooltip');
  assert.equal(cog.getAttribute('title') || null, null,
    'the profile action button does not add a second native tooltip');
  assert.equal(tooltip.classList.contains('open'), false, 'details start hidden without changing card layout');
  const description = tooltip.querySelector('.dock-card-details-description');
  assert.equal(cog.getAttribute('aria-describedby'), description.id);
  assert.equal(tooltip.getAttribute('aria-describedby'), description.id);
  assert.equal(description.textContent.trim(),
    'aka codex-reviewer, aka cold-reviewer, sonnet, effort high, sandbox on');
  assert.equal(tooltip.getAttribute('tabIndex'), '0', 'overflowed details can be keyboard-scrolled');
  assert.equal(tooltip.querySelector('.dock-card-details-name').textContent, 'review');
  const detailIDs = [...host.querySelectorAll('.dock-card-details')].map((details) => details.id);
  assert.equal(new Set(detailIDs).size, detailIDs.length, 'valid profile names cannot collide in ARIA ids');
  assert.deepEqual([...compact.querySelectorAll('.dock-chip')].map((chip) => chip.textContent), [
    'aka codex-reviewer', '+3',
  ]);
  assert.deepEqual([...full.querySelectorAll('.dock-chip')].map((chip) => chip.textContent), [
    'aka codex-reviewer', 'aka cold-reviewer', 'sonnet', 'effort high', 'sandbox on',
  ]);
  assert.equal(full.getAttribute('aria-label'), 'All aliases and settings');
  assert.equal(full.querySelector('.dock-chip-more'), null, 'the tooltip list is never truncated');

  host.getBoundingClientRect = () => ({ top: 0, bottom: 100 });
  card.getBoundingClientRect = () => ({ top: 80, bottom: 100, left: 300, width: 220 });
  tooltip.getBoundingClientRect = () => ({
    height: tooltip.style.width === '100px' ? 90 : 60,
  });
  await harness.act(() => harness.fireEvent(card, 'mouseenter'));
  assert.ok(tooltip.classList.contains('open'), 'hover opens the tooltip');
  assert.equal(tooltip.style.left, '80px');
  assert.equal(tooltip.style.width, '220px');
  assert.equal(tooltip.style.top, '40px', 'the left panel stays within the dock vertical bounds');
  assert.equal(Number.parseFloat(tooltip.style.left) + Number.parseFloat(tooltip.style.width), 300,
    'the details panel ends where the hovered card begins');
  assert.deepEqual([...compact.querySelectorAll('.dock-chip')].map((chip) => chip.textContent), [
    'aka codex-reviewer', '+3',
  ], 'the compact card stays unchanged while its tooltip is open');

  card.getBoundingClientRect = () => ({ top: 80, bottom: 100, left: 108, width: 220 });
  await harness.act(() => harness.fireEvent(harness.window, 'resize'));
  assert.equal(tooltip.style.left, '8px');
  assert.equal(tooltip.style.width, '100px', 'a narrow viewport shrinks the panel before the card column');
  assert.equal(tooltip.style.top, '10px', 'placement is remeasured after the narrower panel wraps taller');
  assert.equal(Number.parseFloat(tooltip.style.left) + Number.parseFloat(tooltip.style.width), 108,
    'the narrowed panel still ends where the hovered card begins');

  card.getBoundingClientRect = () => ({ top: 10, bottom: 30, left: 280, width: 220 });
  await harness.act(() => harness.fireEvent(host, 'scroll'));
  assert.equal(tooltip.style.left, '60px', 'an open tooltip follows horizontal geometry changes');
  assert.equal(tooltip.style.top, '10px', 'an open tooltip follows dock scrolling');

  await harness.act(() => harness.fireEvent(card, 'dragstart'));
  assert.equal(tooltip.classList.contains('open'), false, 'starting a card drag hides its details');
  await harness.act(() => harness.fireEvent(card, 'mouseenter'));
  assert.equal(tooltip.classList.contains('open'), false, 'details stay hidden throughout the drag');
  await harness.act(() => harness.fireEvent(card, 'dragend'));
  assert.ok(tooltip.classList.contains('open'),
    'ending a canceled drag over the same card restores its hover details');

  await harness.act(() => harness.fireEvent(card, 'dragstart'));
  await harness.act(() => harness.fireEvent(card, 'dragleave', {
    relatedTarget: harness.document.body,
  }));
  await harness.act(() => harness.fireEvent(card, 'dragend'));
  assert.equal(tooltip.classList.contains('open'), false,
    'ending a drag away from the card leaves its details hidden');
  await harness.act(() => harness.fireEvent(card, 'mouseenter'));
  assert.ok(tooltip.classList.contains('open'), 'normal hover details resume on the next entry');

  await harness.act(() => harness.fireEvent(card, 'mouseleave'));
  assert.equal(tooltip.classList.contains('open'), false, 'leaving the card closes the tooltip');
  await harness.act(() => harness.fireEvent(card, 'focusin'));
  assert.ok(tooltip.classList.contains('open'), 'keyboard focus opens the same details region');
  await harness.act(() => harness.fireEvent(card, 'focusout', {
    relatedTarget: harness.document.body,
  }));
  assert.equal(tooltip.classList.contains('open'), false, 'focus leaving the card closes the details region');
  await harness.act(() => harness.fireEvent(card, 'mouseenter'));
  await harness.act(() => harness.fireEvent(cog, 'click'));
  assert.equal(tooltip.classList.contains('open'), false, 'the actions menu takes precedence over the tooltip');

  await mounted.unmount();
  host.remove();
});

test('Dock removes its document listeners when the island unmounts', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createDockState }, { Dock }] = await Promise.all([
    harness.importDashboardModule('js/dock-state.js'),
    harness.importDashboardModule('js/dock-island.js'),
  ]);
  const state = createDockState();
  const added = [];
  const removed = [];
  const add = harness.document.addEventListener.bind(harness.document);
  const remove = harness.document.removeEventListener.bind(harness.document);
  harness.document.addEventListener = (type, listener, options) => {
    added.push([type, listener]);
    add(type, listener, options);
  };
  harness.document.removeEventListener = (type, listener, options) => {
    removed.push([type, listener]);
    remove(type, listener, options);
  };
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const mounted = await harness.mount(harness.html`
    <${Dock}
      host=${host}
      state=${state}
      sections=${[section([])]}
      isSectionOpen=${() => true}
      setSectionOpen=${() => {}}
    />
  `, host);

  await mounted.unmount();
  for (const type of ['click', 'keydown']) {
    const installed = added.find(([candidate]) => candidate === type);
    assert.ok(installed, `${type} listener installed`);
    assert.ok(removed.some(([candidate, listener]) =>
      candidate === type && listener === installed[1]), `${type} listener removed`);
  }
  host.remove();
});
