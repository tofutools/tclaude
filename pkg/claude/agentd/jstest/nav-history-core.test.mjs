// nav-history-core.test.mjs — pure-logic unit tests for the dashboard
// back/forward navigation core (TCL-317 / TCL-333). Run with Node's built-in
// test runner via the Go wrapper (TestDashboardJS globs this package's suites),
// so it executes under the repo's single `go test ./...` entry point — no
// bundler, no npm install. To run just this suite directly:
//
//   node --test pkg/claude/agentd/jstest/nav-history-core.test.mjs
//
// Like the other jstest suites, it imports the SAME raw ES module the browser
// loads (../dashboard/js/nav-history-core.js) and lives outside dashboard/ so
// `//go:embed dashboard` doesn't ship the test inside the agentd binary.
//
// Covers TCL-317 acceptance criteria at the logic layer: history creation and
// traversal order (#1), duplicate suppression (#4), stale-target fallback
// (#5/#7), plus path <-> location round-tripping.

import test from 'node:test';
import assert from 'node:assert/strict';
import {
  DEFAULT_TAB, defaultLocation, normalizeLocation, locEquals,
  initialState, current, push, go, indexOf,
  toPath, fromPath, resolveStale, resolvePopstate,
  serializeStack, reviveState, NAV_STATE_VERSION, replaceCurrent,
} from '../dashboard/js/nav-history-core.js';

// A location is only ever compared through the module's own helpers, so tests
// build them with plain object literals matching the { tab, subtab?, selection? }
// model.
const groups = { tab: 'groups' };
const jobs = { tab: 'jobs' };
const config = { tab: 'config' };
const accessSudo = { tab: 'access', subtab: 'sudo' };
const run = { tab: 'processes', subtab: 'runs', selection: 'run-42' };
// The template editor open on `release-train` — /processes/templates/<id>.
const editingTemplate = { tab: 'processes', subtab: 'templates', selection: 'release-train' };

test('A -> B -> C browser traversal visits B, A, B, C (AC #1)', () => {
  let s = initialState(groups);      // A
  s = push(s, jobs);                 // B
  s = push(s, config);               // C
  assert.equal(current(s).tab, 'config');

  s = resolvePopstate(s, jobs, 1);
  assert.equal(current(s).tab, 'jobs', 'Back from C lands on B');
  s = resolvePopstate(s, groups, 0);
  assert.equal(current(s).tab, 'groups', 'Back from B lands on A');

  s = resolvePopstate(s, jobs, 1);
  assert.equal(current(s).tab, 'jobs', 'Forward from A lands on B');
  s = resolvePopstate(s, config, 2);
  assert.equal(current(s).tab, 'config', 'Forward from B lands on C');
});

test('duplicate selection of the current location is suppressed (AC #4)', () => {
  let s = initialState(groups);
  s = push(s, jobs);
  const before = s;
  const after = push(s, { tab: 'jobs' }); // same location again
  assert.equal(after, before, 'push of the current location returns the same state ref');
  assert.equal(after.entries.length, 2, 'no new entry appended');
});

test('duplicate suppression also holds for subtab/selection equality', () => {
  let s = initialState(accessSudo);
  const after = push(s, { tab: 'access', subtab: 'sudo' });
  assert.equal(after, s, 'identical tab+subtab is a no-op push');

  let r = initialState(run);
  const rAfter = push(r, { tab: 'processes', subtab: 'runs', selection: 'run-42' });
  assert.equal(rAfter, r, 'identical selection is a no-op push');
});

// Opening the template editor and closing it again are two distinct locations,
// which is what makes browser Back walk back INTO the editor.
test('opening then closing the template editor is a real back/forward step', () => {
  const list = { tab: 'processes', subtab: 'templates' };
  let s = initialState(list);
  s = push(s, editingTemplate);
  s = push(s, list); // the "← templates" back button
  assert.equal(s.entries.length, 3, 'open and close each recorded an entry');

  s = go(s, s.index - 1);
  assert.ok(locEquals(current(s), editingTemplate), 'Back re-enters the editor');
  assert.equal(toPath(current(s)), '/processes/templates/release-train');
});

test('push after Back truncates the forward tail (browser semantics)', () => {
  let s = initialState(groups);
  s = push(s, jobs);
  s = push(s, config);              // A,B,C  index 2
  s = go(s, 1);                     // index 1 (B), forward tail = [C]
  s = push(s, accessSudo);          // new nav erases C
  assert.equal(current(s).tab, 'access');
  assert.equal(s.entries.length, 3, 'A,B,new — C truncated');
  // Confirm C is really gone, not just hidden.
  assert.ok(!s.entries.some(e => e.tab === 'config'));
});

test('replaceCurrent swaps the current entry in place without changing depth', () => {
  let s = initialState(groups);
  s = push(s, jobs);
  s = push(s, config);            // [groups, jobs, config] @2
  const r = replaceCurrent(s, accessSudo);
  assert.equal(r.index, 2, 'index unchanged');
  assert.equal(r.entries.length, 3, 'no entry added or removed');
  assert.ok(locEquals(current(r), accessSudo), 'current entry swapped');
  assert.ok(locEquals(r.entries[1], jobs), 'other entries untouched');
  // Replacing with the same location is a no-op (same ref) — a reconcile with no
  // drift must not churn history.
  assert.equal(replaceCurrent(r, { tab: 'access', subtab: 'sudo' }), r);
});

test('toPath / fromPath round-trip includes the terminals tab', () => {
  const terminals = { tab: 'terminals' };
  assert.equal(toPath(terminals), '/terminals');
  assert.deepEqual(fromPath('/terminals'), { tab: 'terminals' });
});

test('toPath / fromPath round-trip includes the Usage tab', () => {
  const usage = { tab: 'usage' };
  assert.equal(toPath(usage), '/usage');
  assert.deepEqual(fromPath('/usage'), usage);
});

test('go() jumps to an absolute index and clamps-ignores out-of-range', () => {
  let s = initialState(groups);
  s = push(s, jobs);
  s = push(s, config);              // indices 0,1,2
  assert.equal(current(go(s, 0)).tab, 'groups');
  assert.equal(current(go(s, 1)).tab, 'jobs');
  assert.equal(go(s, 2), s, 'go to the current index is a no-op (same ref)');
  assert.equal(go(s, 9), s, 'out-of-range index is ignored');
  assert.equal(go(s, -1), s, 'negative index is ignored');
  assert.equal(go(s, 1.5), s, 'non-integer index is ignored');
});

test('indexOf finds the last matching entry, or -1 (popstate-recovery helper)', () => {
  let s = initialState(groups);   // A(groups)
  s = push(s, jobs);              // B(jobs)
  s = push(s, config);           // C(config)
  s = push(s, groups);           // D(groups) — groups appears twice now
  assert.equal(indexOf(s, jobs), 1);
  assert.equal(indexOf(s, config), 2);
  assert.equal(indexOf(s, groups), 3, 'returns the LAST occurrence (browser Back moves toward the most recent)');
  assert.equal(indexOf(s, accessSudo), -1, 'absent location -> -1');
  // Normalizes the query location, so a raw/looser input still matches.
  assert.equal(indexOf(s, { tab: 'jobs', subtab: 'ignored' }), 1);
});

test('normalizeLocation drops unknown tab / subtab / misplaced selection', () => {
  assert.deepEqual(normalizeLocation({ tab: 'nope' }), { tab: DEFAULT_TAB });
  assert.deepEqual(
    normalizeLocation({ tab: 'access', subtab: 'bogus' }),
    { tab: 'access' },
    'unknown subtab dropped',
  );
  assert.deepEqual(
    normalizeLocation({ tab: 'groups', selection: 'x' }),
    { tab: 'groups' },
    'selection dropped where no detail view applies',
  );
  assert.deepEqual(
    normalizeLocation({ tab: 'processes', subtab: 'worklist', selection: 'x' }),
    { tab: 'processes' },
    'removed runtime subtabs are dropped',
  );
});

// The open template editor is addressable as /processes/templates/<id>, so the
// URL names the template being edited and the view can be deep-linked.
test('normalizeLocation keeps a template id under the templates subtab', () => {
  assert.deepEqual(
    normalizeLocation({ tab: 'processes', subtab: 'templates', selection: 'release-train' }),
    { tab: 'processes', subtab: 'templates', selection: 'release-train' },
  );
  // A selection needs its subtab: without one the location is just the tab.
  assert.deepEqual(
    normalizeLocation({ tab: 'processes', selection: 'release-train' }),
    { tab: 'processes' },
    'a bare tab cannot carry a selection',
  );
});

test('locEquals is structural and normalizes both sides', () => {
  assert.ok(locEquals({ tab: 'groups' }, { tab: 'groups', subtab: 'ignored' }));
  assert.ok(!locEquals(groups, jobs));
  assert.ok(locEquals({ tab: 'bad' }, defaultLocation()), 'both collapse to default');
});

test('toPath / fromPath round-trip for tab, subtab and selection', () => {
  // Default location is the bare root.
  assert.equal(toPath(groups), '/');
  assert.deepEqual(fromPath('/'), { tab: 'groups' });
  assert.deepEqual(fromPath('/dashboard'), { tab: 'groups' });

  assert.equal(toPath(jobs), '/jobs');
  assert.deepEqual(fromPath('/jobs'), { tab: 'jobs' });

  assert.equal(toPath(accessSudo), '/access/sudo');
  assert.deepEqual(fromPath('/access/sudo'), { tab: 'access', subtab: 'sudo' });

  assert.equal(toPath(run), '/processes');
  assert.deepEqual(fromPath('/processes/runs/run-42'), { tab: 'processes' });

  assert.equal(toPath(editingTemplate), '/processes/templates/release-train');
  assert.deepEqual(fromPath('/processes/templates/release-train'),
    { tab: 'processes', subtab: 'templates', selection: 'release-train' });

  // Round-trip every fixture.
  for (const loc of [groups, jobs, config, accessSudo, run, editingTemplate]) {
    assert.ok(locEquals(fromPath(toPath(loc)), loc), `round-trip ${JSON.stringify(loc)}`);
  }
});

test('fromPath tolerates query, hash, trailing slashes and unknown paths (AC #5)', () => {
  assert.deepEqual(fromPath('/jobs?slop=1'), { tab: 'jobs' }, 'query stripped');
  assert.deepEqual(fromPath('/access/sudo#frag'), { tab: 'access', subtab: 'sudo' });
  assert.deepEqual(fromPath('//jobs//'), { tab: 'jobs' }, 'empty segments ignored');
  assert.deepEqual(fromPath('/totally/unknown'), { tab: 'groups' }, 'unknown tab -> default');
  assert.deepEqual(fromPath(''), { tab: 'groups' });
  assert.deepEqual(fromPath(null), { tab: 'groups' });
});

test('toPath percent-encodes an exotic selection id and fromPath decodes it', () => {
  const loc = { tab: 'processes', subtab: 'runs', selection: 'a/b c' };
  const path = toPath(loc);
  assert.ok(!path.includes(' '), 'space encoded');
  assert.ok(locEquals(fromPath(path), loc), 'decodes back to the same selection');
});

test('resolveStale falls back to the parent list when the selection is gone (AC #7)', () => {
  const gone = () => false;   // predicate: nothing is valid anymore
  const resolved = resolveStale(run, gone);
  assert.deepEqual(resolved, { tab: 'processes' },
    'dead selection dropped, parent list kept');
  assert.ok(!('selection' in resolved));
});

test('resolveStale keeps a still-valid selection, and is a no-op without one', () => {
  const alive = (sel) => sel === 'run-42';
  assert.ok(locEquals(resolveStale(run, alive), run), 'valid selection preserved');
  // No predicate -> treated as valid (no snapshot to check against).
  assert.ok(locEquals(resolveStale(run), run));
  // No selection -> returned normalized, unchanged.
  assert.deepEqual(resolveStale(accessSudo, alive), { tab: 'access', subtab: 'sudo' });
});

test('resolveStale never throws on a malformed location', () => {
  assert.doesNotThrow(() => resolveStale(undefined, () => false));
  assert.deepEqual(resolveStale({ tab: 'bad', selection: 'x' }, () => false), { tab: 'groups' });
});

test('resolvePopstate trusts a stamped index only when it matches the popped URL', () => {
  // Normal in-instance traversal: A,B,C at index 2, Back to B carries navIndex 1.
  let s = initialState(groups);
  s = push(s, jobs);
  s = push(s, config);          // [groups, jobs, config] @2
  const toB = resolvePopstate(s, jobs, 1);
  assert.equal(toB.index, 1, 'a matching in-range index is trusted');
  assert.equal(current(toB).tab, 'jobs');
});

test('resolvePopstate ignores a stale cross-instance index (reload + double Back)', () => {
  // The reviewer scenario: Groups -> Jobs -> Costs, RELOAD at Costs (fresh stack
  // of just [costs]), then Back, Back. The older browser entries still carry
  // their pre-reload navIndex (jobs=1, groups=0), which must NOT be trusted
  // against the smaller fresh stack.
  let s = initialState({ tab: 'costs' });        // fresh post-reload stack: [costs] @0

  // Back → URL /jobs, stale navIndex 1 (out of range for size-1 stack).
  s = resolvePopstate(s, jobs, 1);
  assert.equal(current(s).tab, 'jobs', 'lands on the popped URL, not a stale index');
  assert.ok(locEquals(current(s), fromPath('/jobs')), 'tab matches URL');

  // Back → URL /, stale navIndex 0. It is now IN RANGE for the [jobs] stack, but
  // entries[0] is jobs, not groups — so it must be rejected (this is the bug the
  // pre-fix code hit: it trusted index 0 and stayed on jobs while the URL was /).
  s = resolvePopstate(s, groups, 0);
  assert.equal(current(s).tab, 'groups', 'rejects the in-range-but-mismatched index');
  assert.ok(locEquals(current(s), fromPath('/')), 'tab matches URL after the second Back');
});

test('serializeStack + reviveState round-trip reconstructs the full stack (reload)', () => {
  // Groups -> Jobs -> Costs, then a reload lands on Costs with the persisted
  // history.state. reviveState must rebuild [groups, jobs, costs] @2 instead of
  // reseeding to one.
  let s = initialState(groups);
  s = push(s, jobs);
  s = push(s, config);                 // config stands in for "Costs" here
  const persisted = serializeStack(s);
  assert.equal(persisted.v, NAV_STATE_VERSION);

  const revived = reviveState(persisted, config); // URL on reload = the current entry
  assert.ok(revived, 'a matching payload revives');
  assert.equal(revived.index, 2);
  assert.equal(revived.entries.length, 3, 'full depth restored');
  assert.ok(locEquals(current(revived), config));
});

test('reviveState rejects a stale, cross-URL, out-of-range, or wrong-version payload', () => {
  const good = serializeStack(push(push(initialState(groups), jobs), config)); // @2 = config
  // URL doesn't match the addressed entry → reject (don't restore onto the wrong entry).
  assert.equal(reviveState(good, jobs), null, 'payload index points at config, URL is jobs → reject');
  // Index out of range for the payload's own array.
  assert.equal(reviveState({ v: NAV_STATE_VERSION, navIndex: 9, navStack: [groups] }, groups), null);
  // Wrong / missing version.
  assert.equal(reviveState({ v: 999, navIndex: 0, navStack: [groups] }, groups), null);
  assert.equal(reviveState({ navIndex: 0, navStack: [groups] }, groups), null, 'no version → reject');
  // Malformed / empty.
  assert.equal(reviveState(null, groups), null);
  assert.equal(reviveState({ v: NAV_STATE_VERSION, navIndex: 0, navStack: [] }, groups), null);
  // A revived entry with an unknown tab is normalized, not trusted verbatim.
  const revived = reviveState({ v: NAV_STATE_VERSION, navIndex: 0, navStack: [{ tab: 'bogus' }] }, defaultLocation());
  assert.ok(revived && locEquals(current(revived), defaultLocation()), 'unknown tab normalized to default');
});

test('resolvePopstate relocates within the stack by URL when the index is absent', () => {
  let s = initialState(groups);
  s = push(s, jobs);
  s = push(s, config);          // [groups, jobs, config] @2
  // No usable index (e.g. a clobbered entry) → relocate by URL, preserving depth.
  const r = resolvePopstate(s, jobs, -1);
  assert.equal(r.index, 1, 'found jobs in the existing stack');
  assert.equal(r.entries.length, 3, 'stack depth preserved (not reseeded)');
  // URL not in the stack at all → reseed to a single entry.
  const reseed = resolvePopstate(s, accessSudo, -1);
  assert.equal(reseed.entries.length, 1);
  assert.ok(locEquals(current(reseed), accessSudo));
});
