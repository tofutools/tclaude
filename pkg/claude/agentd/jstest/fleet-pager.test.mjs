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

test('a walk records each page\'s next vector, follows a moved boundary, and restarts when the down nodes change', async (t) => {
  const harness = await createPreactHarness(t);
  const { newWalk, recordVector } = await harness.importDashboardModule('js/fleet-pager.js');
  let w = recordVector(newWalk(), { key: 'k', page: 1, next: { a: 9 }, more: true });
  assert.deepEqual(w.list, [{}, { a: 9 }], 'a new list records page 1 at once');
  w = recordVector(w, { key: 'k', page: 2, next: { a: 5 }, more: true });
  assert.equal(recordVector(w, { key: 'k', page: 1, next: { a: 9 }, more: true }), w, 'an unchanged boundary keeps the walk');
  assert.deepEqual(recordVector(w, { key: 'k', page: 1, next: { a: 12 }, more: true }).list, [{}, { a: 12 }], 'a moved boundary drops the deeper vectors');
  assert.deepEqual(recordVector(w, { key: 'k', page: 2, next: {}, more: false }).list, [{}, { a: 9 }], 'the last page ends the walk');
  assert.deepEqual(recordVector(w, { key: 'k', absent: 'b', page: 3, next: { a: 1 }, more: true }).list, [{}], 'a node going down restarts it');
  assert.deepEqual(recordVector(w, { key: 'other', page: 2, next: { a: 1 }, more: true }).list, [{}], 'another list starts over');
});

test('the merged-list engine applies the fleet liveness rules', async (t) => {
  const harness = await createPreactHarness(t);
  const { createMergedList } = await harness.importDashboardModule('js/fleet-pager.js');
  // Two nodes of 30 rows each, served below a cursor; b can be slowed, held or
  // failed; c is reported offline.
  const a = { id: 'a', name: 'a', local: true }; const b = { id: 'b', name: 'b' }; const c = { id: 'c', name: 'c', online: false };
  const rows = (n) => Array.from({ length: 30 }, (_, i) => ({ id: 30 - i, created_at: new Date(Date.UTC(2026, 0, 1, 0, 0, (30 - i) * 2 + (n === 'b' ? 1 : 0))).toISOString(), n }));
  const data = { a: rows('a'), b: rows('b') };
  const calls = []; let mode = 'ok'; const held = [];
  const readPage = async (node, { before }) => {
    calls.push(node.id);
    if (node.id === 'b' && mode === 'fail') throw Object.assign(new Error('x'), { status: 504 });
    if (node.id === 'b' && mode === 'hold') await new Promise((r) => held.push(r));
    const below = before ? data[node.id].filter((r) => r.id < before) : data[node.id];
    return { rows: below.slice(0, 10), total: 30, totalUnfiltered: 30, hasMore: below.length > 10, cursorPaging: true };
  };
  const timers = { queue: [], setTimeout(fn) { this.queue.push(fn); return this.queue.length; }, clearTimeout() {} };
  const list = createMergedList({ readPage, refusal: (node, e) => `${node.name} ${e.status}`, timers, slowMs: 1 });
  const load = (target, show) => list.load({ nodes: [a, b, c], key: 'k', target, pageSize: 10, show });

  let page = await load(1);
  assert.deepEqual(page.refused.map((r) => [r.node.id, r.offline]), [['c', true]], 'an offline node is not asked, and is no error');
  assert.ok(!calls.includes('c'));
  assert.equal(page.total, 60);

  // A slow node: a new page shows the rest once it is slow; a refresh does not.
  mode = 'hold'; const partials = [];
  const p2 = load(2, (p) => partials.push(p)); await new Promise((r) => setTimeout(r, 0));
  timers.queue.splice(0).forEach((fn) => fn());
  assert.deepEqual(partials.at(-1).pending.map((n) => n.id), ['b'], 'the new page shows without the slow node');
  held.splice(0).forEach((r) => r()); await p2;
  partials.length = 0; const again = load(2, (p) => partials.push(p)); await new Promise((r) => setTimeout(r, 0));
  timers.queue.splice(0).forEach((fn) => fn()); held.splice(0).forEach((r) => r()); await again;
  assert.deepEqual(partials, [], 'a refresh of the page on screen stays whole');

  // A failing node is asked on the page shown, and walked past elsewhere.
  mode = 'fail'; calls.length = 0; page = await load(1);
  assert.equal(page.refused.find((r) => r.node.id === 'b').why, 'b 504');
  calls.length = 0; page = await load(3);
  assert.deepEqual(calls.filter((id) => id === 'b').length, 1, 'only the page shown asks the failing node');

  // A superseded load returns null.
  mode = 'hold'; const stale = load(1); mode = 'ok'; const fresh = load(1);
  held.splice(0).forEach((r) => r());
  assert.equal(await stale, null); assert.ok(await fresh);
});
