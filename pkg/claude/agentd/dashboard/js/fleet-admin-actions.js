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
  const enc = encodeURIComponent;
// rows reads a hub list response, bare or wrapped in its named field.
const rows = (v, key) => (Array.isArray(v) ? v : v?.[key] || []);
  return Object.freeze({
    status: () => call('GET', 'status'),
    // Hub administration (tclaude federation hub …): signed requests this
    // node's daemon relays to the hub over its authenticated connection. Every
    // mutation is confirmed in the UI first.
    hubStatus: () => call('GET', 'hub/status'),
    hubClaim: (token) => call('POST', 'hub/claim', { token }),
    hubAdmins: async () => rows(await call('GET', 'hub/admins'), 'admins'),
    addHubAdmin: (instance, capabilities) => call('POST', 'hub/admins', { instance, capabilities }),
    removeHubAdmin: (instance) => call('DELETE', `hub/admins/${enc(instance)}`),
    hubAdmissions: async () => rows(await call('GET', 'hub/admissions'), 'admissions'),
    admitToHub: (instance, spaces) => call('POST', 'hub/admissions', { instance, spaces }),
    revokeHubAdmission: (instance) => call('DELETE', `hub/admissions/${enc(instance)}`),
    hubInvites: async () => rows(await call('GET', 'hub/invites'), 'invites'),
    createHubInvite: (space, ttlSeconds) => call('POST', 'hub/invites', { space, ttl_seconds: ttlSeconds }),
    revokeHubInvite: (tokenHash) => call('DELETE', `hub/invites/${enc(tokenHash)}`),
    hubSpaces: async () => rows(await call('GET', 'hub/spaces'), 'spaces'),
    setHubSpaces: (instance, spaces) => call('PUT', 'hub/spaces', { instance, spaces }),
    hubSettings: async () => rows(await call('GET', 'hub/settings'), 'settings'),
    patchHubSettings: (overrides) => call('PATCH', 'hub/settings', { overrides }),
    hubHealth: () => call('GET', 'hub/health'),
    hubLogs: (cursor = '', maxEntries = 200) => call('GET', `hub/logs?${new URLSearchParams({ cursor, max_entries: String(maxEntries) })}`),
    pools: async () => (await call('GET', 'nodes/groups'))?.groups || [],
    // A preview names no level: a default profile decides it, and an explicit
    // level that differs from the profile's is refused.
    previewTrust: (opts) => call('POST', 'peers/trust', trustBody({ ...opts, level: '', preview: true })),
    trust: (opts) => call('POST', 'peers/trust', trustBody(opts)),
    untrust: (instance) => call('POST', 'peers/untrust', { instance }),
    setHubEnabled: (enabled) => call('POST', 'config', { enabled }),
    // Hub setup: only the fields sent change (enabled, hub_url, name, invite,
    // hub_ca_file — a path on this node).
    setHubConfig: (body) => call('POST', 'config', body),
    nodeLabels: async () => (await call('GET', 'node-labels'))?.labels || [],
    // Fleet health notice policy: the defaults ('' peer) or one peer's (the
    // effective policy, built-in defaults filled in); a save replaces it.
    healthPolicy: (peer = '') => call('GET', `nodes/health?peer=${encodeURIComponent(peer)}`),
    setHealthPolicy: (peer, policy) => call('POST', `nodes/health?peer=${encodeURIComponent(peer || '')}`, policy),
    setNodeLabels: ({ add = [], remove = [] }) => call('POST', 'node-labels', { add, remove }),
    // This node's identity rotation (tclaude federation identity rotate): the
    // preview is read-only and generates no key; apply signs the successor.
    identityRotations: () => call('GET', 'identity/rotations'),
    previewRotateIdentity: () => call('POST', 'identity/rotate', {}),
    rotateIdentity: () => call('POST', 'identity/rotate', { apply: true }),
    // Peers viewing this node's agent terminals now; kick closes one view.
    viewers: async () => (await call('GET', 'viewers')) || [],
    kickViewer: (id) => call('POST', `viewers/${encodeURIComponent(id)}/kick`, {}),
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
    // Moves a local agent to a peer with its history; the source retires
    // here once the peer confirms its copy is running (move-agent).
    moveAgent: (body) => call('POST', 'move-agent', body),
    teleport: () => call('GET', 'teleport'),
    setTeleport: (disabled) => call('PUT', 'teleport', { disabled }),
    createPool: (name) => call('POST', 'nodes/groups', { name }),
    deletePool: (name) => call('DELETE', `nodes/groups/${encodeURIComponent(name)}`),
    addPoolMember: (name, peer) => call('POST', `nodes/groups/${encodeURIComponent(name)}/members`, { peer }),
    removePoolMember: (name, peer) => call('DELETE', `nodes/groups/${encodeURIComponent(name)}/members`, { peer }),
    setDefaultProfile: (profile) => call('PUT', 'default-peer-profile', { profile }),
    deleteProfile: (name) => call('DELETE', `profiles/${encodeURIComponent(name)}`),
    // One profile with the peers it is applied to; create; save a new
    // revision of the exact record read (a stale revision is refused).
    profile: (name) => call('GET', `profiles/${encodeURIComponent(name)}`),
    createProfile: ({ name, definition }) => call('POST', 'profiles', { name, definition }),
    saveProfile: (record) => call('PUT', `profiles/${encodeURIComponent(record.name)}`, record),
    // Preview (apply: false) returns the plan and its preview_token; apply
    // commits exactly that plan.
    applyProfile: (name, { peer, apply = false, previewToken = '', confirmFingerprint = '' }) => call('POST', `profiles/${encodeURIComponent(name)}/apply`,
      { peer, apply, preview_token: previewToken, confirm_fingerprint: confirmFingerprint }),
    enroll: ({ master, token, previewToken }) => call('POST', 'enroll', { master, token, preview_token: previewToken }),
    // Bundle offers. An incoming offer is addressed by ID plus its source peer
    // (the same ID can come from two peers); import defaults to a preview.
    offers: async (direction) => (await call('GET', `bundle-offers?direction=${direction}`)) || [],
    importOffer: (o, body) => call('POST', `bundle-offers/${encodeURIComponent(o.offer.id)}/import?peer=${encodeURIComponent(o.peer)}`, body),
    declineOffer: (o) => call('POST', `bundle-offers/${encodeURIComponent(o.offer.id)}/decline?peer=${encodeURIComponent(o.peer)}`, {}),
    offerConfig: (body) => call('POST', 'offer-config', body),
    shareAgent: (body) => call('POST', 'share-agent', body),
    offerProfile: (name, peer) => call('POST', `profiles/${encodeURIComponent(name)}/offer`, { peer }),
    // Remote jobs (sent and received) and the repositories peers may use here.
    jobs: async () => (await call('GET', 'jobs'))?.jobs || [],
    runJob: (body) => call('POST', 'jobs', body),
    approveJob: (id) => call('POST', `jobs/${encodeURIComponent(id)}/approve`, {}),
    cancelJob: (id) => call('POST', `jobs/${encodeURIComponent(id)}/cancel`, {}),
    retryJob: (id) => call('POST', `jobs/${encodeURIComponent(id)}/retry`, {}),
    acknowledgeJobStopped: (id) => call('POST', `jobs/${encodeURIComponent(id)}/acknowledge-stopped`, { acknowledge_stopped: true }),
    jobLogs: (id) => call('GET', `jobs/${encodeURIComponent(id)}/logs`),
    // Live output of an active outbound job: the chunks after cursor ('' =
    // from the start), the next cursor, and done once the job is terminal.
    jobOutput: (id, cursor = '', maxBytes = 65536) => call('GET', `jobs/${encodeURIComponent(id)}/output?cursor=${encodeURIComponent(cursor)}&max_bytes=${maxBytes}`),
    repos: async () => (await call('GET', 'repos'))?.repos || [],
    addRepo: (body) => call('POST', 'repos', body),
    updateRepo: (name, body) => call('PUT', `repos/${encodeURIComponent(name)}`, body),
    disableRepo: (name) => call('DELETE', `repos/${encodeURIComponent(name)}`),
    // Model gateways: policy and switches (no provider URLs or credentials),
    // requester-paid leases, and daily usage (UTC day, '' = today).
    models: () => call('GET', 'models/control'),
    setModelSwitch: ({ name = '', peer = '', disabled }) => call('POST', 'models/control', { ...(name ? { name } : {}), ...(peer ? { peer } : {}), disabled }),
    modelLeases: async () => (await call('GET', 'models/leases')) || [],
    revokeModelLease: (id) => call('POST', 'models/leases', { id }),
    modelUsage: async (day = '') => (await call('GET', `models/usage${day ? `?day=${encodeURIComponent(day)}` : ''}`)) || [],
    // Spawn requests: incoming ones peers sent to this node's groups (all
    // states, newest first), and asking a peer for a worker.
    spawnRequests: async () => (await call('GET', 'spawn-requests')) || [],
    sendSpawnRequest: (body) => call('POST', 'spawn-requests', body),
    approveSpawn: (id, overrides = {}) => call('POST', `spawn-requests/${encodeURIComponent(id)}/approve`, overrides),
    denySpawn: (id, reason = '') => call('POST', `spawn-requests/${encodeURIComponent(id)}/deny`, reason ? { reason } : {}),
    abandonSpawn: (id) => call('POST', `spawn-requests/${encodeURIComponent(id)}/abandon`, { acknowledge_late_worker: true }),
    // Delivery state of what this node sent peers (spawn requests, mail).
    outbox: async (limit = 100) => (await call('GET', `outbox?limit=${limit}`)) || [],
  });
}
