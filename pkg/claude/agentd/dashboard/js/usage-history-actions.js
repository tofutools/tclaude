async function responseError(response) {
  let body = '';
  try { body = await response.text(); } catch { /* status fallback */ }
  return body || `HTTP ${response.status}`;
}

export function createUsageHistoryActions({ state, fetchImpl = globalThis.fetch } = {}) {
  if (!state?.view || !state?.beginRequest) throw new TypeError('usage history actions require state');
  if (typeof fetchImpl !== 'function') throw new TypeError('usage history actions require fetch');
  let sequence = 0;
  async function load() {
    const requestId = ++sequence;
    state.beginRequest(requestId);
    try {
      const response = await fetchImpl(`/api/usage-history?hours=${state.view.value.hours}`, { credentials: 'same-origin' });
      if (!response.ok) throw new Error(await responseError(response));
      return state.commitRequest(requestId, await response.json());
    } catch (error) {
      state.failRequest(requestId, error);
      return false;
    }
  }
  return Object.freeze({ load });
}
