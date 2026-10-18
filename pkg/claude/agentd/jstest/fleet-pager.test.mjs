import test from 'node:test'; import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

// A node's folder, newest first, as the server would serve it with before=.
function nodeFolder(node, rows) {
  const sorted = [...rows].sort((a, b) => b.id - a.id);
  return {
    node,
    read(before, size) {
      const below = before ? sorted.filter((r) => r.id < before) : sorted;
      return { node, rows: below.slice(0, size), hasMore: below.length > size, total: sorted.length };
    },
  };
}

const at = (minute) => `2026-10-18T10:${String(minute).padStart(2, '0')}:00Z`;

test('walking a merged folder page by page shows every row once, newest first', async (t) => {
  const harness = await createPreactHarness(t);
  const p = await harness.importDashboardModule('js/fleet-pager.js');
  // Interleaved clocks, a tie across nodes, and a node whose id order does
  // not follow its timestamps (a message imported with an older time).
  const a = nodeFolder('a', [{ id: 1, created_at: at(1) }, { id: 2, created_at: at(4) }, { id: 3, created_at: at(4) }, { id: 4, created_at: at(9) }, { id: 5, created_at: at(2) }]);
  const b = nodeFolder('b', [{ id: 10, created_at: at(3) }, { id: 11, created_at: at(4) }, { id: 12, created_at: at(8) }]);
  const c = nodeFolder('c', []);
  const vectors = [{}];
  const seen = [];
  for (let page = 1; page <= 10; page++) {
    const cursors = vectors[page - 1];
    const streams = [a, b, c].map((f) => f.read(cursors[f.node], 3));
    const out = p.mergeCursorPage(streams, 3, cursors);
    assert.ok(out.rows.length <= 3);
    seen.push(...out.rows.map((r) => `${r.node}${r.row.id}`));
    if (!out.more) break;
    vectors.push(out.next);
  }
  assert.equal(seen.length, 8, 'every row of every node');
  assert.equal(new Set(seen).size, 8, 'no row twice');
  // Each node's rows keep that node's own order: the merge only takes heads.
  const order = (node) => seen.filter((k) => k.startsWith(node)).map((k) => Number(k.slice(1)));
  assert.deepEqual(order('a'), [5, 4, 3, 2, 1]);
  assert.deepEqual(order('b'), [12, 11, 10]);
});

test('a page carries the cursor of a node that gave it no row', async (t) => {
  const harness = await createPreactHarness(t);
  const p = await harness.importDashboardModule('js/fleet-pager.js');
  const out = p.mergeCursorPage([
    { node: 'a', rows: [{ id: 9, created_at: at(9) }, { id: 8, created_at: at(8) }], hasMore: false },
    { node: 'b', rows: [{ id: 4, created_at: at(1) }], hasMore: true },
  ], 2, { a: 10, b: 5 });
  assert.deepEqual(out.next, { a: 8, b: 5 });
  assert.equal(out.more, true, 'b still has rows');
  assert.equal(p.mergeCursorPage([{ node: 'a', rows: [], hasMore: false }], 2).more, false);
});

test('an older node without cursor paging is cut at the cursor client-side', async (t) => {
  const harness = await createPreactHarness(t);
  const p = await harness.importDashboardModule('js/fleet-pager.js');
  const rows = [6, 5, 4, 3, 2, 1].map((id) => ({ id, created_at: at(id) }));
  assert.deepEqual(p.belowCursor(rows, 4).map((r) => r.id), [3, 2, 1]);
  assert.deepEqual(p.belowCursor(rows, 0).length, 6);
  const s = p.legacyStream('old', rows, 5, 2, 6);
  assert.deepEqual(s.rows.map((r) => r.id), [4, 3]);
  assert.equal(s.hasMore, true);
  assert.equal(p.legacyStream('old', rows, 3, 2, 6).hasMore, false, 'all reachable rows were read and none remain below the window');
  assert.equal(p.legacyStream('old', rows.slice(0, 4), 0, 4, 9).hasMore, true, 'rows past the depth read');
  assert.equal(p.legacyDepth(3, 50), 150);
  assert.equal(p.legacyDepth(30, 50), 500);
});

test('counts and the walk toward a requested page', async (t) => {
  const harness = await createPreactHarness(t);
  const p = await harness.importDashboardModule('js/fleet-pager.js');
  assert.equal(p.pageCount(0, 50), 1);
  assert.equal(p.pageCount(101, 50), 3);
  assert.equal(p.walkTarget(1, [{}]), 1);
  assert.equal(p.walkTarget(5, [{}]), 1, 'only page 1 known: show it and walk on');
  assert.equal(p.walkTarget(5, [{}, { a: 3 }, { a: 1 }]), 3);
  assert.equal(p.walkTarget(2, [{}, { a: 3 }, { a: 1 }]), 2, 'a known page is shown directly');
  assert.equal(p.cursorKey({ b: 2, a: 7 }), 'a<7,b<2');
});
