import { trustBody } from './fleet-admin-model.js';

// FleetAdminError carries the daemon's stable error code so dialogs can react
// (e.g. confirmation_required, stale_preview) instead of string-matching.
export class FleetAdminError extends Error {
  constructor(status, body) {
    super(body?.error || `HTTP ${status}`);
    this.status = status;
    this.code = body?.code || '';
    // A refused profile apply returns its plan (conflicts) in the body.
    this.body = body || null;
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
    // Newest first; peer is an instance ID ('' = all), since an ISO time.
    audit: ({ peer = '', since = '', limit = 200 } = {}) => {
      const q = new URLSearchParams({ limit: String(limit) });
      if (peer) q.set('peer', peer);
      if (since) q.set('since', since);
      return call('GET', `audit?${q}`).then((rows) => (Array.isArray(rows) ? rows : []));
    },
    // Durable agent moves (both directions) and this node's teleport freeze.
    moves: async () => (await call('GET', 'moves'))?.moves || [],
    abandonMove: (id) => call('POST', `moves/${encodeURIComponent(id)}/abandon`, {}),
    teleport: () => call('GET', 'teleport'),
    setTeleport: (disabled) => call('PUT', 'teleport', { disabled }),
    createPool: (name) => call('POST', 'nodes/groups', { name }),
    deletePool: (name) => call('DELETE', `nodes/groups/${encodeURIComponent(name)}`),
    addPoolMember: (name, peer) => call('POST', `nodes/groups/${encodeURIComponent(name)}/members`, { peer }),
    removePoolMember: (name, peer) => call('DELETE', `nodes/groups/${encodeURIComponent(name)}/members`, { peer }),
    setDefaultProfile: (profile) => call('PUT', 'default-peer-profile', { profile }),
    deleteProfile: (name) => call('DELETE', `profiles/${encodeURIComponent(name)}`),
    // Preview (apply: false) returns the plan and its preview_token; apply
    // commits exactly that plan.
    applyProfile: (name, { peer, apply = false, previewToken = '', confirmFingerprint = '' }) => call('POST', `profiles/${encodeURIComponent(name)}/apply`,
      { peer, apply, preview_token: previewToken, confirm_fingerprint: confirmFingerprint }),
    enroll: ({ master, token, previewToken }) => call('POST', 'enroll', { master, token, preview_token: previewToken }),
    // Model gateways: policy and switches (no provider URLs or credentials),
    // requester-paid leases, and daily usage (UTC day, '' = today).
    models: () => call('GET', 'models/control'),
    setModelSwitch: ({ name = '', peer = '', disabled }) => call('POST', 'models/control', { ...(name ? { name } : {}), ...(peer ? { peer } : {}), disabled }),
    modelLeases: async () => (await call('GET', 'models/leases')) || [],
    revokeModelLease: (id) => call('POST', 'models/leases', { id }),
    modelUsage: async (day = '') => (await call('GET', `models/usage${day ? `?day=${encodeURIComponent(day)}` : ''}`)) || [],
  });
}
