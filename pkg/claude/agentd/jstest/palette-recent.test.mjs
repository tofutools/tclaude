// Unit tests for the command palette's recent-command memory
// (dashboard/js/palette-recent.js) and its wiring into palette-state.js.

import test from 'node:test';
import assert from 'node:assert/strict';
import {
  RECENT_SHOWN, RECENT_STORED, commandKey, createMemoryRecentStore,
  createPrefsRecentStore, parseRecent, rememberRecent, withRecentFirst,
} from '../dashboard/js/palette-recent.js';
import { rankCommands } from '../dashboard/js/palette-score.js';
import { createPreactHarness } from './preact-harness.mjs';

const cmd = (label, extra = {}) => ({ label, run: () => {}, ...extra });

test('rememberRecent moves a key to the front, deduplicates, and caps', () => {
  assert.deepEqual(rememberRecent(['a', 'b', 'c'], 'b'), ['b', 'a', 'c']);
  assert.deepEqual(rememberRecent([], 'a'), ['a']);
  assert.deepEqual(rememberRecent(['a'], ''), ['a']);
  const many = Array.from({ length: RECENT_STORED }, (_, i) => `k${i}`);
  const next = rememberRecent(many, 'new');
  assert.equal(next.length, RECENT_STORED);
  assert.equal(next[0], 'new');
  assert.equal(commandKey(cmd('Label', { id: 'stable' })), 'stable');
  assert.equal(commandKey(cmd('Label')), 'Label');
});

test('withRecentFirst leads with available enabled recents and keeps the rest in build order', () => {
  const list = [cmd('A'), cmd('B'), cmd('C', { enabled: false }), cmd('D'), cmd('E')];
  const out = withRecentFirst(list, ['D', 'gone', 'C', 'B']);
  assert.deepEqual(out.map(c => c.label), ['D', 'B', 'A', 'C', 'E'],
    'missing and disabled recents are skipped, promoted ones are not duplicated');
  assert.deepEqual(out.map(c => !!c.recent), [true, true, false, false, false]);
  assert.deepEqual(out.map(c => !!c.recentLast), [false, true, false, false, false]);
  assert.equal(list[3].recent, undefined, 'source commands are not mutated');
  assert.deepEqual(withRecentFirst(list, []).map(c => c.label), ['A', 'B', 'C', 'D', 'E']);

  const twins = [cmd('Resume agent: worker', { id: 'agent-resume:a' }),
    cmd('Resume agent: worker', { id: 'agent-resume:b' }), cmd('Same'), cmd('Same')];
  assert.deepEqual(withRecentFirst(twins, ['agent-resume:b']).map(c => c.id)[0], 'agent-resume:b',
    'a stable id promotes the exact agent even when titles collide');
  assert.ok(withRecentFirst(twins, ['Same']).every(c => !c.recent),
    'an ambiguous label key is never promoted');

  const wide = Array.from({ length: 10 }, (_, i) => cmd(`c${i}`));
  const keys = wide.map(c => c.label).reverse();
  assert.equal(withRecentFirst(wide, keys).filter(c => c.recent).length, RECENT_SHOWN);
});

test('rankCommands breaks score ties by recency only', () => {
  const list = [cmd('Focus window: alpha'), cmd('Focus window: beta'), cmd('Focus all windows')];
  const rank = new Map([['Focus window: beta', 0]]);
  const recencyOf = c => rank.get(c.label) ?? Infinity;
  assert.deepEqual(rankCommands(list, 'focus window', recencyOf).map(c => c.label).slice(0, 2),
    ['Focus window: beta', 'Focus window: alpha']);
  assert.deepEqual(rankCommands(list, 'focus all', recencyOf).map(c => c.label)[0],
    'Focus all windows', 'a better match still beats a recent one');
});

test('prefs store round-trips and tolerates malformed values', () => {
  const backing = new Map();
  const prefs = { getItem: k => backing.get(k) ?? null, setItem: (k, v) => backing.set(k, String(v)) };
  const store = createPrefsRecentStore(prefs);
  assert.deepEqual(store.load(), []);
  store.save(['b', 'a']);
  assert.deepEqual(store.load(), ['b', 'a']);
  assert.deepEqual(parseRecent('not json'), []);
  assert.deepEqual(parseRecent('{"a":1}'), []);
  assert.deepEqual(parseRecent('["x", 3, ""]'), ['x']);
});

test('palette state shows recents on an empty query, matches once typing, and records runs', async (t) => {
  const harness = await createPreactHarness(t);
  const { createPaletteState } = await harness.importDashboardModule('js/palette-state.js');
  const calls = [];
  const build = () => ['Alpha', 'Beta', 'Gamma', 'Delta'].map(label => cmd(label, { run: () => calls.push(label) }));
  const store = createMemoryRecentStore();
  const state = createPaletteState({
    snapshot: harness.signals.signal({}),
    commandBuilder: build,
    recentStore: store,
  });

  state.show();
  assert.deepEqual(state.filtered.value.map(c => c.label), ['Alpha', 'Beta', 'Gamma', 'Delta']);
  state.setQuery('gam');
  state.runSelected();
  state.show();
  state.setQuery('delta');
  state.runSelected();
  assert.deepEqual(store.load(), ['Delta', 'Gamma'], 'runs persist most-recent first');

  state.show();
  assert.deepEqual(state.filtered.value.map(c => c.label), ['Delta', 'Gamma', 'Alpha', 'Beta']);
  assert.equal(state.selectedCommand().label, 'Delta', 'the last command is pre-selected');
  state.runSelected();
  assert.deepEqual(calls, ['Gamma', 'Delta', 'Delta'], 'Enter on open repeats the last command');

  state.show();
  state.setQuery('a');
  assert.ok(state.filtered.value.every(c => !c.recent), 'typed queries are plain matches');

  // A fresh palette (page reload) reads the persisted history.
  const reloaded = createPaletteState({
    snapshot: harness.signals.signal({}),
    commandBuilder: build,
    recentStore: store,
  });
  reloaded.show();
  assert.equal(reloaded.selectedCommand().label, 'Delta');
});
