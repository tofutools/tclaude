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

test('re-asserting a collapse state is a no-op, so an unrelated close cannot spring a group open', async (t) => {
  const harness = await createPreactHarness(t);
  const state = await openState(harness, memoryPrefs());
  const group = state.createGroup({ name: 'infra', keys: ['b', 'c'] });
  state.activatePane('a');
  state.setGroupCollapsed(group.id, true);
  assert.equal(state.activeKey.value, 'a', 'collapsing a group the operator is not in leaves activation put');

  // Collapsing an already-collapsed group must not move activation into it.
  state.setGroupCollapsed(group.id, true);
  assert.equal(state.activeKey.value, 'a');

  // Closing the standalone active tab succeeds to another VISIBLE tab, not to a
  // member of the group the operator deliberately collapsed out of the way.
  state.removePane('a');
  assert.equal(state.groups.value[0].collapsed, true, 'closing an unrelated tab leaves the group collapsed');
  assert.equal(state.activeKey.value, 'd', 'succession prefers a visible tab over a collapsed member');

  // Only when nothing visible survives does the group open to host the active
  // tab — the nearest surviving neighbour of the closed tab, here 'c'.
  state.removePane('d');
  assert.equal(state.activeKey.value, 'c');
  assert.equal(state.groups.value[0].collapsed, false, 'with no tab left outside it, the group opens to stay usable');
});

test('a tab can be parked at a group boundary that a drop on a tab cannot reach', async (t) => {
  const harness = await createPreactHarness(t);
  const state = await openState(harness, memoryPrefs());
  // Two adjacent stacks with no ungrouped tab between them: the position
  // between them, and before the leading one, are the drag-unreachable spots.
  const left = state.createGroup({ name: 'left', keys: ['a', 'b'] });
  state.createGroup({ name: 'right', keys: ['c', 'd'] });
  assert.deepEqual(stripOf(state), ['left[a,b]', 'right[c,d]']);

  // Park a right-stack member before the left stack: ungrouped, at strip head.
  const moved = state.movePaneOutsideGroup('c', left.id, { after: false });
  assert.equal(moved.group, null);
  assert.equal(moved.leftGroup.name, 'right', 'the announcement names the stack the tab actually left');
  assert.deepEqual(stripOf(state), ['c', 'left[a,b]', 'right[d]']);

  // Park it after the left stack: between the two stacks, still ungrouped.
  state.movePaneOutsideGroup('c', left.id, { after: true });
  assert.deepEqual(stripOf(state), ['left[a,b]', 'c', 'right[d]']);

  assert.equal(state.movePaneOutsideGroup('c', 'missing', { after: true }), null,
    'parking beside a group that does not exist is refused');
});

test('creating a group at the cap evicts an invisible dormant stack instead of failing forever', async (t) => {
  const harness = await createPreactHarness(t);
  const { createTerminalShellState, MAX_TERMINAL_TAB_GROUPS } =
    await harness.importDashboardModule('js/terminal-shell-state.js');
  const state = createTerminalShellState({ prefs: memoryPrefs() });

  // Fill the cap with stacks whose only tab is then closed: each becomes
  // dormant — remembered for reopening, but with no pill and no menu.
  for (let index = 0; index < MAX_TERMINAL_TAB_GROUPS; index += 1) {
    const key = `k${index}`;
    state.openPane({ ws: `/${key}`, key, label: key });
    state.createGroup({ name: `g${index}`, keys: [key] });
    state.removePane(key);
  }
  assert.equal(state.groups.value.length, MAX_TERMINAL_TAB_GROUPS);
  assert.deepEqual(stripOf(state), [], 'every stack is dormant, so the strip is empty');

  // A fresh group must still be creatable — the oldest dormant stack is evicted
  // rather than the new one being silently refused.
  state.openPane({ ws: '/live', key: 'live', label: 'live' });
  const fresh = state.createGroup({ name: 'fresh', keys: ['live'] });
  assert.ok(fresh, 'creation succeeds at the cap by reclaiming a dormant slot');
  assert.equal(state.groups.value.length, MAX_TERMINAL_TAB_GROUPS);
  assert.equal(state.groups.value.some((group) => group.id === fresh.id), true);
  assert.equal(state.groups.value.some((group) => group.name === 'g0'), false, 'the oldest dormant stack was evicted');
  assert.deepEqual(stripOf(state), ['fresh[live]']);

  // With every stack now backing an OPEN tab there is nothing dormant to evict,
  // so the cap genuinely holds.
  const filler = createTerminalShellState({ prefs: memoryPrefs() });
  for (let index = 0; index < MAX_TERMINAL_TAB_GROUPS; index += 1) {
    const key = `o${index}`;
    filler.openPane({ ws: `/${key}`, key, label: key });
    filler.createGroup({ name: `g${index}`, keys: [key] });
  }
  filler.openPane({ ws: '/extra', key: 'extra', label: 'extra' });
  assert.equal(filler.createGroup({ name: 'nope', keys: ['extra'] }), null,
    'with no dormant slot to reclaim, the cap is a real limit');
});

test('group names strip invisible and bidi-spoofing characters, not just control codes', async (t) => {
  const harness = await createPreactHarness(t);
  const { sanitizeGroupName } = await harness.importDashboardModule('js/terminal-shell-state.js');
  assert.equal(sanitizeGroupName('a‮b​c'), 'a b c',
    'a right-to-left override and a zero-width space become separators, never reach the strip as glyphs');
  assert.equal(sanitizeGroupName('​'.repeat(40)), 'group',
    'a name made only of invisible characters is empty and takes the fallback');
  assert.equal(sanitizeGroupName('﻿⁦spoof⁩'), 'spoof', 'BOM and bidi isolates are stripped');
  assert.equal(sanitizeGroupName('  keep me  '), 'keep me');
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
  // collapsed stack renders as the pill alone. detail:0 is a keyboard-activated
  // click, which collapses instantly (a pointer click waits for a possible
  // double-click; that timing path has its own test).
  await harness.act(() => harness.fireEvent(pill, 'click', { detail: 0 }));
  assert.equal(state.activeKey.value, 'c');
  assert.equal(container.querySelector('.mux-tab-group').classList.contains('collapsed'), true);
  assert.deepEqual([...container.querySelectorAll('.mux-tab-group .mux-tab-label')], []);
  await harness.act(() => harness.fireEvent(
    getByRole(container, 'button', { name: 'infra, 2 terminals' }), 'click', { detail: 0 },
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

async function mountStrip(harness, keys) {
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
  for (const key of keys) await harness.act(() => state.openPane({ ws: `/${key}`, key, label: key }));
  return { state, container };
}

test('a keyboard step across a group boundary keeps focus on the moving tab', async (t) => {
  const harness = await createPreactHarness(t);
  const { state, container } = await mountStrip(harness, ['a', 'b', 'c']);
  state.createGroup({ name: 'infra', keys: ['a', 'b'] });
  await harness.act(() => {});
  const tabFor = (name) => [...container.querySelectorAll('[role="tab"]')]
    .find((tab) => tab.querySelector('.mux-tab-label').textContent === name);

  const bTab = tabFor('b');
  bTab.focus();
  assert.equal(harness.document.activeElement, bTab);
  // b is the right edge of the stack; the next step reparents it out of the
  // group wrapper into the strip. Without a deliberate refocus a real browser
  // drops focus to <body> and the next arrow key is swallowed.
  await harness.act(() => harness.fireEvent(bTab, 'keydown', {
    key: 'ArrowRight', altKey: true, shiftKey: true,
  }));
  assert.equal(state.groupFor('b'), null, 'the step took b out of the group');
  // The refocus is scheduled on a microtask so it runs after the reparent
  // render; let it drain before reading activeElement.
  await new Promise((resolve) => { queueMicrotask(resolve); });
  const movedTab = tabFor('b');
  assert.notEqual(movedTab, bTab, 'the tab node was recreated under a new parent');
  assert.equal(harness.document.activeElement, movedTab,
    'focus followed the tab across the boundary so the next keypress still lands');
});

test('a drop gap parks a tab at a group boundary a tab drop cannot reach', async (t) => {
  const harness = await createPreactHarness(t);
  const { state, container } = await mountStrip(harness, ['a', 'b', 'c', 'd']);
  state.createGroup({ name: 'left', keys: ['a', 'b'] });
  state.createGroup({ name: 'right', keys: ['c', 'd'] });
  await harness.act(() => {});
  // The gap nodes are always present (so beginning a drag never inserts DOM
  // next to a group and aborts the native drag), but none is ACTIVE at rest.
  assert.equal(container.querySelectorAll('.mux-strip-gap.active').length, 0,
    'no gap is active at rest');

  const transfer = {
    data: {}, effectAllowed: '', dropEffect: '',
    setData(type, value) { this.data[type] = value; },
    getData(type) { return this.data[type] || ''; },
  };
  const cLabel = [...container.querySelectorAll('[role="tab"]')]
    .find((tab) => tab.querySelector('.mux-tab-label').textContent === 'c')
    .querySelector('.mux-tab-label');
  await harness.act(() => harness.fireEvent(cLabel, 'dragstart', { dataTransfer: transfer }));
  const gaps = container.querySelectorAll('.mux-strip-gap.active');
  assert.ok(gaps.length >= 2, 'gaps activate at the group boundaries while a drag is in flight');

  // The first gap is the position before the leading stack — the spot no tab
  // drop can express, because the only tab there belongs to the stack.
  const leadingGap = gaps[0];
  await harness.act(() => harness.fireEvent(leadingGap, 'dragover', { dataTransfer: transfer }));
  assert.equal(leadingGap.classList.contains('armed'), true, 'the gap says it will accept the tab');
  await harness.act(() => harness.fireEvent(leadingGap, 'drop', { dataTransfer: transfer }));

  assert.equal(state.groupFor('c'), null, 'the parked tab is ungrouped');
  assert.equal(state.panes.value[0].key, 'c', 'and sits before the formerly-leading stack');
  assert.equal(container.querySelectorAll('.mux-strip-gap.active').length, 0,
    'the gaps deactivate once the drag ends');
  assert.match(container.querySelector('[role="status"]').textContent, /out of group right/);
});

test('starting a drag on a grouped tab preserves its DOM node so the native drag survives', async (t) => {
  const harness = await createPreactHarness(t);
  const { state, container } = await mountStrip(harness, ['a', 'b', 'c']);
  state.createGroup({ name: 'infra', keys: ['a', 'b'] });
  await harness.act(() => {});
  const tabFor = (name) => [...container.querySelectorAll('[role="tab"]')]
    .find((tab) => tab.querySelector('.mux-tab-label').textContent === name);

  const bTab = tabFor('b');
  const bLabel = bTab.querySelector('.mux-tab-label');
  const transfer = {
    data: {}, effectAllowed: '', dropEffect: '',
    setData(type, value) { this.data[type] = value; },
    getData(type) { return this.data[type] || ''; },
  };
  await harness.act(() => harness.fireEvent(bLabel, 'dragstart', { dataTransfer: transfer }));

  // The dragstart re-render (which activates the boundary gaps) must NOT
  // recreate the grouped tab or its drag-source label: a browser aborts a
  // native drag the instant its source node is replaced, which is what made
  // grabbing a grouped tab "do nothing" most of the time.
  assert.equal(tabFor('b'), bTab, 'the grouped tab element is reused, not recreated');
  assert.equal(tabFor('b').querySelector('.mux-tab-label'), bLabel, 'the drag-source label is preserved');
  assert.equal(bTab.classList.contains('dragging'), true, 'and the drag actually started');
});

test('a rename committed by blur still respects a preceding Escape', async (t) => {
  const harness = await createPreactHarness(t);
  const { state, container } = await mountStrip(harness, ['a']);
  const group = state.createGroup({ name: 'infra', keys: ['a'] });
  await harness.act(() => {});

  // Open the rename, type, and commit with blur — the ordinary non-keyboard
  // dismissal path that the Enter-only test never exercises.
  await harness.act(() => harness.fireEvent(
    getByRole(container, 'button', { name: 'infra, 1 terminal' }), 'contextmenu', { clientX: 4, clientY: 6 },
  ));
  await harness.act(() => harness.fireEvent(
    getByRole(container, 'menuitem', { name: 'Rename group…' }), 'click',
  ));
  const rename = container.querySelector('.mux-group-rename');
  rename.value = 'platform';
  await harness.act(() => harness.fireEvent(rename, 'blur'));
  assert.equal(state.groups.value[0].name, 'platform', 'blur commits the typed name');

  // Reopen, type, press Escape, and confirm a late blur cannot resurrect the
  // discarded edit — the guard the review flagged as untested.
  await harness.act(() => harness.fireEvent(
    getByRole(container, 'button', { name: 'platform, 1 terminal' }), 'contextmenu', { clientX: 4, clientY: 6 },
  ));
  await harness.act(() => harness.fireEvent(
    getByRole(container, 'menuitem', { name: 'Rename group…' }), 'click',
  ));
  const second = container.querySelector('.mux-group-rename');
  second.value = 'DISCARD ME';
  await harness.act(() => harness.fireEvent(second, 'keydown', { key: 'Escape' }));
  await harness.act(() => harness.fireEvent(second, 'blur'));
  assert.equal(state.groups.value[0].name, 'platform', 'Escape wins even if a blur commit fires afterward');
  assert.equal(group.id, state.groups.value[0].id);
});

const dragTransfer = () => ({
  data: {}, effectAllowed: '', dropEffect: '',
  setData(type, value) { this.data[type] = value; },
  getData(type) { return this.data[type] || ''; },
});

test('dropping a tab on the centre of another tab groups the two', async (t) => {
  const harness = await createPreactHarness(t);
  const { state, container } = await mountStrip(harness, ['a', 'b', 'c']);
  await harness.act(() => {});
  const tabFor = (name) => [...container.querySelectorAll('[role="tab"]')]
    .find((tab) => tab.querySelector('.mux-tab-label').textContent === name);
  const transfer = dragTransfer();

  const aTab = tabFor('a');
  aTab.getBoundingClientRect = () => ({ left: 0, width: 100 });
  await harness.act(() => harness.fireEvent(tabFor('c').querySelector('.mux-tab-label'), 'dragstart', {
    dataTransfer: transfer,
  }));
  // Centre of the target (fraction 0.5) is the grouping zone; the outer
  // quarters stay reorder.
  await harness.act(() => harness.fireEvent(aTab, 'dragover', { dataTransfer: transfer, clientX: 50 }));
  assert.equal(aTab.classList.contains('drop-group'), true,
    'the whole target tab highlights to show a group will form');
  assert.equal(aTab.classList.contains('drop-before') || aTab.classList.contains('drop-after'), false,
    'the centre zone shows the group highlight, not a reorder insertion line');

  await harness.act(() => harness.fireEvent(aTab, 'drop', { dataTransfer: transfer, clientX: 50 }));
  assert.equal(state.groups.value.length, 1, 'a brand-new group is created by the drop');
  const group = state.groups.value[0];
  assert.deepEqual(state.groupMembers(group.id).map((pane) => pane.key), ['a', 'c'],
    'the group holds the drop target followed by the dragged tab');
  assert.match(container.querySelector('[role="status"]').textContent, /Grouped c with a as/);
});

test('an outer-edge drop still reorders instead of grouping', async (t) => {
  const harness = await createPreactHarness(t);
  const { state, container } = await mountStrip(harness, ['a', 'b', 'c']);
  await harness.act(() => {});
  const tabFor = (name) => [...container.querySelectorAll('[role="tab"]')]
    .find((tab) => tab.querySelector('.mux-tab-label').textContent === name);
  const transfer = dragTransfer();

  const aTab = tabFor('a');
  aTab.getBoundingClientRect = () => ({ left: 0, width: 100 });
  await harness.act(() => harness.fireEvent(tabFor('c').querySelector('.mux-tab-label'), 'dragstart', {
    dataTransfer: transfer,
  }));
  // Fraction 0.1 is well inside the left reorder quarter.
  await harness.act(() => harness.fireEvent(aTab, 'dragover', { dataTransfer: transfer, clientX: 10 }));
  assert.equal(aTab.classList.contains('drop-before'), true, 'the outer quarter shows a reorder insertion line');
  assert.equal(aTab.classList.contains('drop-group'), false);
  await harness.act(() => harness.fireEvent(aTab, 'drop', { dataTransfer: transfer, clientX: 10 }));
  assert.equal(state.groups.value.length, 0, 'an edge drop never creates a group');
  assert.deepEqual(state.panes.value.map((pane) => pane.key), ['c', 'a', 'b'], 'it reorders c before a');
});

test('dropping a tab on the centre of a grouped tab joins that group', async (t) => {
  const harness = await createPreactHarness(t);
  const { state, container } = await mountStrip(harness, ['a', 'b', 'c']);
  const group = state.createGroup({ name: 'infra', keys: ['a'] });
  await harness.act(() => {});
  const tabFor = (name) => [...container.querySelectorAll('[role="tab"]')]
    .find((tab) => tab.querySelector('.mux-tab-label').textContent === name);
  const transfer = dragTransfer();

  const aTab = tabFor('a');
  aTab.getBoundingClientRect = () => ({ left: 0, width: 100 });
  await harness.act(() => harness.fireEvent(tabFor('b').querySelector('.mux-tab-label'), 'dragstart', {
    dataTransfer: transfer,
  }));
  await harness.act(() => harness.fireEvent(aTab, 'dragover', { dataTransfer: transfer, clientX: 50 }));
  assert.equal(aTab.classList.contains('drop-group'), true);
  await harness.act(() => harness.fireEvent(aTab, 'drop', { dataTransfer: transfer, clientX: 50 }));
  assert.equal(state.groups.value.length, 1, 'no second group is created');
  assert.deepEqual(state.groupMembers(group.id).map((pane) => pane.key), ['a', 'b'],
    'the dragged tab joins the existing group');
  assert.match(container.querySelector('[role="status"]').textContent, /Grouped b into infra/);
});

test('double-click renames a group without collapsing it or moving the active terminal', async (t) => {
  const harness = await createPreactHarness(t);
  const { state, container } = await mountStrip(harness, ['a', 'b', 'c']);
  // The group owns the active terminal AND there is a tab outside it — the
  // exact shape in which a spurious collapse would displace activation onto the
  // outside tab. Renaming must not collapse, so activation must not move.
  state.createGroup({ name: 'infra', keys: ['a', 'b'] });
  state.activatePane('b');
  await harness.act(() => {});

  const pill = getByRole(container, 'button', { name: 'infra, 2 terminals' });
  assert.equal(pill.getAttribute('aria-expanded'), 'true');
  // A real double-click is a pointer click (which only ARMS the deferred
  // collapse) immediately followed by a dblclick that cancels it.
  await harness.act(() => harness.fireEvent(pill, 'click'));
  assert.equal(state.groups.value[0].collapsed, false, 'the click alone has not collapsed anything yet');
  await harness.act(() => harness.fireEvent(pill, 'dblclick'));
  const input = container.querySelector('.mux-group-rename');
  assert.ok(input, 'the double-click opens the inline rename field');
  assert.equal(harness.document.activeElement, input, 'and focuses it');
  input.value = 'platform';
  await harness.act(() => harness.fireEvent(input, 'keydown', { key: 'Enter' }));
  assert.equal(state.groups.value[0].name, 'platform', 'the typed name is committed');
  assert.equal(state.groups.value[0].collapsed, false, 'renaming never collapsed the group');
  assert.equal(state.activeKey.value, 'b', 'and never displaced the active terminal onto the outside tab');

  // F2 on the focused pill is the keyboard equivalent.
  const renamedPill = getByRole(container, 'button', { name: 'platform, 2 terminals' });
  renamedPill.focus();
  await harness.act(() => harness.fireEvent(renamedPill, 'keydown', { key: 'F2' }));
  const keyboardInput = container.querySelector('.mux-group-rename');
  assert.ok(keyboardInput, 'F2 opens the rename field');
  keyboardInput.value = 'infra-2';
  await harness.act(() => harness.fireEvent(keyboardInput, 'keydown', { key: 'Enter' }));
  assert.equal(state.groups.value[0].name, 'infra-2');
});

test('a single pointer click on a group pill collapses it only after the double-click grace period', async (t) => {
  const harness = await createPreactHarness(t);
  const { state, container } = await mountStrip(harness, ['a', 'b', 'c']);
  const { GROUP_COLLAPSE_CLICK_DELAY_MS } =
    await harness.importDashboardModule('js/terminal-shell-island.js');
  state.createGroup({ name: 'infra', keys: ['a', 'b'] });
  await harness.act(() => {});
  const pill = () => getByRole(container, 'button', { name: /^infra, 2 terminals$/ });

  // The pointer click arms the deferred collapse but does not collapse yet, so
  // a double-click still has time to cancel it.
  await harness.act(() => harness.fireEvent(pill(), 'click'));
  assert.equal(state.groups.value[0].collapsed, false, 'not collapsed within the grace period');
  await new Promise((resolve) => { setTimeout(resolve, GROUP_COLLAPSE_CLICK_DELAY_MS + 60); });
  assert.equal(state.groups.value[0].collapsed, true, 'collapses once the grace period elapses with no second click');

  // A keyboard-activated click (detail 0) has no double-click to wait for and
  // toggles immediately.
  await harness.act(() => harness.fireEvent(pill(), 'click', { detail: 0 }));
  assert.equal(state.groups.value[0].collapsed, false, 'a keyboard click toggles without waiting');
});
