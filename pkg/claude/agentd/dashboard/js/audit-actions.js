import { auditParams } from './audit-model.js';
import { nodeFetch } from './node-api.js';
import { createMergedList, SLOW_NODE_MS } from './fleet-pager.js';
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

// auditPage is the fused "all nodes" view's response: a merged page
// (fleet-pager.js), every entry tagged with its node.
function auditPage({ rows, total, totalUnfiltered, page, pages, refused, pending, deeper }, pageSize) {
  return {
    entries: rows.map(({ node, row }) => ({ ...row, node })), total, total_unfiltered: totalUnfiltered,
    page, pages, page_size: pageSize, sort: 'time', dir: 'desc', merged: true, refused, pending, deeper,
  };
}

export function createAuditActions({ state, fetchImpl = globalThis.fetch, nodes = fusedAuditNodes, fetchFor = nodeFetch, timers = globalThis, slowMs = SLOW_NODE_MS } = {}) {
  if (!state?.beginRequest) throw new TypeError('audit actions require state');
  if (typeof fetchImpl !== 'function') throw new TypeError('audit actions require fetch');
  async function read(fetcher, params) {
    const response = await fetcher(`/api/audit?${params.toString()}`, { credentials: 'same-origin' });
    if (!response.ok) throw Object.assign(new Error(`HTTP ${response.status}`), { status: response.status, body: await response.text() });
    return response.json();
  }
  // merged pages the fused "all nodes" view, newest first.
  const merged = createMergedList({
    readPage: async (node, { before, depth }, view) => {
      const p = auditParams({ ...view, page: 1, sort: 'time', dir: 'desc' });
      if (before) p.set('before', String(before));
      if (depth) p.set('page_size', String(depth));
      const data = await read(node.local ? fetchImpl : fetchFor(node.id), p);
      return { rows: data?.entries || [], total: data?.total || 0, totalUnfiltered: data?.total_unfiltered || 0, hasMore: !!data?.has_more, cursorPaging: data?.cursor_paging === true };
    },
    refusal: (node, error) => auditRefusal(node, error.status),
    at: (row) => row.at, timers, slowMs,
  });
  async function load() {
    const token = state.beginRequest();
    const view = state.view.value;
    const fused = nodes();
    const target = view.node && fused.find((n) => n.id === view.node && !n.local);
    try {
      if (view.node === 'all' && fused.length > 1) {
        const result = await merged.load({
          nodes: fused, key: auditParams({ ...view, page: 1, sort: 'time', dir: 'desc' }).toString(), target: view.page, pageSize: view.pageSize,
          ctx: view, show: (partial) => state.commitRequest(token, auditPage(partial, view.pageSize)),
        });
        return result ? state.commitRequest(token, auditPage(result, view.pageSize)) : false;
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
