package agentd_test

import "github.com/tofutools/tclaude/pkg/claude/agentd/dashsnap"

// fleetFederationStubJS fakes the federation status and node-summary reads the
// Fleet node row and map make, so the visual harness can show a linked fleet
// without a hub. Every other request passes through to the real daemon.
const fleetFederationStubJS = `(function(){
  var realFetch = window.fetch.bind(window);
  function json(body, status, headers) {
    return Promise.resolve(new Response(JSON.stringify(body), { status: status || 200, headers: Object.assign({ 'Content-Type': 'application/json' }, headers || {}) }));
  }
  var status = { enabled: true, instance_id: 'inst_q4w7pjf2kx3mz6bty5nd', name: 'desk', fingerprint: 'q4w7-pjf2-kx3m-z6bt-y5nd-8c1e', hub_url: 'wss://hub.example:8470', hub: { state: 'connected' },
    peers: [
      { instance_id: 'inst_hn3cxq7a', label: 'forge', name: 'forge', fingerprint: 'hn3c-xq7a-m2rd-90kp-ce4w-1b7f', trusted: true, online: true, level: 'restricted', trusted_at: '2026-09-30T10:00:00Z' },
      { instance_id: 'inst_2p6ym4ke', label: 'lab', fingerprint: '2p6y-m4ke-tt8v-3jx0-hq5n-a9d2', trusted: true, online: false, level: 'unrestricted', last_seen: '2026-10-09T20:37:00Z' },
      { instance_id: 'inst_w5zea3nq', name: 'carol@buildbox', fingerprint: 'w5ze-a3nq-7m1p-kd42-xr8c-0fv6', trusted: false, online: true }
    ],
    peer_grants: [{ peer: 'inst_hn3cxq7a', slug: 'message.direct' }, { peer: 'inst_hn3cxq7a', slug: 'groups.roster.read', scope: 'ops' }, { peer: 'inst_hn3cxq7a', slug: 'sessions.watch', scope: 'ops' }] };
  // Setup guide stages: a node never set up, and one joined with a trusted
  // peer it has not shared anything with yet.
  if (window.__dashsnapSetupFresh) status = { enabled: false, instance_id: status.instance_id, name: 'desk', fingerprint: status.fingerprint, peers: [] };
  if (window.__dashsnapSetupShare) {
    status.hub.spaces = ['home'];
    status.peers = [status.peers[0], status.peers[2]];
    status.peer_grants = [];
    status.remote = [{ peer: 'inst_hn3cxq7a', label: 'forge', online: true, groups: [{ name: 'builders', caps: ['mail', 'roster', 'presence', 'spawn'] }, { name: 'infra', caps: ['agents.status.read', 'sessions'] }] }];
  }
  if (window.__dashsnapKeyConflict) status.peers[1].identity_transition = { state: 'conflict', old_id: 'inst_2p6ym4ke', new_id: 'inst_8r3kq0vd', new_fingerprint: '8r3k-q0vd-n6wz-2hc4-pm7y-e1xa', received_at: '2026-10-10T08:41:00Z', accept_after: '2026-10-10T08:51:00Z', reason: 'competing signed successors' };
  if (window.__dashsnapTrustAsk) {
    status.peers.push({ instance_id: 'inst_d8vk2mqa', name: 'dave-rig', fingerprint: 'd8vk-2mqa-x7rp-0kc3-fe9w-4ny1', trusted: false, online: true, trust_request: { note: "Dave here, the new build box in the lab. Fingerprint read out on Slack.", trusts_you: true, received_at: new Date(Date.now() - 6 * 60e3).toISOString() } });
    status.peers[2].trust_requested_at = new Date(Date.now() - 3 * 60e3).toISOString();
  }
  if (window.__dashsnapKeyPending) status.peers[0].identity_transition = { state: 'pending', old_id: 'inst_hn3cxq7a', new_id: 'inst_c7m2x9tf', new_fingerprint: 'c7m2-x9tf-4kp1-wq8e-zn3d-6hv0', received_at: '2026-10-10T09:02:00Z', accept_after: '2026-10-10T09:12:00Z' };
  var res = { status: 'current', cpu: { logical_cores: 8, load_average: [2.7, 2, 1] }, ram: { total_bytes: 32e9, available_bytes: 12e9, available_estimated: false }, data_disk: { total_bytes: 500e9, available_bytes: 210e9 } };
  window.fetch = function(input, init) {
    var url = typeof input === 'string' ? input : input.url;
    var path = new URL(url, location.href).pathname;
    // Boards: three boards with config and skill items from a trusted peer,
    // this node and an untrusted publisher, for the content browser.
    var bDesc = { itm_prr: 'Runs the repo cold-review checklist before marking a PR ready.', itm_rel: 'Lead and triage roles for a release train.', itm_go: 'Writes table-driven Go tests with subtests and golden files.', itm_fth: 'Reruns a failing Go test until it flakes, then bisects.', itm_sql: 'Forward-only SQLite migrations with rollback notes.' };
    var bAge = { itm_prr: 2, itm_rel: 30, itm_sbx: 6, itm_go: 70, itm_fth: 1, itm_sql: 200, itm_dml: 12, itm_tpl: 400 };
    var bItem = function(id, name, kind, pub, bytes) { return { description: bDesc[id], updated_at: new Date(Date.UTC(2026, 9, 11, 12) - bAge[id] * 3600e3).toISOString(), id: id, version: '7f3c' + id.slice(4) + 'e91a', latest_version: '7f3c' + id.slice(4) + 'e91a', name: name, kind: kind, publisher: pub, republisher: pub, digest: '3e9b0a12c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f', bytes: bytes, provenance: { pubkey: 'UcKOCmQ1x9Lw4tR2vYp8NzEaHs6gJ3fB0kMdT7uWq3c=' } }; };
    var bItems = {
      brd_core: [bItem('itm_prr', 'pr-review-checklist', 'skill', 'inst_hn3cxq7a', 6350), bItem('itm_rel', 'release roles', 'config', 'inst_2p6ym4ke', 2210), bItem('itm_sbx', 'sandbox defaults', 'config', 'inst_q4w7pjf2kx3mz6bty5nd', 1830), bItem('itm_go', 'go-table-tests', 'skill', 'inst_2p6ym4ke', 4120)],
      brd_oss: [bItem('itm_fth', 'flaky-test-hunter', 'skill', 'inst_w5zea3nq', 11670), bItem('itm_sql', 'sqlite-migrations', 'skill', 'inst_hn3cxq7a', 7400), bItem('itm_dml', 'dash-mock-loop', 'skill', 'inst_mx9n2kq4', 3010)],
      brd_lab: [bItem('itm_tpl', 'bugfix worktree templates', 'config', 'inst_hn3cxq7a', 2950)]
    };
    if (path === '/api/federation/boards') return json({ boards: [
      { id: 'brd_core', name: 'team-core', role: 'publisher', epoch: 3 }, { id: 'brd_oss', name: 'oss-friends', role: 'reader', epoch: 1 }, { id: 'brd_lab', name: 'home-lab', role: 'owner', epoch: 1 }] });
    if (path === '/api/federation/boards/updates') return json({ notify: false, updates: [] });
    var bm = /^\/api\/federation\/boards\/([^/]+)\/items(?:\/([^/]+)\/versions\/([^/]+)\/(fetch|contents|preview))?$/.exec(path);
    if (bm && !bm[4]) return json({ items: bItems[bm[1]] || [] });
    if (bm && bm[4] === 'fetch') return json({ state: 'ready' });
    if (bm && bm[4] === 'contents') return json({ entries: [{ path: 'bundle.json', size: 310, kind: 'json' }, { path: 'sections/roles.json', size: 140, kind: 'json' }, { path: 'roles/release-captain.json', size: 980, kind: 'json' }, { path: 'roles/triage-lead.json', size: 770, kind: 'json' }] });
    if (bm && bm[4] === 'preview') {
      var choice = init && init.body ? JSON.parse(init.body) : {};
      var roots = [{ id: 'claude', label: 'Claude Code', path: '/home/you/.claude/skills', kind: 'harness' }, { id: 'agents', label: 'Codex shared (.agents)', path: '/home/you/.agents/skills', kind: 'harness' }, { id: 'codex', label: 'Codex home', path: '/home/you/.codex/skills', kind: 'harness' },
        { id: 'group:tclaude', label: 'Group tclaude', path: '/home/you/git/tclaude/.claude/skills', kind: 'group' }, { id: 'group:ops', label: 'Group ops', path: '/srv/ops/.claude/skills', kind: 'group' }];
      var skill = (bItems[bm[1]] || []).filter(function(i){ return i.id === bm[2]; })[0];
      var name = skill ? skill.name : 'skill';
      return json({ kind: 'skill', skill: name, skill_roots: roots, preview_token: 'pv_' + Date.now(), warnings: [],
        files: [{ path: 'SKILL.md', bytes: 3010, text: '---\nname: ' + name + '\ndescription: Reruns a failing Go test until it flakes, then bisects.\n---\n\n# ' + name + '\n' }, { path: 'bin/rerun.sh', bytes: 1210, executable: true, security: true, text: '#!/usr/bin/env bash\n# rerun one go test N times, stop at the first failure\nset -euo pipefail\npkg="$1"; name="$2"; n="${3:-50}"\nfor i in $(seq "$n"); do go test "$pkg" -run "^${name}$" -count=1 || exit 1; done\n' }, { path: 'bisect.py', bytes: 4930, security: true, text: 'import subprocess\n' }, { path: 'notes/patterns.md', bytes: 2110, text: '# Patterns\n' }],
        changes: (choice.skill_targets || []).map(function(t, i){ var r = roots.filter(function(x){ return x.id === t; })[0]; return { item: t + '/' + name, action: i === 1 ? 'replace' : 'create', path: (r ? r.path : t) + '/' + name }; }) });
    }
    // The setup guide states start before the guide was ever opened: the
    // daemon is shared by every state, so its "seen" pref is kept out of it.
    if (path === '/api/dashboard/prefs' && (window.__dashsnapSetupFresh || window.__dashsnapSetupShare)) {
      if (init && init.method === 'POST') { if (String(init.body || '').indexOf('tclaude.dash.fleet.setupSeen') >= 0) return json({ ok: true }); return realFetch(input, init); }
      return realFetch(input, init).then(function(r){ return r.clone().json().then(function(p){ delete p['tclaude.dash.fleet.setupSeen']; return json(p); }, function(){ return r; }); });
    }
    if (path === '/api/federation/status') return json(status);
    if (path === '/api/federation/join' && window.__dashsnapSetupFresh) return json({ hub_url: 'wss://hub.home:8470', space: 'home', invite: true, expires_at: '2027-01-11T09:00:00Z', expired: false, ca: [{ subject: 'hub.home', sha256: '3F:9A:C2:71:0B:E4:58:D6:1C:A7:93:2E:F0:6B:44:8D:C5:19:7E:A2:30:BB:E1:5F:92:07:D8:6C:4A:F3:21:E9', not_after: '2027-10-10T00:00:00Z', self_signed: true }], current: { enabled: false, name: 'desk' } });
    if (path === '/api/federation/peer-reach' && window.__dashsnapSetupShare) return json({ rows: [
      { kind: 'group', group: 'ops', slug: 'message.direct', peers: ['inst_hn3cxq7a/builders'] },
      { kind: 'group', group: 'ops', slug: 'groups.members.spawn', peers: ['inst_hn3cxq7a/builders'] }
    ] });
    if (path === '/api/federation/nodes/groups') return json({ groups: [{ id: 'pool_1', name: 'rigs', members: [{ instance_id: 'inst_2p6ym4ke', label: 'lab' }] }] });
    if (path === '/api/federation/grants') return json({ grants: [
      { peer: 'inst_hn3cxq7a', slug: 'message.direct', scope: '' },
      { peer: 'inst_hn3cxq7a', slug: 'groups.roster.read', scope: 'group=ops' },
      { peer: 'inst_hn3cxq7a', slug: 'sessions.watch', scope: 'group=ops' },
      { peer: 'inst_hn3cxq7a', slug: 'groups.members.spawn', scope: 'group=ops', spawn_policy: { max_live: 2 } },
      { peer: 'inst_hn3cxq7a', slug: 'routes.consume', scope: '', pool_id: 'pool_1', pool_name: 'rigs' }
    ] });
    if (path === '/api/federation/profiles') return json({ profiles: [
      { id: 'nprof_7h2k', name: 'test-rig', revision: 3, definition: { trust_level: 'restricted', pools: ['pool_1'], peer_grants: [{ slug: 'message.direct' }, { slug: 'groups.roster.read' }, { slug: 'groups.members.spawn', scope: 'group=frontend-squad', spawn_policy: { max_live: 2, profile: 'opus-fast', requester_pays: 'required' } }], labels: ['gpu', 'ci'], worker_permissions: { 'tasks.read': { allow: true } } } },
      { id: 'nprof_9x1q', name: 'build-farm', revision: 1, definition: { trust_level: 'restricted', pools: [], peer_grants: [{ slug: 'jobs.run' }], labels: ['linux'] } }
    ], default: { id: 'nprof_7h2k', name: 'test-rig' } });
    if (path === '/api/federation/enroll-tokens') return json({ tokens: [
      { id: 'etok_4mz81c', public_token: '', max_uses: 3, used: 1, revoked: false, expires_at: '2099-10-11T09:00:00Z' },
      { id: 'etok_q2v7tn', public_token: '', max_uses: 1, used: 1, revoked: false, expires_at: '2026-10-17T09:00:00Z' },
      { id: 'etok_8kd0rw', public_token: '', max_uses: 1, used: 0, revoked: true, expires_at: '2026-10-12T09:00:00Z' }
    ] });
    if (path === '/api/federation/enrollments') return json({ enrollments: [{ direction: 'issuer', token_id: 'etok_q2v7tn', peer: 'inst_hn3cxq7a', retired: false }] });
    if (path === '/api/federation/enroll/preview') return json({ claims: { master: 'inst_w5zea3nq', profile_name: 'worker', profile_id: 'nprof_c4r0l', profile_revision: 2, trust_level: 'restricted', expires_at: '2099-10-11T09:00:00Z' }, preview_token: 'pv', master_fingerprint: 'w5ze-a3nq-7m1p-kd42-xr8c-0fv6', node_fingerprint: 'q4w7-pjf2-kx3m-z6bt-y5nd-8c1e', consent: 'Running enroll trusts the pinned master at the displayed level. Its profile controls this node\'s authority on the master. No default profile or config offer is applied locally.' });
    if (path === '/api/federation/moves') return json({ moves: [
      { id: 'mv3', direction: 'out', teleport: false, peer: 'inst_hn3cxq7a', source_agent: 'agt_7fk2ab01cd23', group: 'ops', state: 'awaiting_confirmation', expires_at: '2099-10-11T09:00:00Z' },
      { id: 'mv2', direction: 'in', teleport: true, peer: 'inst_hn3cxq7a', source_agent: 'agt_q9m1ee45ff67', target_agent: 'agt_r2d4aa89bb01', group: 'rigs', state: 'complete', expires_at: '2026-10-10T07:00:00Z' },
      { id: 'mv1', direction: 'out', teleport: true, peer: 'inst_2p6ym4ke', source_agent: 'agt_x8p3cc12dd34', group: 'ops', state: 'blocked', last_error: 'peer offline', expires_at: '2099-10-10T20:00:00Z' }
    ] });
    if (path === '/api/federation/teleport') return json({ disabled: false });
    if (path === '/api/federation/audit') return json([
      { id: 'a6', at: '2026-10-10T09:41:12Z', source: 'inbound', direction: 'in', peer: 'inst_hn3cxq7a', kind: 'mail', actor: 'agt_7fk2', target: 'agt_q9m1', group: 'ops', state: 'delivered' },
      { id: 'a5', at: '2026-10-10T09:38:02Z', source: 'outbox', direction: 'out', peer: 'inst_2p6ym4ke', kind: 'mail', actor: 'agt_q9m1', target: 'agt_r2d4', state: 'failed' },
      { id: 'a4', at: '2026-10-10T09:30:45Z', source: 'spawns', direction: 'in', peer: 'inst_hn3cxq7a', kind: 'spawn', actor: 'agt_7fk2', group: 'ops', state: 'refused' },
      { id: 'a3', at: '2026-10-10T09:12:09Z', source: 'model_requests', direction: 'in', peer: 'inst_hn3cxq7a', kind: 'model_request', actor: 'agt_7fk2', status: 403 },
      { id: 'a2', at: '2026-10-10T08:55:31Z', source: 'audit', direction: 'event', kind: 'federation.enroll.create', actor: 'operator', target: 'etok_4mz81c', status: 200 },
      { id: 'a1', at: '2026-10-10T08:20:00Z', source: 'audit', direction: 'event', peer: 'inst_hn3cxq7a', kind: 'federation.grant', actor: 'operator', target: 'inst_hn3cxq7a groups.roster.read group=ops', status: 200 }
    ]);
    if (/^\/api\/federation\/profiles\/[^/]+$/.test(path) && !(init && init.method === 'PUT')) return json({ profile: {}, applied_peers: ['inst_hn3cxq7a'] });
    if (path === '/api/federation/away' && !(init && init.method === 'POST')) return json({ away: window.__dashsnapAway ? { cover: 'inst_hn3cxq7a', since: '2026-10-10T08:00:00Z', until: '2026-10-10T18:00:00Z' } : null });
    if (path === '/api/federation/node-labels' && !(init && init.method === 'POST')) return json({ labels: ['gpu', 'ci', 'linux'] });
    if (path === '/api/federation/sessions') return json([
      { instance: 'inst_hn3cxq7a', peer: 'forge', agent: 'agt_ada7k2m9', name: 'ada', groups: ['ops'], watch: true, attach: true, stale: false },
      { instance: 'inst_hn3cxq7a', peer: 'forge', agent: 'agt_bob4n8q1', name: 'bench-runner', groups: ['ops'], watch: true, attach: false, stale: false },
      { instance: 'inst_hn3cxq7a', peer: 'forge', agent: 'agt_cyd2p5w3', name: 'infra-dev', groups: ['infra'], watch: false, attach: false, stale: true }
    ]);
    if (path === '/api/federation/identity/rotations') return json({ local: window.__dashsnapRotationPending
      ? { pending: true, chain: [{ new_id: 'inst_q4w7pjf2kx3mz6bty5nd' }, { new_id: 'inst_r8vn2c5xq7hd3m9kz1wa', new_fingerprint: 'r8vn 2c5x q7hd 3m9k z1wa 6tpe', activate_at: new Date(Date.now() + 38 * 3600e3).toISOString() }] }
      : { pending: false, chain: [{ new_id: 'inst_q4w7pjf2kx3mz6bty5nd' }] } });
    if (path === '/api/federation/identity/rotate') return json({ instance_id: 'inst_q4w7pjf2kx3mz6bty5nd', fingerprint: 'q4w7 pjf2 kx3m z6bt y5nd 8c1e', window_seconds: 172800, hop_count: window.__dashsnapRotationPending ? 2 : 1, hop_limit: 4, pending: !!window.__dashsnapRotationPending,
      effects: { successor_linked: true, streams_reconnect: true, pending_sealed_mail_requires_resend: true, issued_model_credentials_revoked: true, requester_paid_leases_revoked: true } });
    if (path === '/api/federation/nodes/health' && !(init && init.method === 'POST')) return json({ presence: true, resources: true, failures: false, debounce_seconds: 15, disk_free_percent: 10, ram_free_percent: 10, memory_seconds: 120, failure_count: 3, failure_window_seconds: 600, cooldown_seconds: 600 });
    // The viewer rows name real fixture agents, so the per-agent badges land on
    // their rows; the agent IDs are minted at fixture time.
    function __dashsnapAgentID(title, fallback) { var el = document.querySelector('.member-row[data-label="' + title + '"][data-agent], [data-label="' + title + '"][data-agent]'); return (el && el.dataset.agent) || fallback; }
    if (path === '/api/federation/viewers') return json(window.__dashsnapViewers ? [
      { id: 'tv_8k2q', peer: 'inst_hn3cxq7a', agent: __dashsnapAgentID('fe-dev-forms', 'agt_r8k2m4c1x9'), session: 'fe-dev-forms', group: 'frontend-squad', read_only: false, started: new Date(Date.now() - 720000).toISOString(), incoming: true },
      { id: 'tv_3m1x', peer: 'inst_2p6ym4ke', agent: __dashsnapAgentID('infra-lead', 'agt_p2w7d0j5n3'), session: 'infra-lead', group: 'infra-crew', read_only: true, started: new Date(Date.now() - 95000).toISOString(), incoming: true }
    ] : []);
    if (path === '/api/federation/bundle-offers') return json(url.indexOf('direction=out') >= 0 ? [
      { offer: { id: 'off_7q2m', type: 'agent', summary: 'Agent reviewer (config only)', expires_at: '2099-10-17T09:00:00Z' }, peer: 'inst_2p6ym4ke', direction: 'out', state: 'pending' },
      { offer: { id: 'off_3k1x', type: 'config', summary: 'Config bundle: 4 items', expires_at: '2026-10-16T09:00:00Z' }, peer: 'inst_hn3cxq7a', direction: 'out', state: 'applied' }
    ] : [
      { offer: { id: 'off_9c4r', type: 'config', bytes: 6144, sha256: '3f9a0c27d1e84b56a7c2e9f01d3b8a64c5e7f2190ab3d4e6f8a1c2b3d4e5f607', expires_at: '2099-10-17T09:00:00Z', summary: 'Config bundle: 3 items' }, peer: 'inst_hn3cxq7a', direction: 'in', state: 'pending', sender_agent: 'agt_7fk2' },
      { offer: { id: 'off_5m8t', type: 'agent', bytes: 482000, sha256: '9b1d', expires_at: '2099-10-17T09:00:00Z', summary: 'Agent ada with history', group: 'ops', move: { source_agent: 'agt_ada0' } }, peer: 'inst_hn3cxq7a', direction: 'in', state: 'ready' },
      { offer: { id: 'off_2w6p', type: 'config', bytes: 900, expires_at: '2026-10-12T09:00:00Z', summary: 'Config bundle: 1 items' }, peer: 'inst_2p6ym4ke', direction: 'in', state: 'pending', last_error: 'sender offline; fetch again later' }
    ]);
    if (path.indexOf('/api/federation/bundle-offers/') === 0 && path.slice(-6) === '/fetch') return json({ state: 'ready' });
    if (path.indexOf('/api/federation/bundle-offers/') === 0 && path.slice(-9) === '/contents') {
      var entry = new URL(url, location.origin).searchParams.get('path');
      if (!entry) return json({ type: 'config', entries: [
        { path: 'bundle.json', size: 6144, kind: 'json' }, { path: 'sections/roles.json', size: 1830, kind: 'json' },
        { path: 'sections/templates.json', size: 2410, kind: 'json' }, { path: 'sections/config.json', size: 512, kind: 'json' }
      ] });
      return json({ path: entry, kind: 'json', size: 1830, truncated: false, offset: 0, text: JSON.stringify([
        { name: 'reviewer', description: 'Reviews diffs cold; never edits', permissions: { 'tasks.read': { allow: true }, 'repo.write': { allow: false } }, prompt: 'You are a careful reviewer. Report only significant defects.' },
        { name: 'release-captain', description: 'Cuts releases and watches CI', permissions: { 'jobs.run': { allow: true } }, prompt: 'Coordinate the release train; never force-push main.' }
      ]) });
    }
    if (path === '/api/federation/bundle-offers/off_5m8t/import') {
      var ib = JSON.parse((init && init.body) || '{}');
      var cands = [
        { id: 'repo:repo_7k2', cwd: '/srv/tclaude/checkouts/ada-7k2q', reason: 'repo_match', exists: false, checkout_required: true, repo: { id: 'repo_7k2', name: 'tclaude', ref: 'main' } },
        { id: 'same_path', cwd: '/home/johan/git/tclaude', reason: 'same_path', exists: true },
        { id: 'group_default', cwd: '/srv/ops', reason: 'group_default', exists: true }
      ];
      var pick = ib.cwd ? { cwd: ib.cwd, reason: 'explicit', exists: false } : cands.filter(function(c){ return c.id === (ib.landing || 'repo:repo_7k2'); })[0];
      return json({ agent: { name: 'ada', harness: 'claude', role: 'dev' }, group: 'ops', history: true, unresolved: [], warnings: [], findings: [],
        security: 'Permissions and ownership from the sender are not copied.',
        landing: Object.assign({}, pick, { candidates: cands, source_cwd: '/home/johan/git/tclaude', source_repo: 'git@github.com:tofutools/tclaude.git' }) });
    }
    if (path.indexOf('/api/federation/bundle-offers/') === 0 && path.slice(-7) === '/import') return json({ changes: [
      { item: 'roles/reviewer', action: 'create', security: true },
      { item: 'templates/pr-review', action: 'replace', security: true },
      { item: 'config/theme', action: 'unchanged' }
    ], unresolved: [{ name: 'REPO_ROOT', item: 'templates/pr-review', field: 'cwd', original: '/home/ana/src' }], warnings: [], applied: [], security_changes: 2 });
    if (path === '/api/federation/jobs') return json({ jobs: [
      { id: 'job_8f2kq1', direction: 'in', peer: 'inst_hn3cxq7a', state: 'pending', request: { repo: 'tclaude', ref: 'main', group: 'ops', command: 'go test ./pkg/claude/...', timeout_seconds: 1800 }, created_at: '2026-10-10T09:40:00Z', caller_agent: 'agt_7fk2' },
      { id: 'job_3m9tx4', direction: 'in', peer: 'inst_hn3cxq7a', state: 'running', request: { repo: 'tclaude', ref: 'feature/viewers', group: 'ops', harness: 'codex', command: 'Fix the flaky federation test and push a branch', timeout_seconds: 3600 }, created_at: '2026-10-10T09:20:00Z' },
      { id: 'job_q7w2n8', direction: 'out', peer: 'inst_2p6ym4ke', state: 'completed', request: { repo: 'site', ref: 'v2', group: 'web', command: 'npm run build', timeout_seconds: 900 }, result: { state: 'completed', exit_code: 0, commit: '4e1c9a7b2d3f' }, created_at: '2026-10-10T08:50:00Z' },
      { id: 'job_z1v5c6', direction: 'out', peer: 'inst_2p6ym4ke', state: 'timeout', request: { repo: 'site', ref: 'v2', group: 'web', command: 'npm test', timeout_seconds: 600 }, result: { state: 'timeout', exit_code: 124 }, created_at: '2026-10-10T08:10:00Z' }
    ].concat(window.__dashsnapLiveJob ? [
      { id: 'job_live7k', direction: 'out', peer: 'inst_hn3cxq7a', state: 'running', request: { repo: 'tclaude', ref: 'main', group: 'ops', command: 'go test ./pkg/federation/...', timeout_seconds: 1800 }, created_at: '2026-10-10T09:44:00Z' }
    ] : []) });
    if (path === '/api/federation/jobs/job_live7k/output') return json({ chunks: [
      { stream: 'stdout', data: btoa('ok  \tgithub.com/tofutools/tclaude/pkg/federation\t3.112s\nok  \tgithub.com/tofutools/tclaude/pkg/federation/proto\t0.408s\n=== RUN   TestHubRelayBackpressure\n'), encoding: 'base64' },
      { stream: 'stderr', data: btoa('warning: GOFLAGS=-mod=mod ignored for test binaries\n'), encoding: 'base64' }
    ], cursor: 'c1', done: false, state: 'running' });
    if (path === '/api/federation/repos') return json({ repos: [
      { id: 'repo_1', name: 'tclaude', revision: 3, enabled: true, group_names: ['ops'], definition: { url: 'git@github.com:tofutools/tclaude.git', clone: '/home/ana/git/tclaude', groups: [4] } },
      { id: 'repo_2', name: 'infra', revision: 1, enabled: false, group_names: ['ops', 'build'], definition: { url: 'git@github.com:tofutools/infra.git', clone: '/home/ana/git/infra', groups: [4, 6] } }
    ] });
    if (path === '/api/federation/spawn-requests' && !(init && init.method === 'POST')) return json([
      { id: 12, from: 'ada@forge', instance: 'inst_hn3cxq7a', group: 'frontend-squad', name: 'flake-hunter', role: 'dev', profile: 'opus-fast', brief: 'The deploy smoke test fails about one run in five on CI. Find the race and fix it; keep the change small.', status: 'pending', credentials: 'proxy:claude@inst_hn3cxq7a', model_lease: 'mlease_7q2kx9d4hpa1', created_at: '2026-10-10T09:12:00Z', expires_at: '2026-10-13T09:12:00Z' },
      { id: 11, from: 'ada@forge', instance: 'inst_hn3cxq7a', group: 'infra-crew', brief: 'Benchmark the new cache layer against main.', status: 'launching', result_agent: 'agt_k3v9q2m7x1', created_at: '2026-10-10T08:40:00Z', expires_at: '2026-10-13T08:40:00Z' },
      { id: 9, from: 'lab', instance: 'inst_2p6ym4ke', group: 'infra-crew', brief: 'Rebuild the docs index.', status: 'approved', result_agent: 'agt_p8d2w4c6z0', created_at: '2026-10-09T15:00:00Z', expires_at: '2026-10-12T15:00:00Z' }
    ]);
    if (path === '/api/federation/outbox') return json([
      { envelope_id: 'env_7k2q', to: 'reviewers@lab', from: 'human operator', subject: 'spawn request', preview: 'Second pair of eyes on PR 2776', state: 'pending', attempts: 4, last_error: 'peer offline', created_at: '2026-10-10T09:20:00Z', updated_at: '2026-10-10T09:31:00Z' },
      { envelope_id: 'env_5m1x', to: 'ops@forge', from: 'human operator', subject: 'Release window', preview: 'Freeze starts at 18:00', state: 'acked', attempts: 1, created_at: '2026-10-10T08:02:00Z', updated_at: '2026-10-10T08:02:01Z' },
      { envelope_id: 'env_9p4d', to: 'operator@lab', from: 'human operator', subject: 'Release window', preview: 'Freeze starts at 18:00', state: 'pending', attempts: 4, last_error: 'peer offline', created_at: '2026-10-10T09:20:00Z', updated_at: '2026-10-10T09:31:00Z' },
      { envelope_id: 'env_2h6w', to: 'operator@forge', from: 'human operator', subject: 'Re: deploy freeze', preview: 'Agreed', state: 'acked', attempts: 1, created_at: '2026-10-10T08:02:00Z', updated_at: '2026-10-10T08:02:01Z' }
    ]);
    if (path === '/api/federation/models/control' && !(init && init.method === 'POST')) return json({ disabled: false, gateways: {
      claude: { enabled: true, dialect: 'anthropic', models: ['claude-sonnet-5-5', 'claude-haiku-5-5'], daily_requests: 2000, daily_tokens: 20000000, peer_daily_requests: 500, peer_daily_tokens: 5000000, session_daily_requests: 200, session_daily_tokens: 1000000, max_input_tokens: 200000, max_output_tokens: 32000, max_concurrent: 4, requests_per_minute: 60, lease_idle_hours: 8, blocked_peers: ['inst_2p6ym4ke'] },
      openai: { enabled: false, dialect: 'openai', models: ['gpt-5.6'], daily_requests: 500, daily_tokens: 0 }
    } });
    if (path === '/api/federation/models/leases' && !(init && init.method === 'POST')) return json([
      { id: 'mlease_7q2kx9d4hpa1', peer: 'inst_hn3cxq7a', proxy: 'claude', kind: 'requester_paid', worker: 'agt_r8k2m4c1x9', revoked: false, idle_seconds: 28800, touched_at: '2026-10-10T09:31:00Z' },
      { id: 'mlease_3mz81cqv0e8c', peer: 'inst_hn3cxq7a', proxy: 'claude', kind: 'requester_paid', worker: 'agt_p2w7d0j5n3', revoked: true, idle_seconds: 28800, touched_at: '2026-10-09T17:02:00Z' }
    ]);
    if (path === '/api/federation/models/usage') return json([
      { proxy: 'claude', peer: 'inst_hn3cxq7a', model: 'claude-sonnet-5-5', charged_tokens: 812000, input_tokens: 690000, output_tokens: 122000, status: 200, complete: true },
      { proxy: 'claude', peer: 'inst_hn3cxq7a', model: 'claude-sonnet-5-5', charged_tokens: 410000, input_tokens: 380000, output_tokens: 30000, status: 200, complete: true },
      { proxy: 'claude', peer: 'inst_hn3cxq7a', model: 'claude-haiku-5-5', charged_tokens: 52000, input_tokens: 50000, output_tokens: 2000, status: 429, complete: false },
      { proxy: 'claude', peer: 'inst_hn3cxq7a', model: 'claude-sonnet-5-5', charged_tokens: 232000, input_tokens: 0, output_tokens: 0, status: 0, complete: false }
    ]);
    if (path === '/api/federation/hub/status') return json({ hub_id: 'hub_7kq2m9', hub_url: 'wss://hub.lab.example', hub_version: 'v0.43.0', connected: true, admin: true, admin_count: 2, my_capabilities: ['hub.admins.manage', 'hub.admissions.manage', 'hub.invites.manage', 'hub.spaces.manage', 'hub.settings.manage', 'hub.identity.manage', 'hub.health.read', 'hub.logs.read'], bootstrap_claimable: false });
    if (path === '/api/federation/hub/health') return json({ connected_instances: 3, streams: 7, load: { goroutines: 142, heap_bytes: 52428800 }, uptime_seconds: 302400, recent_errors: [{ at: '2026-10-10T09:12:00Z', code: 'stream_reset', message: 'inst_2p6ym4ke reset stream 4' }] });
    if (path === '/api/federation/hub/admissions' && !(init && init.method === 'POST')) return json({ admissions: [
      { instance: 'inst_q4w7pjf2kx3mz6bty5nd', name: 'desk', fingerprint: 'q4w7-pjf2-kx3m-z6bt-y5nd-8c1e', spaces: ['ops'], connected: true },
      { instance: 'inst_hn3cxq7a', name: 'forge', fingerprint: 'hn3c-xq7a-2m8d-p0kf-w4tz-91rb', spaces: ['ops', 'ci'], connected: true },
      { instance: 'inst_2p6ym4ke', name: 'lab', fingerprint: '2p6y-m4ke-v7cq-h1ns-d3xw-5jt0', spaces: ['ci'], last_seen: '2026-10-10T08:40:00Z' },
      { instance: 'inst_m3rk7d2v', name: 'old-laptop', fingerprint: 'm3rk-7d2v-bq5x-ra6h-ue4n-2wpc', spaces: ['ops'], last_seen: '2026-09-28T17:00:00Z', revoked: true }] });
    if (path === '/api/federation/hub/invites' && !(init && init.method === 'POST')) return json({ invites: [{ token_hash: '9f2c4e1ab07d3355', space: 'ci', created_at: '2026-10-10T08:00:00Z', expires_at: '2026-10-11T08:00:00Z', used: false }] });
    if (path === '/api/federation/hub/admins' && !(init && init.method === 'POST')) return json({ admins: [
      { instance: 'inst_q4w7pjf2kx3mz6bty5nd', name: 'desk', fingerprint: 'q4w7-pjf2-kx3m-z6bt-y5nd-8c1e', capabilities: ['hub.admins.manage', 'hub.admissions.manage', 'hub.settings.manage', 'hub.health.read', 'hub.logs.read'], added_at: '2026-10-01T00:00:00Z' },
      { instance: 'inst_hn3cxq7a', name: 'forge', fingerprint: 'hn3c-xq7a-2m8d-p0kf-w4tz-91rb', capabilities: ['hub.admissions.manage', 'hub.health.read'], added_at: '2026-10-03T00:00:00Z' }] });
    if (path === '/api/federation/hub/settings' && !(init && init.method === 'PATCH')) return json({ settings: {
      rotation_window: { type: 'duration', unit: 's', min: 60, max: 604800, effective: 600, source: 'flag', boot: 600, restart_required: false, flag_overridden: false },
      max_streams_per_instance: { type: 'int', min: 1, max: 64, effective: 16, source: 'remote', boot: 8, restart_required: false, flag_overridden: true },
      invite_ttl_max: { type: 'duration', unit: 's', min: 300, max: 2592000, effective: 604800, source: 'default', boot: 604800, restart_required: false, flag_overridden: false },
      listen_backlog: { type: 'int', min: 16, max: 4096, effective: 256, source: 'flag', boot: 256, restart_required: true, flag_overridden: false } } });
    if (path === '/api/federation/hub/run' && !(init && init.method === 'POST')) return json({ accept_remote_scripts: !!window.__dashsnapHubRun, switch_source: 'flag', can_exec: true, service_user: 'tclaude-hub', limits: { max_script_bytes: 16384, default_timeout_seconds: 300, max_timeout_seconds: 3600, max_output_bytes: 4194304 } });
    if (path === '/api/federation/hub/run') return json({ id: 'rhub', state: 'running', exit_code: -1, timeout_seconds: 3600 }, 202);
    if (path === '/api/federation/hub/run/jobs/rhub') return json({ id: 'rhub', state: 'completed', exit_code: 0, duration_ms: 214, stdout_tail: '● tclaude-hub.service - tclaude federation hub\n     Active: active (running) since Fri 2026-10-10 06:12:44 UTC; 6h ago\n   Main PID: 812 (tclaude-hub)\n      Tasks: 14\nESTAB 0 0 10.0.0.4:8470 10.0.0.17:51522\nESTAB 0 0 10.0.0.4:8470 10.0.0.23:40110\n', stderr_tail: '' });
    if (path === '/api/federation/hub/audit') return json({ entries: [
      { id: 'au3', at: '2026-10-10T09:31:00Z', actor: 'inst_q4w7pjf2kx3mz6bty5nd', kind: 'exec', outcome: 'completed', detail: { exit_code: 0, script: 'systemctl status tclaude-hub --no-pager\nss -tn state established', script_sha256: '5be1c0a3d9f2e47b8a61f0c2d3e4b5a69788c1d2e3f4a5b6c7d8e9f0a1b2c3d4' } },
      { id: 'au2', at: '2026-10-10T09:05:41Z', actor: 'inst_q4w7pjf2kx3mz6bty5nd', kind: 'settings', outcome: 'applied', detail: {} },
      { id: 'au1', at: '2026-10-09T17:20:00Z', actor: 'inst_q4w7pjf2kx3mz6bty5nd', kind: 'update', outcome: 'rolled_back', detail: { from_version: 'v0.42.1', to_version: 'v0.43.0' } }], next_cursor: 'au1' });
    if (path === '/api/federation/hub/update') return json(window.__dashsnapHubUpdate === 'unsupervised'
      ? { current_version: 'v0.43.0', latest_version: 'v0.44.0', update_available: true, checked_at: '2026-10-10T08:00:00Z', supervisor: null, blocked: { code: 'not_supervised', message: 'host must run serve --supervised under a verified systemd/launchd restart policy' } }
      : { current_version: 'v0.43.0', latest_version: 'v0.44.0', update_available: true, checked_at: '2026-10-10T08:00:00Z', supervisor: 'systemd', rollback_available: false,
          job: window.__dashsnapHubUpdate === 'rolled_back' ? { id: 'hu1', action: 'apply', from_version: 'v0.43.0', version: 'v0.44.0', state: 'rolled_back', phase: 'rolled_back', rolled_back: true, error: 'health check failed: candidate did not accept connections within 60 s; restored v0.43.0', finished_at: '2026-10-10T09:48:00Z' }
            : { id: 'hu1', action: 'apply', from_version: 'v0.43.0', version: 'v0.44.0', state: 'restarting', phase: 'health_check', deadline: '2026-10-10T09:49:00Z' } });
    if (path === '/api/federation/hub/logs') return json({ entries: [
      { at: '2026-10-10T09:14:02Z', level: 'info', message: 'admitted inst_2p6ym4ke to space ci' },
      { at: '2026-10-10T09:12:00Z', level: 'warn', message: 'stream reset by inst_2p6ym4ke (stream 4)' },
      { at: '2026-10-10T09:05:41Z', level: 'info', message: 'setting max_streams_per_instance = 16 (remote, overrides flag)' }], next_cursor: 'c_1' });
    if (path === '/api/federation/peers/trust') return json({ instance_id: 'inst_w5zea3nq', fingerprint: 'w5ze-a3nq-7m1p-kd42-xr8c-0fv6', level: 'restricted', profile: null, plan: null, applied: false });
    var hav = function(extra){ return { schema: 1, observed_at: '2026-10-10T09:40:00Z', harnesses: [
      { name: 'claude', display_name: 'Claude Code', installed: true, version: '2.1.4', latest_version: extra ? '2.1.4' : '2.2.0', update_available: !extra, version_status: 'known', credential_present: true, usable: true },
      { name: 'codex', display_name: 'Codex', installed: true, version: '0.9.2', latest_version: '0.9.2', update_available: false, version_status: 'known', credential_present: true, usable: true },
      { name: 'opencode', display_name: 'OpenCode', installed: !extra, version: extra ? undefined : '1.0.3', update_available: false, version_status: extra ? 'not_installed' : 'known', credential_present: null, usable: null },
      { name: 'copilot', display_name: 'Copilot', installed: false, version_status: 'not_installed' },
      { name: 'gemini', display_name: 'Gemini', installed: true, version: '0.4.0', version_status: 'unknown', update_available: null, credential_present: null, usable: false }
    ] }; };
    if (path === '/api/harnesses/availability') return json(hav(false));
    if (path === '/api/peer/inst_hn3cxq7a/harnesses/availability') return json(hav(true));
    var post = init && init.method === 'POST';
    if (path === '/api/node/run/settings' || (path === '/api/node/run' && !post)) return json({ accept_remote_scripts: false, resource_limits: { memory: '1GiB', memory_bytes: 1073741824, pids: 256 }, warning: 'Full remote code execution as the agentd user' });
    if (path === '/api/peer/inst_hn3cxq7a/node/run' && !post) return json({ accept_remote_scripts: true, resource_limits: { memory: '2GiB', pids: 512 } });
    if (/\/node\/run$/.test(path) && post) return json({ id: path.indexOf('/peer/') >= 0 ? 'rforge' : 'rdesk', state: 'running', exit_code: -1, timeout_seconds: 3600 }, 202);
    if (path === '/api/node/run/jobs/rdesk') return json({ id: 'rdesk', state: 'completed', exit_code: 0, duration_ms: 842, stdout_tail: 'Filesystem      Size  Used Avail Use% Mounted on\n/dev/nvme0n1p2  468G  271G  174G  61% /\n', stderr_tail: '' });
    if (path === '/api/peer/inst_hn3cxq7a/node/run/jobs/rforge') return json({ id: 'rforge', state: 'failed', exit_code: 127, duration_ms: 35, stdout_tail: '', stderr_tail: 'sh: 3: nvidia-smi: not found\n' });
    if (path === '/api/peer/inst_2p6ym4ke/harnesses/availability') return json({ error: 'peer offline', code: 'peer_unreachable' }, 502);
    if (/\/harnesses\/operations$/.test(path)) return json({ recipes: [{ harness: 'opencode', install_command: 'npm install -g opencode-ai@latest', update_command: 'npm install -g opencode-ai@latest' }, { harness: 'claude', install_command: 'npm install -g @anthropic-ai/claude-code', update_command: 'claude update' }], modes: ['now', 'when_idle'] });
    if (/\/harnesses\/credentials\/backups$/.test(path)) return json({ backups: [{ id: 'c'.repeat(32), harness: 'claude', created_at: '2026-10-09T18:02:00Z', location: '~/.tclaude/data/credential-backups' }] });
    if (path === '/api/node/update') return json({ current_version: 'v0.42.1', install_method: 'release', protocol_version: 1, binaries: [{ name: 'tclaude', path: '/home/op/.local/bin/tclaude', version: 'v0.42.1', install_method: 'release' }, { name: 'tclaude-agentd', path: '/home/op/.local/bin/tclaude-agentd', version: 'v0.42.1', install_method: 'release' }], latest_version: 'v0.43.0', checked_at: '2026-10-10T08:00:00Z', update_available: true, rollback_available: true, warnings: ['Version skew: inst_2p6ym4ke runs v0.41.0'] });
    if (path === '/api/peer/inst_hn3cxq7a/node/update') return json({ error: 'node.update is not shared', code: 'permission_denied' }, 403);
    if (path === '/api/node-summary') return json({ version: 'v0.42.1', latest_version: 'v0.43.0', update_available: true, update_checked_at: '2026-10-10T08:00:00Z', presence: 'online', shared_groups: 2, shared_agents: 10, online_agents: 8, waiting_for_input: 1, resources: res, health: 'current' }, 200, { ETag: '"local"' });
    if (path === '/api/peer/inst_hn3cxq7a/node-summary') return json({ version: 'v0.43.0', update_available: false, presence: 'online', shared_groups: 2, shared_agents: 9, online_agents: 7, waiting_for_input: 1, peer_view: { peer: 'desk', level: 'restricted', included: [], omitted: [{ feature: 'costs', requires: 'costs.read' }, { feature: 'terminals', requires: 'sessions.watch' }] } }, 200, { ETag: '"forge"' });
    if (path === '/api/peer/inst_2p6ym4ke/node-summary') return json({ error: 'peer offline', code: 'peer_unreachable', reason: 'peer_offline', last_seen: '2026-10-09T20:37:00Z' }, 502);
    // A peer view of forge: serve this daemon's own per-node data as if forge
    // answered through the proxy, with forge's peer_view metadata on the snapshot.
    if (path === '/api/peer/inst_hn3cxq7a/peer-access-requests' && post) return json({ id: 'par_8kq2', origin_peer: 'inst_q4w7pjf2kx3mz6bty5nd', perm: 'costs.read', group_id: 0, grant_ttl_seconds: 28800, status: 'pending' }, 202);
    var forgePrefix = '/api/peer/inst_hn3cxq7a/';
    if (path.indexOf(forgePrefix) === 0) {
      var u = new URL(url, location.href);
      var local = '/api/' + path.slice(forgePrefix.length) + u.search;
      if (path === forgePrefix + 'snapshot') return realFetch(local, init).then(function(r){
        return r.json().then(function(snap){
          snap.peer_view = window.__dashsnapPeerMail
            ? { peer: 'desk', included: ['agents.status', 'groups', 'messaging', 'node.messages', 'human.inbox.answer'], omitted: [{ feature: 'costs', requires: 'costs.read' }, { feature: 'terminals', requires: 'sessions.attach' }] }
            : window.__dashsnapPeerSettings
            ? { peer: 'desk', level: 'unrestricted', included: ['agents.status', 'groups', 'groups.settings', 'messaging'], omitted: [] }
            : window.__dashsnapPeerActions
            ? { peer: 'desk', included: ['agents.status', 'groups', 'messaging', 'spawn', 'lifecycle.stop', 'lifecycle.retire', 'lifecycle.clone', 'lifecycle.move', 'lifecycle.teleport'], omitted: [{ feature: 'costs', requires: 'costs.read' }, { feature: 'terminals', requires: 'sessions.attach' }] }
            : { peer: 'desk', included: ['agents.status', 'groups', 'messaging'], omitted: [{ feature: 'costs', requires: 'costs.read' }, { feature: 'spawn', requires: 'groups.members.spawn' }, { feature: 'terminals', requires: 'sessions.attach' }] };
          delete snap.assets_version;
          snap.usage = { available: false }; // what filterPeerFields leaves behind
          return new Response(JSON.stringify(snap), { status: 200, headers: { 'Content-Type': 'application/json' } });
        });
      });
      return realFetch(local, init);
    }
    if (path === '/api/federation/links' && window.__fleetGroupLinks) return json({ groups: [{ group_id: 1, name: 'ops', federation_links: window.__fleetGroupLinks }] });
    if (path === '/api/snapshot' && (window.__fleetGroupLinks || window.__peerAccessRow)) return realFetch(input, init).then(function(r){
      return r.clone().json().then(function(snap){
        var g = (snap.groups || [])[0];
        if (g && window.__fleetGroupLinks) g.federation_links = window.__fleetGroupLinks;
        if (window.__peerAccessRow) { snap.access_requests = [window.__peerAccessRow]; snap.access_requests_pending = 1; }
        return new Response(JSON.stringify(snap), { status: r.status, headers: r.headers });
      }, function(){ return r; });
    });
    return realFetch(input, init);
  };
})();`

// fleetRemoteTerminalJS stands in for the federation terminal socket: the
// hello names the mode (from the URL) and the target's pinned size, then a
// screen of agent output arrives as binary frames, as the bridge sends them.
const fleetRemoteTerminalJS = `(function(){
  var Real = window.WebSocket;
  function Fake(url) {
    if (String(url).indexOf('/api/federation/terminal?') < 0) return new Real(url);
    var self = this; this.readyState = 0; this.binaryType = 'blob';
    var mode = /mode=interactive/.test(url) ? 'interactive' : 'watch';
    var enc = new TextEncoder();
    setTimeout(function(){
      self.readyState = 1; self.onopen && self.onopen();
      self.onmessage && self.onmessage({ data: JSON.stringify({ type: 'hello', mode: mode, cols: 96, rows: 20 }) });
      var lines = ['\x1b[1;36m● ada\x1b[0m  (forge · ops)', '', '> Run the federation tests and fix the relay backpressure failure', '',
        '\x1b[2m  Bash(go test ./pkg/federation/...)\x1b[0m', '  ok   pkg/federation/relay   3.112s', '  \x1b[31mFAIL\x1b[0m pkg/federation/hub  TestHubRelayBackpressure', '',
        '  The credit window is not refilled after a slow reader drains; patching hub/stream.go …', '', '\x1b[7m REMOTE ' + (mode === 'interactive' ? 'INPUT' : 'WATCH') + ' · desk \x1b[0m'];
      self.onmessage && self.onmessage({ data: enc.encode(lines.join('\r\n')).buffer });
    }, 50);
  }
  Fake.OPEN = 1; Fake.CLOSED = 3; Fake.CONNECTING = 0; Fake.CLOSING = 2;
  Fake.prototype.send = function(){}; Fake.prototype.close = function(){ this.readyState = 3; };
  window.WebSocket = Fake;
})();`

// fleetGroupLinksJS decorates the first group's snapshot with federation
// links, as dashboard_group_federation_links.go reports them.
const fleetGroupLinksJS = `window.__fleetGroupLinks = [
  { peer: 'inst_hn3cxq7a', label: 'forge', level: 'restricted', kind: 'grant', direction: 'in', slugs: ['groups.roster.read', 'message.direct'], online: true },
  { peer: 'inst_2p6ym4ke', label: 'lab', level: 'unrestricted', kind: 'grant', direction: 'in', pool: 'rigs', slugs: ['routes.consume'], online: false, last_seen: '2026-10-09T20:37:00Z' },
  { peer: 'inst_2p6ym4ke', label: 'lab', level: 'unrestricted', kind: 'route', direction: 'out', remote: 'reviewers', online: false, last_seen: '2026-10-09T20:37:00Z' }
];`

// fleetRemoteViewJS opens the page as the peer view of forge (?node=) before
// remote-node.js reads the URL.
const fleetRemoteViewJS = `history.replaceState(null, '', location.pathname + '?node=inst_hn3cxq7a' + (location.search.indexOf('wizard=1') >= 0 ? '&wizard=1' : ''));`

// fleetZeroHeightJS proves the node row costs no vertical space: the tab bar
// and the first group sit at the same offsets with the row shown and removed.
const fleetZeroHeightJS = `return (async function(){
  var host = document.querySelector('#node-chips-root');
  for (var i = 0; i < 50 && !host.querySelector('.has-fleet .node-chip'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!host.querySelector('.has-fleet .node-chip')) throw new Error('fleet: node chips did not render');
  function geom() {
    var nav = document.querySelector('nav').getBoundingClientRect();
    var main = document.querySelector('main').getBoundingClientRect();
    return [Math.round(nav.top), Math.round(nav.height), Math.round(main.top)].join(',');
  }
  var withRow = geom();
  var map = document.querySelector('nav [data-tab="map"]');
  host.style.display = 'none'; map.style.display = 'none';
  var without = geom();
  host.style.display = ''; map.style.display = '';
  if (withRow !== without) {
    var tall = Array.from(document.querySelectorAll('nav .nav-inner > *')).map(function(el){ var r = el.getBoundingClientRect(); return (el.dataset.tab || el.id || el.className) + ':' + Math.round(r.width) + 'x' + Math.round(r.height); }).join(' ');
    throw new Error('fleet: node row changed the layout: ' + withRow + ' vs ' + without + ' — ' + tall);
  }
})();`

func fleetStates() []dashsnap.State {
	const showGroups = `document.querySelector('nav [data-tab="groups"]').click();`
	return []dashsnap.State{
		{
			Key:     "fleet-node-row",
			Title:   "Fleet node row (1600)",
			Caption: "This node and its trusted peers as chips at the right of the tab bar, with the map entry. The harness asserts the tab bar and main area keep today's offsets.",
			InitJS:  fleetFederationStubJS,
			JS:      showGroups + fleetZeroHeightJS,
		},
		{
			Key:     "fleet-node-row-1280",
			Title:   "Fleet node row (1280)",
			Caption: "The same row at 1280 px wide: still no added height.",
			Width:   1280,
			InitJS:  fleetFederationStubJS,
			JS:      showGroups + fleetZeroHeightJS,
		},
		{
			Key:     "fleet-map",
			Title:   "Fleet map",
			Caption: "The top-level map: this node's card, a reachable restricted peer with omitted concepts, and an unreachable unrestricted peer, joined by measured link edges. The tab strip becomes the top-level view switch in the same row.",
			InitJS:  fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="map"]').click();
  for (var i = 0; i < 80 && document.querySelectorAll('.fleet-card:not(.hub) .fleet-card-body:not(.fleet-card-trust)').length < 2; i++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('.fleet-edge')) throw new Error('fleet: no map edges');
  if (document.querySelector('nav [data-tab="groups"]').offsetParent !== null) throw new Error('fleet: per-node tabs still visible in the map');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "fleet-map-link",
			Title:   "Fleet map: a link's trust",
			Caption: "Each peer link's label names both directions of trust (peer → this node, this node → peer); clicking it spells out who can do what and who set it. Cards say \"You trust it\" / \"It trusts you\" instead of a bare level badge. lab is unreachable, so its trust in this node is not reported.",
			InitJS:  fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="map"]').click();
  for (var i = 0; i < 80 && !/hidden/.test((document.querySelector('.fleet-edge-label[aria-label^="Trust between forge"]') || {}).textContent || ''); i++) await new Promise(function(r){ setTimeout(r, 100); });
  var badge = document.querySelector('.fleet-edge-label[aria-label^="Trust between forge"]');
  if (!badge) throw new Error('fleet: no link label for forge');
  badge.click();
  for (var j = 0; j < 30 && !document.querySelector('.fleet-link-pop'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('.fleet-link-pop')) throw new Error('fleet: link popover did not open');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "fleet-map-untrusted",
			Title:   "Fleet map: show untrusted",
			Caption: "Show untrusted adds the hub-visible instances this node has not trusted to the map, set apart below the peers in one dotted section with a single dotted link from the hub: uncoloured cards with their self-reported name, instance ID and fingerprint, each with Trust….",
			InitJS:  fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="map"]').click();
  for (var i = 0; i < 50 && !document.querySelector('[data-fleet="show-untrusted"]'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  var box = document.querySelector('[data-fleet="show-untrusted"]');
  if (!box.checked) box.click();
  for (var j = 0; j < 50 && !document.querySelector('.fleet-card.untrusted'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('.fleet-card.untrusted [data-fleet="trust"]')) throw new Error('fleet: no untrusted card with Trust…');
  for (var k = 0; k < 30 && !document.querySelector('.fleet-edge.untrusted'); k++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('.fleet-edge.untrusted')) throw new Error('fleet: no untrusted edge');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "fleet-map-untrusted-trust",
			Title:   "Trust from the map",
			Caption: "Trust… on an untrusted card opens Fleet config's trust dialog: the full fingerprint to compare out of band, a local label, the trust level (or the default profile), and the comparison acknowledgement.",
			InitJS:  fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="map"]').click();
  for (var i = 0; i < 50 && !document.querySelector('[data-fleet="show-untrusted"]'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  var box = document.querySelector('[data-fleet="show-untrusted"]');
  if (!box.checked) box.click();
  for (var j = 0; j < 50 && !document.querySelector('.fleet-card.untrusted [data-fleet="trust"]'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('.fleet-card.untrusted [data-fleet="trust"]').click();
  for (var k = 0; k < 50 && !document.querySelector('#fleet-trust-modal #fleet-trust-submit'); k++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('#fleet-trust-modal')) throw new Error('fleet: trust dialog did not open from the map');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "fleet-map-trust-request",
			Title:   "Fleet map: trust requests",
			Caption: "An instance that asked to be trusted is shown on the map even with Show untrusted off: an amber card first in the untrusted section with its note (its own words) and whether it already trusts this node, Trust… and Dismiss; the top bar counts the requests. A stranger this node asked shows when.",
			InitJS:  "window.__dashsnapTrustAsk = true;" + fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="map"]').click();
  for (var i = 0; i < 50 && !document.querySelector('[data-fleet="show-untrusted"]'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  var box = document.querySelector('[data-fleet="show-untrusted"]');
  if (!box.checked) box.click();
  for (var j = 0; j < 50 && document.querySelectorAll('.fleet-card.untrusted').length < 2; j++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('.fleet-card.untrusted.asks [data-fleet="dismiss"]')) throw new Error('fleet: no trust request card');
  if (!document.querySelector('.fleet-card-asked')) throw new Error('fleet: no asked-at note');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "fleet-map-leave",
			Title:   "] leaves the map",
			Caption: "Map and Fleet config hide every per-node tab and their own anchors, so ] (like [ and ←/→) hands off to the fleet island, which returns to the per-node tab the operator left — here Groups.",
			InitJS:  fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="groups"]').click();
  document.querySelector('nav [data-tab="map"]').click();
  for (var i = 0; i < 50 && document.querySelector('nav [data-tab="groups"]').offsetParent !== null; i++) await new Promise(function(r){ setTimeout(r, 100); });
  document.body.dispatchEvent(new KeyboardEvent('keydown', { key: ']', bubbles: true }));
  for (var j = 0; j < 30 && !document.querySelector('nav [data-tab="groups"].active'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('nav [data-tab="groups"].active')) throw new Error('fleet: ] did not leave the map');
  if (document.querySelector('nav [data-tab="map"].active')) throw new Error('fleet: map still active after ]');
})();`,
			SettleMS: 300,
		},
		{
			Key:     "fleet-node-update",
			Title:   "Node self-update from the map",
			Caption: "Each map card shows the node's tclaude version and ↑ when a newer release is known; update…/manage… opens the node's update dialog: version and install method, latest release and when it was checked, the binaries, version-skew warnings, Check now, Update to the latest (confirms the binary swap and daemon restart) and Roll back.",
			InitJS:  fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="map"]').click();
  for (var i = 0; i < 80 && !document.querySelector('.fleet-card.local [data-fleet="node-update"]'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('.fleet-card.local [data-fleet="node-update"]').click();
  for (var j = 0; j < 30 && !document.querySelector('#fleet-node-update-apply'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('#fleet-node-update-rollback')) throw new Error('fleet: update dialog incomplete');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "fleet-merged-groups",
			Title:   "Groups · fused nodes",
			Caption: "nodes ▾ fuses every node: the Groups tab lists every linked node's groups, named group@node with the node's colour on the suffix (a click opens that node's dashboard). forge answers through the proxy; lab is unreachable, so its node reads stale. The tab strip stays; the ticked nodes' buttons are outlined.",
			InitJS:  fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-fuse'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('#node-chips-root .has-fleet .node-fuse').click();
  document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }));
  for (var i = 0; i < 80 && !document.querySelector('#fleetview-fused-groups-root .fleet-node-suffix[data-fleet-open="inst_hn3cxq7a"]'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('#fleetview-fused-groups-root .fleet-node-suffix[data-fleet-open="inst_hn3cxq7a"]')) throw new Error('fleet: no forge groups in the fused view');
  if (!document.querySelector('#fleetview-fused-groups-root .fleet-node-suffix[data-fleet-open=""]')) throw new Error('fleet: no local groups in the fused view');
  if (document.querySelector('nav [data-tab="groups"]').offsetParent === null) throw new Error('fleet: per-node tabs hidden in the fused view');
  if (new URLSearchParams(location.search).get('nodes') !== 'all') throw new Error('fleet: fused set not in the address: ' + location.search);
})();`,
			SettleMS: 400,
		},
		{
			Key:     "fleet-merged-groups-peer-settings",
			Title:   "Groups · fused nodes, a peer's group settings",
			Caption: "A peer that grants groups.settings (here: unrestricted trust) lets its groups' quick options run from the fused view, as on its own dashboard: the member cap editor is open on a forge group, and its save goes to forge's own group through the peer proxy. Controls a peer does not grant stay greyed in place.",
			InitJS:  fleetFederationStubJS + `;window.__dashsnapPeerSettings = true;`,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-fuse'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('#node-chips-root .has-fleet .node-fuse').click();
  document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }));
  var sel = '#fleetview-fused-groups-root details[data-fleet-node="inst_hn3cxq7a"] .group-max-members';
  for (var i = 0; i < 80 && !document.querySelector(sel); i++) await new Promise(function(r){ setTimeout(r, 100); });
  var chip = document.querySelector(sel);
  if (!chip) throw new Error('fleet: no forge group cap chip in the fused view');
  if (getComputedStyle(chip).opacity !== '1') throw new Error('fleet: a granted forge quick option is greyed');
  chip.click();
  for (var j = 0; j < 30 && !document.querySelector('#fleetview-fused-groups-root .group-max-members-input'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('#fleetview-fused-groups-root .group-max-members-input')) throw new Error('fleet: the forge cap editor did not open');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "fleet-fused-messages",
			Title:   "Messages · fused nodes",
			Caption: "Messages with every node fused: the same panes, filters, bulk bar, pager and reader as one node's Messages. The node chips narrow the view to one node; the fused folders span the nodes in view, and each node lists its own folders under its heading. Rows carry their node's pill, and actions go to that node.",
			InitJS:  fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-fuse'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('#node-chips-root .has-fleet .node-fuse').click();
  document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }));
  document.querySelector('nav [data-tab="messages"]').click();
  for (var i = 0; i < 80 && !document.querySelector('#fused-mail-list .mail-row'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  var row = document.querySelector('#fused-mail-list .mail-row');
  if (!row) throw new Error('fleet: no rows in fused Messages');
  if (!document.querySelector('.fused-mail-chip[data-node="inst_hn3cxq7a"]')) throw new Error('fleet: no forge chip in fused Messages');
  row.click();
  for (var j = 0; j < 30 && !document.querySelector('#fused-mail-reader .mail-headers'); j++) await new Promise(function(r){ setTimeout(r, 100); });
})();`,
			SettleMS: 400,
		},
		{
			Key:     "fleet-fused-messages-notification",
			Title:   "Messages · fused nodes, a peer's notification",
			Caption: "A human notification from forge in the fused view: the same reply, focus, mark read, ⤓ download and delete row as this node's own Messages. Reply opens the usual dialog and sends through forge; what forge keeps for its own operator (focus, delete) stays greyed in place.",
			InitJS:  fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-fuse'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('#node-chips-root .has-fleet .node-fuse').click();
  document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }));
  document.querySelector('nav [data-tab="messages"]').click();
  for (var i = 0; i < 80 && !document.querySelector('#fused-mail-list .mail-row'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  var human = Array.prototype.find.call(document.querySelectorAll('#fused-mail-sidebar .mailbox, .fused-mail-sidebar .mailbox'), function(b){ return /Human notifications/.test(b.textContent); });
  if (!human) throw new Error('fleet: no fused human folder');
  human.click();
  for (var j = 0; j < 80 && !document.querySelector('.mail-row-wrap[data-key^="inst_hn3cxq7a/human/"] .mail-row'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  var row = document.querySelector('.mail-row-wrap[data-key^="inst_hn3cxq7a/human/"] .mail-row');
  if (!row) throw new Error('fleet: no forge notification in the fused view');
  row.click();
  for (var k = 0; k < 30 && !document.querySelector('#fused-mail-reader [data-human="reply"]'); k++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('#fused-mail-reader [data-human="reply"]')) throw new Error('fleet: no reply on a forge notification');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "fleet-admin",
			Title:   "Fleet administration",
			Caption: "The top-level ⚙ Fleet view: this node's identity (fingerprint in full, copyable), the hub connection with Disconnect, and the peer tables — trusted peers with level, grants and pools, and hub-visible instances waiting to be trusted. Sub-pages not yet in the dashboard name their CLI.",
			InitJS:  fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="map"]').click();
  for (var s = 0; s < 30 && !document.querySelectorAll('.fleet-seg-btn')[2]; s++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelectorAll('.fleet-seg-btn')[2].click();
  for (var i = 0; i < 50 && !document.querySelector('#fleet-waiting'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  if (document.querySelectorAll('#fleet-trusted tbody tr').length !== 2) throw new Error('fleet: trusted peers missing');
  if (!document.querySelector('#fleet-waiting [data-fa="trust"]')) throw new Error('fleet: no Trust action for the waiting instance');
  if (document.querySelector('nav [data-tab="groups"]').offsetParent !== null) throw new Error('fleet: per-node tabs still visible in fleet admin');
  if (location.pathname !== '/fleet-admin') throw new Error('fleet: fleet admin not routed to /fleet-admin: ' + location.pathname);
})();`,
			SettleMS: 400,
		},
		{
			Key:     "fleet-setup-join",
			Title:   "Fleet setup guide: join a hub",
			Caption: "A node that has never been set up: the ⌘ Fleet chip stays clickable and says set up, and opens Fleet config on the setup guide. Step 1 takes a pasted join code and shows what the daemon read from it — hub, space, invite expiry and the self-signed certificate's SHA-256 to compare with the admin — before one Join confirmation. A hub address and a bare invite still work below it, and the instance ID and fingerprint are there for an admin who admits by ID.",
			InitJS:  "window.__dashsnapSetupFresh = true;" + fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('[data-fleet-entry] .node-chip-hint'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('[data-fleet-entry] .node-chip-hint')) throw new Error('setup: no set-up hint on the Fleet chip');
  document.querySelector('[data-fleet-entry]').click();
  for (var i = 0; i < 50 && !document.querySelector('#fleet-setup-hub'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('#fleet-setup-hub')) throw new Error('setup: Fleet did not open on the join step');
  var code = document.querySelector('#fleet-setup-code');
  code.value = 'tchj_eyJ2IjoxLCJodWIiOiJ3c3M6Ly9odWIuaG9tZTo4NDcwIiwiaW52aXRlIjoidGNoaV9rMnY5eDdxNG0xIiwic3BhY2UiOiJob21lIn0'; code.dispatchEvent(new Event('input', { bubbles: true }));
  for (var p = 0; p < 50 && !document.querySelector('#fleet-setup-code-preview code'); p++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('#fleet-setup-code-preview code')) throw new Error('setup: the pasted join code has no preview');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "fleet-setup-share",
			Title:   "Fleet setup guide: share a group",
			Caption: "Joined, with forge trusted and carol waiting: the guide opens on step 3. Pick a peer and one of this node's groups, then what the peer may do there; each preset is a set of ordinary peer grants, the CLI line underneath spells them out.",
			InitJS:  "window.__dashsnapSetupShare = true;" + fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('#fleet-setup-share'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('#fleet-setup-share')) throw new Error('setup: guide not on the share step');
  document.querySelector('[data-preset="status"] input').click();
})();`,
			SettleMS: 400,
		},
		{
			Key:     "fleet-setup-agents",
			Title:   "Fleet setup guide: let agents use peers",
			Caption: "Step 4 lists what each trusted peer shares with this node, per group, which of your groups and agents already use it, and gives one access in place: the peer=<peer>/<group> reach merges into their grants, keeping the rest.",
			InitJS:  "window.__dashsnapSetupShare = true;" + fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('.fs-step[data-step="agents"]'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('.fs-step[data-step="agents"]').click();
  for (var j = 0; j < 30 && !document.querySelector('[data-remote-group="builders"]'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('[data-remote-group="builders"]')) throw new Error('setup: no shared group on step 4');
  for (var k = 0; k < 30 && !document.querySelector('[data-remote-group="builders"] [data-holder]'); k++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('[data-remote-group="builders"] [data-holder]')) throw new Error('setup: step 4 shows no holder');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "fleet-boards-list",
			Title:   "Boards content browser: List",
			Caption: "Fleet → Boards opens on Browse content: every item on every board this node belongs to. List is a dense table (kind tabs, search with kind:/publisher:/board: filters) with a detail pane: who signed the selected item, a trusted-peer or unknown-publisher badge (the unknown one also gets an amber banner), signing key, digest, credential scan and every file, scripts flagged and readable as text.",
			InitJS:  fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('.fa-subtab'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  Array.from(document.querySelectorAll('.fa-subtab')).find(function(b){ return b.textContent === 'Boards'; }).click();
  for (var j = 0; j < 50 && !document.querySelector('[data-view-pick="list"]'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('[data-view-pick="list"]').click();
  for (var k = 0; k < 50 && !document.querySelector('[data-row="brd_oss/itm_fth"]'); k++) await new Promise(function(r){ setTimeout(r, 100); });
  if (document.querySelectorAll('#fleet-cb-table tbody tr').length !== 8) throw new Error('boards: browser rows missing');
  document.querySelector('th[data-sort-col="updated"]').click();
  document.querySelector('th[data-sort-col="updated"]').click();
  document.querySelector('[data-row="brd_oss/itm_fth"]').click();
  for (var f = 0; f < 50 && !document.querySelector('[data-file="bin/rerun.sh"]'); f++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('[data-file="bin/rerun.sh"] button').click();
})();`,
			SettleMS: 400,
		},
		{
			Key:     "fleet-boards-cards",
			Title:   "Boards content browser: Cards",
			Caption: "The same content as cards, with facets (kind, board, publisher trust) that count and filter. The List | Cards choice is remembered.",
			InitJS:  fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('.fa-subtab'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  Array.from(document.querySelectorAll('.fa-subtab')).find(function(b){ return b.textContent === 'Boards'; }).click();
  for (var j = 0; j < 50 && !document.querySelector('[data-view-pick="cards"]'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('[data-view-pick="cards"]').click();
  for (var k = 0; k < 50 && !document.querySelector('[data-card]'); k++) await new Promise(function(r){ setTimeout(r, 100); });
  if (document.querySelectorAll('[data-card]').length !== 8) throw new Error('boards: cards missing');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "fleet-boards-install",
			Title:   "Boards: install a skill",
			Caption: "Install… on a skill: target (harness skill directory, a group's directory as a project skill, a human-only custom directory, or a plain download), then a preview of exactly which paths are written and which overwrite, then Install… with the preview's single-use token. Executable bits and overwriting are opt-in.",
			InitJS:  fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('.fa-subtab'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  Array.from(document.querySelectorAll('.fa-subtab')).find(function(b){ return b.textContent === 'Boards'; }).click();
  for (var j = 0; j < 50 && !document.querySelector('[data-view-pick="list"]'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('[data-view-pick="list"]').click();
  for (var k = 0; k < 50 && !document.querySelector('[data-row="brd_oss/itm_fth"]'); k++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('[data-row="brd_oss/itm_fth"]').click();
  for (var f = 0; f < 50 && !document.querySelector('[data-cb-act="install"]'); f++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('[data-cb-act="install"]').click();
  for (var d = 0; d < 50 && !document.querySelector('[data-skill-target="claude"]'); d++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('[data-skill-target="claude"]').click();
  document.querySelector('[data-skill-target="codex"]').click();
  await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('#fleet-cb-preview-btn').click();
  for (var p = 0; p < 50 && !document.querySelector('#fleet-cb-preview'); p++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('#fleet-cb-preview')) throw new Error('boards: install preview missing');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "fleet-grants",
			Title:   "Peer grants",
			Caption: "Fleet → Peer grants for forge: each permission with where it applies (a group, or all groups including future ones, flagged), what it allows, and Revoke; a pool-inherited grant is revoked on its pool. The add row picks a permission, a group scope and, for spawning, the live cap; Grant… confirms with the consequence spelled out.",
			InitJS:  fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('#fleet-trusted [data-fa="grants"]'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('#fleet-trusted [data-fa="grants"]').click();
  for (var j = 0; j < 30 && !document.querySelector('#fleet-grants'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  if (document.querySelectorAll('#fleet-grants tbody tr').length !== 5) throw new Error('fleet: grants missing');
  if (document.querySelectorAll('#fleet-grants [data-fa="revoke"]').length !== 4) throw new Error('fleet: pool grant should not be revocable here');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "fleet-spawns",
			Title:   "Spawn requests",
			Caption: "Fleet → Spawn requests: workers peers asked this node to start in its groups, with who asked, the group, requested name/role/profile (requester-paid flagged), the brief and its state. Pending requests can be approved (optionally overriding name, profile, cwd, harness or model) or denied with a reason; an unconfirmed launch can be abandoned, warning that the original worker may still appear. Request a worker on a peer… asks a peer (or automatic placement) for one. The outbox shows delivery of what this node sent.",
			InitJS:  fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('.fa-subtab'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  Array.from(document.querySelectorAll('.fa-subtab')).find(function(b){ return /Spawn requests/.test(b.textContent); }).click();
  for (var j = 0; j < 30 && !document.querySelector('#fleet-outbox'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  if (document.querySelectorAll('#fleet-spawn-requests tbody tr').length !== 2) throw new Error('fleet: open spawn requests missing');
  document.querySelector('[data-spawn="12"] [data-fa="approve"]').click();
  for (var k = 0; k < 30 && !document.querySelector('#fleet-spawn-approve'); k++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('#fleet-spawn-approve')) throw new Error('fleet: approve dialog did not open');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "fleet-profile-editor",
			Title:   "Editing a node profile",
			Caption: "Fleet → Profiles & pools → Edit…: a profile's trust level, pool memberships, labels, requester-pays default and peer grants (scope and live cap per grant; add or remove), with worker permission overrides, the teleport landing and the config bundle as JSON. Saving makes a new revision and confirms what a peer it is applied to gets; peers it was already applied to keep their settings until it is applied again.",
			InitJS:  fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('.fa-subtab'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  Array.from(document.querySelectorAll('.fa-subtab')).find(function(b){ return /Profiles/.test(b.textContent); }).click();
  for (var j = 0; j < 30 && !document.querySelector('[data-profile="test-rig"] [data-fa="edit-profile"]'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('[data-profile="test-rig"] [data-fa="edit-profile"]').click();
  for (var k = 0; k < 30 && !document.querySelector('#fleet-profile-grants'); k++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('#fleet-profile-grants')) throw new Error('fleet: profile editor did not open');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "fleet-grant-launch",
			Title:   "Grant launch settings",
			Caption: "Fleet → Peer grants with groups.members.spawn picked: launch settings opens the receiver settings this node uses when it starts a worker for the peer — profile, selectable profiles, harness, model, directory and requester pays (jobs.run gets job approval instead). models.proxy grants take a gateway name or cover every gateway.",
			InitJS:  fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('#fleet-trusted [data-fa="grants"]'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('#fleet-trusted [data-fa="grants"]').click();
  for (var j = 0; j < 30 && !document.querySelector('#fleet-grant-slug'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  var sel = document.querySelector('#fleet-grant-slug'); sel.value = 'groups.members.spawn'; sel.dispatchEvent(new Event('change', { bubbles: true }));
  for (var k = 0; k < 20 && !document.querySelector('#fleet-grant-launch-toggle'); k++) await new Promise(function(r){ setTimeout(r, 50); });
  document.querySelector('#fleet-grant-launch-toggle').click();
  for (var l = 0; l < 20 && !document.querySelector('#fleet-grant-launch'); l++) await new Promise(function(r){ setTimeout(r, 50); });
  var p = document.querySelector('[data-launch="profile"]'); p.value = 'opus-fast'; p.dispatchEvent(new Event('input', { bubbles: true }));
})();`,
			SettleMS: 300,
		},
		{
			Key:     "fleet-invites",
			Title:   "Invites & joining",
			Caption: "Fleet → Invites & joining: issue an invite token for a node profile (uses, lifetime, and the trust the joining node grants back), the issued tokens with use counts and state (only active ones revocable), joining a master with a token, and completed enrollments.",
			InitJS:  fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('.fa-subtab'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  [].slice.call(document.querySelectorAll('.fa-subtab')).filter(function(b){ return /Invites/.test(b.textContent); })[0].click();
  for (var j = 0; j < 30 && !document.querySelector('#fleet-tokens'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  if (document.querySelectorAll('#fleet-tokens [data-fa="revoke-token"]').length !== 1) throw new Error('fleet: only the active token should be revocable');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "fleet-join",
			Title:   "Join a master (preview)",
			Caption: "Joining with an invite token previews before anything changes: the master's instance and full fingerprint, this node's fingerprint, the profile that sets this node's authority on the master, expiry, and the trust this node would grant it. Enroll stays disabled until the fingerprint check is ticked.",
			InitJS:  fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('.fa-subtab'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  [].slice.call(document.querySelectorAll('.fa-subtab')).filter(function(b){ return /Invites/.test(b.textContent); })[0].click();
  for (var j = 0; j < 30 && !document.querySelector('#fleet-join-open:not([disabled])'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('#fleet-join-open').click();
  for (var k = 0; k < 20 && !document.querySelector('#fleet-join-token'); k++) await new Promise(function(r){ setTimeout(r, 100); });
  var tok = document.querySelector('#fleet-join-token');
  tok.value = 'tcle1.example'; tok.dispatchEvent(new Event('input', { bubbles: true }));
  await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('#fleet-join-preview').click();
  for (var n = 0; n < 30 && !document.querySelector('#fleet-join-ack'); n++) await new Promise(function(r){ setTimeout(r, 100); });
  if (document.querySelector('#fleet-join-modal').textContent.indexOf('w5ze-a3nq-7m1p-kd42-xr8c-0fv6') < 0) throw new Error('fleet: master fingerprint not shown');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "fleet-profiles",
			Title:   "Profiles & pools",
			Caption: "Fleet → Profiles & pools: node pools with their members (× removes, the picker adds behind a confirm naming the grants gained), Grants… for the pool's grants and Delete; node profiles with trust level, pools, grant count and labels, the default marked, and Apply to peer… / Make default… / Delete… — each previewed or confirmed. Definitions are edited with the CLI.",
			InitJS:  fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('.fa-subtab'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  [].slice.call(document.querySelectorAll('.fa-subtab')).filter(function(b){ return /Profiles/.test(b.textContent); })[0].click();
  for (var j = 0; j < 30 && !(document.querySelector('#fleet-profiles') && document.querySelector('#fleet-pools')); j++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('#fleet-profiles .fa-badge')) throw new Error('fleet: default profile not marked');
  if (!document.querySelector('#fleet-pools [data-pool="rigs"]')) throw new Error('fleet: pool missing');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "fleet-moves",
			Title:   "Agent moves and the teleport switch",
			Caption: "Fleet → Moves: durable agent moves and teleports in both directions (⇢ leaving, ⇠ arriving) with peer, group, state and expiry; outgoing moves that have not started retiring can be abandoned. The teleport switch at the top freezes teleports leaving or landing on this node.",
			InitJS:  fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('.fa-subtab'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  [].slice.call(document.querySelectorAll('.fa-subtab')).filter(function(b){ return /Moves/.test(b.textContent); })[0].click();
  for (var j = 0; j < 30 && !document.querySelector('#fleet-moves'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  if (document.querySelectorAll('#fleet-moves [data-fa="abandon"]').length !== 2) throw new Error('fleet: abandon offered on the wrong moves');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "fleet-audit",
			Title:   "Federation audit",
			Caption: "Fleet → Audit: this node's federation activity, newest first — direction (⇠ a peer acting here, ⇢ this node acting on a peer), peer, kind, actor, target, group and outcome, refused or failed requests in red. Filter by peer and time window.",
			InitJS:  fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('.fa-subtab'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  [].slice.call(document.querySelectorAll('.fa-subtab')).filter(function(b){ return /Audit/.test(b.textContent); })[0].click();
  for (var j = 0; j < 30 && !document.querySelector('#fleet-audit'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  if (document.querySelectorAll('#fleet-audit tbody tr').length !== 6) throw new Error('fleet: audit rows missing');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "fleet-hub",
			Title:   "Hub admin",
			Caption: "Fleet → Hub: the hub's status and health, admissions with their spaces, open invites, admins, settings (effective value, source, and a marker where a remote setting overrides a serve flag) and an escaped log tail. Every change goes through a confirmation; a node that is not a hub admin sees only the claim.",
			InitJS:  fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('.fa-subtab'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  [].slice.call(document.querySelectorAll('.fa-subtab')).filter(function(b){ return b.textContent === 'Hub'; })[0].click();
  for (var j = 0; j < 30 && !document.querySelector('#fleet-hub-settings [data-setting]'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('#fleet-hub-settings [data-flag-overridden]')) throw new Error('fleet: hub settings missing the flag-overridden marker');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "fleet-peer-mail",
			Title:   "Message a peer operator",
			Caption: "Messages → Human notifications → ✉ peer: write to a peer's operator, or to agents on a peer (agent@peer or group:<group>@peer, optionally by role), with the recent outbox and its delivery state below. Peer operator mail in the inbox gets a reply button, and a cover request a peer forwarded while away gets Approve once / Deny.",
			InitJS:  fleetFederationStubJS,
			JS: `return (async function(){
  document.querySelector('nav [data-tab="messages"]').click();
  for (var i = 0; i < 50 && !document.querySelector('#mail-peer-compose:not([hidden])'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  var b = document.querySelector('#mail-peer-compose');
  if (!b || b.hidden) throw new Error('fleet: no peer compose button');
  b.click();
  for (var j = 0; j < 30 && !document.querySelector('#peer-mail-outbox'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  var body = document.querySelector('#peer-mail-body');
  body.value = 'Deploy freeze starts at 18:00 today; please hold merges on your side.';
  body.dispatchEvent(new Event('input', { bubbles: true }));
  var subj = document.querySelector('#peer-mail-subject');
  subj.value = 'Release window'; subj.dispatchEvent(new Event('input', { bubbles: true }));
})();`,
			SettleMS: 400,
		},
		{
			Key:     "fleet-away",
			Title:   "Away cover",
			Caption: "Fleet's identity row shows away cover: here, with away… to pick a trusted peer's operator (and an optional end time) who answers your agents' access requests once each while you are away, or — as here — away with forge covering and return…. Peer operators' access requests are never forwarded.",
			InitJS:  "window.__dashsnapAway = true;" + fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('#fleet-away-return'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('#fleet-away-return')) throw new Error('fleet: away state missing');
})();`,
			SettleMS: 300,
		},
		{
			Key:     "fleet-node-settings",
			Title:   "Node settings: hub and labels",
			Caption: "Fleet's identity row → settings…: the hub connection (URL, display name, a single-use invite, a CA file on this node, connected or not) and this node's labels, which automatic placement matches and peers with node.read see. Saving either confirms what changes; moving hubs warns that peers reachable only through the old hub drop.",
			InitJS:  fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('#fleet-node-settings-open'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('#fleet-node-settings-open').click();
  for (var j = 0; j < 30 && !document.querySelector('#fleet-node-labels'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('#fleet-node-labels')) throw new Error('fleet: node settings did not open');
})();`,
			SettleMS: 300,
		},
		{
			Key:     "fleet-rotate-confirm",
			Title:   "Rotate this node's identity: the confirmation",
			Caption: "Node settings → Identity → Rotate identity… fetches the read-only preview and confirms every consequence before anything is generated: the successor ID is linked to this one, the detection window, streams reconnect, pending sealed mail must be resent, issued model credentials and requester-paid leases are revoked, and the hop limit. Enter confirms and Escape cancels, like every confirm.",
			InitJS:  fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('#fleet-node-settings-open'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('#fleet-node-settings-open').click();
  for (var j = 0; j < 30 && !(document.querySelector('#fleet-rotate-open') && !document.querySelector('#fleet-rotate-open').disabled); j++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('#fleet-rotate-open').click();
  for (var k = 0; k < 30 && !/detection window/.test((document.querySelector('#confirm-body') || {}).textContent || ''); k++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!/hop|linked rotation/.test((document.querySelector('#confirm-body') || {}).textContent || '')) throw new Error('fleet: rotate confirmation missing');
})();`,
			SettleMS: 300,
		},
		{
			Key:     "fleet-rotation-pending",
			Title:   "Identity rotation pending",
			Caption: "After a rotation, node settings shows the successor and when peers accept it at the earliest (the detection window countdown); Rotate is disabled while one is pending, and recovery stays in the CLI.",
			InitJS:  "window.__dashsnapRotationPending = true;" + fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('#fleet-node-settings-open'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('#fleet-node-settings-open').click();
  for (var j = 0; j < 30 && !document.querySelector('#fleet-rotation-pending'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  var p = document.querySelector('#fleet-rotation-pending');
  if (!p || !/no earlier than/.test(p.textContent)) throw new Error('fleet: pending rotation missing');
  document.querySelector('#fleet-identity').scrollIntoView({ block: 'center' });
})();`,
			SettleMS: 300,
		},
		{
			Key:     "fleet-bundle-inspect",
			Title:   "Inspect an offered bundle before importing",
			Caption: "Offers → Inspect… fetches and verifies the payload on this node, lists its entries (bundle.json and sections/*.json for config; manifest.json and the transcript for agents) and shows any of them as plain text, JSON pretty-printed and transcripts paged; Download saves the bundle, Import… continues to the import preview, Decline… refuses it.",
			InitJS:  fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('.fa-subtab'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  [].slice.call(document.querySelectorAll('.fa-subtab')).filter(function(b){ return b.textContent === 'Offers'; })[0].click();
  for (var j = 0; j < 30 && !document.querySelector('[data-offer="off_9c4r"] [data-fa="inspect"]'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('[data-offer="off_9c4r"] [data-fa="inspect"]').click();
  for (var k = 0; k < 30 && !document.querySelector('#fleet-bundle-entries [data-path="sections/roles.json"]'); k++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('#fleet-bundle-entries [data-path="sections/roles.json"]').click();
  for (var m = 0; m < 30 && !document.querySelector('#fleet-bundle-text'); m++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!/release-captain/.test((document.querySelector('#fleet-bundle-text') || {}).textContent || '')) throw new Error('fleet: bundle entry not shown');
})();`,
			SettleMS: 300,
		},
		{
			Key:     "fleet-remote-terminals-picker",
			Title:   "A peer's terminals from the map",
			Caption: "Map → a peer card → Terminals…: that peer's sessions from the cached catalog, each opening interactive (sessions.attach) or watch-only (sessions.watch) as the peer shares it, unshared ones marked, and the CLI attach command kept as the native-terminal option.",
			InitJS:  fleetRemoteTerminalJS + fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="map"]').click();
  for (var i = 0; i < 80 && !document.querySelector('.fleet-card-terms'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('.fleet-card-terms').click();
  for (var j = 0; j < 30 && document.querySelectorAll('#remote-sessions tr').length < 3; j++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('#remote-sessions [data-open="watch"]')) throw new Error('fleet: terminal picker missing');
})();`,
			SettleMS: 300,
		},
		{
			Key:     "fleet-remote-terminal-watch",
			Title:   "A peer's agent terminal, watch-only",
			Caption: "Opening a watch-only session: the same browser terminal as a local one, rendered at the peer's pinned size, with a watch-only badge naming the peer; keystrokes and image paste are not sent.",
			InitJS:  fleetRemoteTerminalJS + fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="map"]').click();
  for (var i = 0; i < 80 && !document.querySelector('.fleet-card-terms'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('.fleet-card-terms').click();
  for (var j = 0; j < 30 && !document.querySelector('#remote-sessions [data-open="watch"]'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('#remote-sessions [data-open="watch"]').click();
  for (var k = 0; k < 60 && !document.querySelector('.mux-pane.active .term-remote-badge.watch'); k++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('.mux-pane.active .term-remote-badge.watch')) throw new Error('fleet: watch-only badge missing');
})();`,
			SettleMS: 600,
		},
		{
			Key:     "fleet-remote-terminal-interactive",
			Title:   "A peer's agent terminal, interactive",
			Caption: "Opening a session the peer lets this node type into (sessions.attach): the badge turns amber, keystrokes and image paste reach the peer's agent, and the peer shows REMOTE INPUT on its pane.",
			InitJS:  fleetRemoteTerminalJS + fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="map"]').click();
  for (var i = 0; i < 80 && !document.querySelector('.fleet-card-terms'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('.fleet-card-terms').click();
  for (var j = 0; j < 30 && !document.querySelector('#remote-sessions [data-open="interactive"]'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('#remote-sessions [data-open="interactive"]').click();
  for (var k = 0; k < 60 && !document.querySelector('.mux-pane.active .term-remote-badge.interactive'); k++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('.mux-pane.active .term-remote-badge.interactive')) throw new Error('fleet: interactive badge missing');
})();`,
			SettleMS: 600,
		},
		{
			Key:     "fleet-viewers",
			Title:   "Who is watching this node's terminals",
			Caption: "Fleet → Peers shows, while any peer is viewing an agent terminal here, who watches or drives which agent (interactive viewers can type, including answering harness prompts) and for how long; Disconnect… closes one view and says the peer can reopen it while it still holds the grant.",
			InitJS:  "window.__dashsnapViewers = true;" + fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('#fleet-viewers'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('#fleet-viewers')) throw new Error('fleet: viewers panel missing');
})();`,
			SettleMS: 300,
		},
		{
			Key:     "fleet-agent-viewer-badges",
			Title:   "Agents a peer is watching or typing into",
			Caption: "While a peer views an agent terminal here, the agent's harness line names it: amber ⌨ forge is typing into fe-dev-forms, 👁 lab is watching infra-lead. Clicking the badge opens Fleet → Peers with the viewers panel filtered to that agent.",
			InitJS:  "window.__dashsnapViewers = true;" + fleetFederationStubJS,
			JS: `return (async function(){
  for (var i = 0; i < 100 && document.querySelectorAll('.remote-viewers-badge').length < 2; i++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelectorAll('details[data-dnd-target-group]').forEach(function(d){ d.open = true; });
  if (document.querySelectorAll('.remote-viewers-badge').length < 2) throw new Error('fleet: agent viewer badges missing');
  document.querySelector('.remote-viewers-badge.typing').scrollIntoView({ block: 'center' });
})();`,
			SettleMS: 300,
		},
		{
			Key:     "fleet-agent-viewer-filtered",
			Title:   "Viewers filtered to one agent",
			Caption: "Clicking an agent's viewer badge opens Fleet → Peers with the viewers panel showing only that agent's viewers, where they can be disconnected; show all lists every viewer again.",
			InitJS:  "window.__dashsnapViewers = true;" + fleetFederationStubJS,
			JS: `return (async function(){
  for (var i = 0; i < 100 && !document.querySelector('.remote-viewers-badge.typing'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('.remote-viewers-badge.typing')) throw new Error('fleet: agent viewer badge missing');
  document.querySelector('.remote-viewers-badge.typing').click();
  for (var j = 0; j < 50 && !document.querySelector('#fleet-viewers-focus'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('#fleet-viewers-focus')) throw new Error('fleet: filtered viewers panel missing');
})();`,
			SettleMS: 300,
		},
		{
			Key:     "fleet-key-transition",
			Title:   "A peer's signing-key transition",
			Caption: "Fleet → Peers marks a peer whose signing key is changing in its fingerprint cell: forge's new key is pending (information only), lab has competing successors. The dialog shows both fingerprints in full and, for the conflict, the recover-peer preview and the apply to run only after verifying the fingerprint with the peer's operator; recovery stays in the CLI.",
			InitJS:  "window.__dashsnapKeyConflict = true; window.__dashsnapKeyPending = true;" + fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('[data-peer="inst_2p6ym4ke"] [data-fa="key"]'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('[data-peer="inst_hn3cxq7a"] [data-fa="key"]')) throw new Error('fleet: pending key marker missing');
  document.querySelector('[data-peer="inst_2p6ym4ke"] [data-fa="key"]').click();
  for (var j = 0; j < 30 && !document.querySelector('#fleet-key-preview'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('#fleet-key-apply')) throw new Error('fleet: recover commands missing');
})();`,
			SettleMS: 300,
		},
		{
			Key:     "fleet-map-key-transition",
			Title:   "Map cards with signing-key transitions",
			Caption: "The map notes a pending new signing key (amber, not yet accepted) and competing keys that need recovery (red) on the peer cards, pointing to Fleet → Peers.",
			InitJS:  "window.__dashsnapKeyConflict = true; window.__dashsnapKeyPending = true;" + fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="map"]').click();
  for (var i = 0; i < 80 && document.querySelectorAll('.fleet-card-key').length < 2; i++) await new Promise(function(r){ setTimeout(r, 100); });
  if (document.querySelectorAll('.fleet-card-key').length !== 2) throw new Error('fleet: key transition notes missing on the map');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "fleet-move-dialog",
			Title:   "Moving an agent to a peer",
			Caption: "Fleet → Moves → Move an agent to a peer…: pick a local agent, a trusted peer and the peer's receiving group; the confirm spells out that the agent retires here once the peer runs its copy, and starts focus on Cancel.",
			InitJS:  fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('.fa-subtab'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  [].slice.call(document.querySelectorAll('.fa-subtab')).filter(function(b){ return /Moves/.test(b.textContent); })[0].click();
  for (var j = 0; j < 30 && !document.querySelector('#fleet-move-open'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('#fleet-move-open').click();
  for (var k = 0; k < 30 && !document.querySelector('#fleet-move-agent'); k++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('#fleet-move-agent')) throw new Error('fleet: move dialog did not open');
})();`,
			SettleMS: 300,
		},
		{
			Key:     "fleet-job-live",
			Title:   "Live output of a running job",
			Caption: "Fleet → Jobs & repos → Live output on a running job this node sent: stdout and stderr as they arrive, read every 2 s only while the dialog is open, Fleet is shown and the tab is visible; the stored output replaces it once the job ends.",
			InitJS:  "window.__dashsnapLiveJob = true;" + fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('.fa-subtab'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  [].slice.call(document.querySelectorAll('.fa-subtab')).filter(function(b){ return b.textContent === 'Jobs & repos'; })[0].click();
  for (var j = 0; j < 30 && !document.querySelector('[data-job="job_live7k"] [data-fa="follow"]'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('[data-job="job_live7k"] [data-fa="follow"]').click();
  for (var k = 0; k < 30 && !(document.querySelector('#fleet-job-logs pre') && document.querySelector('#fleet-job-logs pre').textContent); k++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!/TestHubRelayBackpressure/.test(document.querySelector('#fleet-job-logs').textContent)) throw new Error('fleet: live output missing');
})();`,
			SettleMS: 300,
		},
		{
			Key:     "fleet-offers",
			Title:   "Bundle offers",
			Caption: "Fleet → Offers: config and agent bundles peers offered this node (from, kind — moves and teleports flagged —, summary, size, expiry, state; Preview… and Decline…) and the ones this node sent with their outcome, plus Offer my config… / Offer an agent… / Offer a profile's config….",
			InitJS:  fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('.fa-subtab'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  [].slice.call(document.querySelectorAll('.fa-subtab')).filter(function(b){ return b.textContent === 'Offers'; })[0].click();
  for (var j = 0; j < 30 && !document.querySelector('#fleet-offers-out'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  if (document.querySelectorAll('#fleet-offers-in tbody tr').length !== 3) throw new Error('fleet: incoming offers missing');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "fleet-offer-import",
			Title:   "Previewing a config offer",
			Caption: "Fleet → Offers → Preview…: the offer's size, expiry and full sha256, one row per item (new, overwrites yours, unchanged; security-relevant items marked) with a tick to include it, the values the offer needs on this node, and Apply… — blocked until conflicts are unticked or overwriting is chosen and the placeholders are filled, and confirmed with what changes.",
			InitJS:  fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('.fa-subtab'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  [].slice.call(document.querySelectorAll('.fa-subtab')).filter(function(b){ return b.textContent === 'Offers'; })[0].click();
  for (var j = 0; j < 30 && !document.querySelector('[data-offer="off_9c4r"] [data-fa="preview"]'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('[data-offer="off_9c4r"] [data-fa="preview"]').click();
  for (var k = 0; k < 30 && !document.querySelector('#fleet-offer-changes'); k++) await new Promise(function(r){ setTimeout(r, 100); });
  if (document.querySelectorAll('#fleet-offer-changes tbody tr').length !== 3) throw new Error('fleet: preview rows missing');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "fleet-offer-landing",
			Title:   "Where an arriving agent starts",
			Caption: "Previewing an agent offer (clone, move or teleport): Will start in names the directory this node resolved and why — here a matching Fleet repo, as a new isolated checkout — with the other valid candidates to choose from, a free path field, and the sender's path and repo URL shown as hints only.",
			InitJS:  fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('.fa-subtab'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  [].slice.call(document.querySelectorAll('.fa-subtab')).filter(function(b){ return b.textContent === 'Offers'; })[0].click();
  for (var j = 0; j < 30 && !document.querySelector('[data-offer="off_5m8t"] [data-fa="preview"]'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('[data-offer="off_5m8t"] [data-fa="preview"]').click();
  for (var k = 0; k < 30 && !document.querySelector('#fleet-landing-resolved'); k++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('#fleet-landing-choice')) throw new Error('fleet: landing picker missing');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "fleet-jobs",
			Title:   "Remote jobs and repositories",
			Caption: "Fleet → Jobs & repos: jobs peers sent here (⇠) and jobs this node sent (⇢) with repo@ref, group, command and state — Approve… for jobs a manual-approval grant holds, Cancel…, Resend… for uncertain delivery, Acknowledge stopped… for a job this node lost track of, Output for finished ones — plus Run a job… and the repositories peers may run jobs in here (Allow…, Edit…, Disable…).",
			InitJS:  fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('.fa-subtab'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  [].slice.call(document.querySelectorAll('.fa-subtab')).filter(function(b){ return b.textContent === 'Jobs & repos'; })[0].click();
  for (var j = 0; j < 30 && !(document.querySelector('#fleet-jobs') && document.querySelector('#fleet-repos')); j++) await new Promise(function(r){ setTimeout(r, 100); });
  if (document.querySelectorAll('#fleet-jobs tbody tr').length !== 4) throw new Error('fleet: jobs missing');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "fleet-models",
			Title:   "Model gateways",
			Caption: "Fleet → Model gateways: the all-gateways switch, each gateway's state (a policy missing an allowlist, budget or cap refuses every request and says so), models and limits (daily for all peers, per peer, per session; per-request caps), peers blocked on it with unblock and a block picker, requester-paid leases with Revoke, and the day's usage per gateway, peer and model. Turning a gateway or a peer off revokes the matching leases, and each confirm says so.",
			InitJS:  fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('.fa-subtab'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  Array.from(document.querySelectorAll('.fa-subtab')).find(function(b){ return /Model gateways/.test(b.textContent); }).click();
  for (var j = 0; j < 30 && !document.querySelector('#fleet-model-usage'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  if (document.querySelectorAll('#fleet-model-gateways tbody tr').length !== 2) throw new Error('fleet: gateways missing');
  if (document.querySelectorAll('#fleet-model-leases [data-fa="revoke-lease"]').length !== 1) throw new Error('fleet: revoked lease should not be revocable');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "fleet-hub-run",
			Title:   "Run a script on the hub",
			Caption: "Fleet → Hub → run a script on the hub…: with the host-set accept switch on and hub.exec held, the hub is a target on the Run page, preselected; it runs as the hub's service user, the confirm adds the hub threat note, and the pane shows its live output tail and exit status.",
			InitJS:  "window.__dashsnapHubRun = true;" + fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('.fa-subtab'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  [].slice.call(document.querySelectorAll('.fa-subtab')).filter(function(b){ return b.textContent === 'Hub'; })[0].click();
  for (var j = 0; j < 30 && !document.querySelector('#fleet-hub-run-open'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('#fleet-hub-run-open').click();
  for (var k = 0; k < 30 && !/ready/.test((document.querySelector('#fleet-run-nodes [data-node="hub"]') || {}).textContent || ''); k++) await new Promise(function(r){ setTimeout(r, 100); });
  var area = document.querySelector('#fleet-run-script');
  area.value = '#!/bin/sh\nsystemctl status tclaude-hub --no-pager | head -4\nss -tn state established';
  area.dispatchEvent(new Event('input', { bubbles: true }));
  await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('#fleet-run-submit').click();
  for (var c = 0; c < 30 && !document.querySelector('#confirm-ok'); c++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('#confirm-ok').click();
  for (var n = 0; n < 50 && !/completed/.test((document.querySelector('#fleet-run-results [data-node="hub"]') || {}).textContent || ''); n++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!/tclaude-hub\.service/.test((document.querySelector('#fleet-run-results [data-node="hub"]') || {}).textContent || '')) throw new Error('fleet: hub run output missing');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "fleet-hub-update",
			Title:   "Hub self-update in progress",
			Caption: "Fleet → Hub → update…: the hub's version, its supervisor and the update job restarting into its health check, with the deadline after which it rolls back on its own.",
			InitJS:  "window.__dashsnapHubUpdate = 'running';" + fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('.fa-subtab'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  [].slice.call(document.querySelectorAll('.fa-subtab')).filter(function(b){ return b.textContent === 'Hub'; })[0].click();
  for (var j = 0; j < 30 && !document.querySelector('#fleet-hub-update-open'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('#fleet-hub-update-open').click();
  for (var k = 0; k < 30 && !document.querySelector('#fleet-node-update-job'); k++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('#fleet-node-update-job')) throw new Error('fleet: hub update state missing');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "fleet-hub-update-rolled-back",
			Title:   "Hub update rolled back",
			Caption: "A hub update whose candidate failed its health check: the hub restored the previous version and the outcome stays visible with the reason.",
			InitJS:  "window.__dashsnapHubUpdate = 'rolled_back';" + fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('.fa-subtab'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  [].slice.call(document.querySelectorAll('.fa-subtab')).filter(function(b){ return b.textContent === 'Hub'; })[0].click();
  for (var j = 0; j < 30 && !document.querySelector('#fleet-hub-update-open'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('#fleet-hub-update-open').click();
  for (var k = 0; k < 30 && !document.querySelector('#fleet-node-update-job.fa-danger'); k++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('#fleet-node-update-job.fa-danger')) throw new Error('fleet: hub update state missing');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "fleet-hub-update-unsupervised",
			Title:   "Hub update refused: not supervised",
			Caption: "A hub that is not running under systemd or launchd cannot restart itself or roll back safely, so update is refused with the reason and the update button is disabled.",
			InitJS:  "window.__dashsnapHubUpdate = 'unsupervised';" + fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('.fa-subtab'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  [].slice.call(document.querySelectorAll('.fa-subtab')).filter(function(b){ return b.textContent === 'Hub'; })[0].click();
  for (var j = 0; j < 30 && !document.querySelector('#fleet-hub-update-open'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('#fleet-hub-update-open').click();
  for (var k = 0; k < 30 && !document.querySelector('#fleet-hub-update-blocked'); k++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('#fleet-hub-update-blocked')) throw new Error('fleet: hub update state missing');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "fleet-run",
			Title:   "Run a script on nodes",
			Caption: "Fleet → Run scripts: this node's receiving switch (off, with its limits), the node picker (a peer must grant node.exec and accept remote scripts; offline peers are skipped), the script editor with its size and timeout, and one result pane per node with state, exit code, duration and output tail; failed nodes can be re-run.",
			InitJS:  fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('.fa-subtab'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  [].slice.call(document.querySelectorAll('.fa-subtab')).filter(function(b){ return /Run scripts/.test(b.textContent); })[0].click();
  for (var j = 0; j < 30 && !/ready/.test((document.querySelector('#fleet-run-nodes') || {}).textContent || ''); j++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('#fleet-run-all').click();
  await new Promise(function(r){ setTimeout(r, 100); });
  var area = document.querySelector('#fleet-run-script');
  area.value = '#!/bin/sh\ndf -h /\nnvidia-smi --query-gpu=name,memory.used --format=csv';
  area.dispatchEvent(new Event('input', { bubbles: true }));
  await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('#fleet-run-submit').click();
  for (var k = 0; k < 30 && !document.querySelector('#confirm-ok'); k++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('#confirm-ok').click();
  for (var n = 0; n < 50 && !document.querySelector('#fleet-run-rerun'); n++) await new Promise(function(r){ setTimeout(r, 100); });
  if (document.querySelectorAll('#fleet-run-results .fa-run-pane').length !== 2) throw new Error('fleet: run panes missing');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "fleet-harnesses",
			Title:   "Harnesses across the fleet",
			Caption: "Fleet → Harnesses: every node's harnesses in one table (version, ↑ when an update is available, whether a login is present), this node plus each trusted peer through the peer proxy; an offline or unshared peer says why. Each cell opens install/update and the login push/backup/restore controls.",
			InitJS:  fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('.fa-subtab'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  [].slice.call(document.querySelectorAll('.fa-subtab')).filter(function(b){ return /Harnesses/.test(b.textContent); })[0].click();
  for (var j = 0; j < 30 && document.querySelectorAll('#fleet-harnesses [data-cell]').length < 10; j++) await new Promise(function(r){ setTimeout(r, 100); });
  if (document.querySelectorAll('#fleet-harnesses [data-cell]').length < 10) throw new Error('fleet: harness cells missing');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "fleet-harness-dialog",
			Title:   "Harness on a peer: install and logins",
			Caption: "Opening a harness on a peer: install or update with the exact command it runs, and the login files: push my login (warns that agents there act as you), back up now, and restore a backup.",
			InitJS:  fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('.fa-subtab'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  [].slice.call(document.querySelectorAll('.fa-subtab')).filter(function(b){ return /Harnesses/.test(b.textContent); })[0].click();
  for (var j = 0; j < 30 && !document.querySelector('#fleet-harnesses [data-node="inst_hn3cxq7a"] [data-cell="claude"]'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('#fleet-harnesses [data-node="inst_hn3cxq7a"] [data-cell="claude"]').click();
  for (var k = 0; k < 30 && !document.querySelector('#fleet-harness-backups'); k++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('#fleet-harness-push')) throw new Error('fleet: push missing');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "fleet-trust",
			Title:   "Trust dialog (unrestricted)",
			Caption: "Trusting a waiting instance previews first: the instance ID and the fingerprint the daemon will pin, in full, and an out-of-band check the operator must tick. Choosing Unrestricted repeats what it implies — every peer permission on all groups, including ones created later.",
			InitJS:  fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('#fleet-waiting [data-fa="trust"]'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('#fleet-waiting [data-fa="trust"]').click();
  for (var j = 0; j < 30 && !document.querySelector('#fleet-trust-modal input[name="fa-level"]'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelectorAll('#fleet-trust-modal input[name="fa-level"]')[1].click();
  for (var k = 0; k < 20 && !document.querySelector('#fleet-trust-modal .fa-consequence'); k++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('#fleet-trust-modal .fa-consequence')) throw new Error('fleet: unrestricted consequence not shown');
  if (document.querySelector('#fleet-trust-modal').textContent.indexOf('w5ze-a3nq-7m1p-kd42-xr8c-0fv6') < 0) throw new Error('fleet: full fingerprint not shown');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "fleet-unrestrict",
			Title:   "Make unrestricted dialog",
			Caption: "Raising a trusted peer to unrestricted shows its full fingerprint, repeats what unrestricted grants (every peer permission on all groups, including later ones) and needs the out-of-band check before the button enables.",
			InitJS:  fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('#fleet-trusted [data-fa="unrestrict"]'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('#fleet-trusted [data-fa="unrestrict"]').click();
  for (var j = 0; j < 30 && !document.querySelector('#fleet-level-modal .fa-consequence'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('#fleet-level-modal .fa-consequence')) throw new Error('fleet: unrestrict consequence not shown');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "fleet-untrust-confirm",
			Title:   "Untrust confirm",
			Caption: "Untrust… goes through the shared confirm, which says what the peer loses — every grant, pool grants, catalog and access — before anything changes.",
			InitJS:  fleetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .has-fleet .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('#fleet-trusted [data-fa="unrestrict"]'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('#fleet-trusted [data-fa="untrust"]').click();
  for (var j = 0; j < 30 && !document.querySelector('#confirm-modal.show'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('#confirm-modal.show')) throw new Error('fleet: untrust confirm not shown');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "fleet-group-links",
			Title:   "Linked-group marker",
			Caption: "A group linked to federation peers carries a 🌐 marker after its header chips (green dot: a linked node is live). The popover lists each link — direct or pool grant, or route mirror — with what it allows and a jump to that node's dashboard.",
			InitJS:  fleetGroupLinksJS + fleetFederationStubJS,
			JS: showGroups + `return (async function(){
  for (var i = 0; i < 50 && !document.querySelector('.group-federation-chip'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  var chip = document.querySelector('.group-federation-chip');
  if (!chip) throw new Error('fleet: no federation marker');
  if (document.querySelectorAll('.group-federation-chip').length !== 1) throw new Error('fleet: marker on an unlinked group');
  chip.click();
  for (var j = 0; j < 20 && !document.querySelector('.group-federation-pop'); j++) await new Promise(function(r){ setTimeout(r, 50); });
  if (!document.querySelector('.group-federation-pop')) throw new Error('fleet: popover did not open');
})();`,
			SettleMS: 300,
		},
		{
			Key:     "fleet-request-access",
			Title:   "Request access from a peer",
			Caption: "In a peer view, each feature the peer does not share has a request… link in the peer-view pill. It opens a dialog that asks the peer's operator for that permission: how long and a reason (group permissions without a group cover every group on the peer, which its operator may narrow). Local-only features such as terminals have no link. Once sent, the dialog follows the request until the peer's operator decides.",
			InitJS:  fleetRemoteViewJS + fleetFederationStubJS,
			JS: showGroups + `return (async function(){
  for (var i = 0; i < 50 && !document.querySelector('.remote-node-pill'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('.remote-node-pill').click();
  for (var k = 0; k < 20 && !document.querySelector('.rnp-ask[data-perm="costs.read"]'); k++) await new Promise(function(r){ setTimeout(r, 50); });
  var ask = document.querySelector('.rnp-ask[data-perm="costs.read"]');
  if (document.querySelector('.rnp-ask[data-perm="sessions.attach"]')) throw new Error('fleet: terminals offered a request link');
  if (!ask) throw new Error('fleet: no request link for an unshared feature');
  ask.click();
  for (var j = 0; j < 20 && !document.querySelector('#peer-access-reason'); j++) await new Promise(function(r){ setTimeout(r, 50); });
  var reason = document.querySelector('#peer-access-reason');
  if (!reason) throw new Error('fleet: request dialog did not open');
  reason.value = 'Pair on the flaky deploy test with ada';
  reason.dispatchEvent(new Event('input', { bubbles: true }));
  var ttl = document.querySelector('#peer-access-ttl');
  ttl.value = '28800'; ttl.dispatchEvent(new Event('change', { bubbles: true }));
})();`,
			SettleMS: 300,
		},
		{
			Key:     "fleet-peer-access-decide",
			Title:   "A peer operator's access request",
			Caption: "Messages → Access requests on the receiving node: a request from a peer's operator names the peer, the permission, the scope and how long it asked for, plus its reason. Only this node's operator decides (never an away cover). An any-group request covers every group on this node, including future ones; approval can shorten the grant or narrow it to one group the peer already holds grants in, and there is no \"always\" for peers.",
			InitJS:  fleetGroupLinksJS + `window.__peerAccessRow = { id: 'par_8kq2', origin_peer: 'inst_hn3cxq7a', perm: 'sessions.watch', conv_title: 'operator@inst_hn3cxq7a', caller_state: 'operator', title_status: 'available', grant_ttl_seconds: 86400, body: 'Pair on the flaky deploy test with ada', body_label: 'Reason', path: '/api/peer-access-requests', scope_display: 'group_id=0', created_at: new Date(Date.now() - 40000).toISOString(), deadline: new Date(Date.now() + 260000).toISOString() };` + fleetFederationStubJS,
			JS: `return (async function(){
  document.querySelector('nav [data-tab="messages"]').click();
  for (var i = 0; i < 50 && !document.querySelector('[data-id="access-requests"]'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  var box = document.querySelector('[data-id="access-requests"]');
  if (!box) throw new Error('fleet: no access requests folder');
  box.click();
  for (var j = 0; j < 50 && !document.querySelector('.access-row-wrap'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  var row = document.querySelector('.access-row-wrap');
  if (!row) throw new Error('fleet: no peer access request row');
  row.click();
  for (var k = 0; k < 50 && !document.querySelector('.access-peer-group'); k++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('.access-peer-ttl')) throw new Error('fleet: no peer decide controls');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "fleet-peer-actions",
			Title:   "Acting on a peer's agent",
			Caption: "On a peer view where forge granted lifecycle actions, an agent's retire, clone and stop controls open the peer action dialog instead of the local one: the actions forge shares (stop, retire, clone, move here, teleport here), the consequence spelled out, and the CLI equivalent. Move and teleport always bring the agent to a group on this node.",
			InitJS:  "window.__dashsnapPeerActions = true;" + fleetRemoteViewJS + fleetFederationStubJS,
			JS: showGroups + `return (async function(){
  for (var i = 0; i < 50 && !document.querySelector('.remote-node-pill'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  for (var j = 0; j < 50 && !document.querySelector('[data-act="retire-agent"][data-stable-agent^="agt_"]'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  var btn = document.querySelector('[data-act="retire-agent"][data-stable-agent^="agt_"]');
  if (!btn) throw new Error('fleet: no retire control with an agent ID');
  btn.click();
  for (var k = 0; k < 30 && !document.querySelector('#peer-action-modal'); k++) await new Promise(function(r){ setTimeout(r, 100); });
  if (document.querySelectorAll('#peer-action-modal .peer-action-choices label').length !== 5) throw new Error('fleet: peer actions missing');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "fleet-remote-marker-pop",
			Title:   "What a peer shares with you",
			Caption: "On a peer view, the peer-view pill next to the node name opens what this peer shares and what it does not (each with request… where a permission would open it), its trust level, whether it is live, and the way back to this node.",
			InitJS:  fleetRemoteViewJS + fleetFederationStubJS,
			JS: showGroups + `return (async function(){
  for (var i = 0; i < 50 && !document.querySelector('.remote-node-pill'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  for (var j = 0; j < 50 && !document.querySelector('.remote-node-pill.live'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('.remote-node-pill').click();
  for (var k = 0; k < 30 && !document.querySelector('.remote-node-pop'); k++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('.remote-node-pop')) throw new Error('fleet: remote marker popover did not open');
})();`,
			SettleMS: 300,
		},
		{
			Key:     "fleet-remote-messages",
			Title:   "Messages · a peer node",
			Caption: "Messages on forge's page, where forge shares its mail (node.messages.read) and lets this node answer its notifications: the same Messages client as this node's own, with forge's mailboxes, a human notification open in the reader, and its reply and read actions going to forge.",
			InitJS:  "window.__dashsnapPeerMail = true;" + fleetRemoteViewJS + fleetFederationStubJS,
			JS: `return (async function(){
  for (var i = 0; i < 50 && !document.querySelector('.remote-node-pill'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="messages"]').click();
  for (var j = 0; j < 50 && !document.querySelector('#mail-sidebar .mailbox[data-id="human"]'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  var human = document.querySelector('#mail-sidebar .mailbox[data-id="human"]');
  if (!human) throw new Error('fleet: no human mailbox on forge Messages');
  human.click();
  for (var k = 0; k < 50 && !document.querySelector('#mail-list .mail-row'); k++) await new Promise(function(r){ setTimeout(r, 100); });
  var row = document.querySelector('#mail-list .mail-row');
  if (!row) throw new Error('fleet: no rows on forge Messages');
  row.click();
  for (var m = 0; m < 30 && !document.querySelector('#mail-reader .mail-headers'); m++) await new Promise(function(r){ setTimeout(r, 100); });
})();`,
			SettleMS: 400,
		},
		{
			Key:     "fleet-remote-view",
			Title:   "Peer view of a node",
			Caption: "The whole per-node UI showing the peer forge through the local proxy: forge's name replaces the title, a 2–3px line in forge's colour runs along the top edge, forge's chip is current, and the peer-view pill lists what forge shares. Tabs and actions forge does not offer are greyed in place (usage/costs, config, new group…), and the header says usage and costs are not shared. The harness asserts the header, tab bar and main area keep today's offsets.",
			InitJS:  fleetRemoteViewJS + fleetFederationStubJS,
			JS: showGroups + `return (async function(){
  for (var i = 0; i < 50 && !document.querySelector('.remote-node-pill'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('.remote-node-pill')) throw new Error('fleet: no remote marker');
  for (var j = 0; j < 50 && !document.querySelector('.node-chip.active[aria-current="page"]'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  var cur = document.querySelector('.node-chip[aria-current="page"]');
  if (!cur || cur.textContent.indexOf('forge') < 0) throw new Error('fleet: forge chip not current');
  // What forge does not share stays in place, greyed; a click on a mutation
  // control is stopped before the control's own handler runs.
  if (!document.querySelector('nav [data-tab="usage"].pv-off')) throw new Error('fleet: usage (costs) tab not greyed');
  if (!document.querySelector('nav [data-tab="config"].pv-off')) throw new Error('fleet: config tab not greyed');
  if (document.querySelector('nav [data-tab="groups"].pv-off')) throw new Error('fleet: groups tab greyed');
  if (!document.querySelector('#usage.peer-view-na')) throw new Error('fleet: header usage does not say it is not shared');
  var create = document.getElementById('group-create-open'); var reached = false;
  create.addEventListener('click', function(){ reached = true; });
  create.click();
  if (reached) throw new Error('fleet: a mutation control ran on a peer view');
  document.querySelector('.remote-node-pill').click();
  for (var k = 0; k < 20 && !document.querySelector('.remote-node-pop'); k++) await new Promise(function(r){ setTimeout(r, 50); });
  if (!document.querySelector('.remote-node-pop')) throw new Error('fleet: peer view popover did not open');
})();`,
			SettleMS: 300,
		},
	}
}
