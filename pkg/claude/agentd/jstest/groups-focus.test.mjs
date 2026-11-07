import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness, getByRole } from './preact-harness.mjs';
import { comboPick } from './combobox-helpers.mjs';

function memoryPrefs(initial = {}) {
  const values = new Map(Object.entries(initial));
  return {
    values,
    getItem: (key) => values.get(key) ?? null,
    setItem: (key, value) => values.set(key, String(value)),
    removeItem: (key) => values.delete(key),
  };
}

function snapshot(groups, extra = {}) {
  return {
    groups, pending: [], ungrouped: [], retired: [], conversations: [], replaced: [], paging: {},
    activity_bots: { regular: 'emoji', slop: 'off', wizard: 'off' },
    ...extra,
  };
}

const GROUPS = [
  { name: 'webshop', members: [{ conv_id: 'c1', title: 'checkout-ui' }, { conv_id: 'c2', title: 'search-indexer' }] },
  { name: 'infra', members: [{ conv_id: 'c2', title: 'search-indexer' }, { conv_id: 'c3', title: 'k8s-upgrader' }] },
];

async function setup(t, { enabled = true, prefs = {} } = {}) {
  const harness = await createPreactHarness(t);
  const [{ createGroupsState }, island] = await Promise.all([
    harness.importDashboardModule('js/groups-state.js'),
    harness.importDashboardModule('js/groups-island.js'),
  ]);
  const store = memoryPrefs({ 'tclaude.dash.ungrouped.groups': '0', 'tclaude.dash.retired.groups': '0', ...prefs });
  const state = createGroupsState({
    prefs: store, resetOffsets: () => {},
    columns: { list: () => [], hidden: () => false, setHidden: () => {}, deviationCount: () => 0 },
    reorder: (groups) => groups,
  });
  state.initialize();
  // A fresh snapshot carries no processes flag: this node always has them.
  // A peer that does not share them is the one place the switch goes.
  state.publish(snapshot(GROUPS, enabled ? {} : { peer_view: { level: 'restricted' } }));
  const navigated = [];
  const onNavigated = (event) => navigated.push(event.detail.location);
  harness.document.addEventListener('tclaude:navigated', onNavigated);
  t.after(() => harness.document.removeEventListener('tclaude:navigated', onNavigated));
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  t.after(() => host.remove());
  const mounted = await harness.mount(harness.html`<div>
    <${island.GroupsControls} state=${state} actions=${{ refresh: () => {} }} />
    <${island.GroupsList} host=${host} state=${state} actions=${{ sort: () => {}, page: () => {}, setPageSize: () => {} }} />
  </div>`, host);
  return { harness, state, store, navigated, mounted, root: mounted.container };
}

test('a peer without processes shows the member tables, and no run filter', async (t) => {
  const { harness, root, state } = await setup(t, { enabled: false });
  await harness.act(() => { state.viewOpen.value = true; });
  assert.equal(root.querySelector('.groups-focus-switch'), null);
  assert.equal(root.querySelector('[data-groups-fused]'), null);
  assert.ok(root.querySelector('details[data-group-key="webshop"]'));
  assert.equal(root.querySelector('[data-process-filter]'), null);
});

test('the fused view is the only view: no lens switch, and the tree shows while runs load', async (t) => {
  const { root } = await setup(t);
  assert.equal(root.querySelector('.groups-focus-switch'), null, 'no view switch');
  assert.equal(root.querySelector('[data-groups-focus]'), null);
  assert.ok(root.querySelector('[data-groups-fused] details[data-group-key="webshop"]'));
});

test('the retired lens prefs are cleared on read', async (t) => {
  const { store, root } = await setup(t, { prefs: { 'tclaude.dash.groups.lens': 'processes', 'tclaude.dash.groups.focus': 'agents' } });
  assert.equal(store.values.has('tclaude.dash.groups.lens'), false);
  assert.equal(store.values.has('tclaude.dash.groups.focus'), false);
  assert.ok(root.querySelector('[data-groups-fused]'), 'a remembered lens no longer picks the view');
});

test('the ▾ view menu carries the run-kind checklist: running + planned by default, never empty, remembered', async (t) => {
  const { harness, root, state, store } = await setup(t, { prefs: { 'tclaude.dash.groups.processes.filter': 'done' } });
  assert.equal(state.processFilter.value, 'running,planned', 'the old single-select pref does not override the new default');
  await harness.act(() => { state.viewOpen.value = true; });
  const menu = root.querySelector('#filter-groups-view-menu [role="group"][aria-label="Show processes"]');
  assert.ok(menu, 'the checklist is a section of the view menu');
  const box = (kind) => menu.querySelector(`input[data-process-filter="${kind}"]`);
  const ticked = (kind) => box(kind).hasAttribute('checked');
  assert.deepEqual(['running', 'planned', 'done'].map(ticked), [true, true, false]);

  await harness.act(() => harness.fireEvent(box('done'), 'change'));
  assert.equal(state.processFilter.value, 'running,planned,done');
  assert.equal(store.values.get('tclaude.dash.groups.processes.kinds'), 'running,planned,done');
  await harness.act(() => harness.fireEvent(box('running'), 'change'));
  await harness.act(() => harness.fireEvent(box('planned'), 'change'));
  assert.equal(state.processFilter.value, 'done');
  // As a browser does: the click clears the box before `change` fires.
  box('done').checked = false;
  await harness.act(() => harness.fireEvent(box('done'), 'change'));
  assert.equal(state.processFilter.value, 'done', 'unticking the last kind is refused');
  assert.equal(box('done').checked, true, 'the refused box is ticked again');
});

test('the wizard skin renames the run filter', async (t) => {
  const { harness, root, state } = await setup(t);
  await harness.act(() => harness.document.dispatchEvent(new harness.window.CustomEvent('tclaude:wizard', { detail: { active: true } })));
  await harness.act(() => { state.viewOpen.value = true; });
  assert.ok(root.querySelector('#filter-groups-view-menu [role="group"][aria-label="Show rites"]'));
});

test('the palette offers no lens commands', async (t) => {
  const harness = await createPreactHarness(t);
  await harness.replaceDashboardModule('js/dashboard.js', `export let lastSnapshot = null; export function setLastSnapshot(v) { lastSnapshot=v; } export function webTerminalDefault() { return false; }`);
  harness.document.body.innerHTML = '<nav><button data-tab="groups" class="active"></button><button data-tab="processes"></button></nav>';
  const [{ buildCommands }, { registerFeatureState }, { createGroupsState }] = await Promise.all([
    harness.importDashboardModule('js/palette.js'),
    harness.importDashboardModule('js/feature-state-registry.js'),
    harness.importDashboardModule('js/groups-state.js'),
  ]);
  const state = createGroupsState({ prefs: memoryPrefs(), resetOffsets: () => {}, reorder: (groups) => groups,
    columns: { list: () => [], hidden: () => false, setHidden: () => {}, deviationCount: () => 0 } });
  t.after(registerFeatureState('groups', state));
  const commands = buildCommands({ groups: [], agents: [] });
  assert.equal(commands.filter((c) => /^groups focus /.test(c.keywords || '') || /focus$/.test(c.label || '') && /^Groups ›/.test(c.label || '')).length, 0);
});

test('the Route map Members button announces bare Groups, and showGroupRuns leaves the map', async (t) => {
  const { harness, state, navigated } = await setup(t);
  state.publish({ ...state.snapshot.value, groups_route_map_enabled: true, route_map: { routes: [] } });
  const { GroupsRouteMap } = await harness.importDashboardModule('js/groups-route-map.js');
  const mapHost = harness.document.body.appendChild(harness.document.createElement('div'));
  t.after(() => mapHost.remove());
  const map = await harness.mount(harness.html`<${GroupsRouteMap} state=${state} />`, mapHost);
  await harness.act(() => harness.fireEvent(getByRole(map.container, 'tab', { name: 'Route map' }), 'click'));
  assert.equal(state.subview.value, 'routes');
  await harness.act(() => harness.fireEvent(getByRole(map.container, 'tab', { name: 'Members' }), 'click'));
  assert.deepEqual(navigated.at(-1), { tab: 'groups' });

  await harness.act(() => harness.fireEvent(getByRole(map.container, 'tab', { name: 'Route map' }), 'click'));
  const { showGroupRuns } = await harness.importDashboardModule('js/groups-focus.js');
  navigated.length = 0;
  await harness.act(() => { showGroupRuns(state, { announce: true }); });
  assert.equal(state.subview.value, 'members');
  assert.deepEqual(navigated, [{ tab: 'groups' }]);
  await harness.act(() => { showGroupRuns(state, { announce: true }); });
  assert.equal(navigated.length, 1, 'off the Route map it is a no-op');
});

test('the single-run focus has its own URL: opening, closing and restoring it', async (t) => {
  const { harness, state, navigated } = await setup(t);
  const { focusRun } = await harness.importDashboardModule('js/groups-focus.js');
  const run = { tab: 'groups', subtab: 'runs', selection: 'infra', item: 'run_1' };
  await harness.act(() => { focusRun(state, { group: 'infra', runId: 'run_1' }); });
  assert.deepEqual(navigated, [run]);
  await harness.act(() => { focusRun(state, { group: 'infra', runId: 'run_1' }); });
  assert.equal(navigated.length, 1, 're-opening the shown run adds no entry');
  await harness.act(() => { focusRun(state, null); });
  assert.deepEqual(navigated.at(-1), { tab: 'groups' });
  const restore = (location) => harness.act(() => harness.document.dispatchEvent(
    new harness.window.CustomEvent('tclaude:restore-location', { detail: { location } }),
  ));
  navigated.length = 0;
  state.setSubview('routes', '');
  await restore(run);
  assert.equal(state.subview.value, 'members', 'a run restore leaves the Route map');
  assert.deepEqual(state.focusedRun.value, { group: 'infra', runId: 'run_1' });
  await restore({ tab: 'groups', subtab: 'routes' });
  assert.deepEqual(state.focusedRun.value, { group: 'infra', runId: 'run_1' }, 'a Route map restore leaves the run alone');
  await restore({ tab: 'groups' });
  assert.equal(state.focusedRun.value, null);
  assert.deepEqual(navigated, [], 'restores never announce a navigation');
});

test('the Route map group is in the URL', async (t) => {
  const { harness, state, navigated } = await setup(t);
  state.publish({ ...state.snapshot.value, groups_route_map_enabled: true, route_map: { routes: [] } });
  const { GroupsRouteMap } = await harness.importDashboardModule('js/groups-route-map.js');
  const mapHost = harness.document.body.appendChild(harness.document.createElement('div'));
  t.after(() => mapHost.remove());
  const map = await harness.mount(harness.html`<${GroupsRouteMap} state=${state} />`, mapHost);
  await harness.act(() => harness.fireEvent(getByRole(map.container, 'tab', { name: 'Route map' }), 'click'));
  await comboPick(harness, map.container.querySelector('#route-map-group'), 'infra');
  assert.deepEqual(navigated.at(-1), { tab: 'groups', subtab: 'routes', params: { group: 'infra' } });
  await harness.act(() => harness.document.dispatchEvent(new harness.window.CustomEvent('tclaude:restore-location', {
    detail: { location: { tab: 'groups', subtab: 'routes', params: { group: 'webshop' } } },
  })));
  assert.equal(state.routeGroup.value, 'webshop');
  // The exact list view is in the URL too.
  await harness.act(() => harness.fireEvent(getByRole(map.container, 'button', { name: 'Exact list' }), 'click'));
  assert.deepEqual(navigated.at(-1), { tab: 'groups', subtab: 'routes', params: { group: 'webshop', view: 'list' } });
  await harness.act(() => harness.document.dispatchEvent(new harness.window.CustomEvent('tclaude:restore-location', {
    detail: { location: { tab: 'groups', subtab: 'routes' } },
  })));
  assert.equal(state.routeView.value, 'graph');
  assert.equal(map.container.querySelector('#route-map-group').dataset.value, 'webshop');
});
