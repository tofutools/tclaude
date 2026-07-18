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
      const current = state.view.value;
      const overrides = Object.entries(current.spanOverrides || {})
        .map(([key, hours]) => `${key}:${hours}`).join(',');
      const spans = overrides ? `&spans=${encodeURIComponent(overrides)}` : '';
      const response = await fetchImpl(`/api/usage-history?hours=${current.defaultHours}${spans}`, { credentials: 'same-origin' });
      if (!response.ok) throw new Error(await responseError(response));
      return state.commitRequest(requestId, await response.json());
    } catch (error) {
      state.failRequest(requestId, error);
      return false;
    }
  }
  return Object.freeze({ load });
}
