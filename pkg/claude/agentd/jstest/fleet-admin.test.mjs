import test from 'node:test'; import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

const FP_FORGE = 'k7q2-mx9d-4hpa-zz31-0e8c';
const FP_NEW = 'w5ze-a3nq-9c1b-77f0-d2aa';
const status = () => ({
  enabled: true, instance_id: 'inst_self', name: 'desk', fingerprint: 'self-fp-0000', hub_url: 'wss://hub.example', hub: { state: 'connected' },
  peers: [
    { instance_id: 'inst_forge', label: 'forge', name: 'forge', fingerprint: FP_FORGE, trusted: true, online: true, level: 'restricted', last_seen: '0001-01-01T00:00:00Z' },
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
    audit: async (o) => { log.push(['audit', o]); return Array.from({ length: o.limit === 200 ? 200 : 3 }, (_, i) => ({ id: `a${i}`, at: '2026-10-10T10:00:00Z', source: 'remote', direction: i % 2 ? 'out' : 'in', peer: 'inst_forge', kind: 'mail.send', actor: 'agt_x', status: i === 0 ? 403 : 200 })); },
    createPool: async (n) => { log.push(['createPool', n]); return { ok: true }; },
    deletePool: async (n) => { log.push(['deletePool', n]); return { ok: true }; },
    addPoolMember: async (n, p) => { log.push(['addPoolMember', n, p]); return { ok: true }; },
    removePoolMember: async (n, p) => { log.push(['removePoolMember', n, p]); return { ok: true }; },
    setDefaultProfile: async (n) => { log.push(['setDefaultProfile', n]); return { profile_id: n }; },
    deleteProfile: async (n) => { log.push(['deleteProfile', n]); return { ok: true }; },
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
  assert.equal(s.timers.queue.length, 1);
  await s.harness.act(() => { s.activeTab.value = 'groups'; });
  assert.equal(s.timers.queue.length, 0, 'leaving the view stops the poll');
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
