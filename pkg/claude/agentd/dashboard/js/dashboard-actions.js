export class DashboardActionError extends Error {
  constructor(message, { status = 0, body = null } = {}) {
    super(message);
    this.name = 'DashboardActionError';
    this.status = status;
    this.body = body;
  }
}

async function readResponse(response) {
  if (response.status === 204) return null;
  const contentType = response.headers?.get?.('content-type') || '';
  let text;
  try { text = await response.text(); } catch { return null; }
  if (!text) return null;
  if (contentType.includes('application/json')) {
    try { return JSON.parse(text); } catch { return text; }
  }
  return text;
}

function normalizeAPIPath(path, baseURL) {
  if (typeof path !== 'string') {
    throw new TypeError('dashboard mutations require a same-origin /api/ path');
  }
  const base = new URL(baseURL || 'http://tclaude.invalid/');
  const target = new URL(path, base);
  let decodedPath;
  try { decodedPath = decodeURIComponent(target.pathname); } catch {
    throw new TypeError('dashboard mutations require a valid URL path');
  }
  const decodedNormalized = new URL(decodedPath, base).pathname;
  if (target.origin !== base.origin ||
      !target.pathname.startsWith('/api/') ||
      !decodedNormalized.startsWith('/api/')) {
    throw new TypeError('dashboard mutations require a same-origin /api/ path');
  }
  return target.pathname + target.search;
}

export function createDashboardActions({
  refresh,
  fetchImpl = globalThis.fetch,
  baseURL = globalThis.location?.href,
} = {}) {
  if (typeof refresh !== 'function') throw new TypeError('dashboard actions require refresh');
  if (typeof fetchImpl !== 'function') throw new TypeError('dashboard actions require fetch');

  async function requestMutation(path, {
    method = 'POST',
    body,
    headers = {},
    refreshAfter = true,
  } = {}) {
    path = normalizeAPIPath(path, baseURL);
    const requestHeaders = { ...headers };
    let requestBody = body;
    if (body !== undefined && body !== null && typeof body !== 'string') {
      requestBody = JSON.stringify(body);
      if (!Object.keys(requestHeaders).some((name) => name.toLowerCase() === 'content-type')) {
        requestHeaders['Content-Type'] = 'application/json';
      }
    }
    const response = await fetchImpl(path, {
      method,
      headers: requestHeaders,
      body: requestBody,
      credentials: 'same-origin',
    });
    const result = await readResponse(response);
    if (!response.ok) {
      throw new DashboardActionError(`dashboard mutation failed: HTTP ${response.status}`, {
        status: response.status,
        body: result,
      });
    }
    if (refreshAfter) await refresh();
    return result;
  }

  return Object.freeze({
    refresh: (options) => refresh(options),
    retry: () => refresh(),
    requestMutation,
  });
}

let configuredActions = null;

export function configureDashboardActions(dependencies) {
  configuredActions = createDashboardActions(dependencies);
  return configuredActions;
}

function requireActions() {
  if (!configuredActions) throw new Error('dashboard actions are not configured');
  return configuredActions;
}

// Stable object identity lets components import the action boundary before the
// dashboard boot configures its dependencies. Calls still fail loudly if a
// future island invokes an action before bootstrap completes.
export const dashboardActions = Object.freeze({
  refresh: (options) => requireActions().refresh(options),
  retry: () => requireActions().retry(),
  requestMutation: (path, options) => requireActions().requestMutation(path, options),
});
