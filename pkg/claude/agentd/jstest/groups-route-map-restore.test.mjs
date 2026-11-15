// The Route map deep link (/groups/routes) is restored at boot, which can be
// before the Groups state has a snapshot: it keeps the map until a snapshot
// says it is off, instead of falling back to Members for good.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

async function setup(t) {
  const harness = await createPreactHarness(t);
  const [{ createGroupsState }, { GroupsRouteMap }] = await Promise.all([
    harness.importDashboardModule('js/groups-state.js'),
    harness.importDashboardModule('js/groups-route-map.js'),
  ]);
  const state = createGroupsState({
    prefs: { getItem: () => null, setItem: () => {}, removeItem: () => {} },
    resetOffsets: () => {}, reorder: (groups) => groups,
    columns: { list: () => [], hidden: () => false, setHidden: () => {}, deviationCount: () => 0 },
  });
  state.initialize();
  await harness.mount(harness.html`<${GroupsRouteMap} state=${state} />`);
  const restore = (location) => harness.act(() => harness.window.document.dispatchEvent(
    new harness.window.CustomEvent('tclaude:restore-location', { detail: { location } })));
  return { harness, state, restore };
}

const snapshot = (enabled) => ({ groups: [{ name: 'team', members: [] }], groups_route_map_enabled: enabled, route_map: { routes: [] } });

test('a Route map link restored before the first snapshot keeps the map once it is on', async (t) => {
  const { harness, state, restore } = await setup(t);
  await restore({ tab: 'groups', subtab: 'routes' });
  assert.equal(state.subview.value, 'routes', 'not known yet: the link holds');
  await harness.act(() => state.publish(snapshot(true)));
  assert.equal(state.subview.value, 'routes', 'the map is on: it stays');
});

test('a Route map link restored before the first snapshot falls back once the map is off', async (t) => {
  const { harness, state, restore } = await setup(t);
  await restore({ tab: 'groups', subtab: 'routes' });
  await harness.act(() => state.publish(snapshot(false)));
  assert.equal(state.subview.value, 'members');
  await restore({ tab: 'groups', subtab: 'routes' });
  assert.equal(state.subview.value, 'members', 'a link while the map is known off lands on Members');
});
