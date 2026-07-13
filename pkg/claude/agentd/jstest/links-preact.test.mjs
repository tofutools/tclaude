import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness, getByRole } from './preact-harness.mjs';

function memoryPrefs(initial = {}) {
  const values = new Map(Object.entries(initial));
  return {
    values,
    getItem: (key) => values.get(key) ?? null,
    setItem: (key, value) => values.set(key, String(value)),
    removeItem: (key) => values.delete(key),
  };
}

const sample = {
  links: [
    { id: 1, from: 'alpha', to: 'beta', mode: 'message', created_at: '2026-07-13T12:00:00Z' },
    { id: 2, from: 'gamma', to: 'alpha', mode: 'full', created_at: '2026-07-13T13:00:00Z' },
  ],
};

test('Links state owns persisted filtering and explicit three-state sorting', async (t) => {
  const harness = await createPreactHarness(t);
  const { createLinksState } = await harness.importDashboardModule('js/links-state.js');
  const prefs = memoryPrefs({ 'tclaude.dash.filter.links': 'alpha' });
  const writes = [];
  const state = createLinksState({
    prefs,
    readSort: () => ({ col: 'id', dir: 'desc' }),
    writeSort: (_table, value) => writes.push(value),
  });
  state.initialize();
  state.publish(sample);

  assert.equal(state.view.value.filtered, 2);
  assert.deepEqual(state.view.value.rows.map((row) => row.id), [2, 1]);
  state.setQuery('gamma');
  assert.equal(state.view.value.filtered, 1);
  assert.equal(prefs.values.get('tclaude.dash.filter.links'), 'gamma');

  state.cycleSort('from');
  state.cycleSort('from');
  state.cycleSort('from');
  assert.deepEqual(writes, [
    { col: 'from', dir: 'asc' },
    { col: 'from', dir: 'desc' },
    null,
  ]);
});

test('Links Preact surface preserves keyed rows, focus and delegated action contracts', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createLinksState }, { mountLinksIsland }] = await Promise.all([
    harness.importDashboardModule('js/links-state.js'),
    harness.importDashboardModule('js/links-island.js'),
  ]);
  const state = createLinksState({
    prefs: memoryPrefs(), readSort: () => null, writeSort: () => {},
  });
  const filterHost = harness.document.body.appendChild(harness.document.createElement('div'));
  const listHost = harness.document.body.appendChild(harness.document.createElement('div'));
  const cleanups = [];
  await harness.act(() => mountLinksIsland({
    filterHost, listHost, state, registerCleanup: (cleanup) => cleanups.push(cleanup),
  }));
  await harness.act(() => state.publish(sample));

  const first = listHost.querySelector('tr[data-key="link-1"]');
  const edit = first.querySelector('[data-act="link-edit"]');
  edit.focus();
  await harness.act(() => state.publish({ links: [sample.links[1], { ...sample.links[0], mode: 'full' }] }));
  assert.equal(listHost.querySelector('tr[data-key="link-1"]'), first);
  assert.equal(harness.document.activeElement, edit);
  assert.equal(edit.dataset.id, '1');
  assert.equal(edit.dataset.from, 'alpha');
  assert.equal(first.querySelector('[data-act="link-delete"]').dataset.group, 'alpha');
  assert.equal(first.querySelector('.id').textContent, '1');

  const filter = getByRole(filterHost, 'textbox', { name: 'Filter inter-group links' });
  await harness.input(filter, 'gamma');
  assert.equal(listHost.querySelectorAll('tbody tr').length, 1);
  assert.equal(filterHost.querySelector('#filter-links-count .theme-copy-regular').textContent, '1 / 2');
  assert.equal(filterHost.querySelector('#filter-links-count .theme-copy-wizard').textContent, '1 / 2');
  const clear = getByRole(filterHost, 'button', { name: 'Clear link filter' });
  await harness.act(() => harness.fireEvent(clear, 'click'));
  assert.equal(harness.document.activeElement, filter);
  assert.equal(listHost.querySelectorAll('tbody tr').length, 2);

  const fromHeader = listHost.querySelector('th[data-sort-col="from"]');
  await harness.act(() => harness.fireEvent(fromHeader, 'click'));
  assert.ok(fromHeader.classList.contains('sort-active'));
  assert.deepEqual([...listHost.querySelectorAll('tbody tr')].map((row) => row.dataset.key),
    ['link-1', 'link-2']);

  for (const cleanup of cleanups.reverse()) cleanup();
  assert.equal(filterHost.childElementCount, 0);
  assert.equal(listHost.childElementCount, 0);
});
