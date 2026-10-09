import { trustBody } from './fleet-admin-model.js';

// FleetAdminError carries the daemon's stable error code so dialogs can react
// (e.g. confirmation_required, stale_preview) instead of string-matching.
export class FleetAdminError extends Error {
  constructor(status, body) {
    super(body?.error || `HTTP ${status}`);
    this.status = status;
    this.code = body?.code || '';
  }
}

// createFleetAdminActions wraps the local-only /api/federation routes (the
// cookie-authenticated mirrors of /v1/federation; never peer-proxied).
export function createFleetAdminActions({ fetchImpl = (...a) => globalThis.fetch(...a) } = {}) {
  async function call(method, path, body) {
    const init = { method, credentials: 'same-origin', headers: {} };
    if (body !== undefined) {
      init.headers['Content-Type'] = 'application/json';
      init.body = JSON.stringify(body);
    }
    const res = await fetchImpl(`/api/federation/${path}`, init);
    let data = null;
    try { data = await res.json(); } catch (_) { data = null; }
    if (!res.ok) throw new FleetAdminError(res.status, data);
    return data;
  }
  return Object.freeze({
    status: () => call('GET', 'status'),
    pools: async () => (await call('GET', 'nodes/groups'))?.groups || [],
    // A preview names no level: a default profile decides it, and an explicit
    // level that differs from the profile's is refused.
    previewTrust: (opts) => call('POST', 'peers/trust', trustBody({ ...opts, level: '', preview: true })),
    trust: (opts) => call('POST', 'peers/trust', trustBody(opts)),
    untrust: (instance) => call('POST', 'peers/untrust', { instance }),
    setHubEnabled: (enabled) => call('POST', 'config', { enabled }),
    // target is a peer instance ID or group:<pool name>.
    grants: async (target) => (await call('GET', `grants?peer=${encodeURIComponent(target)}`))?.grants || [],
    grant: (body) => call('POST', 'grants', body),
    revoke: ({ peer, slug, scope }) => call('DELETE', 'grants', { peer, slug, scope }),
    profiles: () => call('GET', 'profiles'),
    tokens: async () => (await call('GET', 'enroll-tokens'))?.tokens || [],
    // The bearer is in this response only; callers show it once and drop it.
    createToken: ({ profile, uses, ttlSeconds, trustLevel }) => call('POST', 'enroll-tokens', { profile, uses, ttl_seconds: ttlSeconds, trust_level: trustLevel }),
    revokeToken: (id) => call('POST', `enroll-tokens/${encodeURIComponent(id)}/revoke`, {}),
    enrollments: async () => (await call('GET', 'enrollments'))?.enrollments || [],
    enrollPreview: ({ master, token }) => call('POST', 'enroll/preview', { master, token }),
    enroll: ({ master, token, previewToken }) => call('POST', 'enroll', { master, token, preview_token: previewToken }),
  });
}
