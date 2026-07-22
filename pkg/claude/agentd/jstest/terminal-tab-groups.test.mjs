import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness, getByRole } from './preact-harness.mjs';

function memoryPrefs(initial = {}) {
  const values = new Map(Object.entries(initial));
  return {
    values,
    getItem(key) { return values.get(key) ?? null; },
    setItem(key, value) { values.set(key, String(value)); },
    removeItem(key) { values.delete(key); },
  };
}

const keysOf = (state) => state.panes.value.map((pane) => pane.key);
const stripOf = (state) => state.segments.value.map((segment) => (segment.type === 'group'
  ? `${segment.group.name}[${segment.panes.map((pane) => pane.key).join(',')}]`
  : segment.pane.key));

async function openState(harness, prefs) {
  const { createTerminalShellState } = await harness.importDashboardModule('js/terminal-shell-state.js');
  const state = createTerminalShellState({ prefs });
  for (const key of ['a', 'b', 'c', 'd']) {
    state.openPane({ ws: `/${key}`, key, label: key, agent: `agt_${key}` });
  }
  return state;
}

test('a tab group is a contiguous run that survives joins, leaves, and dissolution', async (t) => {
  const harness = await createPreactHarness(t);
  const state = await openState(harness, memoryPrefs());

  assert.deepEqual(stripOf(state), ['a', 'b', 'c', 'd']);
  const work = state.createGroup({ name: '  TCL-618  ', keys: ['b', 'd'] });
  assert.equal(work.name, 'TCL-618', 'group names are trimmed on the way in');
  assert.deepEqual(keysOf(state), ['a', 'b', 'd', 'c'],
    'a new group pulls its members next to the first of them, disturbing nothing else');
  assert.deepEqual(stripOf(state), ['a', 'TCL-618[b,d]', 'c']);

  assert.equal(state.createGroup({ name: 'empty', keys: [] }), null,
    'a group with no members has nothing to render or drop onto and is never created');
  assert.equal(state.createGroup({ name: 'unknown', keys: ['gone'] }), null);

  // Dropping a tab ON another tab adopts that tab's membership: this is the
  // whole join/leave gesture, with no separate drop zone.
  state.reorderPane('a', 'd', { after: true });
  assert.deepEqual(stripOf(state), ['TCL-618[b,d,a]', 'c'], 'a drop inside the stack joins it');
  state.reorderPane('a', 'c', { after: false });
  assert.deepEqual(stripOf(state), ['TCL-618[b,d]', 'a', 'c'], 'a drop on an ungrouped tab leaves the stack');

  state.assignPaneToGroup('c', work.id);
  assert.deepEqual(stripOf(state), ['TCL-618[b,d,c]', 'a'], 'joining by command lands at the end of the stack');
  assert.deepEqual(state.groupMembers(work.id).map((pane) => pane.key), ['b', 'd', 'c']);
  assert.equal(state.groupFor('c').id, work.id);
  assert.equal(state.groupFor('a'), null);

  assert.equal(state.dissolveGroup(work.id), true);
  assert.deepEqual(stripOf(state), ['b', 'd', 'c', 'a'],
    'ungrouping keeps the terminals and their order, and only drops the stack');
  assert.equal(state.dissolveGroup(work.id), false, 'a dissolved stack cannot be dissolved twice');
  assert.deepEqual(state.groups.value, []);
});

test('keyboard tab movement steps inside a group, out at its edges, and hops whole groups', async (t) => {
  const harness = await createPreactHarness(t);
  const state = await openState(harness, memoryPrefs());
  const group = state.createGroup({ name: 'infra', keys: ['b', 'c'] });
  assert.deepEqual(stripOf(state), ['a', 'infra[b,c]', 'd']);

  assert.equal(state.movePaneByOffset('b', 1).group.id, group.id);
  assert.deepEqual(stripOf(state), ['a', 'infra[c,b]', 'd'],
    'a step inside the stack reorders siblings and stays in the stack');

  const left = state.movePaneByOffset('b', 1);
  assert.equal(left.group, null);
  assert.equal(left.leftGroup.id, group.id, 'a step past the stack edge leaves the stack');
  assert.deepEqual(stripOf(state), ['a', 'infra[c]', 'b', 'd']);

  // An ungrouped tab hops the whole stack rather than tunnelling into it: the
  // contiguity normalization would undo a landing inside on the next keypress.
  assert.ok(state.movePaneByOffset('b', -1));
  assert.deepEqual(stripOf(state), ['a', 'b', 'infra[c]', 'd']);
  assert.ok(state.movePaneByOffset('b', -1));
  assert.deepEqual(stripOf(state), ['b', 'a', 'infra[c]', 'd']);
  assert.equal(state.movePaneByOffset('b', -1), null, 'the first position is the end of the strip');

  // The last member stepping out takes the memberless stack with it.
  assert.ok(state.movePaneByOffset('c', 1));
  assert.deepEqual(stripOf(state), ['b', 'a', 'c', 'd']);
  assert.deepEqual(state.groups.value, [], 'a stack with no members left is dropped');
});

test('collapsing a group never hides the terminal the operator is looking at', async (t) => {
  const harness = await createPreactHarness(t);
  const state = await openState(harness, memoryPrefs());
  const group = state.createGroup({ name: 'infra', keys: ['b', 'c'] });

  state.activatePane('c');
  assert.equal(state.activeKey.value, 'c');
  state.setGroupCollapsed(group.id, true);
  assert.equal(state.groups.value[0].collapsed, true);
  assert.equal(state.activeKey.value, 'd',
    'collapsing over the active terminal moves activation to the nearest tab outside the stack');

  // Activating a member of a collapsed stack has to show it again, otherwise
  // the active terminal would have no visible tab.
  state.activatePane('b');
  assert.equal(state.groups.value[0].collapsed, false);

  state.setGroupCollapsed(group.id, true);
  assert.equal(state.activeKey.value, 'd');
  // With nothing outside the stack there is nowhere for activation to go, so
  // the active member stays active — the island keeps its tab visible.
  state.removePanes(['a', 'd']);
  state.activatePane('b');
  state.setGroupCollapsed(group.id, true);
  assert.equal(state.activeKey.value, 'b');
  assert.equal(state.groups.value[0].collapsed, true);

  assert.equal(state.toggleGroupCollapsed(group.id).collapsed, false);
  assert.equal(state.toggleGroupCollapsed('missing'), null);
  assert.equal(state.renameGroup(group.id, '   ').name, 'infra', 'an empty rename keeps the old name');
  assert.equal(state.renameGroup(group.id, 'x'.repeat(80)).name, 'x'.repeat(40));
  assert.equal(state.setGroupColor(group.id, 'purple').color, 'purple');
  assert.equal(state.setGroupColor(group.id, 'chartreuse').color, 'blue',
    'an unknown colour falls back to a palette slot instead of arbitrary CSS');
});

test('tab groups persist, restore, and are bounded like the pane order they ride alongside', async (t) => {
  const harness = await createPreactHarness(t);
  const { createTerminalShellState, TERMINAL_TAB_GROUP_KEY, MAX_TERMINAL_TAB_GROUPS } =
    await harness.importDashboardModule('js/terminal-shell-state.js');
  const prefs = memoryPrefs();
  const state = await openState(harness, prefs);
  const group = state.createGroup({ name: 'infra', keys: ['b', 'c'] });

  const stored = JSON.parse(prefs.getItem(TERMINAL_TAB_GROUP_KEY));
  assert.deepEqual(stored.groups, [{ id: group.id, name: 'infra', color: 'blue', collapsed: false }]);
  assert.deepEqual(stored.members, { b: group.id, c: group.id });

  // Membership outlives the terminals themselves, exactly like the pane order:
  // closing every tab of a stack and reopening one restores it to its stack.
  state.removePanes(['b', 'c']);
  assert.deepEqual(JSON.parse(prefs.getItem(TERMINAL_TAB_GROUP_KEY)).members, { b: group.id, c: group.id });

  const restored = createTerminalShellState({ prefs });
  for (const key of ['a', 'c']) restored.openPane({ ws: `/${key}`, key, label: key });
  assert.deepEqual(stripOf(restored), ['a', 'infra[c]'], 'a reopened tab returns to its stack');
  assert.equal(restored.groups.value[0].id, group.id);

  const hostile = createTerminalShellState({
    prefs: memoryPrefs({
      'tclaude.dash.terminals.groups': JSON.stringify({
        groups: [
          { id: 'group-1', name: 'okbell', color: 'not-a-colour' },
          { id: 'group-1', name: 'duplicate id' },
          { name: 'no id' },
          ...Array.from({ length: 40 }, (_, index) => ({ id: `flood-${index}`, name: `flood ${index}` })),
        ],
        members: { a: 'group-1', b: 'nonexistent', 7: 'group-1' },
      }),
    }),
  });
  hostile.openPane({ ws: '/a', key: 'a', label: 'a' });
  hostile.openPane({ ws: '/b', key: 'b', label: 'b' });
  assert.equal(hostile.groups.value.length, MAX_TERMINAL_TAB_GROUPS, 'the stored group count is capped');
  assert.equal(hostile.groups.value[0].name, 'ok bell', 'control characters never reach the strip');
  assert.equal(hostile.groups.value[0].color, 'blue');
  assert.deepEqual(hostile.groups.value.map((entry) => entry.id).filter((id) => id === 'group-1'), ['group-1'],
    'a duplicate id is dropped rather than shadowing the first');
  assert.equal(hostile.groupFor('a').id, 'group-1');
  assert.equal(hostile.groupFor('b'), null, 'membership pointing at no group is ungrouped');
});

test('the strip renders group stacks with a collapsing pill, a join drop target, and group actions', async (t) => {
  const harness = await createPreactHarness(t);
  const { createTerminalShellState } = await harness.importDashboardModule('js/terminal-shell-state.js');
  const { createTerminalShellActions } = await harness.importDashboardModule('js/terminal-shell-actions.js');
  const { TerminalTabs } = await harness.importDashboardModule('js/terminal-shell-island.js');
  const state = createTerminalShellState({ prefs: memoryPrefs(), persistOrder: false });
  const actions = createTerminalShellActions({ state, fetchImpl: async () => ({ ok: true }) });
  const widgetFactory = () => ({
    connect: async () => true,
    fit() {}, focus() {}, sendResize() {}, copy: async () => {},
    setActive() {}, status: () => 'connected', reconnectAvailable: () => false,
    isDisposed: () => false, dispose() {},
  });

  const { container } = await harness.mount(
    harness.html`<${TerminalTabs} state=${state} actions=${actions} widgetFactory=${widgetFactory} />`,
  );
  for (const key of ['a', 'b', 'c']) {
    await harness.act(() => state.openPane({ ws: `/${key}`, key, label: key }));
  }
  const group = state.createGroup({ name: 'infra', keys: ['a', 'b'] });
  await harness.act(() => {});

  const stack = getByRole(container, 'group', { name: 'infra — 2 terminals' });
  const pill = getByRole(stack, 'button', { name: 'infra, 2 terminals' });
  assert.equal(pill.getAttribute('aria-expanded'), 'true');
  assert.equal(stack.querySelector('.mux-group-count').textContent, '2');
  assert.equal(container.querySelector('.mux-tab-group').classList.contains('mux-group-blue'), true);
  assert.deepEqual([...stack.querySelectorAll('.mux-tab-label')].map((label) => label.textContent), ['a', 'b']);

  // Collapsing hands the active terminal to the tab outside the stack, so the
  // collapsed stack renders as the pill alone.
  await harness.act(() => harness.fireEvent(pill, 'click'));
  assert.equal(state.activeKey.value, 'c');
  assert.equal(container.querySelector('.mux-tab-group').classList.contains('collapsed'), true);
  assert.deepEqual([...container.querySelectorAll('.mux-tab-group .mux-tab-label')], []);
  await harness.act(() => harness.fireEvent(
    getByRole(container, 'button', { name: 'infra, 2 terminals' }), 'click',
  ));
  assert.equal(container.querySelectorAll('.mux-tab-group .mux-tab-label').length, 2);

  // Dropping a tab on the stack joins it at the end.
  const transfer = {
    data: {}, effectAllowed: '', dropEffect: '',
    setData(type, value) { this.data[type] = value; },
    getData(type) { return this.data[type] || ''; },
  };
  const tabFor = (name) => [...container.querySelectorAll('[role="tab"]')]
    .find((tab) => tab.querySelector('.mux-tab-label').textContent === name);
  await harness.act(() => harness.fireEvent(tabFor('c').querySelector('.mux-tab-label'), 'dragstart', {
    dataTransfer: transfer,
  }));
  await harness.act(() => harness.fireEvent(container.querySelector('.mux-tab-group'), 'dragover', {
    dataTransfer: transfer,
  }));
  assert.equal(container.querySelector('.mux-tab-group').classList.contains('drop-into'), true,
    'the stack says it will accept the tab before the release');
  await harness.act(() => harness.fireEvent(container.querySelector('.mux-tab-group'), 'drop', {
    dataTransfer: transfer,
  }));
  assert.deepEqual(state.groupMembers(group.id).map((pane) => pane.key), ['a', 'b', 'c']);
  assert.match(container.querySelector('[role="status"]').textContent, /Moved c into group infra\./);
  assert.equal(container.querySelector('.mux-tab-group').classList.contains('drop-into'), false);

  // The stack's own context menu carries the commands drag cannot express.
  await harness.act(() => harness.fireEvent(
    getByRole(container, 'button', { name: 'infra, 3 terminals' }), 'contextmenu', { clientX: 8, clientY: 12 },
  ));
  const menu = getByRole(container, 'menu', { name: 'Actions for infra' });
  assert.deepEqual([...menu.querySelectorAll('[role="menuitem"]')].map((item) => item.textContent),
    ['Collapse group', 'Rename group…', 'Ungroup tabs', 'Close tabs in group']);

  await harness.act(() => harness.fireEvent(getByRole(menu, 'menuitem', { name: 'Rename group…' }), 'click'));
  const rename = container.querySelector('.mux-group-rename');
  assert.equal(harness.document.activeElement, rename, 'renaming focuses its input instead of the strip');
  rename.value = 'platform';
  await harness.act(() => harness.fireEvent(rename, 'keydown', { key: 'Enter' }));
  assert.equal(state.groups.value[0].name, 'platform');
  assert.equal(container.querySelector('.mux-group-name').textContent, 'platform');

  await harness.act(() => harness.fireEvent(
    getByRole(container, 'button', { name: 'platform, 3 terminals' }), 'contextmenu', { clientX: 8, clientY: 12 },
  ));
  await harness.act(() => harness.fireEvent(
    getByRole(container, 'menuitem', { name: 'Ungroup tabs' }), 'click',
  ));
  assert.equal(container.querySelector('.mux-tab-group'), null);
  assert.equal(container.querySelectorAll('[role="tab"]').length, 3, 'ungrouping keeps every terminal');
});

test('a tab context menu offers the grouping commands drag-and-drop cannot reach from the keyboard', async (t) => {
  const harness = await createPreactHarness(t);
  const { createTerminalShellState } = await harness.importDashboardModule('js/terminal-shell-state.js');
  const { createTerminalShellActions } = await harness.importDashboardModule('js/terminal-shell-actions.js');
  const { TerminalTabs } = await harness.importDashboardModule('js/terminal-shell-island.js');
  const state = createTerminalShellState({ prefs: memoryPrefs(), persistOrder: false });
  const actions = createTerminalShellActions({ state, fetchImpl: async () => ({ ok: true }) });
  const widgetFactory = () => ({
    connect: async () => true,
    fit() {}, focus() {}, sendResize() {}, copy: async () => {},
    setActive() {}, status: () => 'connected', reconnectAvailable: () => false,
    isDisposed: () => false, dispose() {},
  });
  const { container } = await harness.mount(
    harness.html`<${TerminalTabs} state=${state} actions=${actions} widgetFactory=${widgetFactory} />`,
  );
  for (const key of ['a', 'b']) {
    await harness.act(() => state.openPane({ ws: `/${key}`, key, label: key }));
  }
  const tabFor = (name) => [...container.querySelectorAll('[role="tab"]')]
    .find((tab) => tab.querySelector('.mux-tab-label').textContent === name);
  const openMenu = async (name) => {
    await harness.act(() => harness.fireEvent(tabFor(name), 'contextmenu', { clientX: 4, clientY: 6 }));
    return getByRole(container, 'menu', { name: `Actions for ${name}` });
  };

  const newGroup = getByRole(await openMenu('a'), 'menuitem', { name: 'New group from this tab' });
  await harness.act(() => harness.fireEvent(newGroup, 'click'));
  assert.equal(state.groupMembers(state.groups.value[0].id).map((pane) => pane.key).join(), 'a');
  const rename = container.querySelector('.mux-group-rename');
  assert.equal(rename.value, 'group 1', 'a new stack opens named and ready to be renamed');
  await harness.act(() => harness.fireEvent(rename, 'keydown', { key: 'Escape' }));
  assert.equal(state.groups.value[0].name, 'group 1', 'Escape abandons the rename without touching the name');

  const menu = await openMenu('b');
  assert.deepEqual([...menu.querySelectorAll('[role="menuitem"]')].map((item) => item.textContent),
    ['New group from this tab', 'Add to “group 1”', 'Detach tab', 'Close tab', 'Close other tabs', 'Close all tabs']);
  await harness.act(() => harness.fireEvent(
    getByRole(menu, 'menuitem', { name: 'Add to “group 1”' }), 'click',
  ));
  assert.deepEqual(state.groupMembers(state.groups.value[0].id).map((pane) => pane.key), ['a', 'b']);

  const joined = await openMenu('b');
  assert.deepEqual([...joined.querySelectorAll('[role="menuitem"]')].map((item) => item.textContent),
    ['New group from this tab', 'Remove from group', 'Detach tab', 'Close tab', 'Close other tabs', 'Close all tabs'],
    'the stack a tab is already in is offered as a leave, not as a join');
  await harness.act(() => harness.fireEvent(
    getByRole(joined, 'menuitem', { name: 'Remove from group' }), 'click',
  ));
  assert.equal(state.groupFor('b'), null);
});
