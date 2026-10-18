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
  return status ? `${node.name} did not answer (HTTP ${status})` : `${node.name} is unreachable`;
}

// mergeAudits is one page of the fused "all nodes" view (fleet-pager.js):
// each node's log below its cursor, merged newest first and cut to one page,
// every entry tagged with its node. results are the nodes' reads ({ node,
// data, legacy } or { node, why }); a node on an older build (legacy) was
// read by depth and is cut at its cursor here.
export function mergeAudits(results, pageSize, page = 1, cursors = {}) {
  const refused = [];
  let total = 0; let totalUnfiltered = 0;
  const streams = [];
  for (const { node, data, why, legacy } of results) {
    if (why) { refused.push({ node, why }); continue; }
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
    merged: true, refused, deeper, next: merged.next, more: merged.more,
  };
}

export function createAuditActions({ state, fetchImpl = globalThis.fetch, nodes = fusedAuditNodes, fetchFor = nodeFetch } = {}) {
  if (!state?.beginRequest) throw new TypeError('audit actions require state');
  if (typeof fetchImpl !== 'function') throw new TypeError('audit actions require fetch');
  async function read(fetcher, params) {
    const response = await fetcher(`/api/audit?${params.toString()}`, { credentials: 'same-origin' });
    if (!response.ok) throw Object.assign(new Error(`HTTP ${response.status}`), { status: response.status, body: await response.text() });
    return response.json();
  }
  // walk holds the merged view's known cursor vectors.
  let walk = newWalk();
  // readMerged reads page `page` of every fused node below its cursor.
  function readMerged(fused, view, page, cursors) {
    const params = (extra) => { const p = auditParams({ ...view, page: 1, sort: 'time', dir: 'desc' }); for (const [k, v] of Object.entries(extra)) p.set(k, String(v)); return p; };
    return Promise.all(fused.map(async (node) => {
      const fetcher = node.local ? fetchImpl : fetchFor(node.id);
      const before = cursors[node.id];
      try {
        const data = await read(fetcher, params(before ? { before } : {}));
        if (data?.cursor_paging === true) return { node, data };
        // An older build ignores before=: read it newest first to this page's depth.
        return { node, legacy: true, data: before || page > 1 ? await read(fetcher, params({ page_size: legacyDepth(page, view.pageSize) })) : data };
      } catch (error) { return { node, why: auditRefusal(node, error.status) }; }
    }));
  }
  // loadMerged walks to the requested page one cursor vector at a time,
  // recording each page's next vector as it goes.
  async function loadMerged(fused, view) {
    const key = `${auditParams({ ...view, page: 1, sort: 'time', dir: 'desc' })}\n${fused.map((n) => n.id).join(',')}`;
    let page = walkTarget(view.page, walk.key === key ? walk.list : [{}]);
    for (let restarts = 0; ; ) {
      const cursors = (walk.key === key && walk.list[page - 1]) || {};
      const result = mergeAudits(await readMerged(fused, view, page, cursors), view.pageSize, page, cursors);
      const absent = result.refused.map((r) => r.node.id).join(',');
      walk = recordVector(walk, { key, absent, page, next: result.next, more: result.more });
      // The set of down nodes changed under the walk: restart from page 1
      // (once; a node flapping between reads gets the page as read).
      if (walk.list.length < page && restarts++ < 1) { page = 1; continue; }
      if (page < view.page && walk.list.length > page) { page += 1; continue; }
      return result;
    }
  }
  async function load() {
    const token = state.beginRequest();
    const view = state.view.value;
    const fused = nodes();
    const target = view.node && fused.find((n) => n.id === view.node && !n.local);
    try {
      if (view.node === 'all' && fused.length > 1) return state.commitRequest(token, await loadMerged(fused, view));
      if (target) {
        try { return state.commitRequest(token, await read(fetchFor(target.id), auditParams(view))); }
        catch (error) { throw Object.assign(new Error(auditRefusal(target, error.status)), { status: error.status }); }
      }
      return state.commitRequest(token, await read(fetchImpl, auditParams(view)));
    } catch (error) { state.failRequest(token, error); return false; }
  }
  return Object.freeze({ load, nodes });
}
