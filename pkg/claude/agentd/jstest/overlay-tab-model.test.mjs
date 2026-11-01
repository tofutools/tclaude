// Tests for what an overlay window holds (tcl-e4i44z), and above all that a
// window saved by an older page still comes back.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

async function model(t) {
  return (await createPreactHarness(t)).importDashboardModule('js/overlay-tab-model.js');
}

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
    { id: 'chat:agt_0123456789abcdef', tabs: [{ id: 'chat:agt_0123456789abcdef', kind: 'chat', agent: 'agt_0123456789abcdef', label: 'Worker' }], active: 'chat:agt_0123456789abcdef', minimized: false },
    { id: 'terminal:/api/term-ws/abc', tabs: [{ id: 'terminal:/api/term-ws/abc', kind: 'terminal', paneKey: '/api/term-ws/abc', ws: '/api/term-ws/abc', agent: '', label: 'repo shell' }], active: 'terminal:/api/term-ws/abc', minimized: true },
    { id: 'board:plan', tabs: [{ id: 'board:plan', kind: 'board', board: 'plan', label: 'Release plan' }], active: 'board:plan', minimized: false, focusAsked: 2 },
  ]);
  assert.deepEqual(minimizedTabs, []);
});

test('a saved set reads back the same in the new shape', async (t) => {
  const { parseSavedOverlays, savedOverlays } = await model(t);
  const first = parseSavedOverlays(JSON.stringify(OLD_SAVED), anyTab);
  const minimizedTabs = [{ tab: { id: 'board:map', kind: 'board', board: 'map', label: 'Map' }, home: 'board:plan' }];
  assert.deepEqual(parseSavedOverlays(savedOverlays(first.windows, minimizedTabs), anyTab), { windows: first.windows, minimizedTabs });
});

test('a window keeps its valid tabs, and goes with none left', async (t) => {
  const { parseSavedOverlays } = await model(t);
  const tabs = [{ id: 'bad', kind: 'no' }, { id: 'good', kind: 'yes' }];
  const { windows } = parseSavedOverlays(JSON.stringify({ windows: [
    { id: 'w', tabs, active: 'bad', minimized: false },
    { id: 'gone', tabs: [{ id: 'worse', kind: 'no' }], active: 'worse' },
  ] }), (tab) => tab.kind === 'yes');
  assert.deepEqual(windows, [{ id: 'w', tabs: [tabs[1]], active: 'good', minimized: false }]);
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

test('whatever else was saved reads as nothing', async (t) => {
  const { parseSavedOverlays } = await model(t);
  const none = { windows: [], minimizedTabs: [] };
  for (const raw of [null, '', 'not json', '42', 'null', '{}', '{"windows":3}', '["junk",null,{"kind":"k"}]', '{"windows":[{"id":"w","tabs":["junk"]}]}']) {
    assert.deepEqual(parseSavedOverlays(raw, anyTab), none, String(raw));
  }
  assert.deepEqual(parseSavedOverlays('{"minimizedTabs":[{"tab":{"id":"t"},"home":3},{"home":"w"}]}', anyTab), { windows: [], minimizedTabs: [{ tab: { id: 't' }, home: '' }] });
});

test('a drop point is not saved, and every tab reads what it takes from its window', async (t) => {
  const { savedOverlays, tabsOfWindows, windowHolding, windowOfTab } = await model(t);
  const window = windowOfTab({ id: 'a', kind: 'k', label: 'A', at: { x: 1, y: 2 } });
  assert.deepEqual(window.at, { x: 1, y: 2 });
  assert.equal(JSON.parse(savedOverlays([window], [])).windows[0].at, undefined);
  assert.deepEqual(tabsOfWindows([{ ...window, minimized: true }]), [{ id: 'a', kind: 'k', label: 'A', windowId: 'a', minimized: true, focusAsked: undefined, at: { x: 1, y: 2 } }]);
  assert.equal(windowHolding([window], 'a'), window);
  assert.equal(windowHolding([window], 'b'), null);
});
