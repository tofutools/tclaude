// fleet-pager.js — DOM-free paging for a list merged from several nodes
// (tcl-6lr184). It is the pattern for any fleet view that pages one list
// spread over the nodes in view: each node keeps its own rows, is queried on
// demand, and nothing is copied into this node's database, so revoking a
// peer's access cuts its rows off at once.
//
// Every node serves its list newest-first with a keyset cursor: `before=<id>`
// returns the rows older than that id in the node's own order, with `total`
// (the filtered count), `has_more`, and `cursor_paging: true` so a client can
// tell it from an older build that ignores the parameter.
//
// A merged page is a cursor vector — for each node, the id its next read
// starts below (absent: from the newest). Page p reads up to pageSize rows
// from every node below its cursor, merges the streams by taking the newest
// head each time, and keeps the first pageSize rows. Each stream only ever
// gives up a prefix, so the last row taken from a node is that node's cursor
// on page p+1: no row is skipped or shown twice, however the nodes' clocks
// interleave. A page reads at most nodes×pageSize rows, polling re-reads the
// same vector (new arrivals only move page 1), and Prev is the vector kept
// for the page before. There is no random access: a later page is reached by
// walking forward (walkTarget), which is bounded by the page count. The known
// vectors form a walk, kept current by recordVector.
//
// A node on an older build pages by offset only. It is read newest-first to
// the depth the page needs (legacyDepth) and its rows are cut at the cursor
// client-side (belowCursor), which keeps the merge exact down to MAX_DEPTH.

// LEGACY_MAX_DEPTH is the deepest an older node can be read in one request
// (the server's largest page size).
export const LEGACY_MAX_DEPTH = 500;

function ts(s) {
  const t = Date.parse(s || '');
  return Number.isFinite(t) ? t : 0;
}

// newer orders two heads newest first: their time (at), then node id, then
// the node's own id (higher is newer), so the merge is a total order.
function newer(a, b, at) {
  const d = ts(at(b.row)) - ts(at(a.row));
  if (d) return d < 0;
  if (a.node !== b.node) return String(a.node) < String(b.node);
  return Number(a.row.id) > Number(b.row.id);
}

// mergeCursorPage merges one page from each node's stream.
//   streams: [{ node: id, rows: newest-first rows (each with id, created_at),
//              hasMore: the node has rows past these }]
//   cursors: the page's cursor vector ({ [node]: id }), carried into next
//            for a node that gives up no row on this page.
//   at:      a row's timestamp (created_at unless the list names it otherwise).
// It returns the page's rows (each { node, row }), the next page's cursor
// vector, and whether any node has rows past this page.
export function mergeCursorPage(streams, pageSize, cursors = {}, at = (row) => row.created_at) {
  const heads = streams.map((s) => ({ node: s.node, rows: s.rows || [], at: 0, hasMore: !!s.hasMore }));
  const rows = [];
  const next = { ...cursors };
  while (rows.length < pageSize) {
    let best = null;
    for (const h of heads) {
      if (h.at >= h.rows.length) continue;
      const cand = { node: h.node, row: h.rows[h.at], h };
      if (!best || newer(cand, best, at)) best = cand;
    }
    if (!best) break;
    best.h.at += 1;
    rows.push({ node: best.node, row: best.row });
    next[best.node] = best.row.id;
  }
  const more = heads.some((h) => h.at < h.rows.length || h.hasMore);
  return { rows, next, more };
}

// belowCursor keeps an older node's rows that a cursor read would return.
export function belowCursor(rows, before) {
  return before ? (rows || []).filter((r) => Number(r.id) < Number(before)) : rows || [];
}

// legacyDepth is how far back to read an older node for page `page`.
export function legacyDepth(page, pageSize) {
  return Math.min(LEGACY_MAX_DEPTH, page * pageSize);
}

// legacyStream turns an older node's newest-first read into the stream the
// merge expects: the rows below the cursor, at most pageSize, and whether it
// has more (rows it did not return, or rows past this window).
export function legacyStream(node, rows, before, pageSize, total) {
  const below = belowCursor(rows, before);
  const reachable = Math.min(total || 0, LEGACY_MAX_DEPTH);
  return { node, rows: below.slice(0, pageSize), hasMore: below.length > pageSize || (rows || []).length < reachable };
}

// pageCount derives the pager's page count from the nodes' summed totals.
export function pageCount(total, pageSize) {
  return Math.max(1, Math.ceil((total || 0) / pageSize));
}

// cursorKey names a cursor vector, for a read signature.
export function cursorKey(cursors) {
  return Object.keys(cursors || {}).sort().map((k) => `${k}<${cursors[k]}`).join(',');
}

// walkTarget is the page to show on the way to a requested one. vectors
// holds the known cursor vectors, vectors[p - 1] for page p (page 1's, {},
// is always known). A page whose vector is known is shown directly; past
// them, the deepest known page is shown, and its merge yields the next
// vector, so the walk advances one read per page.
export function walkTarget(target, vectors) {
  return Math.max(1, Math.min(target, vectors.length));
}

// A walk is the known cursor vectors of one merged list: { key, absent,
// list }, list[p - 1] for page p. key names the list (filters, page size,
// nodes in view); absent names the nodes that were down while it was walked.
export const newWalk = (key = '', absent = '') => ({ key, absent, list: [{}] });

// recordVector folds a merged page into its walk once every node has
// answered or is down. The shown page's merge is the truth for where the next
// page starts: polling moves page 1 as rows arrive, and a deletion moves any
// page, so a changed boundary drops the deeper vectors derived from the old
// one. A down node has no cursor in the vectors, so when the set of down nodes
// changes every page past the first is stale and the walk restarts. It
// returns the walk unchanged (the same object) when nothing moved.
export function recordVector(walk, { key, absent = '', page, next, more }) {
  if (walk.key !== key || walk.absent !== absent) {
    return page === 1 && more ? { key, absent, list: [{}, next] } : newWalk(key, absent);
  }
  const { list } = walk;
  if (list.length < page) return walk;
  if (!more) return list.length > page ? { ...walk, list: list.slice(0, page) } : walk;
  if (list.length > page && cursorKey(list[page]) === cursorKey(next)) return walk;
  return { ...walk, list: [...list.slice(0, page), next] };
}

// mergeNodePages merges one page from the nodes' reads. results are, per
// node, { node, page } (page: { rows, total, totalUnfiltered, hasMore,
// legacy }), { node, why, offline } when it was refused or is down, or
// { node, pending } while it has not answered. It returns the merged page:
// rows ({ node, row }, node being the node object), the summed counts, the
// page count, the refused / pending nodes, the older-build nodes whose rows
// run past LEGACY_MAX_DEPTH (deeper), and the next cursor vector.
export function mergeNodePages(results, pageSize, page = 1, cursors = {}, at = (row) => row.created_at) {
  const refused = [];
  const pending = [];
  const streams = [];
  let total = 0; let totalUnfiltered = 0;
  for (const r of results) {
    if (r.why) { refused.push({ node: r.node, why: r.why, offline: !!r.offline }); continue; }
    if (r.pending) { pending.push(r.node); continue; }
    const p = r.page;
    total += p.total || 0; totalUnfiltered += p.totalUnfiltered || 0;
    streams.push(p.legacy ? legacyStream(r.node.id, p.rows, cursors[r.node.id], pageSize, p.total) : { node: r.node.id, rows: p.rows || [], hasMore: !!p.hasMore });
  }
  const merged = mergeCursorPage(streams, pageSize, cursors, at);
  const byID = Object.fromEntries(results.map(({ node }) => [node.id, node]));
  return {
    rows: merged.rows.map(({ node, row }) => ({ node: byID[node], row })), total, totalUnfiltered,
    page, pages: merged.more ? Math.max(pageCount(total, pageSize), page + 1) : page,
    refused, pending, deeper: results.filter((r) => r.page?.legacy && (r.page.total || 0) > LEGACY_MAX_DEPTH).map((r) => r.node),
    next: merged.next, more: merged.more,
  };
}

// SLOW_NODE_MS is how long a newly opened merged page waits for every node
// before it shows the ones that answered. Reads of a large list take a second
// or more, so this is well past a busy node's answer; a refresh of the page on
// screen keeps it until every node has answered.
export const SLOW_NODE_MS = 2500;

// createMergedList is the engine of one fused list merged from several nodes:
// it walks the cursor vectors to the page asked for, and applies the fleet's
// liveness rules, so every merged view behaves alike:
//   - A node the fleet reports offline is not asked, and shows as offline
//     rather than as an error.
//   - A node that checks in as online is asked on every read; it is given up
//     on only when its read fails (a peer read times out after 15s), and is
//     asked again on the next refresh.
//   - A slow node does not hold up the page: a newly opened page shows the
//     nodes that answered after slowMs, naming the ones still awaited; a
//     refresh of the page on screen keeps it whole until all have answered.
//   - A walk steps past the nodes whose last read failed (only the page shown
//     asks them again), so a silent node costs one wait, not one per page.
//   - A load superseded by a newer one stops without touching the vectors.
//
// readPage(node, { before, depth }, ctx) reads one node's newest-first page:
// below the cursor `before`, or (for an older build, depth set) its newest
// `depth` rows; ctx is what the load was given (its filters). It returns { rows, total, totalUnfiltered, hasMore, cursorPaging } or
// throws an error with .status; refusal(node, error) words that failure. at is
// a row's timestamp.
export function createMergedList({ readPage, refusal, at = (row) => row.created_at, timers = globalThis, slowMs = SLOW_NODE_MS }) {
  let walk = newWalk();
  let walking = 0;
  let down = new Map();
  let shownKey = '';
  async function readNode(node, page, pageSize, before, ctx) {
    if (!node.local && node.online === false) return { node, why: `${node.name} is offline`, offline: true };
    try {
      const got = await readPage(node, { before }, ctx);
      if (got.cursorPaging) return { node, page: got };
      // An older build ignores the cursor: read it newest first to this
      // page's depth and cut it at the cursor in the merge.
      const deep = before || page > 1 ? await readPage(node, { depth: legacyDepth(page, pageSize) }, ctx) : got;
      return { node, page: { ...deep, legacy: true } };
    } catch (error) { return { node, why: refusal(node, error) }; }
  }
  // readAll reads page `page` of every node below its cursor, but the
  // skipped ones. onAnswer hears the results so far once a node is slow.
  function readAll(nodes, page, pageSize, cursors, ctx, { skip = new Map(), onAnswer } = {}) {
    const results = nodes.map((node) => (skip.has(node.id) ? { node, why: skip.get(node.id) } : { node, pending: true }));
    let slow = false;
    const timer = onAnswer ? timers.setTimeout(() => { slow = true; if (results.some((r) => r.pending)) onAnswer(results.slice()); }, slowMs) : 0;
    return Promise.all(nodes.map(async (node, i) => {
      if (skip.has(node.id)) return;
      results[i] = await readNode(node, page, pageSize, cursors[node.id], ctx);
      if (slow) onAnswer(results.slice());
    })).then(() => { timers.clearTimeout(timer); return results; });
  }
  // load walks to page `target` of the list named `key` (its filters; the
  // nodes and page size are added here) and returns the merged page, or null
  // when a newer load superseded it. show(page) hears a partial page; ctx is
  // passed to readPage.
  async function load({ nodes, key: listKey, target, pageSize, show = () => {}, ctx }) {
    const key = `${listKey}\n${pageSize}\n${nodes.map((n) => n.id).join(',')}`;
    const id = ++walking;
    let page = walkTarget(target, walk.key === key ? walk.list : [{}]);
    // failed are the nodes this load already waited on in vain: a restart's
    // shown page does not wait on them again.
    const failed = new Map();
    for (let restarts = 0; ; ) {
      const cursors = (walk.key === key && walk.list[page - 1]) || {};
      const shown = page >= target;
      // A node that was down when the vectors were walked has no cursor in
      // them: its rows wait for the restart rather than show on a later page.
      const uncursored = new Set(page > 1 && walk.key === key ? walk.absent.split(',').filter(Boolean) : []);
      const partial = (results) => {
        if (id !== walking || !results.some((r) => r.pending)) return;
        show(mergeNodePages(results.map((r) => (uncursored.has(r.node.id) && !r.why ? { node: r.node, pending: true } : r)), pageSize, page, cursors, at));
      };
      const pageKey = `${key}\n${page}`;
      const results = await readAll(nodes, page, pageSize, cursors, ctx, shown ? { skip: failed, onAnswer: pageKey === shownKey ? undefined : partial } : { skip: down });
      if (id !== walking) return null;
      const result = mergeNodePages(results, pageSize, page, cursors, at);
      down = new Map(result.refused.map((r) => [r.node.id, r.why]));
      if (shown) for (const [node, why] of down) failed.set(node, why);
      walk = recordVector(walk, { key, absent: result.refused.map((r) => r.node.id).join(','), page, next: result.next, more: result.more });
      // The set of down nodes changed under the walk: restart from page 1
      // (once; a node flapping between reads gets the page as read).
      if (walk.list.length < page && restarts++ < 1) { page = 1; continue; }
      if (page < target && walk.list.length > page) { page += 1; continue; }
      shownKey = pageKey;
      return result;
    }
  }
  return Object.freeze({ load });
}
