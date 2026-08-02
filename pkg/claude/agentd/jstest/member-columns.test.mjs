// Unit tests for the Groups tab's per-column show/hide store
// (dashboard/js/member-columns.js), run with Node's BUILT-IN test runner
// (`node --test`, asserting via `node:assert`). No bundler: the test imports
// the same raw ES module the browser loads. The Go wrapper
// `dashboard_node_test.go` (TestDashboardJS) globs the package's `*.test.mjs`,
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

test('with no pref, ID is hidden by default and nothing deviates', () => {
  reset();
  assert.deepEqual(visKeys(), MEMBER_COLS.filter(c => c.key !== 'id').map(c => c.key));
  assert.equal(memberColHidden('id'), true);
  assert.equal(memberColDeviationCount(), 0);
});

// --- hiding a hideable column -----------------------------------------

test('hiding a shown column drops it from the visible list and counts as a deviation', () => {
  reset();
  setMemberColHidden('state', true);
  assert.equal(memberColHidden('state'), true);
  assert.ok(!visKeys().includes('state'), 'state is gone from the visible columns');
  // The load-bearing identity columns stay regardless.
  assert.ok(visKeys().includes('ctl') && visKeys().includes('title'));
  assert.equal(memberColDeviationCount(), 1);
});

test('order is preserved when a middle column is hidden', () => {
  reset();
  setMemberColHidden('state', true);
  // visibleMemberCols is MEMBER_COLS minus the hidden entry, same order — so
  // the header (sortHead) and each row emit the same cells in the same order.
  assert.deepEqual(visKeys(), MEMBER_COLS.filter(c => !['id', 'state'].includes(c.key)).map(c => c.key));
});

test('unhiding restores the column and clears the deviation', () => {
  reset();
  setMemberColHidden('state', true);
  setMemberColHidden('state', false);
  assert.equal(memberColHidden('state'), false);
  assert.ok(visKeys().includes('state'));
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
  assert.equal(memberColHidden('id'), true);
  assert.deepEqual(visKeys(), MEMBER_COLS.filter(c => c.key !== 'id').map(c => c.key));
  assert.equal(memberColDeviationCount(), 0);
  reset();
});

test('a legacy array-shaped pref is ignored rather than misread', () => {
  // An earlier prototype stored a JSON array; the current store is an object
  // map. An array must not be read as deviations — degrade to defaults.
  dashPrefs.setItem(KEY, JSON.stringify(['id', 'cwd']));
  assert.equal(memberColHidden('id'), true);
  assert.equal(memberColDeviationCount(), 0);
  reset();
});

test('a stale key for a removed column is pruned and never counted', () => {
  dashPrefs.setItem(KEY, JSON.stringify({ state: true, ghost_col: true }));
  // ghost_col isn't a real hideable column, so it doesn't count...
  assert.equal(memberColDeviationCount(), 1);
  // ...and the next legit write drops it from storage entirely.
  setMemberColHidden('cwd', true);
  const stored = JSON.parse(dashPrefs.getItem(KEY));
  assert.ok(!('ghost_col' in stored), 'stale key pruned on write');
  assert.deepEqual(Object.keys(stored).sort(), ['cwd', 'state']);
  reset();
});

// --- default-hidden columns (the contract new opt-in columns rely on) --

test('the default-hidden ID column starts hidden; showing it is the deviation', () => {
  reset();
  // Hidden out of the box, with NO stored pref — so it is not a deviation.
  assert.equal(memberColHidden('id'), true);
  assert.ok(!visKeys().includes('id'));
  assert.equal(memberColDeviationCount(), 0);
  // Opting it in deviates from the default.
  setMemberColHidden('id', false);
  assert.equal(memberColHidden('id'), false);
  assert.ok(visKeys().includes('id'));
  assert.equal(memberColDeviationCount(), 1);
  // Setting it back to its (hidden) default drops the stored deviation.
  setMemberColHidden('id', true);
  assert.equal(memberColHidden('id'), true);
  assert.equal(memberColDeviationCount(), 0);
  reset();
});
