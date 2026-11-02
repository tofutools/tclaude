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
