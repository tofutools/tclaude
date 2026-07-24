import { dayKey, spanRange } from './costs-model.js';

async function responseError(response) {
  let body = '';
  try { body = await response.text(); } catch { /* use status */ }
  if (body) {
    try {
      const parsed = JSON.parse(body);
      return parsed.error || parsed.message || body;
    } catch { return body; }
  }
  return `HTTP ${response.status}`;
}

export function createCostsActions({ state, fetchImpl = globalThis.fetch } = {}) {
  if (!state?.view || !state?.beginRequest) throw new TypeError('costs actions require state');
  if (typeof fetchImpl !== 'function') throw new TypeError('costs actions require fetch');
  let loadSequence = 0;
  let factorSaveQueue = Promise.resolve();

  async function load() {
    const sequence = ++loadSequence;
    state.beginRequest(sequence);
    const current = state.view.value;
    const range = spanRange(current.span, current.monthOffset);
    const path = '/api/costs?from=' + encodeURIComponent(dayKey(range.from))
      + '&to=' + encodeURIComponent(dayKey(range.to));
    try {
      const response = await fetchImpl(path, { credentials: 'same-origin' });
      if (!response.ok) throw new Error(await responseError(response));
      const data = await response.json();
      return state.commitRequest(sequence, data);
    } catch (error) {
      state.failRequest(sequence, error);
      return false;
    }
  }

  async function loadFactor() {
    const token = state.beginFactor('');
    try {
      const response = await fetchImpl('/api/cost-factor', { credentials: 'same-origin' });
      if (!response.ok) throw new Error(`HTTP ${response.status}`);
      const data = await response.json();
      const value = Number(data.estimate_factor);
      const raw = Number.isFinite(value) && value !== 1 ? String(+value.toFixed(4)) : '';
      return state.commitFactor(token, { raw, status: '' });
    } catch (error) {
      // Loading the factor is best-effort and must not block cost history.
      state.commitFactor(token, { status: '' });
      return false;
    }
  }

  function saveFactor(raw) {
    raw = String(raw ?? '').trim();
    let value = null;
    if (raw !== '') {
      value = Number(raw);
      if (!Number.isFinite(value) || value <= 0 || value > 10) {
        const token = state.beginFactor('');
        state.failFactor(token, 'must be 0–10');
        return Promise.resolve(false);
      }
    }
    const token = state.beginFactor('saving…');
    // The endpoint persists a shared scalar and has no revision precondition.
    // Serialize POSTs in input order so an older, slower request can never
    // overwrite a newer value on the server. Tokens still prevent obsolete
    // responses from changing the latest input/status in the client.
    const operation = factorSaveQueue.then(async () => {
      try {
        const response = await fetchImpl('/api/cost-factor', {
          method: 'POST', credentials: 'same-origin',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ estimate_factor: value }),
        });
        if (!response.ok) throw new Error(await responseError(response));
        if (!state.commitFactor(token, { status: 'saved' })) return false;
        await load();
        return true;
      } catch (error) {
        state.failFactor(token, error);
        return false;
      }
    });
    factorSaveQueue = operation.then(() => undefined, () => undefined);
    return operation;
  }

  return Object.freeze({ load, loadFactor, saveFactor });
}
