import { auditParams } from './audit-model.js';
import { nodeFetch } from './node-api.js';
import { legacyDepth, legacyStream, mergeCursorPage, newWalk, pageCount, recordVector, walkTarget, LEGACY_MAX_DEPTH } from './fleet-pager.js';
import { fleetState } from './fleet-state.js';
import { tickedNodes } from './fleet-scope.js';

// fusedAuditNodes are the nodes a fused view's audit can show (this node
// first), or [] outside a fused view (always this node's page).
export function fusedAuditNodes(fleet = fleetState, remote = globalThis.__tclaudeRemoteNode) {
  const value = fleet.fleet.value;
  const fused = fleet.fused.value;
  return value && fused && !remote ? tickedNodes(value, fused) : [];
}

// auditRefusal words a node's refused audit read.
export function auditRefusal(node, status) {
  if (status === 403) return `${node.name} does not share its audit log with you (needs federation.audit.read)`;
  // The peer route gives up after 15s; a slow query alone does not get there.
  if (status === 504) return `${node.name} is online but did not answer within 15s; retrying on the next refresh`;
  return status ? `${node.name} did not answer (HTTP ${status})` : `${node.name} is unreachable`;
}

// mergeAudits is one page of the fused "all nodes" view (fleet-pager.js):
// each node's log below its cursor, merged newest first and cut to one page,
// every entry tagged with its node. results are the nodes' reads ({ node,
// data, legacy }, { node, why }, or { node, pending } while it has not
// answered); a node on an older build (legacy) was read by depth and is cut
// at its cursor here.
export function mergeAudits(results, pageSize, page = 1, cursors = {}) {
  const refused = [];
  const pending = [];
  let total = 0; let totalUnfiltered = 0;
  const streams = [];
  for (const { node, data, why, legacy, offline, pending: waiting } of results) {
    if (why) { refused.push({ node, why, offline: !!offline }); continue; }
    if (waiting) { pending.push(node); continue; }
    total += data?.total || 0; totalUnfiltered += data?.total_unfiltered || 0;
    const rows = data?.entries || [];
    streams.push(legacy ? legacyStream(node.id, rows, cursors[node.id], pageSize, data?.total) : { node: node.id, rows, hasMore: !!data?.has_more });
  }
  const merged = mergeCursorPage(streams, pageSize, cursors, (row) => row.at);
  const byID = Object.fromEntries(results.map(({ node }) => [node.id, node]));
  const deeper = results.filter((r) => r.legacy && (r.data?.total || 0) > LEGACY_MAX_DEPTH).map((r) => r.node);
  return {
    entries: merged.rows.map(({ node, row }) => ({ ...row, node: byID[node] })), total, total_unfiltered: totalUnfiltered,
    page, pages: merged.more ? Math.max(pageCount(total, pageSize), page + 1) : page, page_size: pageSize, sort: 'time', dir: 'desc',
    merged: true, refused, pending, deeper, next: merged.next, more: merged.more,
  };
}

// SLOW_NODE_MS is how long a newly opened merged page waits for every node
// before it shows the ones that answered. Reads of a large log take a second
// or more, so this is well past a busy node's answer; a refresh of the page
// on screen keeps it until every node has answered.
const SLOW_NODE_MS = 2500;

export function createAuditActions({ state, fetchImpl = globalThis.fetch, nodes = fusedAuditNodes, fetchFor = nodeFetch, timers = globalThis, slowMs = SLOW_NODE_MS } = {}) {
  if (!state?.beginRequest) throw new TypeError('audit actions require state');
  if (typeof fetchImpl !== 'function') throw new TypeError('audit actions require fetch');
  async function read(fetcher, params) {
    const response = await fetcher(`/api/audit?${params.toString()}`, { credentials: 'same-origin' });
    if (!response.ok) throw Object.assign(new Error(`HTTP ${response.status}`), { status: response.status, body: await response.text() });
    return response.json();
  }
  // walk holds the merged view's known cursor vectors; walking names the
  // newest merged load, so a superseded walk stops instead of writing them.
  // down holds the nodes whose last read failed (id -> why): a walk steps
  // past them without waiting, and only the page shown reads them again, so
  // a node that does not answer costs one wait, not one per page.
  let walk = newWalk();
  let walking = 0;
  let down = new Map();
  // shownKey names the merged page last committed in full: a refresh of it
  // keeps it on screen until every node has answered.
  let shownKey = '';
  // readMerged reads page `page` of every fused node below its cursor, but
  // the skipped ones. onAnswer sees the results so far as each node answers.
  function readMerged(fused, view, page, cursors, { skip = new Map(), onAnswer } = {}) {
    const params = (extra) => { const p = auditParams({ ...view, page: 1, sort: 'time', dir: 'desc' }); for (const [k, v] of Object.entries(extra)) p.set(k, String(v)); return p; };
    const results = fused.map((node) => (skip.has(node.id) ? { node, why: skip.get(node.id) } : { node, pending: true }));
    const readNode = async (node) => {
      // A node the fleet reports offline is not asked; one that still checks
      // in as online is asked on every read until it is reported offline.
      if (!node.local && node.online === false) return { node, why: `${node.name} is offline`, offline: true };
      const fetcher = node.local ? fetchImpl : fetchFor(node.id);
      const before = cursors[node.id];
      try {
        const data = await read(fetcher, params(before ? { before } : {}));
        if (data?.cursor_paging === true) return { node, data };
        // An older build ignores before=: read it newest first to this page's depth.
        return { node, legacy: true, data: before || page > 1 ? await read(fetcher, params({ page_size: legacyDepth(page, view.pageSize) })) : data };
      } catch (error) { return { node, why: auditRefusal(node, error.status) }; }
    };
    // onAnswer only hears once a node is slow: until then the page waits.
    let slow = false;
    const timer = onAnswer ? timers.setTimeout(() => { slow = true; if (results.some((r) => r.pending)) onAnswer(results.slice()); }, slowMs) : 0;
    return Promise.all(fused.map(async (node, i) => {
      if (skip.has(node.id)) return;
      results[i] = await readNode(node);
      if (slow) onAnswer(results.slice());
    })).then(() => { timers.clearTimeout(timer); return results; });
  }
  // loadMerged walks to the requested page one cursor vector at a time,
  // recording each page's next vector as it goes. The page shown is passed
  // to show as its nodes answer.
  async function loadMerged(fused, view, show) {
    const key = `${auditParams({ ...view, page: 1, sort: 'time', dir: 'desc' })}\n${fused.map((n) => n.id).join(',')}`;
    const id = ++walking;
    let page = walkTarget(view.page, walk.key === key ? walk.list : [{}]);
    // failed are the nodes this load already waited on in vain: a restart's
    // shown page does not wait on them again.
    const failed = new Map();
    for (let restarts = 0; ; ) {
      const cursors = (walk.key === key && walk.list[page - 1]) || {};
      const shown = page >= view.page;
      // A node that was down when the vectors were walked has no cursor in
      // them: its rows wait for the restart rather than show on a later page.
      const uncursored = new Set(page > 1 && walk.key === key ? walk.absent.split(',').filter(Boolean) : []);
      const partialShow = (partial) => {
        if (id !== walking || !partial.some((r) => r.pending)) return;
        show(mergeAudits(partial.map((r) => (uncursored.has(r.node.id) && !r.why ? { node: r.node, pending: true } : r)), view.pageSize, page, cursors));
      };
      const pageKey = `${key}\n${page}`;
      const results = await readMerged(fused, view, page, cursors, shown ? { skip: failed, onAnswer: pageKey === shownKey ? undefined : partialShow } : { skip: down });
      if (id !== walking) return null;
      const result = mergeAudits(results, view.pageSize, page, cursors);
      down = new Map(result.refused.map((r) => [r.node.id, r.why]));
      if (shown) for (const [node, why] of down) failed.set(node, why);
      const absent = result.refused.map((r) => r.node.id).join(',');
      walk = recordVector(walk, { key, absent, page, next: result.next, more: result.more });
      // The set of down nodes changed under the walk: restart from page 1
      // (once; a node flapping between reads gets the page as read).
      if (walk.list.length < page && restarts++ < 1) { page = 1; continue; }
      if (page < view.page && walk.list.length > page) { page += 1; continue; }
      shownKey = pageKey;
      return result;
    }
  }
  async function load() {
    const token = state.beginRequest();
    const view = state.view.value;
    const fused = nodes();
    const target = view.node && fused.find((n) => n.id === view.node && !n.local);
    try {
      if (view.node === 'all' && fused.length > 1) {
        const result = await loadMerged(fused, view, (partial) => state.commitRequest(token, partial));
        return result ? state.commitRequest(token, result) : false;
      }
      if (target) {
        try { return state.commitRequest(token, await read(fetchFor(target.id), auditParams(view))); }
        catch (error) { throw Object.assign(new Error(auditRefusal(target, error.status)), { status: error.status }); }
      }
      return state.commitRequest(token, await read(fetchImpl, auditParams(view)));
    } catch (error) { state.failRequest(token, error); return false; }
  }
  return Object.freeze({ load, nodes });
}
