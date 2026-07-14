// Unit tests for the dashboard's column-sort specs for the virtual
// sub-tables (Retired / Conversations / Pending), run with Node's BUILT-IN
// test runner (`node --test`, asserting via `node:assert`). No bundler: the
// test imports the same raw ES module the browser loads. The Go wrapper
// `palette_score_node_test.go` (TestPaletteScore_JS) globs `jstest/*.test.mjs`,
// so this suite runs under `go test ./...` with no new wrapper and skips when
// node is absent. Lives OUTSIDE dashboard/ so `//go:embed dashboard` doesn't
// ship the test inside the agentd binary.
//
// Scope: the per-table column specs + value accessors added when the virtual
// "non-real" groups (Retired / Conversations / Pending) gained sortable,
// agent-id-leading headers like real groups. applySort / sortHead / cycleSort
// themselves are the pre-existing generic machinery (already exercised by the
// members/cron/sudo/links/replaced tables) — what's new and worth locking in
// is that THESE tables expose the right sortable columns, sort the id column on
// the stable agent_id (conv-id fallback), and tag their headers with the table
// key the click handler routes on.

import test from 'node:test';
import assert from 'node:assert/strict';
import {
  cycleSort, sortHead, applySort, loadSortState,
  persistTableSort,
  MEMBER_COLS,
  RETIRED_COLS, RETIRED_ACCESSORS,
  CONVERSATIONS_COLS, CONVERSATIONS_ACCESSORS,
  PENDING_COLS, PENDING_ACCESSORS,
} from '../dashboard/js/sort.js';
import { dashPrefs } from '../dashboard/js/prefs.js';

// colKeys lists every sortable column key in a spec (entries with a `col`),
// in order. The leading online-dot and trailing action columns carry no
// `col`, so they're correctly excluded — proving they stay non-sortable.
const colKeys = cols => cols.filter(c => c.col).map(c => c.col);

test('legacy sort writes preserve feature-island entries added after boot', () => {
  dashPrefs.setItem('tclaude.dash.sort', JSON.stringify({ sudo: { col: 'slug', dir: 'asc' } }));
  loadSortState();
  persistTableSort('jobs', { col: 'name', dir: 'desc' });
  cycleSort('sudo', 'slug');
  assert.deepEqual(JSON.parse(dashPrefs.getItem('tclaude.dash.sort')), {
    sudo: { col: 'slug', dir: 'desc' },
    jobs: { col: 'name', dir: 'desc' },
  });
  dashPrefs.removeItem('tclaude.dash.sort');
  loadSortState();
});

// --- Retired -----------------------------------------------------------

test('RETIRED spec exposes the expected sortable columns, dot + actions inert', () => {
  // First and last cells are the online dot and the reinstate-button cell:
  // both label-less and non-sortable.
  assert.equal(RETIRED_COLS[0].col, undefined);
  assert.equal(RETIRED_COLS[RETIRED_COLS.length - 1].col, undefined);
  assert.deepEqual(colKeys(RETIRED_COLS), ['id', 'title', 'retired', 'by', 'reason']);
});

test('RETIRED id accessor leads with the stable agent_id, conv-id fallback', () => {
  // The headline change: the id column shows + sorts on agent_id when present.
  assert.equal(RETIRED_ACCESSORS.id({ agent_id: 'agt_abc', conv_id: 'c1' }), 'agt_abc');
  // A retired row with no stable agent_id falls back to its conv-id.
  assert.equal(RETIRED_ACCESSORS.id({ conv_id: 'c1' }), 'c1');
});

test('RETIRED accessors read the audit fields, "by" prefers the resolved display', () => {
  const row = {
    title: 'worker', retired_at: '2026-06-01T00:00:00Z',
    retired_by_display: 'po (agt_999)', retired_by: 'raw-conv', retire_reason: 'done',
  };
  assert.equal(RETIRED_ACCESSORS.title(row), 'worker');
  assert.equal(RETIRED_ACCESSORS.retired(row), '2026-06-01T00:00:00Z');
  assert.equal(RETIRED_ACCESSORS.by(row), 'po (agt_999)');
  assert.equal(RETIRED_ACCESSORS.reason(row), 'done');
  // Falls back to the raw retired_by when no resolved display is present.
  assert.equal(RETIRED_ACCESSORS.by({ retired_by: 'human' }), 'human');
});

test("applySort('retired', …) orders on the agent_id the id column shows", () => {
  loadSortState(); // reset to {} (empty dashPrefs cache under node)
  const rows = [
    { agent_id: 'agt_ccc', conv_id: 'x', title: 'c' },
    { agent_id: 'agt_aaa', conv_id: 'y', title: 'a' },
    { conv_id: 'agt_bbb_via_conv', title: 'b' }, // no agent_id → conv-id key
  ];
  // No active sort → server order preserved untouched.
  assert.deepEqual(applySort('retired', rows, RETIRED_ACCESSORS).map(r => r.title), ['c', 'a', 'b']);
  // One click → ascending by id (agent_id, conv-id fallback):
  // agt_aaa < agt_bbb_via_conv < agt_ccc → titles a, b, c.
  cycleSort('retired', 'id');
  assert.deepEqual(applySort('retired', rows, RETIRED_ACCESSORS).map(r => r.title), ['a', 'b', 'c']);
  // Second click → descending.
  cycleSort('retired', 'id');
  assert.deepEqual(applySort('retired', rows, RETIRED_ACCESSORS).map(r => r.title), ['c', 'b', 'a']);
  // Third click → back to server order.
  cycleSort('retired', 'id');
  assert.deepEqual(applySort('retired', rows, RETIRED_ACCESSORS).map(r => r.title), ['c', 'a', 'b']);
});

test("sortHead('retired', …) tags headers with the table key the click handler routes on", () => {
  loadSortState();
  const html = sortHead('retired', RETIRED_COLS);
  assert.match(html, /data-sort-table="retired"/);
  assert.match(html, /data-sort-col="id"/);
  assert.match(html, /data-sort-col="reason"/);
  // The dot/action columns render as plain, non-clickable headers.
  assert.match(html, /<th><\/th>/);
});

test("member headers carry wizard-mode labels for class, quest and lore", () => {
  const regular = sortHead('members', MEMBER_COLS);
  assert.match(regular, /class="theme-copy-regular">Role<\/span><span class="theme-copy-wizard">Class<\/span>/);
  assert.match(regular, /class="theme-copy-regular">Task link<\/span><span class="theme-copy-wizard">Quest<\/span>/);
  assert.match(regular, /class="theme-copy-regular">Description<\/span><span class="theme-copy-wizard">Lore<\/span>/);
  assert.match(regular, /data-sort-col="role" title="Sort by Role"/);
  assert.match(regular, /data-sort-col="task" title="Sort by Task link"/);
  assert.match(regular, /data-sort-col="descr" title="Sort by Description"/);

  const wizard = sortHead('members', MEMBER_COLS, true);
  assert.match(wizard, /data-sort-col="role" title="Sort by Class"/);
  assert.match(wizard, /data-sort-col="task" title="Sort by Quest"/);
  assert.match(wizard, /data-sort-col="descr" title="Sort by Lore"/);
});

// --- Conversations -----------------------------------------------------

test('CONVERSATIONS spec stays conv-id keyed (plain conversations have no agent_id)', () => {
  assert.equal(CONVERSATIONS_COLS[0].col, undefined);
  assert.equal(CONVERSATIONS_COLS[CONVERSATIONS_COLS.length - 1].col, undefined);
  assert.deepEqual(colKeys(CONVERSATIONS_COLS), ['conv', 'title', 'last']);
  assert.equal(CONVERSATIONS_ACCESSORS.conv({ conv_id: 'c1' }), 'c1');
  assert.equal(CONVERSATIONS_ACCESSORS.title({ title: 't' }), 't');
  assert.equal(CONVERSATIONS_ACCESSORS.last({ modified: '2026-06-02T00:00:00Z' }), '2026-06-02T00:00:00Z');
});

test("applySort('conversations', …) orders by last-activity timestamp", () => {
  loadSortState();
  const rows = [
    { conv_id: 'a', modified: '2026-06-03T00:00:00Z' },
    { conv_id: 'b', modified: '2026-06-01T00:00:00Z' },
    { conv_id: 'c', modified: '2026-06-02T00:00:00Z' },
  ];
  cycleSort('conversations', 'last');
  assert.deepEqual(applySort('conversations', rows, CONVERSATIONS_ACCESSORS).map(r => r.conv_id), ['b', 'c', 'a']);
  loadSortState();
});

// --- Pending -----------------------------------------------------------

test('PENDING spec exposes its sortable columns, dot + focus action inert', () => {
  assert.equal(PENDING_COLS[0].col, undefined);
  assert.equal(PENDING_COLS[PENDING_COLS.length - 1].col, undefined);
  assert.deepEqual(colKeys(PENDING_COLS), ['id', 'name', 'group', 'dir', 'age']);
  assert.deepEqual(PENDING_COLS.map(c => c.label), ['', 'ID', 'Name', 'Group', 'CWD', 'Age', '']);
});

test('PENDING name accessor falls back from name to role', () => {
  assert.equal(PENDING_ACCESSORS.name({ name: 'alice', role: 'dev' }), 'alice');
  assert.equal(PENDING_ACCESSORS.name({ role: 'dev' }), 'dev');
  assert.equal(PENDING_ACCESSORS.id({ agent_id: 'agt_123', label: 'lbl-1' }), 'agt_123');
  assert.equal(PENDING_ACCESSORS.id({ label: 'lbl-1' }), 'lbl-1');
  assert.equal(PENDING_ACCESSORS.group({ group: 'g' }), 'g');
  assert.equal(PENDING_ACCESSORS.dir({ cwd: '/tmp/x' }), '/tmp/x');
  assert.equal(PENDING_ACCESSORS.age({ created_at: '2026-06-04T00:00:00Z' }), '2026-06-04T00:00:00Z');
});

test("applySort('pending', …) blank cells sort last regardless of direction", () => {
  loadSortState();
  const rows = [
    { label: 'p1', group: 'beta' },
    { label: 'p2', group: '' },     // ungrouped → blank group
    { label: 'p3', group: 'alpha' },
  ];
  cycleSort('pending', 'group'); // ascending
  // alpha, beta, then the blank-group row last.
  assert.deepEqual(applySort('pending', rows, PENDING_ACCESSORS).map(r => r.label), ['p3', 'p1', 'p2']);
  cycleSort('pending', 'group'); // descending — blank STILL last.
  assert.deepEqual(applySort('pending', rows, PENDING_ACCESSORS).map(r => r.label), ['p1', 'p3', 'p2']);
  loadSortState();
});
