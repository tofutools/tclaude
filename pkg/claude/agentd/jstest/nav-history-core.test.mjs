// nav-history-core.test.mjs — pure-logic unit tests for the dashboard
// back/forward navigation core (TCL-317 / TCL-333). Run with Node's built-in
// test runner via the Go wrapper (TestPaletteScore_JS globs jstest/*.test.mjs),
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
// traversal order (#1), disabled-state derivation (#3), duplicate suppression
// (#4), stale-target fallback (#5/#7), plus path <-> location round-tripping.

import test from 'node:test';
import assert from 'node:assert/strict';
import {
  DEFAULT_TAB, defaultLocation, normalizeLocation, locEquals,
  initialState, current, push, back, forward, go,
  canBack, canForward, toPath, fromPath, resolveStale,
} from '../dashboard/js/nav-history-core.js';

// A location is only ever compared through the module's own helpers, so tests
// build them with plain object literals matching the { tab, subtab?, selection? }
// model.
const groups = { tab: 'groups' };
const jobs = { tab: 'jobs' };
const config = { tab: 'config' };
const accessSudo = { tab: 'access', subtab: 'sudo' };
const run = { tab: 'processes', subtab: 'runs', selection: 'run-42' };

test('A -> B -> C then Back visits B then A; Forward visits B then C (AC #1)', () => {
  let s = initialState(groups);      // A
  s = push(s, jobs);                 // B
  s = push(s, config);               // C
  assert.equal(current(s).tab, 'config');

  s = back(s);
  assert.equal(current(s).tab, 'jobs', 'Back from C lands on B');
  s = back(s);
  assert.equal(current(s).tab, 'groups', 'Back from B lands on A');

  s = forward(s);
  assert.equal(current(s).tab, 'jobs', 'Forward from A lands on B');
  s = forward(s);
  assert.equal(current(s).tab, 'config', 'Forward from B lands on C');
});

test('canBack / canForward correct at both ends and mid-stack (AC #3)', () => {
  let s = initialState(groups);
  assert.equal(canBack(s), false, 'no back at the first entry');
  assert.equal(canForward(s), false, 'no forward with a single entry');

  s = push(s, jobs);
  s = push(s, config);              // at tip
  assert.equal(canBack(s), true);
  assert.equal(canForward(s), false, 'no forward at the tip');

  s = back(s);                      // mid-stack
  assert.equal(canBack(s), true, 'can still go further back');
  assert.equal(canForward(s), true, 'can go forward from mid-stack');

  s = back(s);                      // first entry
  assert.equal(canBack(s), false);
  assert.equal(canForward(s), true);
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

test('push after Back truncates the forward tail (browser semantics)', () => {
  let s = initialState(groups);
  s = push(s, jobs);
  s = push(s, config);              // A,B,C  index 2
  s = back(s);                      // index 1 (B), forward tail = [C]
  assert.equal(canForward(s), true);
  s = push(s, accessSudo);          // new nav erases C
  assert.equal(canForward(s), false, 'forward tail gone after a fresh push');
  assert.equal(current(s).tab, 'access');
  assert.equal(s.entries.length, 3, 'A,B,new — C truncated');
  // Confirm C is really gone, not just hidden.
  assert.ok(!s.entries.some(e => e.tab === 'config'));
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

test('back at the first entry / forward at the tip are inert', () => {
  let s = initialState(groups);
  assert.equal(back(s), s, 'Back at the first entry returns same ref');
  assert.equal(forward(s), s, 'Forward at the tip returns same ref');
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
    normalizeLocation({ tab: 'processes', subtab: 'runs', selection: 7 }),
    { tab: 'processes', subtab: 'runs', selection: '7' },
    'selection coerced to string and kept under runs',
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

  assert.equal(toPath(run), '/processes/runs/run-42');
  assert.deepEqual(fromPath('/processes/runs/run-42'),
    { tab: 'processes', subtab: 'runs', selection: 'run-42' });

  // Round-trip every fixture.
  for (const loc of [groups, jobs, config, accessSudo, run]) {
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
  assert.deepEqual(resolved, { tab: 'processes', subtab: 'runs' },
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
