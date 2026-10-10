import test from 'node:test'; import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const FP_FORGE = 'k7q2-mx9d-4hpa-zz31-0e8c';
const FP_NEW = 'w5ze-a3nq-9c1b-77f0-d2aa';
let s_transition = null;
const status = () => ({
  enabled: true, instance_id: 'inst_self', name: 'desk', fingerprint: 'self-fp-0000', hub_url: 'wss://hub.example', hub: { state: 'connected' },
  peers: [
    { instance_id: 'inst_forge', label: 'forge', name: 'forge', fingerprint: FP_FORGE, trusted: true, online: true, level: 'restricted', last_seen: '0001-01-01T00:00:00Z', ...(s_transition ? { identity_transition: s_transition } : {}) },
    { instance_id: 'inst_lab', label: 'lab', trusted: true, online: false, level: 'unrestricted', last_seen: '2026-10-09T20:00:00Z' },
    { instance_id: 'inst_carol', name: 'Carol@Buildbox', fingerprint: FP_NEW, trusted: false, online: true },
  ],
  peer_grants: [{ peer: 'inst_forge', slug: 'message.direct' }, { peer: 'inst_forge', slug: 'groups.roster.read', scope: 'ops' }],
});

test('adminView splits trusted from waiting instances, with grants and pools per peer', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/fleet-admin-model.js');
  const v = m.adminView(status(), { pools: [{ name: 'rigs', members: [{ instance_id: 'inst_lab' }] }] });
  assert.equal(v.self.fingerprint, 'self-fp-0000'); assert.equal(v.self.hubState, 'connected');
  assert.deepEqual(v.trusted.map((r) => r.id), ['inst_forge', 'inst_lab']);
  assert.deepEqual(v.waiting.map((r) => r.id), ['inst_carol']);
  assert.deepEqual(v.trusted[0].grants, { total: 2, nodeWide: 1, scoped: 1 });
  assert.equal(v.trusted[0].lastSeen, null, 'a zero time is not a last-seen time');
  assert.deepEqual(v.trusted[1].pools, ['rigs']);
  assert.equal(m.adminView(null), null);
  assert.deepEqual(m.trustBody({ instance: 'i', profile: 'p', level: 'unrestricted', previewToken: 't' }), { instance: 'i', profile: 'p', preview_token: 't' }, 'a profile decides the level');
  assert.deepEqual(m.trustBody({ instance: 'i', level: 'unrestricted', confirmFingerprint: FP_NEW }), { instance: 'i', level: 'unrestricted', confirm_fingerprint: FP_NEW });
  assert.match(m.UNRESTRICTED_CONSEQUENCE, /every peer permission on all groups/);
  assert.match(m.UNRESTRICTED_CONSEQUENCE, /created later/);
});

test('actions call the local /api/federation routes and surface the daemon error code', async (t) => {
  const harness = await createPreactHarness(t);
  const { createFleetAdminActions, FleetAdminError } = await harness.importDashboardModule('js/fleet-admin-actions.js');
  const calls = [];
  const fetchImpl = async (url, init) => {
    calls.push([init.method, url, init.body ? JSON.parse(init.body) : null]);
    if (url.endsWith('untrust')) return { ok: false, status: 404, json: async () => ({ error: 'no such peer', code: 'not_found' }) };
    return { ok: true, status: 200, json: async () => ({ ok: true }) };
  };
  const a = createFleetAdminActions({ fetchImpl });
  await a.previewTrust({ instance: 'inst_carol', noDefaultProfile: true });
  await a.setHubEnabled(false);
  await assert.rejects(a.untrust('inst_x'), (e) => e instanceof FleetAdminError && e.code === 'not_found' && e.status === 404);
  assert.deepEqual(calls, [
    ['POST', '/api/federation/peers/trust', { instance: 'inst_carol', no_default_profile: true, preview: true }],
    ['POST', '/api/federation/config', { enabled: false }],
    ['POST', '/api/federation/peers/untrust', { instance: 'inst_x' }],
  ]);
});

function fakeTimers() {
  const queue = []; let seq = 0;
  return { queue, setTimeout(fn, ms) { const id = ++seq; queue.push({ id, fn, ms }); return id; }, clearTimeout(id) { const i = queue.findIndex((q) => q.id === id); if (i >= 0) queue.splice(i, 1); } };
}

let s_viewers = [];
async function setup(t, { preview = { instance_id: 'inst_carol', fingerprint: FP_NEW, level: 'restricted', profile: null, plan: null } } = {}) {
  const harness = await createPreactHarness(t);
  const [stateMod, island] = await Promise.all([harness.importDashboardModule('js/skynet-state.js'), harness.importDashboardModule('js/fleet-admin-island.js')]);
  const activeTab = harness.signals.signal('groups');
  const state = stateMod.createSkynetState({ activeTab });
  const log = []; const toasts = []; const confirms = [];
  const actions = {
    status: async () => { log.push(['status']); return status(); },
    pools: async () => [],
    previewTrust: async (o) => { log.push(['preview', o]); return preview; },
    trust: async (o) => { log.push(['trust', o]); return { ok: true }; },
    untrust: async (id) => { log.push(['untrust', id]); return { ok: true }; },
    setHubEnabled: async (on) => { log.push(['hub', on]); return { ok: true }; },
    grants: async (target) => { log.push(['grants', target]); return target === 'inst_forge' ? [
      { peer: 'inst_forge', slug: 'groups.presence.read', scope: '' },
      { peer: 'inst_forge', slug: 'groups.roster.read', scope: 'group=ops' },
      { peer: 'inst_forge', slug: 'routes.consume', scope: '', pool_id: 'p1', pool_name: 'rigs' },
    ] : []; },
    grant: async (body) => { log.push(['grant', body]); return { ok: true, warnings: ['WARNING: unscoped peer grant covers every active group, including future groups'] }; },
    revoke: async (body) => { log.push(['revoke', body]); return { ok: true }; },
    profiles: async () => ({ profiles: [{ id: 'prof_1', name: 'test-rig', revision: 3, definition: { trust_level: 'restricted' } }, { id: 'prof_2', name: 'ops-full', revision: 1, definition: { trust_level: 'unrestricted' } }], default: null }),
    tokens: async () => [
      { id: 'tok_live', used: 0, max_uses: 2, revoked: false, expires_at: '2099-01-01T00:00:00Z' },
      { id: 'tok_old', used: 1, max_uses: 1, revoked: false, expires_at: '2099-01-01T00:00:00Z' },
    ],
    enrollments: async () => [{ direction: 'issuer', token_id: 'tok_old', peer: 'inst_forge', retired: false }],
    createToken: async (o) => { log.push(['createToken', o]); return { token: 'tcle1.SECRET', claims: { profile_name: 'test-rig', expires_at: '2099-01-02T00:00:00Z' }, master_fingerprint: 'self-fp-0000', uses: o.uses }; },
    revokeToken: async (id) => { log.push(['revokeToken', id]); return { ok: true }; },
    enrollPreview: async (o) => { log.push(['enrollPreview', o]); return { claims: { master: 'inst_carol', profile_name: 'worker', profile_id: 'prof_x', profile_revision: 2, trust_level: 'unrestricted', expires_at: '2099-01-01T00:00:00Z' }, preview_token: 'pv1', master_fingerprint: FP_NEW, node_fingerprint: 'self-fp-0000', consent: 'Running enroll trusts the pinned master.' }; },
    enroll: async (o) => { log.push(['enroll', o]); return { accepted: true }; },
    moves: async () => { log.push(['moves']); return [
      { id: 'm1', direction: 'out', teleport: false, peer: 'inst_forge', source_agent: 'agt_aaaaaaaaaaaaaaaa', group: 'ops', state: 'awaiting_confirmation', expires_at: '2099-01-01T00:00:00Z' },
      { id: 'm2', direction: 'in', teleport: true, peer: 'inst_forge', source_agent: 'agt_bbbbbbbbbbbbbbbb', target_agent: 'agt_cccccccccccccccc', group: 'rigs', state: 'complete', expires_at: '2026-10-09T00:00:00Z' },
      { id: 'm3', direction: 'out', teleport: true, peer: 'inst_lab', source_agent: 'agt_dddddddddddddddd', group: 'ops', state: 'retiring', expires_at: '2099-01-01T00:00:00Z' },
    ]; },
    abandonMove: async (id) => { log.push(['abandon', id]); return { id, state: 'abandoned' }; },
    teleport: async () => ({ disabled: false }),
    setTeleport: async (disabled) => { log.push(['teleport', disabled]); return { disabled }; },
    audit: async (o) => { log.push(['audit', o]); return Array.from({ length: o.limit === 200 ? 200 : 3 }, (_, i) => ({ id: `a${i}`, at: '2026-10-10T10:00:00Z', source: 'remote', direction: i % 2 ? 'out' : 'in', peer: 'inst_forge', kind: 'mail.send', actor: 'agt_x', status: i === 0 ? 403 : 200 })); },
    createPool: async (n) => { log.push(['createPool', n]); return { ok: true }; },
    deletePool: async (n) => { log.push(['deletePool', n]); return { ok: true }; },
    addPoolMember: async (n, p) => { log.push(['addPoolMember', n, p]); return { ok: true }; },
    removePoolMember: async (n, p) => { log.push(['removePoolMember', n, p]); return { ok: true }; },
    setDefaultProfile: async (n) => { log.push(['setDefaultProfile', n]); return { profile_id: n }; },
    deleteProfile: async (n) => { log.push(['deleteProfile', n]); return { ok: true }; },
    spawnRequests: async () => { log.push(['spawnRequests']); return [
      { id: 7, from: 'ada@forge', instance: 'inst_forge', group: 'ops', name: 'fixer', role: 'dev', brief: 'Fix the flaky deploy test', status: 'pending', credentials: 'proxy:claude@inst_forge', model_lease: 'lease_1', created_at: '2026-10-10T09:00:00Z', expires_at: '2099-01-01T00:00:00Z' },
      { id: 6, from: 'ada@forge', instance: 'inst_forge', group: 'ops', brief: 'Bench run', status: 'launching', result_agent: 'agt_late1', created_at: '2026-10-10T08:00:00Z', expires_at: '2099-01-01T00:00:00Z' },
      { id: 5, from: 'bob@lab', instance: 'inst_lab', group: 'ops', brief: 'Old', status: 'denied', reason: 'busy', created_at: '2026-10-09T08:00:00Z', expires_at: '2026-10-12T08:00:00Z' },
    ]; },
    approveSpawn: async (id, o) => { log.push(['approveSpawn', id, o]); return { id, group: 'ops', agent_id: 'agt_new', label: 'fixer' }; },
    denySpawn: async (id, reason) => { log.push(['denySpawn', id, reason]); return { ok: true }; },
    abandonSpawn: async (id) => { log.push(['abandonSpawn', id]); return { id, status: 'pending', warning: 'a late worker may still appear' }; },
    sendSpawnRequest: async (body) => { log.push(['sendSpawn', body]); return { envelope_id: 'env1', to: 'ops@forge', state: 'queued', hub_connected: true }; },
    outbox: async () => [{ envelope_id: 'env0', to: 'ops@forge', from: 'human operator', subject: 'spawn request', preview: 'Bench run', state: 'failed', attempts: 3, last_error: 'peer offline', updated_at: '2026-10-10T09:00:00Z' }],
    profile: async (n) => { log.push(['profile', n]); return { profile: {}, applied_peers: ['inst_forge'] }; },
    createProfile: async (o) => { log.push(['createProfile', o]); return { id: 'prof_new', name: o.name, revision: 1, definition: o.definition }; },
    saveProfile: async (o) => { log.push(['saveProfile', o]); if (o.revision === 99) { const e = new Error('reload the current profile revision'); e.status = 409; e.code = 'stale_profile'; throw e; } return { ...o, revision: o.revision + 1 }; },
    setHubConfig: async (b) => { log.push(['hubConfig', b]); return { ok: true }; },
    nodeLabels: async () => ['gpu', 'ci'],
    setNodeLabels: async (o) => { log.push(['labels', o]); return { ok: true }; },
    viewers: async () => s_viewers,
    kickViewer: async (id) => { log.push(['kick', id]); s_viewers = s_viewers.filter((v) => v.id !== id); return { ok: true }; },
    offers: async (dir) => { log.push(['offers', dir]); return dir === 'in' ? [
      { offer: { id: 'off_cfg', type: 'config', bytes: 2048, sha256: 'ab'.repeat(32), expires_at: '2099-01-01T00:00:00Z', summary: 'Config bundle: 3 items' }, peer: 'inst_forge', direction: 'in', state: 'pending' },
      { offer: { id: 'off_mv', type: 'agent', bytes: 9000, sha256: 'cd'.repeat(32), expires_at: '2099-01-01T00:00:00Z', summary: 'Agent ada', group: 'ops', move: { source_agent: 'agt_src' } }, peer: 'inst_forge', direction: 'in', state: 'ready', sender_agent: 'agt_src' },
      { offer: { id: 'off_old', type: 'config', summary: 'old' }, peer: 'inst_lab', direction: 'in', state: 'applied' },
    ] : [{ offer: { id: 'off_out', type: 'config', summary: 'Config bundle: 1 items', expires_at: '2099-01-01T00:00:00Z' }, peer: 'inst_lab', direction: 'out', state: 'declined' }]; },
    importOffer: async (o, body) => {
      log.push(['import', o.offer.id, body]);
      if (o.offer.type === 'agent') {
        const preview = { agent: { name: 'ada', harness: 'claude' }, cwd: body.cwd || '/srv/ada', group: body.group, history: true, unresolved: body.values?.REPO ? [] : [{ name: 'REPO', item: 'paths', field: 'cwd', original: '/home/x/repo' }], warnings: [], security: 'Permissions and ownership are advisory only.', findings: [{ kind: 'api_key', count: 1, locations: ['turn 3'] }], applied: !!body.apply };
        if (body.apply) return { ...preview, spawn: { agent_id: 'agt_new' } };
        return preview;
      }
      const all = [{ item: 'roles/reviewer', action: 'create', security: true }, { item: 'config/theme', action: 'replace' }, { item: 'templates/t', action: 'unchanged', security: true }];
      const changes = all.filter((c) => !(body.skip || []).includes(c.item));
      return { changes, applied: body.apply ? changes.filter((c) => c.action !== 'unchanged').map((c) => c.item) : [], security_changes: 1 };
    },
    declineOffer: async (o) => { log.push(['decline', o.offer.id, o.peer]); return { state: 'declined' }; },
    offerConfig: async (body) => { log.push(['offerConfig', body]); if (!body.allow_flagged) { const e = new Error('suspected credentials'); e.code = 'flagged_credentials'; e.status = 422; e.body = { flags: [{ item: 'roles/reviewer', field: 'prompt', hint: 'looks like a token' }] }; throw e; } return { offer: {} }; },
    shareAgent: async (body) => { log.push(['shareAgent', body]); return { offer: {} }; },
    offerProfile: async (n, peer) => { log.push(['offerProfile', n, peer]); const e = new Error('profile not applied to peer'); e.status = 409; throw e; },
    jobs: async () => { log.push(['jobs']); return [
      { id: 'job_in1', direction: 'in', peer: 'inst_forge', state: 'pending', request: { repo: 'tclaude', ref: 'main', group: 'ops', command: 'go test ./pkg/...', timeout_seconds: 600 }, created_at: '2026-10-10T09:00:00Z' },
      { id: 'job_in2', direction: 'in', peer: 'inst_forge', state: 'unknown', request: { repo: 'tclaude', ref: 'main', group: 'ops', command: 'make' }, result: { state: 'unknown', code: 'daemon_interrupted', exit_code: 1 }, created_at: '2026-10-10T08:00:00Z' },
      { id: 'job_out1', direction: 'out', peer: 'inst_lab', state: 'submitted', request: JSON.stringify({ repo: 'site', ref: 'v2', group: 'web', harness: 'codex', command: 'fix the flaky test' }), created_at: '2026-10-10T07:00:00Z' },
      { id: 'job_out2', direction: 'out', peer: 'inst_lab', state: 'completed', request: { repo: 'site', ref: 'v2', group: 'web', command: 'ls' }, result: { state: 'completed', exit_code: 0, commit: 'abcdef1234567890', logs: { id: 'l1' } }, created_at: '2026-10-10T06:00:00Z' },
    ]; },
    runJob: async (body) => { log.push(['runJob', body]); return body.nodes ? { results: [{ peer: 'inst_forge', status: 200, delivered: true }, { peer: 'inst_lab', status: 403, error: 'peer does not export this group for jobs' }] } : { job: {}, delivered: false }; },
    approveJob: async (id) => { log.push(['approveJob', id]); return { id }; },
    cancelJob: async (id) => { log.push(['cancelJob', id]); return { id }; },
    retryJob: async (id) => { log.push(['retryJob', id]); return { id, delivered: true }; },
    acknowledgeJobStopped: async (id) => { log.push(['ackJob', id]); return { id }; },
    jobLogs: async (id) => { log.push(['jobLogs', id]); return { stdout: 'index.html\n', stderr: '', exit_code: 0 }; },
    repos: async () => [{ id: 'r1', name: 'tclaude', revision: 2, enabled: true, definition: { url: 'git@github.com:tofutools/tclaude.git', clone: '/home/me/git/tclaude', groups: [7] } }, { id: 'r2', name: 'old', revision: 4, enabled: false, group_names: ['ops'], definition: { url: 'git@x:old.git', clone: '/srv/old', groups: [1] } }],
    addRepo: async (body) => { log.push(['addRepo', body]); return body; },
    updateRepo: async (name, body) => { log.push(['updateRepo', name, body]); return body; },
    disableRepo: async (name) => { log.push(['disableRepo', name]); return { ok: true }; },
    models: async () => { log.push(['models']); return { disabled: false, gateways: {
      claude: { enabled: true, dialect: 'anthropic', models: ['claude-sonnet-5-5'], daily_requests: 500, daily_tokens: 2000000, peer_daily_requests: 100, peer_daily_tokens: 500000, session_daily_requests: 50, session_daily_tokens: 100000, max_input_tokens: 100000, max_output_tokens: 8000, max_concurrent: 4, requests_per_minute: 30, blocked_peers: ['inst_lab'] },
      openai: { enabled: false, models: [] },
    } }; },
    setModelSwitch: async (o) => { log.push(['modelSwitch', o]); return o; },
    modelLeases: async () => [
      { id: 'lease_aaaaaaaaaaaa1', peer: 'inst_forge', proxy: 'claude', kind: 'requester_paid', worker: 'agt_w1', revoked: false, idle_seconds: 7200, touched_at: '2026-10-10T09:00:00Z' },
      { id: 'lease_bbbbbbbbbbbb2', peer: 'inst_forge', proxy: 'claude', worker: 'agt_w0', revoked: true, touched_at: '2026-10-09T09:00:00Z' },
    ],
    revokeModelLease: async (id) => { log.push(['revokeLease', id]); return { revoked: true }; },
    modelUsage: async (day) => { log.push(['usage', day]); return [
      { proxy: 'claude', peer: 'inst_forge', model: 'claude-sonnet-5-5', charged_tokens: 1200, input_tokens: 1000, output_tokens: 200, status: 200, complete: true },
      { proxy: 'claude', peer: 'inst_forge', model: 'claude-sonnet-5-5', charged_tokens: 300, input_tokens: 300, output_tokens: 0, status: 429, complete: false },
      { proxy: 'claude', peer: 'inst_forge', model: 'claude-sonnet-5-5', charged_tokens: 108000, input_tokens: 0, output_tokens: 0, status: 0, complete: false },
    ]; },
    applyProfile: async (n, o) => { log.push(['applyProfile', n, o]); return { preview_token: 'ptok', changes: [{ item: 'trust_level', before: 'restricted', after: 'unrestricted', security: true }, { item: 'pool/pool_r', before: false, after: true, security: true }], pools: [{ id: 'pool_r', name: 'rigs', live_grants: [{ slug: 'groups.members.spawn', scope: '' }] }], security_changes: 2, conflicts: [] }; },
  };
  const snapshot = harness.signals.signal({ groups: [{ name: 'ops' }, { name: 'build' }], agents: [{ agent_id: 'agt_a1', title: 'ada', online: true }, { conv_id: 'c-no-id', title: 'legacy' }] });
  // Like shellConfirm: a confirmed action resolves to the action's result.
  const confirm = async (opts) => { confirms.push(opts); return opts.action ? opts.action() : true; };
  const timers = fakeTimers();
  const harnessLog = [];
  let workersBusy = false;
  const avail = (name) => ({ schema: 1, harnesses: [
    { name: 'claude', display_name: 'Claude Code', installed: true, version: '2.1.0', latest_version: '2.2.0', update_available: true, version_status: 'known', credential_present: true, usable: true },
    { name: 'codex', display_name: 'Codex', installed: name !== 'inst_forge', version: '0.9.0', update_available: false, version_status: name === 'inst_forge' ? 'not_installed' : 'known', credential_present: null, usable: null },
    { name: 'copilot', display_name: 'Copilot', installed: false, version_status: 'not_installed' },
  ] });
  const harnessActions = {
    availability: async (node, o) => { harnessLog.push(['availability', node.id, o]); if (node.id === 'inst_lab') { const e = new Error('not shared'); e.status = 403; e.code = 'permission_denied'; throw e; } return avail(node.id); },
    operations: async (node) => { harnessLog.push(['operations', node.id]); return { recipes: [{ harness: 'codex', install_command: 'npm install -g @openai/codex@latest', update_command: 'npm install -g @openai/codex@latest' }, { harness: 'claude', install_command: 'npm i claude', update_command: 'claude update' }], modes: ['now', 'when_idle'] }; },
    start: async (node, req) => { harnessLog.push(['start', node.id, req]); if (workersBusy && !req.mode) { const e = new Error('busy'); e.status = 409; e.code = 'harness_workers_busy'; throw e; } return { id: 'job1', action: req.action, harnesses: [req.harness || 'all'], state: 'running', phase: 'installing', started_at: '2026-10-10T10:00:00Z', log: [], results: [] }; },
    job: async (node, id) => { harnessLog.push(['job', node.id, id]); return { id, action: 'install', harnesses: ['codex'], state: 'succeeded', phase: 'done', started_at: '2026-10-10T10:00:00Z', log: [], results: [{ harness: 'codex', state: 'succeeded', credentials: { backup_id: 'b'.repeat(32), copied: true } }], availability: avail('') }; },
    backups: async (node, h) => { harnessLog.push(['backups', node.id, h]); return [{ id: 'a'.repeat(32), harness: h, created_at: '2026-10-09T10:00:00Z', location: '/x' }]; },
    backup: async (node, h) => { harnessLog.push(['backup', node.id, h]); return { receipt: {} }; },
    restore: async (node, h, b) => { harnessLog.push(['restore', node.id, h, b]); return { receipt: {}, availability: avail('') }; },
    push: async (node, h) => { harnessLog.push(['push', node.id, h]); return { receipt: {}, availability: avail('') }; },
  };
  const runLog = [];
  let runSettings = { accept_remote_scripts: false, resource_limits: { memory: '1GiB', pids: 256 }, warning: 'Full remote code execution' };
  const runActions = {
    status: async (node) => { runLog.push(['status', node.id]); return { accept_remote_scripts: true, resource_limits: {} }; },
    settings: async () => runSettings,
    saveSettings: async (body) => { runLog.push(['save', body]); runSettings = { ...runSettings, ...body }; return runSettings; },
    start: async (node, script, secs) => { runLog.push(['start', node.id, script, secs]); return { id: `j-${node.id}`, node: node.id, state: 'running', exit_code: -1 }; },
    job: async (node, id) => { runLog.push(['job', node.id, id]); return node.id === 'inst_forge'
      ? { id, state: 'failed', exit_code: 2, duration_ms: 1500, stdout_tail: '', stderr_tail: 'no such file' }
      : { id, state: 'completed', exit_code: 0, duration_ms: 320, stdout_tail: 'hello from desk\n', stderr_tail: '' }; },
    fullLog: async (node, id, stream) => { runLog.push(['log', node.id, id, stream]); return 'line 1\nline 2'; },
  };
  const mounted = await harness.mount(harness.html`<${island.FleetAdmin} state=${state} actions=${actions} harnessActions=${harnessActions} runActions=${runActions} confirm=${confirm} toast=${(m) => toasts.push(m)} timers=${timers} remote="" copy=${async () => {}} snapshot=${snapshot} />`);
  const q = (sel) => mounted.container.querySelector(sel);
  const check = async (el) => { el.checked = true; await harness.act(() => harness.fireEvent(el, 'change')); };
  const settle = () => new Promise((r) => setTimeout(r, 25));
  const click = async (el) => { await harness.act(() => el.click()); await harness.act(settle); };
  // Effects run when an act ends, so the first read settles in a second one.
  const show = async () => { await harness.act(() => { activeTab.value = 'fleet-admin'; }); await harness.act(settle); };
  t.after(() => state.dispose());
  return { runLog, harnessLog, harnessActions, setWorkersBusy: (v) => { workersBusy = v; }, harness, show, state, activeTab, actions, log, toasts, confirms, timers, mounted, q, check, click };
}

test('the admin view reads status only while shown and lists trusted and waiting peers', async (t) => {
  const s = await setup(t);
  assert.equal(s.log.length, 0, 'hidden view reads nothing');
  await s.show();
  assert.deepEqual(s.log, [['status']]);
  assert.match(s.mounted.container.textContent, /self-fp-0000/, 'this node\'s fingerprint is shown whole');
  assert.equal(s.q('#fleet-trusted').querySelectorAll('tbody tr').length, 2);
  assert.equal(s.q('#fleet-waiting').querySelectorAll('tbody tr').length, 1);
  // The status poll; the live-viewers poll (5 s) runs alongside on Peers.
  assert.equal(s.timers.queue.filter((q) => q.ms !== 5000).length, 1);
  await s.harness.act(() => new Promise((r) => setTimeout(r, 25)));
  await s.harness.act(() => { s.activeTab.value = 'groups'; });
  assert.equal(s.timers.queue.length, 0, 'leaving the view stops both polls');
});

test('Trust previews, shows the full fingerprint, and needs the out-of-band check; unrestricted repeats the consequence', async (t) => {
  const s = await setup(t);
  await s.show();
  await s.click(s.q('[data-fa="trust"]'));
  assert.deepEqual(s.log.at(-1), ['preview', { instance: 'inst_carol', noDefaultProfile: false }]);
  assert.match(s.q('#fleet-trust-modal').textContent, new RegExp(FP_NEW), 'the fingerprint the daemon will pin, in full');
  assert.equal(s.q('#fleet-trust-label').value, 'carol-buildbox', 'a valid label is suggested from the reported name');
  assert.equal(s.q('#fleet-trust-submit').disabled, true, 'not before the fingerprint check is confirmed');
  assert.equal(s.q('.fa-consequence'), null);
  await s.check(s.q('input[name="fa-level"]:not(:checked)'));
  assert.match(s.q('.fa-consequence').textContent, /every peer permission on all groups.*created later/);
  await s.check(s.q('#fleet-trust-ack'));
  assert.equal(s.q('#fleet-trust-submit').disabled, false);
  await s.click(s.q('#fleet-trust-submit'));
  assert.deepEqual(s.log.find((l) => l[0] === 'trust'), ['trust', { instance: 'inst_carol', label: 'carol-buildbox', level: 'unrestricted', noDefaultProfile: false, previewToken: '', confirmFingerprint: FP_NEW }]);
  assert.equal(s.q('#fleet-trust-modal'), null, 'closes when done');
});

test('a default profile decides the level and its preview token is sent back', async (t) => {
  const s = await setup(t, { preview: { instance_id: 'inst_carol', fingerprint: FP_NEW, level: 'restricted', profile: { name: 'worker' }, plan: { preview_token: 'tok1', changes: [{}], security_changes: 1 } } });
  await s.show();
  await s.click(s.q('[data-fa="trust"]'));
  assert.match(s.q('#fleet-trust-modal').textContent, /Default profile worker applies/);
  assert.equal(s.q('input[name="fa-level"]'), null, 'no level choice while a profile applies');
  await s.check(s.q('#fleet-trust-ack'));
  await s.click(s.q('#fleet-trust-submit'));
  const sent = s.log.find((l) => l[0] === 'trust')[1];
  assert.equal(sent.previewToken, 'tok1'); assert.equal(sent.level, ''); assert.equal(sent.confirmFingerprint, '');
});

test('Unrestrict and Untrust confirm with the consequence before acting', async (t) => {
  const s = await setup(t);
  await s.show();
  await s.click(s.q('[data-peer="inst_forge"] [data-fa="unrestrict"]'));
  assert.match(s.q('#fleet-level-modal').textContent, new RegExp(FP_FORGE));
  assert.match(s.q('#fleet-level-modal .fa-consequence').textContent, /created later/);
  assert.equal(s.q('#fleet-level-submit').disabled, true);
  await s.check(s.q('#fleet-level-ack'));
  await s.click(s.q('#fleet-level-submit'));
  assert.deepEqual(s.log.find((l) => l[0] === 'trust'), ['trust', { instance: 'inst_forge', level: 'unrestricted', confirmFingerprint: FP_FORGE }]);
  await s.click(s.q('[data-peer="inst_forge"] [data-fa="untrust"]'));
  assert.match(s.confirms.at(-1).body, /grant you gave it \(2\)/);
  assert.deepEqual(s.log.find((l) => l[0] === 'untrust'), ['untrust', 'inst_forge']);
  await s.click([...s.mounted.container.querySelectorAll('.fa-identity button')].find((b) => b.textContent === 'Disconnect'));
  assert.match(s.confirms.at(-1).body, /no peer can reach this node/);
  assert.deepEqual(s.log.find((l) => l[0] === 'hub'), ['hub', false]);
});

test('a peer view hands fleet administration back to this node', async (t) => {
  const harness = await createPreactHarness(t);
  const [stateMod, island] = await Promise.all([harness.importDashboardModule('js/skynet-state.js'), harness.importDashboardModule('js/fleet-admin-island.js')]);
  const state = stateMod.createSkynetState({ activeTab: harness.signals.signal('fleet-admin') });
  const homes = []; const reads = [];
  const actions = { status: async () => { reads.push(1); return status(); }, pools: async () => [] };
  const mounted = await harness.mount(harness.html`<${island.FleetAdmin} state=${state} actions=${actions} remote="inst_forge" switchHome=${() => homes.push(1)} timers=${fakeTimers()} />`);
  assert.equal(homes.length, 1); assert.equal(reads.length, 0, 'never reads this node\'s federation state into a peer page');
  await mounted.unmount(); state.dispose();
});

test('a level change refuses a peer untrusted elsewhere since the last poll', async (t) => {
  const s = await setup(t);
  await s.show();
  s.actions.status = async () => ({ ...status(), peers: status().peers.filter((p) => p.instance_id !== 'inst_forge') });
  await s.click(s.q('[data-peer="inst_forge"] [data-fa="unrestrict"]'));
  await s.check(s.q('#fleet-level-ack'));
  await s.click(s.q('#fleet-level-submit'));
  assert.equal(s.log.some((l) => l[0] === 'trust'), false, 'never re-trusts it');
  assert.match(s.q('#fleet-level-modal').textContent, /no longer trusted/);
});

test('grant rows mark all-groups and pool-inherited grants; the consequence names future groups', async (t) => {
  const harness = await createPreactHarness(t);
  const m = await harness.importDashboardModule('js/fleet-admin-model.js');
  const rows = m.grantRows([
    { slug: 'routes.consume', scope: '', pool_name: 'rigs' },
    { slug: 'message.direct', scope: '' },
    { slug: 'node.read', scope: '' },
    { slug: 'groups.members.spawn', scope: 'group=ops', spawn_policy: { max_live: 3 } },
  ]);
  assert.deepEqual(rows.map((r) => [r.slug, r.allGroups, r.group, r.pool]), [
    ['groups.members.spawn', false, 'ops', ''], ['message.direct', true, '', ''], ['node.read', false, '', ''], ['routes.consume', true, '', 'rigs'],
  ]);
  assert.equal(rows[0].maxLive, 3); assert.equal(rows[0].sensitive, true);
  assert.match(m.grantConsequence({ target: 'forge', slug: 'message.direct', group: '' }), /EVERY group.*created later/);
  assert.match(m.grantConsequence({ target: 'forge', slug: 'message.direct', group: 'ops' }), /in group ops\./);
  assert.match(m.grantConsequence({ target: 'forge', slug: 'node.read', group: 'ops' }), /node-wide/);
});

test('the grants page lists, grants with a spelled-out confirm, and revokes direct grants only', async (t) => {
  const s = await setup(t);
  await s.show();
  await s.click(s.q('[data-peer="inst_forge"] [data-fa="grants"]'));
  assert.deepEqual(s.log.at(-1), ['grants', 'inst_forge']);
  const rows = s.q('#fleet-grants').querySelectorAll('tbody tr');
  assert.equal(rows.length, 3);
  assert.match(s.q('#fleet-grants').textContent, /all groups, incl\. future/);
  assert.equal(rows[2].querySelector('[data-fa="revoke"]'), null, 'a pool grant is revoked on the pool');
  assert.match(rows[2].textContent, /via pool rigs/);
  // Default form: message.direct on all groups.
  await s.click(s.q('#fleet-grant-submit'));
  assert.match(s.confirms.at(-1).body, /EVERY group.*created later/);
  assert.deepEqual(s.log.find((l) => l[0] === 'grant'), ['grant', { peer: 'inst_forge', slug: 'message.direct', scope: '' }]);
  assert.ok(s.toasts.some((m) => /future groups/.test(m)), 'daemon warnings are shown');
  // A spawn grant scoped to a group carries its live cap.
  // linkedom's select.value is read-only: pick the option instead.
  const pick = async (sel, value) => {
    const el = s.q(sel);
    for (const o of el.querySelectorAll('option')) { if (o.value === value) o.setAttribute('selected', ''); else o.removeAttribute('selected'); }
    await s.harness.act(() => s.harness.fireEvent(el, 'change'));
  };
  await pick('#fleet-grant-slug', 'groups.members.spawn');
  await pick('#fleet-grant-group', 'ops');
  await s.click(s.q('#fleet-grant-submit'));
  assert.match(s.confirms.at(-1).body, /in group ops\. Live cap: 2\. This lets it act on this node/);
  assert.deepEqual(s.log.filter((l) => l[0] === 'grant').at(-1)[1], { peer: 'inst_forge', slug: 'groups.members.spawn', scope: 'group=ops', spawn_policy: { max_live: 2 } });
  // A slug that requires a group cannot be granted without one.
  await pick('#fleet-grant-slug', 'agents.receive');
  await pick('#fleet-grant-group', '');
  assert.equal(s.q('#fleet-grant-submit').disabled, true);
  await s.click(s.q('#fleet-grants [data-slug="groups.roster.read"] [data-fa="revoke"]'));
  assert.match(s.confirms.at(-1).body, /loses groups\.roster\.read .* in group ops/);
  assert.deepEqual(s.log.find((l) => l[0] === 'revoke'), ['revoke', { peer: 'inst_forge', slug: 'groups.roster.read', scope: 'group=ops' }]);
});

test('a pool page revokes its own grants; re-granting keeps CLI launch settings and only changes the cap', async (t) => {
  const s = await setup(t);
  const m = await s.harness.importDashboardModule('js/fleet-admin-model.js');
  const own = m.grantRows([{ slug: 'routes.consume', scope: '', pool_id: 'p1', pool_name: 'rigs' }], { ownPool: 'rigs' });
  assert.equal(own[0].pool, '', 'direct on its own pool page');
  const gone = m.grantRows([{ slug: 'message.direct', scope: 'group=12' }], { groups: ['ops'] });
  assert.equal(gone[0].deletedGroup, true);
  assert.deepEqual(m.extraPolicy({ max_live: 2, job_approval: 'manual', allowed_profiles: [] }), ['job_approval']);

  s.actions.grants = async () => [{ peer: 'inst_forge', slug: 'jobs.run', scope: 'group=ops', spawn_policy: { max_live: 2, job_approval: 'manual', profile: 'safe' } }];
  await s.show();
  await s.click(s.q('[data-peer="inst_forge"] [data-fa="grants"]'));
  const pick = async (sel, value) => {
    const el = s.q(sel);
    for (const o of el.querySelectorAll('option')) { if (o.value === value) o.setAttribute('selected', ''); else o.removeAttribute('selected'); }
    await s.harness.act(() => s.harness.fireEvent(el, 'change'));
  };
  await pick('#fleet-grant-slug', 'jobs.run');
  await pick('#fleet-grant-group', 'ops');
  const cap = s.q('#fleet-grant-cap'); cap.value = '5';
  await s.harness.act(() => s.harness.fireEvent(cap, 'input'));
  await s.click(s.q('#fleet-grant-submit'));
  assert.match(s.confirms.at(-1).title, /^Update jobs\.run/);
  assert.match(s.confirms.at(-1).body, /cap 2 → 5\); its other launch settings \(job_approval, profile\) are kept/);
  assert.deepEqual(s.log.find((l) => l[0] === 'grant')[1].spawn_policy, { max_live: 5, job_approval: 'manual', profile: 'safe' });
});

test('grants scoped by stable group ID label by group_name and keep deleted-group grants revocable', async (t) => {
  const s = await setup(t);
  const m = await s.harness.importDashboardModule('js/fleet-admin-model.js');
  const rows = m.grantRows([
    { slug: 'message.direct', scope: 'group_id=7', group_id: 7, group_name: 'ops', group_deleted: false },
    { slug: 'sessions.watch', scope: 'group_id=12', group_id: 12, group_name: '', group_deleted: true },
    { slug: 'routes.consume', scope: 'group=12' },
  ], { groups: ['ops'] });
  const by = Object.fromEntries(rows.map((r) => [r.slug, r]));
  assert.equal(by['message.direct'].group, 'ops'); assert.equal(by['message.direct'].revocable, true);
  assert.equal(by['sessions.watch'].deletedGroup, true); assert.equal(by['sessions.watch'].revocable, true, 'a newer daemon revokes by stable ID');
  assert.equal(by['routes.consume'].deletedGroup, true); assert.equal(by['routes.consume'].revocable, false, 'an older daemon would resolve the id as a name');

  s.actions.grants = async () => [{ peer: 'inst_forge', slug: 'sessions.watch', scope: 'group_id=12', group_id: 12, group_name: '', group_deleted: true }];
  await s.show();
  await s.click(s.q('[data-peer="inst_forge"] [data-fa="grants"]'));
  assert.match(s.q('#fleet-grants').textContent, /deleted group #12/);
  await s.click(s.q('#fleet-grants [data-fa="revoke"]'));
  assert.match(s.confirms.at(-1).body, /in a deleted group/);
  assert.deepEqual(s.log.find((l) => l[0] === 'revoke')[1], { peer: 'inst_forge', slug: 'sessions.watch', scope: 'group_id=12' }, 'the scope goes back unchanged');
});

test('an all-groups grant is new even when the peer holds the same permission on a deleted group', async (t) => {
  const s = await setup(t);
  s.actions.grants = async () => [{ peer: 'inst_forge', slug: 'message.direct', scope: 'group_id=12', group_id: 12, group_deleted: true }];
  await s.show();
  await s.click(s.q('[data-peer="inst_forge"] [data-fa="grants"]'));
  await s.click(s.q('#fleet-grant-submit'));
  assert.match(s.confirms.at(-1).title, /^Grant message\.direct/);
  assert.deepEqual(s.log.find((l) => l[0] === 'grant')[1], { peer: 'inst_forge', slug: 'message.direct', scope: '' });
});

test('invites: create confirms the terms and shows the bearer once; revoke keeps enrolled nodes', async (t) => {
  const s = await setup(t);
  const m = await s.harness.importDashboardModule('js/fleet-admin-model.js');
  assert.equal(m.tokenState({ used: 1, max_uses: 1 }), 'used up');
  assert.equal(m.tokenState({ used: 0, max_uses: 1, expires_at: '2000-01-01T00:00:00Z' }), 'expired');
  assert.equal(m.joinCommand('inst_self'), 'tclaude federation enroll inst_self --token-stdin');
  await s.show();
  await s.click([...s.mounted.container.querySelectorAll('.fa-subtab')].find((b) => /Invites/.test(b.textContent)));
  assert.equal(s.q('#fleet-tokens').querySelectorAll('[data-fa="revoke-token"]').length, 1, 'only the active token is revocable');
  assert.match(s.q('#fleet-enrollments').textContent, /forge/);
  await s.click(s.q('#fleet-invite-create'));
  assert.match(s.confirms.at(-1).body, /one node within 24 hours.*profile test-rig.*restricted trust/);
  assert.deepEqual(s.log.find((l) => l[0] === 'createToken')[1], { profile: 'test-rig', uses: 1, ttlSeconds: 86400, trustLevel: 'restricted' });
  assert.equal(s.q('#fleet-token-bearer').value || s.q('#fleet-token-bearer').textContent, 'tcle1.SECRET');
  assert.match(s.q('#fleet-token-modal').textContent, /--token-stdin/);
  await s.click(s.q('#fleet-token-close'));
  assert.equal(s.q('#fleet-token-modal'), null);
  assert.equal(s.mounted.container.textContent.includes('tcle1.SECRET'), false, 'the bearer is gone once closed');
  await s.click(s.q('#fleet-tokens [data-fa="revoke-token"]'));
  assert.match(s.confirms.at(-1).body, /stay trusted/);
  assert.deepEqual(s.log.find((l) => l[0] === 'revokeToken'), ['revokeToken', 'tok_live']);
});

test('an invite for an unrestricted profile repeats the consequence', async (t) => {
  const s = await setup(t);
  await s.show();
  await s.click([...s.mounted.container.querySelectorAll('.fa-subtab')].find((b) => /Invites/.test(b.textContent)));
  const sel = s.q('#fleet-invite-profile');
  for (const o of sel.querySelectorAll('option')) { if (o.value === 'ops-full') o.setAttribute('selected', ''); else o.removeAttribute('selected'); }
  await s.harness.act(() => s.harness.fireEvent(sel, 'change'));
  await s.click(s.q('#fleet-invite-create'));
  assert.match(s.confirms.at(-1).body, /at unrestricted trust — .*created later/);
});

test('joining previews the master fingerprint and trust level, and needs the check before enrolling', async (t) => {
  const s = await setup(t);
  await s.show();
  await s.click([...s.mounted.container.querySelectorAll('.fa-subtab')].find((b) => /Invites/.test(b.textContent)));
  await s.click(s.q('#fleet-join-open'));
  const tok = s.q('#fleet-join-token'); tok.value = ' tcle1.FROMCAROL ';
  await s.harness.act(() => s.harness.fireEvent(tok, 'input'));
  await s.click(s.q('#fleet-join-preview'));
  assert.deepEqual(s.log.find((l) => l[0] === 'enrollPreview')[1], { master: 'inst_carol', token: 'tcle1.FROMCAROL' });
  const modal = s.q('#fleet-join-modal').textContent;
  assert.match(modal, new RegExp(FP_NEW)); assert.match(modal, /unrestricted trust on this node/); assert.match(modal, /created later/);
  assert.equal(s.q('#fleet-join-submit').disabled, true);
  await s.check(s.q('#fleet-join-ack'));
  await s.click(s.q('#fleet-join-submit'));
  assert.deepEqual(s.log.find((l) => l[0] === 'enroll')[1], { master: 'inst_carol', token: 'tcle1.FROMCAROL', previewToken: 'pv1' });
  assert.equal(s.q('#fleet-join-modal'), null);
});

async function openProfiles(s, pools) {
  s.actions.pools = async () => pools;
  await s.show();
  await s.click([...s.mounted.container.querySelectorAll('.fa-subtab')].find((b) => /Profiles/.test(b.textContent)));
}

test('pools: adding a member names the grants it gains; deleting and removing confirm the loss', async (t) => {
  const s = await setup(t);
  const m = await s.harness.importDashboardModule('js/fleet-admin-model.js');
  assert.equal(m.changeText({ item: 'level', before: 'a', after: 'b' }), 'level: a → b');
  assert.equal(m.changeText({ item: 'pool rigs', after: 'member' }), 'pool rigs: add member');
  assert.equal(m.POOL_NAME_RE.test('Rigs'), false);
  s.actions.grants = async () => [{ slug: 'routes.consume', scope: '' }, { slug: 'message.direct', scope: 'group_id=7', group_name: 'ops' }];
  s.actions.profiles = async () => ({ profiles: [{ id: 'prof_1', name: 'test-rig', revision: 3, definition: { trust_level: 'restricted', pools: ['pool_r'] } }], default: { id: 'prof_1' } });
  await openProfiles(s, [{ id: 'pool_r', name: 'rigs', members: [{ instance_id: 'inst_lab' }] }]);
  assert.match(s.q('#fleet-profiles [data-profile="test-rig"]').textContent, /rigs/, 'profile pools are named, not shown by ID');
  const row = s.q('#fleet-pools [data-pool="rigs"]');
  assert.match(row.textContent, /lab/);
  const pick = row.querySelector('[data-fa="member-pick"]');
  for (const o of pick.querySelectorAll('option')) { if (o.value === 'inst_forge') o.setAttribute('selected', ''); else o.removeAttribute('selected'); }
  await s.harness.act(() => s.harness.fireEvent(pick, 'change'));
  await s.click(row.querySelector('[data-fa="add-member"]'));
  assert.match(s.confirms.at(-1).body, /forge gains its 2 grant\(s\): routes\.consume \(EVERY group, including future ones\), message\.direct \(group ops\) — and any grant added to the pool later/);
  assert.deepEqual(s.log.find((l) => l[0] === 'addPoolMember'), ['addPoolMember', 'rigs', 'inst_forge']);
  await s.click(s.q('#fleet-pools [data-fa="remove-member"]'));
  assert.deepEqual(s.log.find((l) => l[0] === 'removePoolMember'), ['removePoolMember', 'rigs', 'inst_lab']);
  await s.click(s.q('#fleet-pools [data-fa="delete-pool"]'));
  assert.match(s.confirms.at(-1).body, /1 member\(s\) lose its 2 grant/);
  assert.match(s.confirms.at(-1).body, /Profiles test-rig include this pool: applying them, trusting new peers with the default profile and enrolling with their invites fail/);
  assert.deepEqual(s.log.find((l) => l[0] === 'deletePool'), ['deletePool', 'rigs']);
  await s.click(s.q('#fleet-pools [data-fa="pool-grants"]'));
  assert.match(s.q('.fa-grants-head').textContent, /A pool's grants apply to every member node/, 'Grants… opens the pool on the grants page');
});

test('profiles: default and apply preview the effect; applying unrestricted confirms the fingerprint', async (t) => {
  const s = await setup(t);
  s.actions.grants = async () => [{ slug: 'jobs.run', scope: '' }];
  await openProfiles(s, [{ id: 'pool_r', name: 'rigs', members: [] }]);
  assert.equal(s.q('#fleet-profiles').querySelectorAll('tbody tr').length, 2);
  await s.click(s.q('#fleet-profiles [data-profile="ops-full"] [data-fa="make-default"]'));
  assert.match(s.confirms.at(-1).body, /Every peer trusted from now on .* ops-full: unrestricted trust.*created later/);
  assert.deepEqual(s.log.find((l) => l[0] === 'setDefaultProfile'), ['setDefaultProfile', 'ops-full']);
  await s.click(s.q('#fleet-profiles [data-profile="ops-full"] [data-fa="apply-profile"]'));
  assert.deepEqual(s.log.find((l) => l[0] === 'applyProfile'), ['applyProfile', 'ops-full', { peer: 'inst_forge' }], 'previews for the first peer');
  const modal = s.q('#fleet-apply-modal');
  assert.match(modal.textContent, /2 change\(s\), 2 security-relevant/);
  assert.match(modal.textContent, /joins pool rigs/);
  assert.match(modal.textContent, /Via pool rigs it gets: groups\.members\.spawn \(EVERY group, including future ones\)/);
  assert.match(modal.textContent, /trust_level: restricted → unrestricted/);
  assert.match(modal.textContent, /forge becomes unrestricted.*created later/);
  assert.equal(s.q('#fleet-apply-submit').disabled, true);
  await s.check(s.q('#fleet-apply-ack'));
  await s.click(s.q('#fleet-apply-submit'));
  assert.deepEqual(s.log.filter((l) => l[0] === 'applyProfile').at(-1), ['applyProfile', 'ops-full', { peer: 'inst_forge', apply: true, previewToken: 'ptok', confirmFingerprint: FP_FORGE }]);
  await s.click(s.q('#fleet-profiles [data-profile="test-rig"] [data-fa="delete-profile"]'));
  assert.deepEqual(s.log.find((l) => l[0] === 'deleteProfile'), ['deleteProfile', 'test-rig']);
});

test('audit reads newest-first metadata by peer and time window, and pages up to 1000', async (t) => {
  const s = await setup(t);
  const m = await s.harness.importDashboardModule('js/fleet-admin-model.js');
  assert.equal(m.auditSince(0), '');
  assert.equal(m.auditSince(3600e3, Date.parse('2026-10-10T12:00:00Z')), '2026-10-10T11:00:00.000Z');
  await s.show();
  await s.click([...s.mounted.container.querySelectorAll('.fa-subtab')].find((b) => /Audit/.test(b.textContent)));
  const first = s.log.find((l) => l[0] === 'audit')[1];
  assert.equal(first.peer, ''); assert.equal(first.limit, 200); assert.ok(first.since, 'defaults to the last 24 hours');
  assert.equal(s.q('#fleet-audit').querySelectorAll('tbody tr').length, 200);
  assert.match(s.q('#fleet-audit tbody tr').textContent, /forge/);
  assert.ok(s.q('#fleet-audit tbody tr .fa-danger'), 'a refused request reads as an error');
  await s.click(s.q('#fleet-audit-more'));
  assert.equal(s.log.filter((l) => l[0] === 'audit').at(-1)[1].limit, 400);
  const sel = s.q('#fleet-audit-peer');
  for (const o of sel.querySelectorAll('option')) { if (o.value === 'inst_forge') o.setAttribute('selected', ''); else o.removeAttribute('selected'); }
  await s.harness.act(() => s.harness.fireEvent(sel, 'change'));
  await s.harness.act(() => new Promise((r) => setTimeout(r, 25)));
  const last = s.log.filter((l) => l[0] === 'audit').at(-1)[1];
  assert.equal(last.peer, 'inst_forge'); assert.equal(last.limit, 200, 'a new filter starts from the first page');
});

test('audit actions build the query', async (t) => {
  const harness = await createPreactHarness(t);
  const { createFleetAdminActions } = await harness.importDashboardModule('js/fleet-admin-actions.js');
  const urls = [];
  const a = createFleetAdminActions({ fetchImpl: async (url) => { urls.push(url); return { ok: true, status: 200, json: async () => [] }; } });
  await a.audit({ peer: 'inst_forge', since: '2026-10-10T11:00:00.000Z', limit: 400 });
  assert.equal(urls[0], '/api/federation/audit?limit=400&peer=inst_forge&since=2026-10-10T11%3A00%3A00.000Z');
});

test('audit marks state-only failures red, keeps rows on a failed read, and clears them on a new filter', async (t) => {
  const s = await setup(t);
  let fail = false;
  s.actions.audit = async (o) => { s.log.push(['audit', o]); if (fail) throw new Error('federation audit unavailable'); return [
    { id: 'o1', at: '2026-10-10T10:00:00Z', source: 'outbox', direction: 'out', peer: 'inst_forge', kind: 'mail', state: 'failed' },
    { id: 'o2', at: '2026-10-10T09:00:00Z', source: 'inbound', direction: 'in', peer: 'inst_forge', kind: 'mail', state: 'delivered' },
  ]; };
  await s.show();
  await s.click([...s.mounted.container.querySelectorAll('.fa-subtab')].find((b) => /Audit/.test(b.textContent)));
  const rows = s.q('#fleet-audit').querySelectorAll('tbody tr');
  assert.ok(rows[0].querySelector('.fa-danger'), 'a failed outbox row is red without an HTTP status');
  assert.equal(rows[1].querySelector('.fa-danger'), null);
  fail = true;
  await s.click(s.q('#fleet-audit-refresh'));
  assert.match(s.mounted.container.textContent, /federation audit unavailable/);
  assert.equal(s.q('#fleet-audit').querySelectorAll('tbody tr').length, 2, 'a failed read keeps the rows shown');
  assert.doesNotMatch(s.mounted.container.textContent, /No federation activity/);
});

async function openHarnesses(s) {
  await s.show();
  await s.click([...s.mounted.container.querySelectorAll('.fa-subtab')].find((b) => /Harnesses/.test(b.textContent)));
}

test('harness matrix: one row per node, versions and updates per cell, an unshared peer explained', async (t) => {
  const s = await setup(t);
  const m = await s.harness.importDashboardModule('js/fleet-harness-model.js');
  assert.equal(m.nodeBase({ local: true }), '/api');
  assert.equal(m.nodeBase({ id: 'inst_a/b' }), '/api/peer/inst_a%2Fb');
  assert.equal(m.cellView({ name: 'x', installed: false }).text, 'not installed');
  assert.equal(m.cellView({ name: 'x', installed: true, version: '1', update_available: true, usable: true }).state, 'update');
  await openHarnesses(s);
  assert.deepEqual(s.harnessLog.filter((l) => l[0] === 'availability').map((l) => l[1]).sort(), ['inst_forge', 'inst_lab', 'inst_self']);
  const self = s.q('#fleet-harnesses [data-node="inst_self"]');
  assert.match(self.querySelector('[data-cell="claude"]').textContent, /2\.1\.0.*↑/);
  assert.ok(self.querySelector('[data-fa="update-all"]'), 'a node with an update offers Update all');
  assert.match(s.q('#fleet-harnesses [data-node="inst_lab"]').textContent, /not shared with you \(needs node\.harnesses\.read\)/);
  await s.click(s.q('#fleet-harness-refresh'));
  assert.deepEqual(s.harnessLog.filter((l) => l[0] === 'availability').at(-1)[2], { refresh: true });
  await s.click(self.querySelector('[data-fa="update-all"]'));
  assert.deepEqual(s.harnessLog.find((l) => l[0] === 'start'), ['start', 'inst_self', { action: 'update', all: true, mode: 'when_idle' }]);
  assert.ok(s.q('#fleet-harness-jobs [data-job="job1"]'), 'the job is tracked');
});

test('remote install can copy my login with the share warning; busy workers ask now or when idle; job polling', async (t) => {
  const s = await setup(t);
  await openHarnesses(s);
  await s.click(s.q('#fleet-harnesses [data-node="inst_forge"] [data-cell="codex"]'));
  assert.match(s.q('#fleet-harness-modal').textContent, /Runs npm install -g @openai\/codex@latest/);
  await s.check(s.q('#fleet-harness-copy'));
  assert.match(s.q('#fleet-harness-modal .fa-consequence').textContent, /act as you/);
  s.setWorkersBusy(true);
  await s.click(s.q('#fleet-harness-run'));
  assert.match(s.confirms.at(-1).body, /login files are copied there too\. Agents on the target node will act as you.*If a login already exists there, it is kept/);
  await s.harness.act(() => new Promise((r) => setTimeout(r, 25)));
  assert.deepEqual(s.harnessLog.filter((l) => l[0] === 'start')[0][2], { action: 'install', harness: 'codex', copy_credentials: true });
  assert.ok(s.q('#fleet-harness-idle'), 'busy workers offer when-idle');
  await s.click(s.q('#fleet-harness-idle'));
  assert.deepEqual(s.harnessLog.filter((l) => l[0] === 'start').at(-1)[2], { action: 'install', harness: 'codex', mode: 'when_idle', copy_credentials: true });
  assert.equal(s.q('#fleet-harness-modal'), null);
  const tick = s.timers.queue.find((q) => q.ms === 1000);
  assert.ok(tick, 'an active job is polled every second');
  await s.harness.act(async () => { s.timers.queue.splice(s.timers.queue.indexOf(tick), 1); await tick.fn(); });
  assert.match(s.q('#fleet-harness-jobs').textContent, /succeeded/);
  assert.match(s.q('#fleet-harness-jobs').textContent, /login copied/);
});

test('login files: push to a peer confirms the share; restore confirms and backs up first', async (t) => {
  const s = await setup(t);
  await openHarnesses(s);
  await s.click(s.q('#fleet-harnesses [data-node="inst_forge"] [data-cell="claude"]'));
  assert.deepEqual(s.harnessLog.find((l) => l[0] === 'backups'), ['backups', 'inst_forge', 'claude']);
  await s.click(s.q('#fleet-harness-push'));
  assert.match(s.confirms.at(-1).body, /act as you.*backed up first, then replaced/);
  assert.deepEqual(s.harnessLog.find((l) => l[0] === 'push'), ['push', 'inst_forge', 'claude']);
  await s.click(s.q('#fleet-harness-backups [data-fa="restore"]'));
  assert.match(s.confirms.at(-1).body, /current files are backed up first/);
  assert.deepEqual(s.harnessLog.find((l) => l[0] === 'restore'), ['restore', 'inst_forge', 'claude', 'a'.repeat(32)]);
  await s.click([...s.q('#fleet-harness-modal').querySelectorAll('button')].find((b) => b.textContent === 'Close'));
  await s.click(s.q('#fleet-harnesses [data-node="inst_self"] [data-cell="claude"]'));
  assert.equal(s.q('#fleet-harness-push'), null, 'no push to this node itself');
});

test('run scripts: pick ready nodes, run with a confirm, one pane per node, full log, re-run only the failed', async (t) => {
  const s = await setup(t);
  const m = await s.harness.importDashboardModule('js/fleet-admin-run.js');
  assert.equal(m.readiness({ id: 'p', online: true }, { error: { status: 403 } }).text, 'not granted (needs node.exec)');
  assert.equal(m.readiness({ id: 'p', online: true }, { data: { accept_remote_scripts: false } }).text, 'does not accept remote scripts');
  await s.show();
  await s.click([...s.mounted.container.querySelectorAll('.fa-subtab')].find((b) => /Run scripts/.test(b.textContent)));
  assert.deepEqual(s.runLog.filter((l) => l[0] === 'status').map((l) => l[1]), ['inst_forge'], 'only online peers are probed');
  assert.match(s.q('#fleet-run-nodes [data-node="inst_lab"]').textContent, /offline/);
  assert.equal(s.q('#fleet-run-nodes [data-node="inst_lab"] input').disabled, true);
  await s.click(s.q('#fleet-run-all'));
  const area = s.q('#fleet-run-script');
  area.value = 'echo hello';
  await s.harness.act(() => s.harness.fireEvent(area, 'input'));
  assert.match(s.q('#fleet-run-submit').textContent, /Run on 2 nodes/);
  await s.click(s.q('#fleet-run-submit'));
  assert.match(s.confirms.at(-1).body, /\/bin\/sh as the tclaude user on desk, forge, with a 3600 s timeout/);
  assert.deepEqual(s.runLog.filter((l) => l[0] === 'start').map((l) => [l[1], l[2], l[3]]), [['inst_self', 'echo hello', 3600], ['inst_forge', 'echo hello', 3600]]);
  const tick = s.timers.queue.find((x) => x.ms === 1000);
  assert.ok(tick, 'running jobs are polled every second');
  await s.harness.act(async () => { s.timers.queue.splice(s.timers.queue.indexOf(tick), 1); await tick.fn(); });
  await s.harness.act(() => new Promise((r) => setTimeout(r, 25)));
  const self = s.q('#fleet-run-results [data-node="inst_self"]');
  assert.match(self.textContent, /completed.*exit 0.*320 ms.*hello from desk/s);
  assert.match(s.q('#fleet-run-results [data-node="inst_forge"]').textContent, /failed.*exit 2.*no such file/s);
  await s.click(self.querySelector('[data-fa="log-stdout"]'));
  assert.match(self.querySelector('.fa-run-full').textContent, /line 1\s*line 2/);
  await s.click(s.q('#fleet-run-rerun'));
  assert.match(s.confirms.at(-1).title, /Re-run the script on 1 node/);
  assert.deepEqual(s.runLog.filter((l) => l[0] === 'start').at(-1).slice(1), ['inst_forge', 'echo hello', 3600]);
  assert.ok(s.q('#fleet-run-results [data-node="inst_self"]'), 'a re-run keeps the other panes');
  // A new run replaces every pane, so a later re-run never sends it to old failures.
  const tick2 = s.timers.queue.find((x) => x.ms === 1000);
  await s.harness.act(async () => { s.timers.queue.splice(s.timers.queue.indexOf(tick2), 1); await tick2.fn(); });
  await s.harness.act(() => new Promise((r) => setTimeout(r, 25)));
  const forgeBox = s.q('#fleet-run-nodes [data-node="inst_forge"] input');
  forgeBox.checked = false;
  await s.harness.act(() => s.harness.fireEvent(forgeBox, 'change'));
  area.value = 'uptime';
  await s.harness.act(() => s.harness.fireEvent(area, 'input'));
  await s.click(s.q('#fleet-run-submit'));
  assert.equal(s.q('#fleet-run-results [data-node="inst_forge"]'), null, 'a new run drops the previous panes');
  assert.deepEqual(s.runLog.filter((l) => l[0] === 'start').at(-1).slice(1), ['inst_self', 'uptime', 3600]);
});

test('accepting remote scripts confirms full remote code execution; node.exec grants repeat it', async (t) => {
  const s = await setup(t);
  await s.show();
  await s.click([...s.mounted.container.querySelectorAll('.fa-subtab')].find((b) => /Run scripts/.test(b.textContent)));
  assert.match(s.q('#fleet-run-settings').textContent, /does not accept remote scripts/);
  await s.click(s.q('#fleet-run-accept'));
  assert.match(s.confirms.at(-1).body, /^Full remote code execution\. Any peer granted node\.exec — and every unrestricted peer — can then run any shell command.*memory 1GiB, 256 processes/);
  assert.deepEqual(s.runLog.find((l) => l[0] === 'save'), ['save', { accept_remote_scripts: true }]);
  assert.match(s.q('#fleet-run-settings').textContent, /accepts remote scripts from peers with node\.exec/);
  const model = await s.harness.importDashboardModule('js/fleet-admin-model.js');
  assert.match(model.slugInfo('node.exec').warning, /Full remote code execution/);
  const pids = s.q('#fleet-run-pids');
  pids.value = '';
  await s.harness.act(() => s.harness.fireEvent(pids, 'input'));
  await s.click(s.q('#fleet-run-limits'));
  assert.match(s.confirms.at(-1).body, /no processes limit any more.*peers' scripts may use that much/);
  assert.deepEqual(s.runLog.filter((l) => l[0] === 'save').at(-1), ['save', { resource_limits: { memory: '1GiB' } }]);
  const cpu = s.q('#fleet-run-cpu');
  cpu.value = 'lots';
  await s.harness.act(() => s.harness.fireEvent(cpu, 'input'));
  const saves = s.runLog.filter((l) => l[0] === 'save').length;
  await s.click(s.q('#fleet-run-limits'));
  assert.equal(s.runLog.filter((l) => l[0] === 'save').length, saves, 'a non-numeric cpu is rejected, never sent');
});

test('spawn requests: approve with overrides, deny with a reason, abandon an unconfirmed launch, and ask a peer for a worker', async (t) => {
  const s = await setup(t);
  await s.show();
  await s.click([...s.mounted.container.querySelectorAll('.fa-subtab')].find((b) => /Spawn requests/.test(b.textContent)));
  const doc = s.harness.document;
  const fill = async (sel, v, ev = 'input') => { const el = doc.querySelector(sel); el.value = v; await s.harness.act(() => s.harness.fireEvent(el, ev)); };
  assert.deepEqual([...s.mounted.container.querySelectorAll('#fleet-spawn-requests tbody tr')].map((r) => r.dataset.spawn), ['7', '6'], 'decided requests are hidden by default');
  assert.match(s.q('[data-spawn="7"]').textContent, /requester-paid/);
  assert.match(s.q('#fleet-outbox').textContent, /failed.*peer offline/s);
  await s.click(s.q('[data-spawn="7"] [data-fa="approve"]'));
  await fill('#fleet-spawn-cwd', '/srv/ops');
  await s.click(doc.querySelector('#fleet-spawn-approve-go'));
  assert.match(s.confirms.at(-1).body, /starts in your group ops on this node, as you.*ada@forge's model gateway \(requester-paid.*worker permissions in the node profile/);
  assert.deepEqual(s.log.findLast((l) => l[0] === 'approveSpawn'), ['approveSpawn', 7, { cwd: '/srv/ops' }], 'unchanged requested fields are not sent as overrides');
  await s.click(s.q('[data-spawn="7"] [data-fa="deny"]'));
  await fill('#fleet-spawn-deny-reason', ' no capacity ');
  await s.click(doc.querySelector('#fleet-spawn-deny-go'));
  assert.deepEqual(s.log.findLast((l) => l[0] === 'denySpawn'), ['denySpawn', 7, 'no capacity']);
  await s.click(s.q('[data-spawn="6"] [data-fa="abandon"]'));
  assert.match(s.confirms.at(-1).body, /original worker may still appear late \(agt_late1\)/);
  assert.deepEqual(s.log.findLast((l) => l[0] === 'abandonSpawn'), ['abandonSpawn', 6]);
  await s.click(s.q('#fleet-spawn-new'));
  assert.equal(doc.querySelector('#fleet-spawn-send-go').disabled, true, 'a brief and group are needed');
  await fill('#fleet-spawn-group', 'ops');
  await fill('#fleet-spawn-brief', 'Review the release notes');
  await s.click(doc.querySelector('#fleet-spawn-send-go'));
  assert.match(s.confirms.at(-1).body, /forge's operator gets your brief for its group ops.*starts right away without its operator deciding/);
  assert.deepEqual(s.log.findLast((l) => l[0] === 'sendSpawn'), ['sendSpawn', { brief: 'Review the release notes', group: 'ops', peer: 'inst_forge' }]);
  const all = s.q('.fa-spawns input[type=checkbox]');
  await s.check(all);
  assert.equal(s.mounted.container.querySelectorAll('#fleet-spawn-requests tbody tr').length, 3);
});

test('grant launch settings and model gateway scopes reach the grant body and the confirm', async (t) => {
  const s = await setup(t);
  await s.show();
  await s.click(s.q('[data-peer="inst_forge"] [data-fa="grants"]'));
  const pick = async (sel, value) => {
    const el = s.q(sel);
    for (const o of el.querySelectorAll('option')) { if (o.value === value) o.setAttribute('selected', ''); else o.removeAttribute('selected'); }
    await s.harness.act(() => s.harness.fireEvent(el, 'change'));
  };
  const type = async (sel, v) => { const el = s.q(sel); el.value = v; await s.harness.act(() => s.harness.fireEvent(el, 'input')); };
  await pick('#fleet-grant-slug', 'groups.members.spawn');
  await pick('#fleet-grant-group', 'ops');
  await s.click(s.q('#fleet-grant-launch-toggle'));
  await type('[data-launch="profile"]', 'opus-fast');
  await type('[data-launch="allowed_profiles"]', 'opus-fast, sonnet-review');
  await type('[data-launch="cwd"]', '/srv/ops');
  await pick('[data-launch="requester_pays"]', 'required');
  await s.click(s.q('#fleet-grant-submit'));
  // A spawn-only setting typed, then a switch to jobs.run: it is not sent.
  await pick('[data-launch="requester_pays"]', 'allowed');
  assert.match(s.confirms.at(-1).body, /Workers start with profile opus-fast; selectable opus-fast, sonnet-review; directory \/srv\/ops; requester pays: required\. Its workers must use its own model gateway/);
  assert.deepEqual(s.log.filter((l) => l[0] === 'grant').at(-1)[1], { peer: 'inst_forge', slug: 'groups.members.spawn', scope: 'group=ops',
    spawn_policy: { profile: 'opus-fast', allowed_profiles: ['opus-fast', 'sonnet-review'], cwd: '/srv/ops', requester_pays: 'required', max_live: 2 } });
  await pick('#fleet-grant-slug', 'jobs.run');
  assert.equal(s.q('[data-launch="requester_pays"]'), null, 'requester pays is a spawn setting');
  await pick('[data-launch="job_approval"]', 'manual');
  await s.click(s.q('#fleet-grant-submit'));
  assert.match(s.confirms.at(-1).body, /Each job waits for your approval/);
  assert.equal(s.log.filter((l) => l[0] === 'grant').at(-1)[1].spawn_policy.requester_pays, undefined, 'a spawn-only setting never rides along');
  await pick('#fleet-grant-slug', 'models.proxy');
  await type('#fleet-grant-gateway', 'claude');
  await s.click(s.q('#fleet-grant-submit'));
  assert.match(s.confirms.at(-1).body, /on gateway claude only/);
  assert.deepEqual(s.log.filter((l) => l[0] === 'grant').at(-1)[1], { peer: 'inst_forge', slug: 'models.proxy', scope: 'http_proxy=claude' });
});

test('profile editor: create and edit confirm the effect, keep advanced fields, and explain a stale revision', async (t) => {
  const s = await setup(t);
  s.actions.profiles = async () => ({ profiles: [{ id: 'prof_1', name: 'test-rig', revision: 3, definition: { trust_level: 'restricted', pools: ['pool_r'], labels: ['gpu'], peer_grants: [{ slug: 'jobs.run', scope: 'group=7', spawn_policy: { max_live: 3, job_approval: 'manual' } }], worker_permissions: { 'tasks.read': { allow: true } } } }], default: null });
  await openProfiles(s, [{ id: 'pool_r', name: 'rigs', members: [] }, { id: 'pool_b', name: 'builders', members: [] }]);
  const doc = s.harness.document; const q = (x) => doc.querySelector(x);
  const type = async (sel, v) => { const el = q(sel); el.value = v; await s.harness.act(() => s.harness.fireEvent(el, 'input')); };
  await s.click(s.q('#fleet-profile-new'));
  await type('#fleet-profile-name', 'Bad Name');
  await s.click(q('#fleet-profile-save'));
  assert.match(q('#fleet-profile-editor [role=alert]').textContent, /lowercase/);
  await type('#fleet-profile-name', 'ci-workers');
  await type('#fleet-profile-labels', 'ci, linux');
  await s.check(q('[data-pool="pool_b"]'));
  await s.click(q('#fleet-profile-add-grant'));
  await s.click(q('#fleet-profile-save'));
  assert.match(s.confirms.at(-1).body, /restricted trust, 1 peer grant\(s\) and 1 pool membership.*Nothing changes for any peer until you apply it/);
  assert.deepEqual(s.log.findLast((l) => l[0] === 'createProfile')[1], { name: 'ci-workers', definition: { trust_level: 'restricted', pools: ['pool_b'], labels: ['ci', 'linux'], peer_grants: [{ slug: 'message.direct', scope: '' }] } });
  await s.click(s.q('#fleet-profiles [data-profile="test-rig"] [data-fa="edit-profile"]'));
  assert.match(q('#fleet-profile-grants').textContent, /jobs\.run.*group #7/s);
  assert.match(q('#fleet-profile-advanced').value, /tasks\.read/);
  const lvl = q('#fleet-profile-level');
  for (const o of lvl.querySelectorAll('option')) { if (o.value === 'unrestricted') o.setAttribute('selected', ''); else o.removeAttribute('selected'); }
  await s.harness.act(() => s.harness.fireEvent(lvl, 'change'));
  await s.click(q('#fleet-profile-save'));
  assert.match(s.confirms.at(-1).title, /revision 3 → 4/);
  assert.match(s.confirms.at(-1).body, /unrestricted trust.*created later.*applied to 1 peer\(s\); they keep their current settings until you apply it again.*Invites already issued for test-rig pin revision 3 and stop working/);
  const saved = s.log.findLast((l) => l[0] === 'saveProfile')[1];
  assert.deepEqual(saved, { id: 'prof_1', name: 'test-rig', revision: 3, definition: { trust_level: 'unrestricted', pools: ['pool_r'], labels: ['gpu'],
    peer_grants: [{ slug: 'jobs.run', scope: 'group=7', spawn_policy: { max_live: 3, job_approval: 'manual' } }], worker_permissions: { 'tasks.read': { allow: true } } } }, 'launch settings and advanced fields survive');
});

test('node settings: moving to another hub and changing labels confirm the consequence and send only changes', async (t) => {
  const s = await setup(t);
  await s.show();
  await s.click(s.q('#fleet-node-settings-open'));
  const doc = s.harness.document; const q = (x) => doc.querySelector(x);
  const type = async (sel, v) => { const el = q(sel); el.value = v; await s.harness.act(() => s.harness.fireEvent(el, 'input')); };
  assert.equal(q('#fleet-hub-url').value, 'wss://hub.example');
  await s.click(q('#fleet-hub-save'));
  assert.match(q('#fleet-node-settings [role=alert]').textContent, /Nothing changed/);
  await type('#fleet-hub-url', 'wss://hub2.example:8470');
  await type('#fleet-hub-ca', 'certs/ca.pem');
  await s.click(q('#fleet-hub-save'));
  assert.match(q('#fleet-node-settings [role=alert]').textContent, /absolute path/);
  await type('#fleet-hub-ca', '/etc/tclaude/hub-ca.pem');
  await type('#fleet-hub-invite', 'inv-123');
  await s.click(q('#fleet-hub-save'));
  assert.match(s.confirms.at(-1).title, /Move this node to the hub at wss:\/\/hub2\.example:8470/);
  assert.match(s.confirms.at(-1).body, /nothing here is shared with a peer until you trust it.*current hub connection drops.*single-use.*\/etc\/tclaude\/hub-ca\.pem/);
  assert.deepEqual(s.log.findLast((l) => l[0] === 'hubConfig')[1], { hub_url: 'wss://hub2.example:8470', invite: 'inv-123', hub_ca_file: '/etc/tclaude/hub-ca.pem' });
  await s.click(s.q('#fleet-node-settings-open'));
  await type('#fleet-hub-url', 'wss://hub3.example');
  await s.click(q('#fleet-hub-save'));
  assert.match(s.confirms.at(-1).body, /pinned hub CA file is kept/);
  assert.deepEqual(s.log.findLast((l) => l[0] === 'hubConfig')[1], { hub_url: 'wss://hub3.example', invite: '' }, 'a move never resends the old invite');
  await s.click(s.q('#fleet-node-settings-open'));
  await type('#fleet-hub-url', 'wss://hub4.example');
  const clear = q('#fleet-hub-ca-clear'); clear.checked = true;
  await s.harness.act(() => s.harness.fireEvent(clear, 'change'));
  await s.click(q('#fleet-hub-save'));
  assert.match(s.confirms.at(-1).body, /CA file is cleared/);
  assert.deepEqual(s.log.findLast((l) => l[0] === 'hubConfig')[1], { hub_url: 'wss://hub4.example', invite: '', hub_ca_file: '' });
  await s.click(s.q('#fleet-node-settings-open'));
  await s.harness.act(() => new Promise((r) => setTimeout(r, 25)));
  await type('#fleet-node-labels', 'gpu linux, bad label!');
  await s.click(q('#fleet-labels-save'));
  assert.match(q('#fleet-node-settings [role=alert]').textContent, /label!/);
  await type('#fleet-node-labels', 'gpu, linux');
  await s.click(q('#fleet-labels-save'));
  assert.match(s.confirms.at(-1).body, /Adds linux\. Removes ci\..*stops landing here/);
  assert.deepEqual(s.log.findLast((l) => l[0] === 'labels')[1], { add: ['linux'], remove: ['ci'] });
});

test('live terminal viewers show on Peers only while someone watches, and disconnecting one says it can come back', async (t) => {
  const s = await setup(t);
  s_viewers = [{ id: 'v1', peer: 'inst_forge', agent: 'agt_a1', session: 'ada', group: 'ops', read_only: false, started: '2026-10-10T09:00:00Z', incoming: true }];
  t.after(() => { s_viewers = []; });
  await s.show();
  assert.match(s.q('#fleet-viewers').textContent, /forge.*ada.*ops.*interactive/s);
  await s.click(s.q('[data-viewer="v1"] [data-fa="kick"]'));
  assert.match(s.confirms.at(-1).body, /interactive session \(it can type, including answering harness prompts\) of ada closes now.*(any sessions\.watch \(read-only\) or sessions\.attach \(typing\) grant covers group ops — direct, all-groups or through a pool)/);
  assert.deepEqual(s.log.findLast((l) => l[0] === 'kick'), ['kick', 'v1']);
  assert.equal(s.q('#fleet-viewers'), null, 'nobody watching: nothing shown');
});

test('offers: preview then apply a config offer item by item, start a moved agent, decline, and send offers with flagged-credential handling', async (t) => {
  const s = await setup(t);
  const doc = s.harness.document; const q = (x) => doc.querySelector(x);
  const type = async (el, v) => { el.value = v; await s.harness.act(() => s.harness.fireEvent(el, 'input')); };
  const tick = async (el, on) => { el.checked = on; await s.harness.act(() => s.harness.fireEvent(el, 'change')); };
  await s.show();
  await s.click([...s.mounted.container.querySelectorAll('.fa-subtab')].find((b) => b.textContent === 'Offers'));
  assert.equal(s.q('#fleet-offers-in').querySelectorAll('tbody tr').length, 3);
  assert.equal(s.q('[data-offer="off_old"] [data-fa]'), null, 'a finished offer has no actions');
  assert.match(s.q('[data-offer="off_mv"]').textContent, /agent move.*→ group ops/);
  assert.match(s.q('#fleet-offers-out').textContent, /lab.*declined/);

  // Config: a conflicting item blocks apply until it is unticked (or replaced).
  await s.click(s.q('[data-offer="off_cfg"] [data-fa="preview"]'));
  assert.deepEqual(s.log.findLast((l) => l[0] === 'import'), ['import', 'off_cfg', { apply: false }]);
  assert.equal(q('#fleet-offer-changes').querySelectorAll('tbody tr').length, 3);
  assert.match(q('[data-item="roles/reviewer"]').textContent, /new.*security/);
  assert.match(q('[data-item="config/theme"]').textContent, /overwrites yours/);
  assert.equal(q('#fleet-offer-apply').disabled, true, 'a conflict needs replace or exclusion');
  await tick(q('[data-item="config/theme"] input'), false);
  assert.equal(q('#fleet-offer-apply').disabled, true, 'a changed selection needs a fresh preview');
  await s.click(q('#fleet-offer-preview'));
  assert.deepEqual(s.log.findLast((l) => l[0] === 'import')[2], { apply: false, skip: ['config/theme'] });
  assert.ok(q('[data-item="config/theme"]'), 'a skipped item stays listed so it can be re-included');
  assert.equal(q('#fleet-offer-apply').disabled, false);
  await s.click(q('#fleet-offer-apply'));
  assert.match(s.confirms.at(-1).body, /^Applies 1 item from forge.*take effect immediately: 1 new\. Security-relevant: roles\/reviewer.*forge is told the offer was applied/);
  assert.deepEqual(s.log.findLast((l) => l[0] === 'import')[2], { apply: true, skip: ['config/theme'] });
  assert.equal(q('#fleet-offer-import'), null);
  assert.match(s.toasts.at(-1), /Applied 1 items from forge/);

  // Agent move: a placeholder needs a value; the confirm names the move.
  await s.click(s.q('[data-offer="off_mv"] [data-fa="preview"]'));
  assert.deepEqual(s.log.findLast((l) => l[0] === 'import')[2], { apply: false }, 'the group bound on receipt applies unless overridden');
  assert.equal(q('#fleet-offer-skip-history'), null, 'a move always carries history');
  assert.match(q('#fleet-offer-import').textContent, /Suspected credentials in the history: api_key ×1/);
  assert.equal(q('#fleet-offer-apply').disabled, true, 'an unresolved placeholder blocks apply');
  await type(q('[data-placeholder="REPO"]'), '/srv/repo');
  await s.click(q('#fleet-offer-preview'));
  await s.click(q('#fleet-offer-apply'));
  assert.match(s.confirms.at(-1).title, /Start ada from forge/);
  assert.match(s.confirms.at(-1).body, /new agent ada on this node in \/srv\/ada.*shared conversation history.*suspected credentials: api_key ×1.*not copied\. This is a move: once it runs here, forge retires its source agent/);
  assert.deepEqual(s.log.findLast((l) => l[0] === 'import')[2], { apply: true, values: { REPO: '/srv/repo' } });
  assert.match(s.toasts.at(-1), /Started agt_new/);

  await s.click(s.q('[data-offer="off_cfg"] [data-fa="decline"]'));
  assert.match(s.confirms.at(-1).body, /payload is deleted and forge is told it was declined/);
  assert.deepEqual(s.log.findLast((l) => l[0] === 'decline'), ['decline', 'off_cfg', 'inst_forge']);

  // Offer config: flagged items are shown and need an explicit send-anyway.
  await s.click(s.q('#fleet-offer-config-open'));
  await tick(q('[data-section="roles"]'), true);
  await s.click(q('#fleet-offer-config-send'));
  assert.match(s.confirms.at(-1).body, /Sends roles to forge's operator.*free text .* is sent as written/);
  assert.match(q('#fleet-offer-config').textContent, /roles\/reviewer prompt: looks like a token/);
  assert.equal(q('#fleet-offer-config-send').disabled, true);
  await tick(q('#fleet-offer-config-allow'), true);
  await s.click(q('#fleet-offer-config-send'));
  assert.match(s.confirms.at(-1).body, /includes 1 item flagged as possible credentials/);
  assert.deepEqual(s.log.findLast((l) => l[0] === 'offerConfig')[1], { peer: 'inst_forge', only: ['roles'], allow_flagged: true });

  // Share an agent: only agents with a stable ID; the receiving group is required.
  await s.click(s.q('#fleet-share-agent-open'));
  assert.deepEqual([...q('#fleet-share-agent-agent').querySelectorAll('option')].map((o) => o.value), ['agt_a1']);
  await s.click(q('#fleet-share-agent-send'));
  assert.match(q('#fleet-share-agent [role=alert]').textContent, /receiving group/);
  await type(q('#fleet-share-agent-group'), 'team');
  await tick(q('#fleet-share-agent-history'), true);
  await s.click(q('#fleet-share-agent-send'));
  assert.match(s.confirms.at(-1).body, /and a copy of its conversation history.*group team\. ada \(agt_a1\) keeps running here/);
  assert.deepEqual(s.log.findLast((l) => l[0] === 'shareAgent')[1], { agent: 'agt_a1', peer: 'inst_forge', group: 'team', history: true, allow_flagged: false });

  await s.click(s.q('#fleet-offer-profile-open'));
  await s.click(q('#fleet-offer-profile-send'));
  assert.deepEqual(s.log.findLast((l) => l[0] === 'offerProfile'), ['offerProfile', 'ops-full', 'inst_forge']);
  assert.match(q('#fleet-offer-profile [role=alert]').textContent, /apply the profile to forge first/);
  assert.match(s.confirms.at(-1).body, /as one config offer/);
  s.actions.offerProfile = async () => ({ unchanged: true, offer_id: 'off_p' });
  await s.click(q('#fleet-offer-profile-send'));
  assert.match(q('#fleet-offer-profile [role=alert]').textContent, /Nothing sent: an identical offer \(off_p\)/);
  assert.ok(q('#fleet-offer-profile'), 'nothing sent: the dialog stays open');

  const m = await s.harness.importDashboardModule('js/fleet-admin-offers.js');
  const tp = { offer: { type: 'agent', teleport: { credentials: 'peer-proxy' } } };
  assert.match(m.agentConsequence('forge', tp, { agent: { name: 'ada' }, credentials: 'peer-proxy' }), /credential mode peer-proxy \(arranged with forge, not this node's own harness login\).*teleport/);
  assert.match(m.agentConsequence('forge', { offer: { type: 'agent' } }, { agent: { name: 'ada' } }), /this node's harness credentials.*keeps running there/);
});

test('offer actions address an incoming offer by ID and source peer', async (t) => {
  const harness = await createPreactHarness(t);
  const { createFleetAdminActions } = await harness.importDashboardModule('js/fleet-admin-actions.js');
  const calls = [];
  const a = createFleetAdminActions({ fetchImpl: async (url, init) => { calls.push([init.method, url, init.body ? JSON.parse(init.body) : null]); return { ok: true, status: 200, json: async () => [] }; } });
  const o = { offer: { id: 'off 1' }, peer: 'inst_forge' };
  await a.offers('in');
  await a.importOffer(o, { apply: false });
  await a.declineOffer(o);
  await a.offerProfile('rig', 'inst_forge');
  assert.deepEqual(calls, [
    ['GET', '/api/federation/bundle-offers?direction=in', null],
    ['POST', '/api/federation/bundle-offers/off%201/import?peer=inst_forge', { apply: false }],
    ['POST', '/api/federation/bundle-offers/off%201/decline?peer=inst_forge', {}],
    ['POST', '/api/federation/profiles/rig/offer', { peer: 'inst_forge' }],
  ]);
});

test('moves: both directions listed, only abandonable outgoing moves offer Abandon, and the teleport freeze confirms', async (t) => {
  const s = await setup(t);
  await s.show();
  await s.click([...s.mounted.container.querySelectorAll('.fa-subtab')].find((b) => /Moves/.test(b.textContent)));
  const rows = [...s.mounted.container.querySelectorAll('#fleet-moves tbody tr')];
  assert.equal(rows.length, 3);
  assert.match(s.q('[data-move="m1"]').textContent, /⇢.*move.*forge.*ops.*awaiting_confirmation/s);
  assert.match(s.q('[data-move="m2"]').textContent, /⇠.*teleport.*→/s);
  assert.deepEqual(rows.filter((r) => r.querySelector('[data-fa="abandon"]')).map((r) => r.dataset.move), ['m1'], 'arriving and retiring moves cannot be abandoned');
  await s.click(s.q('[data-move="m1"] [data-fa="abandon"]'));
  assert.match(s.confirms.at(-1).body, /does not retire it here.*copy stays there as an independent agent/);
  assert.ok(s.log.some((l) => l[0] === 'abandon' && l[1] === 'm1'));
  await s.click(s.q('#fleet-teleport-toggle'));
  assert.match(s.confirms.at(-1).body, /can no longer teleport to a peer, and teleports from peers can no longer land here/);
  assert.ok(s.log.some((l) => l[0] === 'teleport' && l[1] === true));
  assert.match(s.q('#fleet-teleport').textContent, /frozen/);
});

test('jobs & repos: approve, cancel, resend and acknowledge with spelled-out consequences; send jobs; allow repositories', async (t) => {
  const s = await setup(t);
  const doc = s.harness.document; const q = (x) => doc.querySelector(x);
  const type = async (el, v) => { el.value = v; await s.harness.act(() => s.harness.fireEvent(el, 'input')); };
  const tick = async (el, on) => { el.checked = on; await s.harness.act(() => s.harness.fireEvent(el, 'change')); };
  await s.show();
  await s.click([...s.mounted.container.querySelectorAll('.fa-subtab')].find((b) => b.textContent === 'Jobs & repos'));
  assert.equal(s.q('#fleet-jobs').querySelectorAll('tbody tr').length, 4);
  assert.match(s.mounted.container.textContent, /1 waiting for your approval/);
  assert.ok(s.timers.queue.some((x) => x.ms === 5000), 'the job list polls while shown');
  const acts = (id) => [...s.q(`[data-job="${id}"]`).querySelectorAll('[data-fa]')].map((b) => b.dataset.fa);
  assert.deepEqual(acts('job_in1'), ['approve', 'cancel']);
  assert.deepEqual(acts('job_in2'), ['ack']);
  assert.deepEqual(acts('job_out1'), ['cancel', 'retry']);
  assert.deepEqual(acts('job_out2'), ['logs']);
  assert.match(s.q('[data-job="job_out1"]').textContent, /site@v2.*fix the flaky test.*codex/);

  await s.click(s.q('[data-job="job_in1"] [data-fa="approve"]'));
  assert.match(s.confirms.at(-1).body, /one-shot worker runs this shell command in tclaude@main \(group ops\) on this node, as the user tclaude runs as, for up to 600 s.*logins and network.*Command: go test \.\/pkg\/\.\.\./);
  assert.deepEqual(s.log.findLast((l) => l[0] === 'approveJob'), ['approveJob', 'job_in1']);
  await s.click(s.q('[data-job="job_in1"] [data-fa="cancel"]'));
  assert.match(s.confirms.at(-1).body, /refused without running/);
  await s.click(s.q('[data-job="job_out1"] [data-fa="retry"]'));
  assert.match(s.confirms.at(-1).body, /same job ID.*will not run twice/);
  await s.click(s.q('[data-job="job_in2"] [data-fa="ack"]'));
  assert.match(s.confirms.at(-1).body, /Only confirm after checking that its worker and anything it started have stopped/);
  assert.deepEqual(s.log.findLast((l) => l[0] === 'ackJob'), ['ackJob', 'job_in2']);
  await s.click(s.q('[data-job="job_out2"] [data-fa="logs"]'));
  assert.match(q('#fleet-job-logs').textContent, /abcdef123456.*exit 0.*index\.html/s);
  await s.click([...q('#fleet-job-logs').querySelectorAll('button')].find((b) => b.textContent === 'Close'));

  // Fan-out: per-node refusals are reported.
  await s.click(s.q('#fleet-job-open'));
  await s.click(q('#fleet-job-send'));
  assert.match(q('#fleet-run-job [role=alert]').textContent, /at least one node/);
  await tick(q('[data-node="inst_forge"]'), true);
  await tick(q('[data-node="inst_lab"]'), true);
  await type(q('#fleet-job-repo'), 'tclaude');
  await type(q('#fleet-job-ref'), 'main');
  await type(q('#fleet-job-group'), 'ops');
  await type(q('#fleet-job-command'), 'go vet ./...');
  await s.click(q('#fleet-job-send'));
  assert.match(s.confirms.at(-1).body, /to forge, lab: a one-shot worker runs it in a checkout of tclaude at main in group ops, for up to 3600 s\. It runs as soon as a node admits it — unless that node's jobs\.run grant to you asks for manual approval/);
  assert.deepEqual(s.log.findLast((l) => l[0] === 'runJob')[1], { repo: 'tclaude', ref: 'main', group: 'ops', command: 'go vet ./...', timeout_seconds: 3600, nodes: ['inst_forge', 'inst_lab'] });
  assert.match(s.toasts.at(-1), /1 job sent; 1 refused \(lab: peer does not export this group for jobs\)/);

  // Repositories: group IDs show until names come; editing re-picks groups.
  assert.match(s.q('[data-repo="tclaude"]').textContent, /group #7/);
  await s.click(s.q('#fleet-repo-add'));
  await type(q('#fleet-repo-name'), 'site');
  await type(q('#fleet-repo-url'), 'git@example:site.git');
  await type(q('#fleet-repo-clone'), 'src/site');
  await tick(q('[data-group="ops"]'), true);
  await s.click(q('#fleet-repo-save'));
  assert.match(q('#fleet-repo [role=alert]').textContent, /absolute path/);
  await type(q('#fleet-repo-clone'), '/srv/site');
  await s.click(q('#fleet-repo-save'));
  assert.match(s.confirms.at(-1).body, /Peers granted jobs\.run in ops can ask to run commands in a checkout of \/srv\/site.*any ref they name\. Jobs run as soon as they arrive.*job_approval=manual/);
  assert.deepEqual(s.log.findLast((l) => l[0] === 'addRepo')[1], { name: 'site', url: 'git@example:site.git', clone: '/srv/site', groups: ['ops'] });
  await s.click(s.q('[data-repo="tclaude"] [data-fa="edit"]'));
  assert.match(q('#fleet-repo').textContent, /Currently group #7; tick the groups to keep/);
  await tick(q('[data-group="build"]'), true);
  await s.click(q('#fleet-repo-save'));
  assert.deepEqual(s.log.findLast((l) => l[0] === 'updateRepo'), ['updateRepo', 'tclaude', { name: 'tclaude', url: 'git@github.com:tofutools/tclaude.git', clone: '/home/me/git/tclaude', groups: ['build'], revision: 2 }]);
  await s.click(s.q('[data-repo="tclaude"] [data-fa="disable"]'));
  assert.match(s.confirms.at(-1).body, /still waiting for approval can no longer run: approving one fails it.*Re-enable… allows it again/);
  assert.deepEqual(s.log.findLast((l) => l[0] === 'disableRepo'), ['disableRepo', 'tclaude']);
  await s.click(s.q('[data-repo="old"] [data-fa="enable"]'));
  assert.match(q('#fleet-repo-title').textContent, /Re-enable repository old/);
  await s.click(q('#fleet-repo-save'));
  assert.deepEqual(s.log.findLast((l) => l[0] === 'updateRepo'), ['updateRepo', 'old', { name: 'old', url: 'git@x:old.git', clone: '/srv/old', groups: ['ops'], revision: 4 }], 're-enabling is a PUT with the revision');

  await s.harness.act(() => { s.activeTab.value = 'groups'; });
  await s.harness.act(() => new Promise((r) => setTimeout(r, 25)));
  assert.equal(s.timers.queue.filter((x) => x.ms === 5000).length, 0, 'leaving Fleet stops the job poll');
});

test('job actions hit the local job and repo routes', async (t) => {
  const harness = await createPreactHarness(t);
  const { createFleetAdminActions } = await harness.importDashboardModule('js/fleet-admin-actions.js');
  const calls = [];
  const a = createFleetAdminActions({ fetchImpl: async (url, init) => { calls.push([init.method, url, init.body ? JSON.parse(init.body) : null]); return { ok: true, status: 200, json: async () => ({}) }; } });
  await a.acknowledgeJobStopped('j1');
  await a.jobLogs('j1');
  await a.updateRepo('my repo', { revision: 3 });
  await a.disableRepo('r');
  assert.deepEqual(calls, [
    ['POST', '/api/federation/jobs/j1/acknowledge-stopped', { acknowledge_stopped: true }],
    ['GET', '/api/federation/jobs/j1/logs', null],
    ['PUT', '/api/federation/repos/my%20repo', { revision: 3 }],
    ['DELETE', '/api/federation/repos/r', null],
  ]);
});

test('model gateways: switches confirm what they revoke, leases revoke, and usage sums per gateway, peer and model', async (t) => {
  const s = await setup(t);
  await s.show();
  await s.click([...s.mounted.container.querySelectorAll('.fa-subtab')].find((b) => /Model gateways/.test(b.textContent)));
  assert.match(s.q('#fleet-models-master').textContent, /on \(1 of 2 enabled\)/);
  assert.match(s.q('[data-gateway="claude"]').textContent, /claude-sonnet-5-5.*500 req \/ 2M tok · 100 req \/ 500k tok · 50 req \/ 100k tok.*4 at once.*lab/s);
  assert.match(s.q('[data-gateway="openai"]').textContent, /disabled.*none/s);
  await s.click(s.q('#fleet-models-master-toggle'));
  assert.match(s.confirms.at(-1).body, /every active lease is revoked at once/);
  assert.deepEqual(s.log.findLast((l) => l[0] === 'modelSwitch'), ['modelSwitch', { disabled: true }]);
  await s.click(s.q('[data-gateway="openai"] [data-fa="gateway"]'));
  assert.match(s.confirms.at(-1).body, /charged to this node's provider account.*missing model allowlist, daily budget.*still refuses every request/);
  assert.deepEqual(s.log.findLast((l) => l[0] === 'modelSwitch'), ['modelSwitch', { name: 'openai', disabled: false }]);
  await s.click(s.q('[data-gateway="claude"] [data-fa="unblock"]'));
  assert.deepEqual(s.log.findLast((l) => l[0] === 'modelSwitch'), ['modelSwitch', { name: 'claude', peer: 'inst_lab', disabled: false }]);
  const sel = s.q('[data-gateway="claude"] .fa-model-block');
  assert.deepEqual([...sel.querySelectorAll('option')].map((o) => o.textContent), ['block a peer…', 'forge'], 'a blocked peer is not offered again');
  for (const o of sel.querySelectorAll('option')) { if (o.value === 'inst_forge') o.setAttribute('selected', ''); else o.removeAttribute('selected'); }
  await s.harness.act(() => s.harness.fireEvent(sel, 'change'));
  await s.click(s.q('[data-gateway="claude"] [data-fa="block"]'));
  assert.match(s.confirms.at(-1).body, /forge can no longer use claude.*leases on claude are revoked/);
  assert.deepEqual(s.log.findLast((l) => l[0] === 'modelSwitch'), ['modelSwitch', { name: 'claude', peer: 'inst_forge', disabled: true }]);
  assert.equal(s.mounted.container.querySelectorAll('#fleet-model-leases [data-fa="revoke-lease"]').length, 1, 'a revoked lease offers nothing');
  await s.click(s.q('[data-lease="lease_aaaaaaaaaaaa1"] [data-fa="revoke-lease"]'));
  assert.match(s.confirms.at(-1).body, /forge worker agt_w1 using this lease loses model access through claude/);
  assert.deepEqual(s.log.findLast((l) => l[0] === 'revokeLease'), ['revokeLease', 'lease_aaaaaaaaaaaa1']);
  const usage = [...s.mounted.container.querySelectorAll('#fleet-model-usage tbody tr')].map((r) => [...r.querySelectorAll('td')].map((c) => c.textContent.trim()).join('|'));
  assert.deepEqual(usage, ['claude|forge|claude-sonnet-5-5|3 (1 in progress) (1 failed)|109.5k|1.3k · 200']);
});

test('a pending key transition is information only, with no recovery command', async (t) => {
  t.after(() => { s_transition = null; });
  s_transition = { state: 'pending', old_id: 'inst_forge', new_id: 'inst_forge2', new_fingerprint: FP_NEW, received_at: '2026-10-10T09:00:00Z', accept_after: '2026-10-10T09:10:00Z' };
  const s = await setup(t);
  await s.show();
  const q = (x) => s.harness.document.querySelector(x);
  assert.match(s.q('[data-peer="inst_forge"] [data-fa="key"]').textContent, /new key pending/);
  assert.equal(s.q('[data-peer="inst_lab"] [data-fa="key"]'), null);
  await s.click(s.q('[data-peer="inst_forge"] [data-fa="key"]'));
  assert.match(q('#fleet-key-transition').textContent, new RegExp(`inst_forge2.*${FP_NEW}.*earliest acceptance.*nothing needs doing now.*Acceptance is not guaranteed`, 's'));
  assert.equal(q('#fleet-key-preview'), null, 'pending offers no recovery command');
  assert.equal(/needs recovery|recover-peer/.test(q('#fleet-key-transition').textContent), false);
});

test('a conflicted key transition shows the recover-peer preview and, separately, the verified apply', async (t) => {
  t.after(() => { s_transition = null; });
  s_transition = { state: 'conflict', old_id: 'inst_forge', new_id: 'inst_forge2', new_fingerprint: FP_NEW, received_at: '2026-10-10T09:00:00Z', reason: 'competing successors' };
  const s = await setup(t);
  await s.show();
  const q = (x) => s.harness.document.querySelector(x);
  assert.match(s.q('[data-peer="inst_forge"] [data-fa="key"]').textContent, /competing keys/);
  await s.click(s.q('[data-peer="inst_forge"] [data-fa="key"]'));
  assert.equal(q('#fleet-key-preview').textContent, 'tclaude federation identity recover-peer inst_forge inst_forge2');
  assert.equal(q('#fleet-key-apply').textContent, `tclaude federation identity recover-peer inst_forge inst_forge2 --fingerprint ${FP_NEW} --apply`);
  assert.match(q('#fleet-key-transition').textContent, /one candidate.*others.*identity rotations.*out of band.*Only if the operator confirmed this exact candidate's fingerprint/s);
});

test('keyTransition shows only pending and conflict, and recoverCommands refuses odd IDs', async (t) => {
  const harness = await createPreactHarness(t);
  const { keyTransition, recoverCommands, normalizeFleet } = await harness.importDashboardModule('js/skynet-model.js');
  assert.equal(keyTransition({ identity_transition: { state: 'accepted' } }), null);
  assert.equal(keyTransition({}), null);
  const t1 = keyTransition({ identity_transition: { state: 'conflict', old_id: 'inst_a', new_id: 'inst_b', new_fingerprint: 'ab-cd' } });
  assert.equal(t1.state, 'conflict');
  assert.equal(recoverCommands({ ...t1, newID: 'inst_b; rm -rf ~' }), null);
  assert.equal(recoverCommands({ ...t1, newFingerprint: '$(x)' }).apply, '');
  assert.equal(recoverCommands({ ...t1, newID: '--apply' }), null, 'a flag-shaped ID never lands in a command');
  assert.equal(recoverCommands({ ...t1, newFingerprint: '--x' }).apply, '');
  const fleet = normalizeFleet({ instance_id: 'inst_self', peers: [{ instance_id: 'inst_a', trusted: true, identity_transition: { state: 'pending', old_id: 'inst_a', new_id: 'inst_b' } }] });
  assert.equal(fleet.peers[0].keyTransition.state, 'pending');
});

