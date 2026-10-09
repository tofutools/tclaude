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
      { peer: 'inst_forge', slug: 'message.direct', scope: '' },
      { peer: 'inst_forge', slug: 'groups.roster.read', scope: 'group=ops' },
      { peer: 'inst_forge', slug: 'routes.consume', scope: '', pool_id: 'p1', pool_name: 'rigs' },
    ] : []; },
    grant: async (body) => { log.push(['grant', body]); return { ok: true, warnings: ['WARNING: unscoped peer grant covers every active group, including future groups'] }; },
    revoke: async (body) => { log.push(['revoke', body]); return { ok: true }; },
  };
  const snapshot = harness.signals.signal({ groups: [{ name: 'ops' }, { name: 'build' }] });
  // Like shellConfirm: a confirmed action resolves to the action's result.
  const confirm = async (opts) => { confirms.push(opts); return opts.action ? opts.action() : true; };
  const timers = fakeTimers();
  const mounted = await harness.mount(harness.html`<${island.FleetAdmin} state=${state} actions=${actions} confirm=${confirm} toast=${(m) => toasts.push(m)} timers=${timers} remote="" copy=${async () => {}} snapshot=${snapshot} />`);
  const q = (sel) => mounted.container.querySelector(sel);
  const check = async (el) => { el.checked = true; await harness.act(() => harness.fireEvent(el, 'change')); };
  const settle = () => new Promise((r) => setTimeout(r, 25));
  const click = async (el) => { await harness.act(() => el.click()); await harness.act(settle); };
  // Effects run when an act ends, so the first read settles in a second one.
  const show = async () => { await harness.act(() => { activeTab.value = 'fleet-admin'; }); await harness.act(settle); };
  t.after(() => state.dispose());
  return { harness, show, state, activeTab, actions, log, toasts, confirms, timers, mounted, q, check, click };
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
