// Unit tests for the Groups tab's per-column show/hide store
// (dashboard/js/member-columns.js), run with Node's BUILT-IN test runner
// (`node --test`, asserting via `node:assert`). No bundler: the test imports
// the same raw ES module the browser loads. The Go wrapper
// `palette_score_node_test.go` (TestPaletteScore_JS) globs `jstest/*.test.mjs`,
// so this suite runs under `go test ./...` with no new wrapper and skips when
// node is absent. Lives OUTSIDE dashboard/ so `//go:embed dashboard` doesn't
// ship the test inside the agentd binary.
//
// Scope: the tri-state visibility contract other features (e.g. the
// task-reference-link Task column) build on — a column follows its own
// default until the user explicitly toggles it, storage holds only
// deviations from that default, non-hideable columns can't be hidden, the
// header/body render off the SAME visible-column list, and a malformed /
// stale pref degrades to "everything at its default".

import test from 'node:test';
import assert from 'node:assert/strict';
import { dashPrefs } from '../dashboard/js/prefs.js';
import { MEMBER_COLS } from '../dashboard/js/sort.js';
import {
  hideableMemberCols, memberColHidden, setMemberColHidden,
  visibleMemberCols, memberColDeviationCount,
} from '../dashboard/js/member-columns.js';

const KEY = 'tclaude.dash.members.hidden';
const reset = () => dashPrefs.removeItem(KEY);
const visKeys = () => visibleMemberCols().map(c => c.key);

// --- defaults ----------------------------------------------------------

test('with no pref, every column is visible and nothing deviates', () => {
  reset();
  assert.deepEqual(visKeys(), MEMBER_COLS.map(c => c.key));
  assert.equal(memberColHidden('id'), false);
  assert.equal(memberColDeviationCount(), 0);
});

// --- hiding a hideable column -----------------------------------------

test('hiding a column drops it from the visible list and counts as a deviation', () => {
  reset();
  setMemberColHidden('id', true);
  assert.equal(memberColHidden('id'), true);
  assert.ok(!visKeys().includes('id'), 'id is gone from the visible columns');
  // The load-bearing identity columns stay regardless.
  assert.ok(visKeys().includes('ctl') && visKeys().includes('title'));
  assert.equal(memberColDeviationCount(), 1);
});

test('order is preserved when a middle column is hidden', () => {
  reset();
  setMemberColHidden('state', true);
  // visibleMemberCols is MEMBER_COLS minus the hidden entry, same order — so
  // the header (sortHead) and each row emit the same cells in the same order.
  assert.deepEqual(visKeys(), MEMBER_COLS.filter(c => c.key !== 'state').map(c => c.key));
});

test('unhiding restores the column and clears the deviation', () => {
  reset();
  setMemberColHidden('id', true);
  setMemberColHidden('id', false);
  assert.equal(memberColHidden('id'), false);
  assert.ok(visKeys().includes('id'));
  assert.equal(memberColDeviationCount(), 0);
});

test('multiple hidden columns each add to the deviation count', () => {
  reset();
  setMemberColHidden('cwd', true);
  setMemberColHidden('branch', true);
  assert.equal(memberColDeviationCount(), 2);
  assert.ok(!visKeys().includes('cwd') && !visKeys().includes('branch'));
});

// --- non-hideable columns ---------------------------------------------

test('the controls and Name columns can never be hidden', () => {
  reset();
  setMemberColHidden('ctl', true);
  setMemberColHidden('title', true);
  assert.equal(memberColHidden('ctl'), false);
  assert.equal(memberColHidden('title'), false);
  assert.ok(visKeys().includes('ctl') && visKeys().includes('title'));
  assert.equal(memberColDeviationCount(), 0);
  // Only the flagged columns are offered in the menu.
  assert.ok(hideableMemberCols().every(c => c.hideable));
  assert.ok(!hideableMemberCols().some(c => c.key === 'ctl' || c.key === 'title'));
});

// --- resilience --------------------------------------------------------

test('a malformed pref degrades to "everything at its default"', () => {
  dashPrefs.setItem(KEY, 'not-json{');
  assert.equal(memberColHidden('id'), false);
  assert.deepEqual(visKeys(), MEMBER_COLS.map(c => c.key));
  assert.equal(memberColDeviationCount(), 0);
  reset();
});

test('a legacy array-shaped pref is ignored rather than misread', () => {
  // An earlier prototype stored a JSON array; the current store is an object
  // map. An array must not be read as deviations — degrade to defaults.
  dashPrefs.setItem(KEY, JSON.stringify(['id', 'cwd']));
  assert.equal(memberColHidden('id'), false);
  assert.equal(memberColDeviationCount(), 0);
  reset();
});

test('a stale key for a removed column is pruned and never counted', () => {
  dashPrefs.setItem(KEY, JSON.stringify({ id: true, ghost_col: true }));
  // ghost_col isn't a real hideable column, so it doesn't count...
  assert.equal(memberColDeviationCount(), 1);
  // ...and the next legit write drops it from storage entirely.
  setMemberColHidden('cwd', true);
  const stored = JSON.parse(dashPrefs.getItem(KEY));
  assert.ok(!('ghost_col' in stored), 'stale key pruned on write');
  assert.deepEqual(Object.keys(stored).sort(), ['cwd', 'id']);
  reset();
});

// --- default-hidden columns (the contract new opt-in columns rely on) --

test('a defaultHidden column starts hidden; showing it is the deviation', () => {
  // No shipped column defaults hidden yet, so exercise the contract against a
  // real MEMBER_COLS entry by flipping its flag for the duration of the test
  // (restored in finally). This is the path a link-style column — e.g. the
  // Task column — plugs into with `defaultHidden: true`.
  const role = MEMBER_COLS.find(c => c.key === 'role');
  const prev = role.defaultHidden;
  role.defaultHidden = true;
  try {
    reset();
    // Hidden out of the box, with NO stored pref — so it's not a deviation.
    assert.equal(memberColHidden('role'), true);
    assert.ok(!visKeys().includes('role'));
    assert.equal(memberColDeviationCount(), 0);
    // Opting it in deviates from the default.
    setMemberColHidden('role', false);
    assert.equal(memberColHidden('role'), false);
    assert.ok(visKeys().includes('role'));
    assert.equal(memberColDeviationCount(), 1);
    // Setting it back to its (hidden) default drops the stored deviation.
    setMemberColHidden('role', true);
    assert.equal(memberColHidden('role'), true);
    assert.equal(memberColDeviationCount(), 0);
  } finally {
    if (prev === undefined) delete role.defaultHidden;
    else role.defaultHidden = prev;
    reset();
  }
});
