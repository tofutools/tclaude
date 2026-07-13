import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness, getByRole } from './preact-harness.mjs';

function section(calls) {
  return {
    key: 'profiles',
    icon: '⚙',
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
