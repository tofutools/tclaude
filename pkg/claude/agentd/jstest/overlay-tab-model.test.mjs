// Tests for what an overlay window holds (tcl-e4i44z), and above all that a
// window saved by an older page still comes back.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

async function model(t) {
  return (await createPreactHarness(t)).importDashboardModule('js/overlay-tab-model.js');
}
const model_ = model;

const anyTab = () => true;

// Real windows as the last page before tabs saved them, one of each kind.
const OLD_SAVED = [
  { minimized: false, id: 'chat:agt_0123456789abcdef', kind: 'chat', agent: 'agt_0123456789abcdef', label: 'Worker' },
  { minimized: true, id: 'terminal:/api/term-ws/abc', kind: 'terminal', paneKey: '/api/term-ws/abc', ws: '/api/term-ws/abc', agent: '', label: 'repo shell' },
  { minimized: false, id: 'board:plan', kind: 'board', board: 'plan', label: 'Release plan', focusAsked: 2 },
];

test('every window saved before tabs comes back as a window of one tab, with its id', async (t) => {
  const { parseSavedOverlays } = await model(t);
  const { windows, minimizedTabs } = parseSavedOverlays(JSON.stringify(OLD_SAVED), anyTab);
  assert.deepEqual(windows, [
    { id: 'chat:agt_0123456789abcdef', tabs: [{ id: 'chat:agt_0123456789abcdef', kind: 'chat', agent: 'agt_0123456789abcdef', label: 'Worker' }], active: 'chat:agt_0123456789abcdef', minimized: false, place: 'tclaude.dash.chat-window.placement.agt_0123456789abcdef' },
    { id: 'terminal:/api/term-ws/abc', tabs: [{ id: 'terminal:/api/term-ws/abc', kind: 'terminal', paneKey: '/api/term-ws/abc', ws: '/api/term-ws/abc', agent: '', label: 'repo shell' }], active: 'terminal:/api/term-ws/abc', minimized: true, place: 'tclaude.dash.terminal-window.placement./api/term-ws/abc' },
    { id: 'board:plan', tabs: [{ id: 'board:plan', kind: 'board', board: 'plan', label: 'Release plan' }], active: 'board:plan', minimized: false, place: 'tclaude.dash.board-overlay.placement.plan', focusAsked: 2 },
  ]);
  assert.deepEqual(minimizedTabs, []);
});

test('a saved set reads back the same in the new shape', async (t) => {
  const { parseSavedOverlays, savedOverlays } = await model(t);
  const first = parseSavedOverlays(JSON.stringify(OLD_SAVED), anyTab);
  const minimizedTabs = [{ tab: { id: 'board:map', kind: 'board', board: 'map', label: 'Map' }, home: 'board:plan', place: 'tclaude.dash.board-overlay.placement.plan' }];
  assert.deepEqual(parseSavedOverlays(savedOverlays(first.windows, minimizedTabs), anyTab), { windows: first.windows, minimizedTabs });
});

test('a window keeps its valid tabs, and goes with none left', async (t) => {
  const { parseSavedOverlays } = await model(t);
  const tabs = [{ id: 'bad', kind: 'no' }, { id: 'good', kind: 'yes' }];
  const { windows } = parseSavedOverlays(JSON.stringify({ windows: [
    { id: 'w', tabs, active: 'bad', minimized: false },
    { id: 'gone', tabs: [{ id: 'worse', kind: 'no' }], active: 'worse' },
  ] }), (tab) => tab.kind === 'yes');
  assert.deepEqual(windows, [{ id: 'w', tabs: [tabs[1]], active: 'good', minimized: false, place: '' }]);
});

test('a tab saved twice opens in one place', async (t) => {
  const { parseSavedOverlays } = await model(t);
  const tab = { id: 't', kind: 'k' };
  const saved = parseSavedOverlays(JSON.stringify({
    windows: [{ id: 'a', tabs: [tab], active: 't' }, { id: 'b', tabs: [tab], active: 't' }],
    minimizedTabs: [{ tab, home: 'a' }],
  }), anyTab);
  assert.deepEqual(saved.windows.map((window) => window.id), ['a']);
  assert.deepEqual(saved.minimizedTabs, []);
});

test('a window keeps its place whatever its tabs do, and a bad one is not trusted', async (t) => {
  const { parseSavedOverlays, placementKeyOf } = await model(t);
  const tab = { id: 'board:map', kind: 'board', board: 'map' };
  const place = 'tclaude.dash.chat-window.placement.agt_1';
  const read = (saved) => parseSavedOverlays(JSON.stringify({ windows: [{ id: 'w', tabs: [tab], active: tab.id, place: saved }] }), anyTab).windows[0].place;
  assert.equal(read(place), place, 'the chat it was made around has since left it');
  assert.equal(read('someone.elses.pref'), placementKeyOf(tab));
  assert.equal(read(7), placementKeyOf(tab));
});

test('whatever else was saved reads as nothing', async (t) => {
  const { parseSavedOverlays } = await model(t);
  const none = { windows: [], minimizedTabs: [] };
  for (const raw of [null, '', 'not json', '42', 'null', '{}', '{"windows":3}', '["junk",null,{"kind":"k"}]', '{"windows":[{"id":"w","tabs":["junk"]}]}']) {
    assert.deepEqual(parseSavedOverlays(raw, anyTab), none, String(raw));
  }
  assert.deepEqual(parseSavedOverlays('{"minimizedTabs":[{"tab":{"id":"t"},"home":3},{"home":"w"}]}', anyTab), { windows: [], minimizedTabs: [{ tab: { id: 't' }, home: '', place: '' }] });
});

test('a drop point is not saved, and every tab reads what it takes from its window', async (t) => {
  const { activeTabOf, savedOverlays, tabsOfWindows, windowHolding, windowOfTab } = await model(t);
  const window = windowOfTab({ id: 'a', kind: 'k', label: 'A', at: { x: 1, y: 2 } });
  assert.deepEqual(window.at, { x: 1, y: 2 });
  assert.equal(JSON.parse(savedOverlays([window], [])).windows[0].at, undefined);
  assert.deepEqual(tabsOfWindows([{ ...window, minimized: true }]), [{ id: 'a', kind: 'k', label: 'A', windowId: 'a', minimized: true, focusAsked: undefined, closeAsked: undefined, at: { x: 1, y: 2 } }]);
  assert.equal(activeTabOf(window).at.x, 1);
  assert.equal(activeTabOf({ ...window, active: 'gone' }), null);
  assert.equal(windowHolding([window], 'a'), window);
  assert.equal(windowHolding([window], 'b'), null);
});

const tab = (id, kind = 'board') => ({ id, kind, board: id, label: id });
const twoTabs = (model) => ({ ...model.windowOfTab(tab('a')), tabs: [tab('a'), tab('b')] });
const ids = (window) => window.tabs.map((one) => one.id);

test('a tab reorders in its window and joins another, and the emptied window goes', async (t) => {
  const model = await model_(t);
  const other = model.windowOfTab(tab('c'));
  const inOrder = model.movedTab([twoTabs(model), other], 'a', 'a', 1);
  assert.deepEqual(ids(inOrder[0]), ['b', 'a']);
  const joined = model.movedTab([twoTabs(model), other], 'a', 'c', 0);
  assert.deepEqual(joined.map((window) => [window.id, ids(window), window.active]), [['a', ['b'], 'b'], ['c', ['a', 'c'], 'a']]);
  const emptied = model.movedTab([other, model.windowOfTab(tab('d'))], 'c', 'd', 1);
  assert.deepEqual(emptied.map((window) => [window.id, ids(window)]), [['d', ['d', 'c']]]);
  const lone = [other];
  assert.equal(model.movedTab(lone, 'c', 'c', 0), lone, 'a last tab has nowhere to go in its own window');
  assert.equal(model.movedTab(lone, 'c', 'nowhere', 0), lone);
});

test('a joined window comes out minimized no more', async (t) => {
  const model = await model_(t);
  const [, target] = model.movedTab([twoTabs(model), { ...model.windowOfTab(tab('c')), minimized: true }], 'a', 'c', 1);
  assert.equal(target.minimized, false);
});

test('a tab torn out does not take the id or the placement of the window named after it', async (t) => {
  const model = await model_(t);
  const [left, torn] = model.tornOutTab([twoTabs(model)], 'a', { x: 1, y: 2 });
  assert.deepEqual([left.id, left.place, ids(left)], ['a', 'tclaude.dash.board-overlay.placement.a', ['b']]);
  assert.deepEqual([torn.id, torn.place, ids(torn), torn.at], ['a#2', 'tclaude.dash.board-overlay.placement.a#2', ['a'], { x: 1, y: 2 }]);
  const third = model.windowAround([left, torn], tab('a'));
  assert.equal(third.id, 'a#3', 'and the next one neither');
  const lone = [model.windowOfTab(tab('c'))];
  assert.equal(model.tornOutTab(lone, 'c', null), lone, 'a last tab has nothing to leave');
});

test('a tab minimized on its own comes back to its window, or to a new one in its place', async (t) => {
  const model = await model_(t);
  const start = { windows: [twoTabs(model)], minimizedTabs: [] };
  const out = model.minimizedTab(start, 'b');
  assert.deepEqual(ids(out.windows[0]), ['a'], 'the window stays while it has tabs');
  assert.deepEqual(out.minimizedTabs.map(({ tab: one, home }) => [one.id, home]), [['b', 'a']]);
  const back = model.restoredTab(out, 'b');
  assert.deepEqual([ids(back.windows[0]), back.windows[0].active, back.minimizedTabs], [['a', 'b'], 'b', []]);

  const custom = { ...model.windowOfTab(tab('c')), place: 'tclaude.dash.board-overlay.placement.c#2' };
  const gone = model.minimizedTab({ windows: [custom], minimizedTabs: [] }, 'c');
  assert.deepEqual(gone.windows, [], 'its last tab takes the window');
  const reopened = model.restoredTab(gone, 'c');
  assert.deepEqual([reopened.windows[0].id, reopened.windows[0].place], ['c', custom.place], 'where that window stood');
});

test('every tab of a closed window finds the others again', async (t) => {
  const model = await model_(t);
  const closed = ['a', 'b'].reduce(model.minimizedTab, { windows: [twoTabs(model)], minimizedTabs: [] });
  assert.deepEqual(closed.windows, []);
  const first = model.restoredTab(closed, 'a');
  const both = model.restoredTab(first, 'b');
  assert.deepEqual(both.windows.map(ids), [['a', 'b']]);
});

test('a tab is taken off for good from a window or the dock', async (t) => {
  const model = await model_(t);
  const state = model.minimizedTab({ windows: [twoTabs(model)], minimizedTabs: [] }, 'b');
  assert.deepEqual(model.removedTab(state, 'b').minimizedTabs, []);
  assert.deepEqual(model.removedTab(state, 'a').windows, []);
});

test('the dock shows a minimized window once, with its tabs, and each loose tab', async (t) => {
  const model = await model_(t);
  const entries = model.dockEntriesOf([{ ...twoTabs(model), minimized: true, active: 'b' }, model.windowOfTab(tab('c'))], [{ tab: tab('d'), home: 'gone', place: '' }]);
  assert.deepEqual(entries.map(({ key, tab: one, tabs }) => [key, one.id, tabs.length]), [['a', 'b', 2], ['tab:d', 'd', 1]]);
});

test('a drop lands by the middles of the tabs, and a strip is left only well clear of it', async (t) => {
  const model = await model_(t);
  const spans = [{ left: 0, width: 100 }, { left: 100, width: 100 }];
  assert.deepEqual([40, 60, 140, 160].map((x) => model.dropIndex(spans, x)), [0, 1, 1, 2]);
  assert.equal(model.dropIndex([], 5), 0);
  const rect = { left: 0, right: 100, top: 0, bottom: 30 };
  assert.equal(model.pulledOff(rect, { x: 50, y: 60 }, 40), false);
  assert.equal(model.pulledOff(rect, { x: 50, y: 80 }, 40), true);
});

test('a saved tab on the dock keeps its window and its place', async (t) => {
  const model = await model_(t);
  const raw = JSON.stringify({ windows: [], minimizedTabs: [{ tab: tab('a'), home: 'w', place: 'tclaude.dash.x' }, { tab: tab('b'), home: 'w', place: 'elsewhere' }] });
  assert.deepEqual(model.parseSavedOverlays(raw, anyTab).minimizedTabs.map(({ home, place }) => [home, place]), [['w', 'tclaude.dash.x'], ['w', '']]);
});

const groupWindow = () => ({
  id: 'w', place: 'tclaude.dash.x', minimized: false, active: 'a',
  groups: [{ id: 'g', name: 'Work', color: 'blue', collapsed: false }],
  tabs: [{ id: 'a', kind: 'board', board: 'a', groupId: 'g' }, { id: 'b', kind: 'board', board: 'b', groupId: 'g' }, { id: 'c', kind: 'board', board: 'c' }],
});

test('a group goes with its last member, and a tab dropped outside it leaves it', async (t) => {
  const model = await model_(t);
  const [left] = model.movedTab([groupWindow()], 'a', 'w', 2);
  assert.equal(left.tabs.find((tab) => tab.id === 'a').groupId, undefined);
  assert.deepEqual(left.groups.map((group) => group.id), ['g']);
  const [gone] = model.movedTab([left], 'b', 'w', 2);
  assert.equal(gone.groups, undefined);
});

test('a tab dropped inside a group joins it, and its members stay together', async (t) => {
  const model = await model_(t);
  const [joined] = model.movedTab([groupWindow()], 'c', 'w', 1, 'g');
  assert.deepEqual(ids(joined), ['a', 'c', 'b']);
  assert.equal(joined.tabs[1].groupId, 'g');
  const [grouped] = model.groupedTab([{ ...groupWindow(), tabs: [groupWindow().tabs[0], groupWindow().tabs[2], groupWindow().tabs[1]] }], 'c', null);
  assert.deepEqual(ids(grouped), ['a', 'b', 'c'], 'a stray member comes back beside its group');
});

test('a tab that leaves a window leaves its group, and comes back to it from the dock', async (t) => {
  const model = await model_(t);
  const torn = model.tornOutTab([groupWindow()], 'a', null);
  assert.equal(torn[1].tabs[0].groupId, undefined);
  const docked = model.minimizedTab({ windows: [groupWindow()], minimizedTabs: [] }, 'a');
  const back = model.restoredTab(docked, 'a');
  assert.equal(back.windows[0].tabs.find((tab) => tab.id === 'a').groupId, 'g');
});

test('a tab back from the dock after its group went comes back ungrouped', async (t) => {
  const model = await model_(t);
  const docked = model.minimizedTab(model.minimizedTab({ windows: [groupWindow()], minimizedTabs: [] }, 'a'), 'b');
  assert.equal(docked.windows[0].groups, undefined);
  const back = model.restoredTab(docked, 'a');
  assert.equal(back.windows[0].tabs.find((tab) => tab.id === 'a').groupId, undefined);
});

test('a group\'s only tab dropped back inside it keeps the group', async (t) => {
  const model = await model_(t);
  const lone = { ...groupWindow(), tabs: [groupWindow().tabs[0], groupWindow().tabs[2]] };
  const [window] = model.movedTab([lone], 'a', 'w', 0, 'g');
  assert.deepEqual(window.groups.map((group) => group.name), ['Work']);
  assert.equal(window.tabs[0].groupId, 'g');
});

test('a tab on the dock forgets a group that went, even when the id comes round again', async (t) => {
  const model = await model_(t);
  const docked = model.minimizedTab(model.minimizedTab({ windows: [groupWindow()], minimizedTabs: [] }, 'a'), 'b');
  assert.ok(docked.minimizedTabs.every(({ tab }) => !tab.groupId));
  const group = model.newTabGroup(docked.windows[0]);
  assert.equal(group.id, 'group-1');
  const windows = model.groupedTab(docked.windows, 'c', group);
  const back = model.restoredTab({ windows, minimizedTabs: docked.minimizedTabs }, 'a');
  assert.equal(back.windows[0].tabs.find((tab) => tab.id === 'a').groupId, undefined);
});

test('a whole group moves along its strip, to another window, and out to its own', async (t) => {
  const model = await model_(t);
  const [along] = model.movedGroup([groupWindow()], 'w', 'g', 'w', 1);
  assert.deepEqual(ids(along), ['c', 'a', 'b']);
  const other = { id: 'x', place: 'tclaude.dash.y', minimized: false, active: 'd', groups: [{ id: 'g', name: 'Taken', color: 'red', collapsed: false }], tabs: [{ id: 'd', kind: 'board', board: 'd', groupId: 'g' }] };
  const [left, target] = model.movedGroup([groupWindow(), other], 'w', 'g', 'x', 1);
  assert.deepEqual(ids(left), ['c']);
  assert.equal(left.active, 'c');
  assert.deepEqual(ids(target), ['d', 'a', 'b']);
  assert.equal(target.groups.length, 2, 'a group id taken there is not shared');
  assert.notEqual(target.tabs[1].groupId, 'g');
  const out = model.tornOutGroup([groupWindow()], 'w', 'g', { x: 5, y: 6 });
  assert.deepEqual(out.map(ids), [['c'], ['a', 'b']]);
  assert.deepEqual(out[1].at, { x: 5, y: 6 });
  assert.deepEqual(out[1].groups.map((group) => group.name), ['Work']);
  assert.notEqual(out[0].place, out[1].place);
  assert.equal(model.tornOutGroup([{ ...groupWindow(), tabs: groupWindow().tabs.slice(0, 2) }], 'w', 'g', null).length, 1, 'a window of only that group has nothing to leave');
});

test('a group collapsing hands over the showing tab, and ungrouping leaves tabs in place', async (t) => {
  const model = await model_(t);
  const [collapsed] = model.changedGroup([groupWindow()], 'w', 'g', { collapsed: true });
  assert.equal(collapsed.active, 'c');
  const alone = { ...groupWindow(), tabs: groupWindow().tabs.slice(0, 2) };
  assert.equal(model.changedGroup([alone], 'w', 'g', { collapsed: true })[0].active, 'a', 'with nothing else to show it stays');
  const [plain] = model.dissolvedGroup([groupWindow()], 'w', 'g');
  assert.deepEqual(ids(plain), ['a', 'b', 'c']);
  assert.ok(plain.tabs.every((tab) => !tab.groupId));
  assert.equal(plain.groups, undefined);
});

test('a new group is named and coloured apart from those there', async (t) => {
  const model = await model_(t);
  const group = model.newTabGroup(groupWindow());
  assert.notEqual(group.id, 'g');
  assert.equal(group.name, 'group 2');
  assert.equal(group.color, 'purple');
  const [window] = model.groupedTab([groupWindow()], 'c', group);
  assert.equal(window.groups.length, 2);
  assert.equal(window.tabs[2].groupId, group.id);
});

test('saved groups come back checked, and a window saved before groups has none', async (t) => {
  const model = await model_(t);
  const saved = model.savedOverlays([groupWindow()], []);
  const { windows } = model.parseSavedOverlays(saved, () => true);
  assert.deepEqual(windows[0].groups, groupWindow().groups);
  const raw = JSON.stringify({ windows: [{ ...groupWindow(), groups: [{ id: 'g', name: 'a\u202eb', color: 'nope', collapsed: 1 }, 7, { id: 'h', name: 'empty' }], tabs: [...groupWindow().tabs, { id: 'e', kind: 'board', board: 'e', groupId: 'lost' }] }] });
  const [window] = model.parseSavedOverlays(raw, () => true).windows;
  assert.deepEqual(window.groups, [{ id: 'g', name: 'a b', color: 'blue', collapsed: true }]);
  assert.equal(window.tabs.find((tab) => tab.id === 'e').groupId, undefined);
  const [flat] = model.parseSavedOverlays(JSON.stringify([{ id: 'board:a', kind: 'board', board: 'a', label: 'A' }]), () => true).windows;
  assert.equal(flat.groups, undefined);
});
