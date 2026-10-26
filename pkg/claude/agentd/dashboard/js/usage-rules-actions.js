// HTTP calls behind the "Usage limits & budgets" dialog: the dashboard's
// cookie-auth twins of /v1/usage/* (usage_rules_api.go).
const API = '/api/usage';

async function request(path, options = {}, fetchImpl = (...args) => globalThis.fetch(...args)) {
  const init = { credentials: 'same-origin', ...options };
  if (options.body !== undefined) {
    init.headers = { 'Content-Type': 'application/json', ...(options.headers || {}) };
    init.body = JSON.stringify(options.body);
  }
  const response = await fetchImpl(API + path, init);
  if (!response.ok) {
    const text = await response.text();
    let message = text;
    try { message = JSON.parse(text).error || text; } catch { /* plain text */ }
    throw new Error(message || `HTTP ${response.status}`);
  }
  return response.status === 204 ? null : response.json();
}

export function createUsageRulesApi(fetchImpl) {
  const call = (path, options) => request(path, options, fetchImpl);
  return Object.freeze({
    overview: () => call('/rules'),
    createLimit: ({ scope, group, rule, enabled = true }) => call('/limits', { method: 'POST', body: { scope, group, rule, enabled } }),
    updateLimit: (id, { rule, enabled }) => call(`/limits/${id}`, { method: 'PUT', body: { rule, enabled } }),
    deleteLimit: (id) => call(`/limits/${id}`, { method: 'DELETE' }),
    createProfile: (profile) => call('/profiles', { method: 'POST', body: profile }),
    updateProfile: (id, profile) => call(`/profiles/${id}`, { method: 'PUT', body: profile }),
    deleteProfile: (id) => call(`/profiles/${id}`, { method: 'DELETE' }),
    assign: (scope, target, profiles) => call('/assignments', { method: 'PUT', body: { scope, target, profiles } }),
    adjust: (agentID, body) => call(`/agents/${encodeURIComponent(agentID)}/adjust`, { method: 'POST', body }),
  });
}

export const usageRulesApi = createUsageRulesApi();
