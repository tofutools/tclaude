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
  state.publish(snapshot(GROUPS, { processes_enabled: enabled }));
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

test('the focus switch is hidden while features.processes is off', async (t) => {
  const { root, state } = await setup(t, { enabled: false, prefs: { 'tclaude.dash.groups.focus': 'processes' } });
  assert.equal(root.querySelector('.groups-focus-switch'), null);
  assert.equal(state.focus.value, 'processes', 'the remembered focus survives');
  assert.equal(root.querySelector('[data-groups-processes]'), null, 'but Agents is what shows');
  assert.ok(root.querySelector('details[data-group-key="webshop"]'));
});

test('switching lenses swaps the frame, persists and pushes one entry each', async (t) => {
  const { harness, root, state, store, navigated } = await setup(t);
  const radios = root.querySelector('[role="group"][aria-label="Groups focus"]');
  assert.ok(radios);
  const both = getByRole(radios, 'button', { name: /Both/ });
  const agents = getByRole(radios, 'button', { name: /Agents/ });
  const processes = getByRole(radios, 'button', { name: /Processes/ });
  assert.deepEqual([...radios.querySelectorAll('[data-groups-focus]')].map((b) => b.dataset.groupsFocus), ['both', 'agents', 'processes']);
  assert.equal(both.getAttribute('aria-pressed'), 'true', 'Both is the default');
  assert.ok(root.querySelector('[data-groups-fused]'));
  assert.ok(root.querySelector('details[data-group-key="webshop"]'), 'Both shows the tree while runs load');
  assert.equal(agents.querySelector('.groups-focus-count').textContent, '3', 'distinct agents across shown groups');

  await harness.act(() => harness.fireEvent(processes, 'click'));
  assert.equal(processes.getAttribute('aria-pressed'), 'true');
  assert.equal(state.focus.value, 'processes');
  assert.equal(store.values.get('tclaude.dash.groups.lens'), 'processes');
  assert.deepEqual(navigated, [{ tab: 'groups', subtab: 'processes' }]);
  assert.ok(root.querySelector('[data-groups-processes]'));
  assert.equal(root.querySelector('details[data-group-key="webshop"]'), null);
  assert.ok(harness.document.body.classList.contains('groups-focus-processes'));

  await harness.act(() => harness.fireEvent(processes, 'click'));
  assert.equal(navigated.length, 1, 're-selecting the shown focus adds no entry');

  await harness.act(() => harness.fireEvent(agents, 'click'));
  assert.deepEqual(navigated[1], { tab: 'groups', subtab: 'agents' });
  assert.equal(store.values.get('tclaude.dash.groups.lens'), 'agents');
  assert.ok(root.querySelector('details[data-group-key="webshop"]'));
  assert.equal(root.querySelector('[data-groups-fused]'), null);
  assert.equal(harness.document.body.classList.contains('groups-focus-processes'), false);

  await harness.act(() => harness.fireEvent(both, 'click'));
  assert.deepEqual(navigated[2], { tab: 'groups' });
  assert.ok(root.querySelector('[data-groups-fused] details[data-group-key="webshop"]'));
});

test('the two-lens pref carries over only a remembered Processes', async (t) => {
  const { harness, state } = await setup(t, { prefs: { 'tclaude.dash.groups.focus': 'agents' } });
  assert.equal(state.focus.value, 'both', 'the old default reads as the new one');
  const { restoreGroupsFocus } = await harness.importDashboardModule('js/groups-focus.js');
  assert.equal(restoreGroupsFocus(memoryPrefs({ 'tclaude.dash.groups.focus': 'processes' })), 'processes');
  assert.equal(restoreGroupsFocus(memoryPrefs()), 'both');
  assert.equal(restoreGroupsFocus(memoryPrefs({ 'tclaude.dash.groups.focus': 'processes', 'tclaude.dash.groups.lens': 'agents' })), 'agents',
    'the new pref wins');
});

test('the Processes icon carries the run-kind checklist: running + planned by default, never empty, remembered', async (t) => {
  const { harness, root, state, store } = await setup(t, { prefs: { 'tclaude.dash.groups.processes.filter': 'done' } });
  assert.equal(state.processFilter.value, 'running,planned', 'the old single-select pref does not override the new default');
  const menu = root.querySelector('.groups-focus-runs [role="group"][aria-label="Show processes"]');
  assert.ok(menu, 'the checklist floats off the Processes icon');
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

test('history restores set the focus; Route map restores leave it alone', async (t) => {
  const { harness, root, state, navigated } = await setup(t);
  const restore = (location) => harness.act(() => harness.document.dispatchEvent(
    new harness.window.CustomEvent('tclaude:restore-location', { detail: { location } }),
  ));
  await restore({ tab: 'groups', subtab: 'processes' });
  assert.equal(state.focus.value, 'processes');
  assert.ok(root.querySelector('[data-groups-processes]'));
  await restore({ tab: 'groups', subtab: 'routes' });
  assert.equal(state.focus.value, 'processes');
  await restore({ tab: 'terminals' });
  assert.equal(state.focus.value, 'processes');
  await restore({ tab: 'groups', subtab: 'agents' });
  assert.equal(state.focus.value, 'agents');
  await restore({ tab: 'groups' });
  assert.equal(state.focus.value, 'both');
  assert.ok(root.querySelector('[data-groups-fused]'));
  assert.deepEqual(navigated, [], 'restores never announce a navigation');
});

test('the wizard skin renames the lenses', async (t) => {
  const { harness, root } = await setup(t);
  await harness.act(() => harness.document.dispatchEvent(new harness.window.CustomEvent('tclaude:wizard', { detail: { active: true } })));
  const radios = root.querySelector('[role="group"][aria-label="Party lens"]');
  assert.ok(getByRole(radios, 'button', { name: /^Familiars/ }));
  assert.ok(getByRole(radios, 'button', { name: /Whole party/ }));
  assert.ok(getByRole(radios, 'button', { name: /Rites/ }));
});

test('the palette offers every lens while processes are on and switches through the Groups state', async (t) => {
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
  const navigated = [];
  harness.document.addEventListener('tclaude:navigated', (event) => navigated.push(event.detail.location));
  const focusCommands = () => buildCommands({ groups: [], agents: [] }).filter((c) => /^groups focus /.test(c.keywords || ''));

  harness.document.body.classList.add('hide-processes');
  assert.equal(focusCommands().length, 0, 'no focus commands while processes are off');
  harness.document.body.classList.remove('hide-processes');
  const commands = focusCommands();
  assert.deepEqual(commands.map((c) => c.label), ['Groups › Both focus', 'Groups › Processes focus', 'Groups › Agents focus']);
  commands[1].run();
  assert.equal(state.focus.value, 'processes');
  assert.deepEqual(navigated, [{ tab: 'groups', subtab: 'processes' }]);
});

test('choosing a lens leaves the Route map, and its Members button announces the remembered lens', async (t) => {
  const { harness, state, navigated } = await setup(t, { prefs: { 'tclaude.dash.groups.focus': 'processes' } });
  state.publish({ ...state.snapshot.value, groups_route_map_enabled: true, route_map: { routes: [] } });
  const { GroupsRouteMap } = await harness.importDashboardModule('js/groups-route-map.js');
  const mapHost = harness.document.body.appendChild(harness.document.createElement('div'));
  t.after(() => mapHost.remove());
  const map = await harness.mount(harness.html`<${GroupsRouteMap} state=${state} />`, mapHost);
  await harness.act(() => harness.fireEvent(getByRole(map.container, 'tab', { name: 'Route map' }), 'click'));
  assert.equal(state.subview.value, 'routes');
  await harness.act(() => harness.fireEvent(getByRole(map.container, 'tab', { name: 'Members' }), 'click'));
  assert.deepEqual(navigated.at(-1), { tab: 'groups', subtab: 'processes' }, 'Members shows (and names) the Processes lens');

  await harness.act(() => harness.fireEvent(getByRole(map.container, 'tab', { name: 'Route map' }), 'click'));
  const { chooseGroupsFocus } = await harness.importDashboardModule('js/groups-focus.js');
  navigated.length = 0;
  await harness.act(() => { chooseGroupsFocus(state, 'processes'); });
  assert.equal(state.subview.value, 'members', 'the lens choice leaves the Route map even when the focus is unchanged');
  assert.deepEqual(navigated, [{ tab: 'groups', subtab: 'processes' }]);
});

test('the single-run focus has its own URL: opening, closing and restoring it', async (t) => {
  const { harness, state, navigated } = await setup(t, { prefs: { 'tclaude.dash.groups.lens': 'agents' } });
  const { focusRun, chooseGroupsFocus } = await harness.importDashboardModule('js/groups-focus.js');
  const run = { tab: 'groups', subtab: 'runs', selection: 'infra', item: 'run_1' };
  await harness.act(() => { chooseGroupsFocus(state, 'processes'); });
  navigated.length = 0;
  await harness.act(() => { focusRun(state, { group: 'infra', runId: 'run_1' }); });
  assert.deepEqual(navigated, [run]);
  // Switching to Both keeps the run open, so the URL still names it.
  await harness.act(() => { chooseGroupsFocus(state, 'both'); });
  assert.deepEqual(navigated.at(-1), run);
  await harness.act(() => { focusRun(state, null); });
  assert.deepEqual(navigated.at(-1), { tab: 'groups' });
  // Restores: a run from the Agents lens opens in Processes; a lens closes it.
  const restore = (location) => harness.act(() => harness.document.dispatchEvent(
    new harness.window.CustomEvent('tclaude:restore-location', { detail: { location } }),
  ));
  state.setFocus('agents');
  navigated.length = 0;
  await restore(run);
  assert.equal(state.focus.value, 'processes');
  assert.deepEqual(state.focusedRun.value, { group: 'infra', runId: 'run_1' });
  await restore({ tab: 'groups', subtab: 'processes' });
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
  const select = map.container.querySelector('select');
  await harness.act(() => {
    select.querySelector('option[value="infra"]').selected = true;
    select.dispatchEvent(new harness.window.Event('change', { bubbles: true }));
  });
  assert.deepEqual(navigated.at(-1), { tab: 'groups', subtab: 'routes', params: { group: 'infra' } });
  await harness.act(() => harness.document.dispatchEvent(new harness.window.CustomEvent('tclaude:restore-location', {
    detail: { location: { tab: 'groups', subtab: 'routes', params: { group: 'webshop' } } },
  })));
  assert.equal(state.routeGroup.value, 'webshop');
  assert.equal(map.container.querySelector('select').getAttribute('value'), 'webshop');
});
