import { classifyFailure, summaryURL } from './skynet-model.js';

async function readJSON(response) {
  try { return await response.json(); } catch (_) { return null; }
}

// createSkynetActions loads the federation node list and per-node summaries.
// Summary reads send the last ETag so an unchanged node answers with a bodyless
// 304, per the node-summary caching contract.
export function createSkynetActions({ state, fetchImpl = globalThis.fetch } = {}) {
  if (!state?.setStatus) throw new TypeError('skynet actions require state');
  if (typeof fetchImpl !== 'function') throw new TypeError('skynet actions require fetch');

  async function loadStatus() {
    try {
      const response = await fetchImpl('/api/federation/status?summary=1', { credentials: 'same-origin' });
      if (!response.ok) { state.clearFleet(); return null; }
      return state.setStatus(await readJSON(response));
    } catch (_) {
      // Keep the last known fleet on a transient local failure; the connection
      // banner already reports a dashboard that cannot reach its daemon.
      return state.fleet.value;
    }
  }

  async function loadSummary(node) {
    const prev = state.entry(node.id);
    const headers = prev?.etag ? { 'If-None-Match': prev.etag } : {};
    try {
      const response = await fetchImpl(summaryURL(node), { credentials: 'same-origin', headers });
      if (response.status === 304) { state.commitSummary(node.id, null, prev?.etag); return true; }
      if (!response.ok) { state.failSummary(node.id, classifyFailure(response.status, await readJSON(response))); return false; }
      state.commitSummary(node.id, await readJSON(response), response.headers?.get?.('ETag') || '');
      return true;
    } catch (error) {
      state.failSummary(node.id, classifyFailure(0, { error: String(error?.message || error) }));
      return false;
    }
  }

  return Object.freeze({ loadStatus, loadSummary });
}
