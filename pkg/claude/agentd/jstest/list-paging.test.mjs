import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

function memoryPrefs(initial = {}) {
  const values = new Map(Object.entries(initial));
  return {
    getItem: (key) => values.get(key) ?? null,
    setItem: (key, value) => values.set(key, String(value)),
    removeItem: (key) => values.delete(key),
  };
}

function stateDependencies() {
  return {
    resetOffsets: () => {},
    columns: {
      list: () => [],
      hidden: () => false,
      setHidden: () => {},
      deviationCount: () => 0,
    },
    reorder: (groups) => groups,
  };
}

test('retired roster polling follows default, live and persisted visibility', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ createGroupsState }, { fetchVisibleGroupListPages }] = await Promise.all([
    harness.importDashboardModule('js/groups-state.js'),
    harness.importDashboardModule('js/list-paging.js'),
  ]);
  const requested = [];
  const get = (path) => {
    requested.push(path);
    return Promise.resolve(path);
  };

  const fresh = createGroupsState({ prefs: memoryPrefs(), ...stateDependencies() });
  fresh.initialize();
  await Promise.all(fetchVisibleGroupListPages(fresh, true, '', get));
  assert.deepEqual(requested, [], 'default-hidden lists make no polling requests');

  fresh.setVisible('retired', true);
  await Promise.all(fetchVisibleGroupListPages(fresh, true, 'old agent', get));
  assert.deepEqual(requested, [
    '/api/retired?offset=0&limit=50&q=old%20agent',
  ], 'checking show retired starts its windowed roster poll');

  requested.length = 0;
  const restored = createGroupsState({
    prefs: memoryPrefs({ 'tclaude.dash.retired.groups': '1' }),
    ...stateDependencies(),
  });
  restored.initialize();
  await Promise.all(fetchVisibleGroupListPages(restored, true, '', get));
  assert.deepEqual(requested, [
    '/api/retired?offset=0&limit=50',
  ], 'a persisted enabled preference resumes retired polling');

  requested.length = 0;
  await Promise.all(fetchVisibleGroupListPages(restored, false, '', get));
  assert.deepEqual(requested, [], 'leaving the Groups tab stops retired polling');
});
