import { auditParams } from './audit-model.js';
import { nodeFetch } from './node-api.js';
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

// mergeAudits is the fused "all nodes" page: each node's newest page, merged
// newest first and cut to one page, every entry tagged with its node.
export function mergeAudits(results, pageSize) {
  const entries = [];
  const refused = [];
  let total = 0; let totalUnfiltered = 0;
  for (const { node, data, why } of results) {
    if (why) { refused.push({ node, why }); continue; }
    total += data?.total || 0; totalUnfiltered += data?.total_unfiltered || 0;
    for (const entry of data?.entries || []) entries.push({ ...entry, node });
  }
  entries.sort((a, b) => String(b.at || '').localeCompare(String(a.at || '')));
  return { entries: entries.slice(0, pageSize), total, total_unfiltered: totalUnfiltered, page: 1, page_size: pageSize, sort: 'time', dir: 'desc', merged: true, refused };
}

export function createAuditActions({ state, fetchImpl = globalThis.fetch, nodes = fusedAuditNodes, fetchFor = nodeFetch } = {}) {
  if (!state?.beginRequest) throw new TypeError('audit actions require state');
  if (typeof fetchImpl !== 'function') throw new TypeError('audit actions require fetch');
  async function read(fetcher, params) {
    const response = await fetcher(`/api/audit?${params.toString()}`, { credentials: 'same-origin' });
    if (!response.ok) throw Object.assign(new Error(`HTTP ${response.status}`), { status: response.status, body: await response.text() });
    return response.json();
  }
  async function load() {
    const token = state.beginRequest();
    const view = state.view.value;
    const fused = nodes();
    const target = view.node && fused.find((n) => n.id === view.node && !n.local);
    try {
      if (view.node === 'all' && fused.length > 1) {
        const params = auditParams({ ...view, page: 1, sort: 'time', dir: 'desc' });
        const results = await Promise.all(fused.map(async (node) => {
          try { return { node, data: await read(node.local ? fetchImpl : fetchFor(node.id), params) }; }
          catch (error) { return { node, why: auditRefusal(node, error.status) }; }
        }));
        return state.commitRequest(token, mergeAudits(results, view.pageSize));
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
