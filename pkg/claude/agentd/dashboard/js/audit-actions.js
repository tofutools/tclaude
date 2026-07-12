import { auditParams } from './audit-model.js';

export function createAuditActions({ state, fetchImpl = globalThis.fetch } = {}) {
  if (!state?.beginRequest) throw new TypeError('audit actions require state');
  if (typeof fetchImpl !== 'function') throw new TypeError('audit actions require fetch');
  async function load() {
    const token = state.beginRequest();
    try {
      const response = await fetchImpl(`/api/audit?${auditParams(state.view.value).toString()}`, { credentials: 'same-origin' });
      if (!response.ok) throw Object.assign(new Error(`HTTP ${response.status}`), { body: await response.text() });
      return state.commitRequest(token, await response.json());
    } catch (error) { state.failRequest(token, error); return false; }
  }
  return Object.freeze({ load });
}
