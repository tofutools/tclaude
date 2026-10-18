import { logsParams } from './logs-model.js';
import { activityFetch } from './activity-node.js';

export function createLogsActions({ state, fetchImpl = globalThis.fetch, now = () => Date.now(), route = activityFetch } = {}) {
  if (!state?.beginRequest) throw new TypeError('logs actions require state');
  if (typeof fetchImpl !== 'function') throw new TypeError('logs actions require fetch');
  async function load() {
    const token = state.beginRequest();
    const current = state.view.value;
    try {
      const response = await route(fetchImpl)(`/api/logs?${logsParams(current, now()).toString()}`, { credentials: 'same-origin' });
      if (!response.ok) throw Object.assign(new Error(`HTTP ${response.status}`), { body: await response.text() });
      return state.commitRequest(token, await response.json());
    } catch (error) {
      state.failRequest(token, error);
      return false;
    }
  }
  return Object.freeze({ load });
}
