package agentd_test

import "github.com/tofutools/tclaude/pkg/claude/agentd/dashsnap"

// skynetFederationStubJS fakes the federation status and node-summary reads the
// Skynet node row and map make, so the visual harness can show a linked fleet
// without a hub. Every other request passes through to the real daemon.
const skynetFederationStubJS = `(function(){
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
  if (window.__dashsnapKeyConflict) status.peers[1].identity_transition = { state: 'conflict', old_id: 'inst_2p6ym4ke', new_id: 'inst_8r3kq0vd', new_fingerprint: '8r3k-q0vd-n6wz-2hc4-pm7y-e1xa', received_at: '2026-10-10T08:41:00Z', accept_after: '2026-10-10T08:51:00Z', reason: 'competing signed successors' };
  if (window.__dashsnapKeyPending) status.peers[0].identity_transition = { state: 'pending', old_id: 'inst_hn3cxq7a', new_id: 'inst_c7m2x9tf', new_fingerprint: 'c7m2-x9tf-4kp1-wq8e-zn3d-6hv0', received_at: '2026-10-10T09:02:00Z', accept_after: '2026-10-10T09:12:00Z' };
  var res = { status: 'current', cpu: { logical_cores: 8, load_average: [2.7, 2, 1] }, ram: { total_bytes: 32e9, available_bytes: 12e9, available_estimated: false }, data_disk: { total_bytes: 500e9, available_bytes: 210e9 } };
  window.fetch = function(input, init) {
    var url = typeof input === 'string' ? input : input.url;
    var path = new URL(url, location.href).pathname;
    if (path === '/api/federation/status') return json(status);
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
      { id: 'etok_4mz81c', public_token: '', max_uses: 3, used: 1, revoked: false, expires_at: '2026-10-11T09:00:00Z' },
      { id: 'etok_q2v7tn', public_token: '', max_uses: 1, used: 1, revoked: false, expires_at: '2026-10-17T09:00:00Z' },
      { id: 'etok_8kd0rw', public_token: '', max_uses: 1, used: 0, revoked: true, expires_at: '2026-10-12T09:00:00Z' }
    ] });
    if (path === '/api/federation/enrollments') return json({ enrollments: [{ direction: 'issuer', token_id: 'etok_q2v7tn', peer: 'inst_hn3cxq7a', retired: false }] });
    if (path === '/api/federation/enroll/preview') return json({ claims: { master: 'inst_w5zea3nq', profile_name: 'worker', profile_id: 'nprof_c4r0l', profile_revision: 2, trust_level: 'restricted', expires_at: '2026-10-11T09:00:00Z' }, preview_token: 'pv', master_fingerprint: 'w5ze-a3nq-7m1p-kd42-xr8c-0fv6', node_fingerprint: 'q4w7-pjf2-kx3m-z6bt-y5nd-8c1e', consent: 'Running enroll trusts the pinned master at the displayed level. Its profile controls this node\'s authority on the master. No default profile or config offer is applied locally.' });
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
    if (path === '/api/federation/viewers') return json(window.__dashsnapViewers ? [
      { id: 'tv_8k2q', peer: 'inst_hn3cxq7a', agent: 'agt_r8k2m4c1x9', session: 'fe-dev-forms', group: 'frontend-squad', read_only: false, started: new Date(Date.now() - 720000).toISOString(), incoming: true },
      { id: 'tv_3m1x', peer: 'inst_2p6ym4ke', agent: 'agt_p2w7d0j5n3', session: 'infra-bench', group: 'infra-crew', read_only: true, started: new Date(Date.now() - 95000).toISOString(), incoming: true }
    ] : []);
    if (path === '/api/federation/bundle-offers') return json(url.indexOf('direction=out') >= 0 ? [
      { offer: { id: 'off_7q2m', type: 'agent', summary: 'Agent reviewer (config only)', expires_at: '2026-10-17T09:00:00Z' }, peer: 'inst_2p6ym4ke', direction: 'out', state: 'pending' },
      { offer: { id: 'off_3k1x', type: 'config', summary: 'Config bundle: 4 items', expires_at: '2026-10-16T09:00:00Z' }, peer: 'inst_hn3cxq7a', direction: 'out', state: 'applied' }
    ] : [
      { offer: { id: 'off_9c4r', type: 'config', bytes: 6144, sha256: '3f9a0c27d1e84b56a7c2e9f01d3b8a64c5e7f2190ab3d4e6f8a1c2b3d4e5f607', expires_at: '2026-10-17T09:00:00Z', summary: 'Config bundle: 3 items' }, peer: 'inst_hn3cxq7a', direction: 'in', state: 'pending', sender_agent: 'agt_7fk2' },
      { offer: { id: 'off_5m8t', type: 'agent', bytes: 482000, sha256: '9b1d', expires_at: '2026-10-17T09:00:00Z', summary: 'Agent ada with history', group: 'ops', move: { source_agent: 'agt_ada0' } }, peer: 'inst_hn3cxq7a', direction: 'in', state: 'ready' },
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
    if (path === '/api/peer/inst_hn3cxq7a/node-summary') return json({ version: 'v0.43.0', update_available: false, presence: 'online', shared_groups: 2, shared_agents: 9, online_agents: 7, waiting_for_input: 1, peer_view: { peer: 'desk', included: [], omitted: [{ feature: 'costs', requires: 'costs.read' }, { feature: 'terminals', requires: 'sessions.watch' }] } }, 200, { ETag: '"forge"' });
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
          snap.peer_view = window.__dashsnapPeerActions
            ? { peer: 'desk', included: ['agents.status', 'groups', 'messaging', 'spawn', 'lifecycle.stop', 'lifecycle.retire', 'lifecycle.clone', 'lifecycle.move', 'lifecycle.teleport'], omitted: [{ feature: 'costs', requires: 'costs.read' }, { feature: 'terminals', requires: 'sessions.attach' }] }
            : { peer: 'desk', included: ['agents.status', 'groups', 'messaging'], omitted: [{ feature: 'costs', requires: 'costs.read' }, { feature: 'spawn', requires: 'groups.members.spawn' }, { feature: 'terminals', requires: 'sessions.attach' }] };
          delete snap.assets_version;
          snap.usage = { available: false }; // what filterPeerFields leaves behind
          return new Response(JSON.stringify(snap), { status: 200, headers: { 'Content-Type': 'application/json' } });
        });
      });
      return realFetch(local, init);
    }
    if (path === '/api/federation/links' && window.__skynetGroupLinks) return json({ groups: [{ group_id: 1, name: 'ops', federation_links: window.__skynetGroupLinks }] });
    if (path === '/api/snapshot' && (window.__skynetGroupLinks || window.__peerAccessRow)) return realFetch(input, init).then(function(r){
      return r.clone().json().then(function(snap){
        var g = (snap.groups || [])[0];
        if (g && window.__skynetGroupLinks) g.federation_links = window.__skynetGroupLinks;
        if (window.__peerAccessRow) { snap.access_requests = [window.__peerAccessRow]; snap.access_requests_pending = 1; }
        return new Response(JSON.stringify(snap), { status: r.status, headers: r.headers });
      }, function(){ return r; });
    });
    return realFetch(input, init);
  };
})();`

// skynetRemoteTerminalJS stands in for the federation terminal socket: the
// hello names the mode (from the URL) and the target's pinned size, then a
// screen of agent output arrives as binary frames, as the bridge sends them.
const skynetRemoteTerminalJS = `(function(){
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

// skynetGroupLinksJS decorates the first group's snapshot with federation
// links, as dashboard_group_federation_links.go reports them.
const skynetGroupLinksJS = `window.__skynetGroupLinks = [
  { peer: 'inst_hn3cxq7a', label: 'forge', level: 'restricted', kind: 'grant', direction: 'in', slugs: ['groups.roster.read', 'message.direct'], online: true },
  { peer: 'inst_2p6ym4ke', label: 'lab', level: 'unrestricted', kind: 'grant', direction: 'in', pool: 'rigs', slugs: ['routes.consume'], online: false, last_seen: '2026-10-09T20:37:00Z' },
  { peer: 'inst_2p6ym4ke', label: 'lab', level: 'unrestricted', kind: 'route', direction: 'out', remote: 'reviewers', online: false, last_seen: '2026-10-09T20:37:00Z' }
];`

// skynetRemoteViewJS opens the page as the peer view of forge (?node=) before
// remote-node.js reads the URL.
const skynetRemoteViewJS = `history.replaceState(null, '', location.pathname + '?node=inst_hn3cxq7a' + (location.search.indexOf('wizard=1') >= 0 ? '&wizard=1' : ''));`

// skynetZeroHeightJS proves the node row costs no vertical space: the tab bar
// and the first group sit at the same offsets with the row shown and removed.
const skynetZeroHeightJS = `return (async function(){
  var host = document.querySelector('#node-chips-root');
  for (var i = 0; i < 50 && !host.querySelector('.node-chip'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!host.querySelector('.node-chip')) throw new Error('skynet: node chips did not render');
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
    throw new Error('skynet: node row changed the layout: ' + withRow + ' vs ' + without + ' — ' + tall);
  }
})();`

func skynetStates() []dashsnap.State {
	const showGroups = `document.querySelector('nav [data-tab="groups"]').click();`
	return []dashsnap.State{
		{
			Key:     "skynet-node-row",
			Title:   "Skynet node row (1600)",
			Caption: "This node and its trusted peers as chips at the right of the tab bar, with the map entry. The harness asserts the tab bar and main area keep today's offsets.",
			InitJS:  skynetFederationStubJS,
			JS:      showGroups + skynetZeroHeightJS,
		},
		{
			Key:     "skynet-node-row-1280",
			Title:   "Skynet node row (1280)",
			Caption: "The same row at 1280 px wide: still no added height.",
			Width:   1280,
			InitJS:  skynetFederationStubJS,
			JS:      showGroups + skynetZeroHeightJS,
		},
		{
			Key:     "skynet-map",
			Title:   "Skynet map",
			Caption: "The top-level map: this node's card, a reachable restricted peer with omitted concepts, and an unreachable unrestricted peer, joined by measured link edges. The tab strip becomes the top-level view switch in the same row.",
			InitJS:  skynetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="map"]').click();
  for (var i = 0; i < 80 && document.querySelectorAll('.skynet-card .skynet-card-body').length < 2; i++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('.skynet-edge')) throw new Error('skynet: no map edges');
  if (document.querySelector('nav [data-tab="groups"]').offsetParent !== null) throw new Error('skynet: per-node tabs still visible in the map');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "skynet-node-update",
			Title:   "Node self-update from the map",
			Caption: "Each map card shows the node's tclaude version and ↑ when a newer release is known; update…/manage… opens the node's update dialog: version and install method, latest release and when it was checked, the binaries, version-skew warnings, Check now, Update to the latest (confirms the binary swap and daemon restart) and Roll back.",
			InitJS:  skynetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="map"]').click();
  for (var i = 0; i < 80 && !document.querySelector('.skynet-card.local [data-skynet="node-update"]'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('.skynet-card.local [data-skynet="node-update"]').click();
  for (var j = 0; j < 30 && !document.querySelector('#fleet-node-update-apply'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('#fleet-node-update-rollback')) throw new Error('skynet: update dialog incomplete');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "skynet-merged-groups",
			Title:   "Groups · all nodes",
			Caption: "The top-level merged view: today's Groups listing over every linked node, named group@node with the node's colour on the suffix (a click opens that node's dashboard). forge answers through the proxy; lab is unreachable, so its node reads stale. The view switch replaces the tab strip, like the map.",
			InitJS:  skynetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="map"]').click();
  for (var s = 0; s < 30 && !document.querySelector('.skynet-seg-btn:not(.on)'); s++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('.skynet-seg-btn:not(.on)').click();
  for (var i = 0; i < 80 && !document.querySelector('#skynet-fleet-root .fleet-node-suffix[data-fleet-open="inst_hn3cxq7a"]'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('#skynet-fleet-root .fleet-node-suffix[data-fleet-open="inst_hn3cxq7a"]')) throw new Error('skynet: no forge groups in the merged view');
  if (!document.querySelector('#skynet-fleet-root .fleet-node-suffix[data-fleet-open=""]')) throw new Error('skynet: no local groups in the merged view');
  if (document.querySelector('nav [data-tab="groups"]').offsetParent !== null) throw new Error('skynet: per-node tabs still visible in the merged view');
  if (location.pathname !== '/fleet') throw new Error('skynet: merged view not routed to /fleet: ' + location.pathname);
})();`,
			SettleMS: 400,
		},
		{
			Key:     "skynet-fleet-admin",
			Title:   "Fleet administration",
			Caption: "The top-level ⚙ Fleet view: this node's identity (fingerprint in full, copyable), the hub connection with Disconnect, and the peer tables — trusted peers with level, grants and pools, and hub-visible instances waiting to be trusted. Sub-pages not yet in the dashboard name their CLI.",
			InitJS:  skynetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="map"]').click();
  for (var s = 0; s < 30 && !document.querySelectorAll('.skynet-seg-btn')[2]; s++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelectorAll('.skynet-seg-btn')[2].click();
  for (var i = 0; i < 50 && !document.querySelector('#fleet-waiting'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  if (document.querySelectorAll('#fleet-trusted tbody tr').length !== 2) throw new Error('skynet: trusted peers missing');
  if (!document.querySelector('#fleet-waiting [data-fa="trust"]')) throw new Error('skynet: no Trust action for the waiting instance');
  if (document.querySelector('nav [data-tab="groups"]').offsetParent !== null) throw new Error('skynet: per-node tabs still visible in fleet admin');
  if (location.pathname !== '/fleet-admin') throw new Error('skynet: fleet admin not routed to /fleet-admin: ' + location.pathname);
})();`,
			SettleMS: 400,
		},
		{
			Key:     "skynet-fleet-grants",
			Title:   "Peer grants",
			Caption: "Fleet → Peer grants for forge: each permission with where it applies (a group, or all groups including future ones, flagged), what it allows, and Revoke; a pool-inherited grant is revoked on its pool. The add row picks a permission, a group scope and, for spawning, the live cap; Grant… confirms with the consequence spelled out.",
			InitJS:  skynetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('#fleet-trusted [data-fa="grants"]'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('#fleet-trusted [data-fa="grants"]').click();
  for (var j = 0; j < 30 && !document.querySelector('#fleet-grants'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  if (document.querySelectorAll('#fleet-grants tbody tr').length !== 5) throw new Error('skynet: grants missing');
  if (document.querySelectorAll('#fleet-grants [data-fa="revoke"]').length !== 4) throw new Error('skynet: pool grant should not be revocable here');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "skynet-fleet-spawns",
			Title:   "Spawn requests",
			Caption: "Fleet → Spawn requests: workers peers asked this node to start in its groups, with who asked, the group, requested name/role/profile (requester-paid flagged), the brief and its state. Pending requests can be approved (optionally overriding name, profile, cwd, harness or model) or denied with a reason; an unconfirmed launch can be abandoned, warning that the original worker may still appear. Request a worker on a peer… asks a peer (or automatic placement) for one. The outbox shows delivery of what this node sent.",
			InitJS:  skynetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('.fa-subtab'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  Array.from(document.querySelectorAll('.fa-subtab')).find(function(b){ return /Spawn requests/.test(b.textContent); }).click();
  for (var j = 0; j < 30 && !document.querySelector('#fleet-outbox'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  if (document.querySelectorAll('#fleet-spawn-requests tbody tr').length !== 2) throw new Error('skynet: open spawn requests missing');
  document.querySelector('[data-spawn="12"] [data-fa="approve"]').click();
  for (var k = 0; k < 30 && !document.querySelector('#fleet-spawn-approve'); k++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('#fleet-spawn-approve')) throw new Error('skynet: approve dialog did not open');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "skynet-fleet-profile-editor",
			Title:   "Editing a node profile",
			Caption: "Fleet → Profiles & pools → Edit…: a profile's trust level, pool memberships, labels, requester-pays default and peer grants (scope and live cap per grant; add or remove), with worker permission overrides, the teleport landing and the config bundle as JSON. Saving makes a new revision and confirms what a peer it is applied to gets; peers it was already applied to keep their settings until it is applied again.",
			InitJS:  skynetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('.fa-subtab'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  Array.from(document.querySelectorAll('.fa-subtab')).find(function(b){ return /Profiles/.test(b.textContent); }).click();
  for (var j = 0; j < 30 && !document.querySelector('[data-profile="test-rig"] [data-fa="edit-profile"]'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('[data-profile="test-rig"] [data-fa="edit-profile"]').click();
  for (var k = 0; k < 30 && !document.querySelector('#fleet-profile-grants'); k++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('#fleet-profile-grants')) throw new Error('skynet: profile editor did not open');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "skynet-fleet-grant-launch",
			Title:   "Grant launch settings",
			Caption: "Fleet → Peer grants with groups.members.spawn picked: launch settings opens the receiver settings this node uses when it starts a worker for the peer — profile, selectable profiles, harness, model, directory and requester pays (jobs.run gets job approval instead). models.proxy grants take a gateway name or cover every gateway.",
			InitJS:  skynetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
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
			Key:     "skynet-fleet-invites",
			Title:   "Invites & joining",
			Caption: "Fleet → Invites & joining: issue an invite token for a node profile (uses, lifetime, and the trust the joining node grants back), the issued tokens with use counts and state (only active ones revocable), joining a master with a token, and completed enrollments.",
			InitJS:  skynetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('.fa-subtab'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  [].slice.call(document.querySelectorAll('.fa-subtab')).filter(function(b){ return /Invites/.test(b.textContent); })[0].click();
  for (var j = 0; j < 30 && !document.querySelector('#fleet-tokens'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  if (document.querySelectorAll('#fleet-tokens [data-fa="revoke-token"]').length !== 1) throw new Error('skynet: only the active token should be revocable');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "skynet-fleet-join",
			Title:   "Join a master (preview)",
			Caption: "Joining with an invite token previews before anything changes: the master's instance and full fingerprint, this node's fingerprint, the profile that sets this node's authority on the master, expiry, and the trust this node would grant it. Enroll stays disabled until the fingerprint check is ticked.",
			InitJS:  skynetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
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
  if (document.querySelector('#fleet-join-modal').textContent.indexOf('w5ze-a3nq-7m1p-kd42-xr8c-0fv6') < 0) throw new Error('skynet: master fingerprint not shown');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "skynet-fleet-profiles",
			Title:   "Profiles & pools",
			Caption: "Fleet → Profiles & pools: node pools with their members (× removes, the picker adds behind a confirm naming the grants gained), Grants… for the pool's grants and Delete; node profiles with trust level, pools, grant count and labels, the default marked, and Apply to peer… / Make default… / Delete… — each previewed or confirmed. Definitions are edited with the CLI.",
			InitJS:  skynetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('.fa-subtab'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  [].slice.call(document.querySelectorAll('.fa-subtab')).filter(function(b){ return /Profiles/.test(b.textContent); })[0].click();
  for (var j = 0; j < 30 && !(document.querySelector('#fleet-profiles') && document.querySelector('#fleet-pools')); j++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('#fleet-profiles .fa-badge')) throw new Error('skynet: default profile not marked');
  if (!document.querySelector('#fleet-pools [data-pool="rigs"]')) throw new Error('skynet: pool missing');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "skynet-fleet-moves",
			Title:   "Agent moves and the teleport switch",
			Caption: "Fleet → Moves: durable agent moves and teleports in both directions (⇢ leaving, ⇠ arriving) with peer, group, state and expiry; outgoing moves that have not started retiring can be abandoned. The teleport switch at the top freezes teleports leaving or landing on this node.",
			InitJS:  skynetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('.fa-subtab'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  [].slice.call(document.querySelectorAll('.fa-subtab')).filter(function(b){ return /Moves/.test(b.textContent); })[0].click();
  for (var j = 0; j < 30 && !document.querySelector('#fleet-moves'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  if (document.querySelectorAll('#fleet-moves [data-fa="abandon"]').length !== 2) throw new Error('skynet: abandon offered on the wrong moves');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "skynet-fleet-audit",
			Title:   "Federation audit",
			Caption: "Fleet → Audit: this node's federation activity, newest first — direction (⇠ a peer acting here, ⇢ this node acting on a peer), peer, kind, actor, target, group and outcome, refused or failed requests in red. Filter by peer and time window.",
			InitJS:  skynetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('.fa-subtab'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  [].slice.call(document.querySelectorAll('.fa-subtab')).filter(function(b){ return /Audit/.test(b.textContent); })[0].click();
  for (var j = 0; j < 30 && !document.querySelector('#fleet-audit'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  if (document.querySelectorAll('#fleet-audit tbody tr').length !== 6) throw new Error('skynet: audit rows missing');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "skynet-fleet-hub",
			Title:   "Hub admin",
			Caption: "Fleet → Hub: the hub's status and health, admissions with their spaces, open invites, admins, settings (effective value, source, and a marker where a remote setting overrides a serve flag) and an escaped log tail. Every change goes through a confirmation; a node that is not a hub admin sees only the claim.",
			InitJS:  skynetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('.fa-subtab'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  [].slice.call(document.querySelectorAll('.fa-subtab')).filter(function(b){ return b.textContent === 'Hub'; })[0].click();
  for (var j = 0; j < 30 && !document.querySelector('#fleet-hub-settings [data-setting]'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('#fleet-hub-settings [data-flag-overridden]')) throw new Error('skynet: hub settings missing the flag-overridden marker');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "skynet-peer-mail",
			Title:   "Message a peer operator",
			Caption: "Messages → Human notifications → ✉ peer: write to a peer's operator, or to agents on a peer (agent@peer or group:<group>@peer, optionally by role), with the recent outbox and its delivery state below. Peer operator mail in the inbox gets a reply button, and a cover request a peer forwarded while away gets Approve once / Deny.",
			InitJS:  skynetFederationStubJS,
			JS: `return (async function(){
  document.querySelector('nav [data-tab="messages"]').click();
  for (var i = 0; i < 50 && !document.querySelector('#mail-peer-compose:not([hidden])'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  var b = document.querySelector('#mail-peer-compose');
  if (!b || b.hidden) throw new Error('skynet: no peer compose button');
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
			Key:     "skynet-fleet-away",
			Title:   "Away cover",
			Caption: "Fleet's identity row shows away cover: here, with away… to pick a trusted peer's operator (and an optional end time) who answers your agents' access requests once each while you are away, or — as here — away with forge covering and return…. Peer operators' access requests are never forwarded.",
			InitJS:  "window.__dashsnapAway = true;" + skynetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('#fleet-away-return'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('#fleet-away-return')) throw new Error('skynet: away state missing');
})();`,
			SettleMS: 300,
		},
		{
			Key:     "skynet-fleet-node-settings",
			Title:   "Node settings: hub and labels",
			Caption: "Fleet's identity row → settings…: the hub connection (URL, display name, a single-use invite, a CA file on this node, connected or not) and this node's labels, which automatic placement matches and peers with node.read see. Saving either confirms what changes; moving hubs warns that peers reachable only through the old hub drop.",
			InitJS:  skynetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('#fleet-node-settings-open'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('#fleet-node-settings-open').click();
  for (var j = 0; j < 30 && !document.querySelector('#fleet-node-labels'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('#fleet-node-labels')) throw new Error('skynet: node settings did not open');
})();`,
			SettleMS: 300,
		},
		{
			Key:     "skynet-fleet-rotate-confirm",
			Title:   "Rotate this node's identity: the confirmation",
			Caption: "Node settings → Identity → Rotate identity… fetches the read-only preview and confirms every consequence before anything is generated: the successor ID is linked to this one, the detection window, streams reconnect, pending sealed mail must be resent, issued model credentials and requester-paid leases are revoked, and the hop limit. Enter confirms and Escape cancels, like every confirm.",
			InitJS:  skynetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('#fleet-node-settings-open'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('#fleet-node-settings-open').click();
  for (var j = 0; j < 30 && !(document.querySelector('#fleet-rotate-open') && !document.querySelector('#fleet-rotate-open').disabled); j++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('#fleet-rotate-open').click();
  for (var k = 0; k < 30 && !/detection window/.test((document.querySelector('#confirm-body') || {}).textContent || ''); k++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!/hop|linked rotation/.test((document.querySelector('#confirm-body') || {}).textContent || '')) throw new Error('skynet: rotate confirmation missing');
})();`,
			SettleMS: 300,
		},
		{
			Key:     "skynet-fleet-rotation-pending",
			Title:   "Identity rotation pending",
			Caption: "After a rotation, node settings shows the successor and when peers accept it at the earliest (the detection window countdown); Rotate is disabled while one is pending, and recovery stays in the CLI.",
			InitJS:  "window.__dashsnapRotationPending = true;" + skynetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('#fleet-node-settings-open'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('#fleet-node-settings-open').click();
  for (var j = 0; j < 30 && !document.querySelector('#fleet-rotation-pending'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  var p = document.querySelector('#fleet-rotation-pending');
  if (!p || !/no earlier than/.test(p.textContent)) throw new Error('skynet: pending rotation missing');
  document.querySelector('#fleet-identity').scrollIntoView({ block: 'center' });
})();`,
			SettleMS: 300,
		},
		{
			Key:     "skynet-fleet-bundle-inspect",
			Title:   "Inspect an offered bundle before importing",
			Caption: "Offers → Inspect… fetches and verifies the payload on this node, lists its entries (bundle.json and sections/*.json for config; manifest.json and the transcript for agents) and shows any of them as plain text, JSON pretty-printed and transcripts paged; Download saves the bundle, Import… continues to the import preview, Decline… refuses it.",
			InitJS:  skynetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('.fa-subtab'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  [].slice.call(document.querySelectorAll('.fa-subtab')).filter(function(b){ return b.textContent === 'Offers'; })[0].click();
  for (var j = 0; j < 30 && !document.querySelector('[data-offer="off_9c4r"] [data-fa="inspect"]'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('[data-offer="off_9c4r"] [data-fa="inspect"]').click();
  for (var k = 0; k < 30 && !document.querySelector('#fleet-bundle-entries [data-path="sections/roles.json"]'); k++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('#fleet-bundle-entries [data-path="sections/roles.json"]').click();
  for (var m = 0; m < 30 && !document.querySelector('#fleet-bundle-text'); m++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!/release-captain/.test((document.querySelector('#fleet-bundle-text') || {}).textContent || '')) throw new Error('skynet: bundle entry not shown');
})();`,
			SettleMS: 300,
		},
		{
			Key:     "skynet-remote-terminals-picker",
			Title:   "A peer's terminals from the map",
			Caption: "Map → a peer card → Terminals…: that peer's sessions from the cached catalog, each opening interactive (sessions.attach) or watch-only (sessions.watch) as the peer shares it, unshared ones marked, and the CLI attach command kept as the native-terminal option.",
			InitJS:  skynetRemoteTerminalJS + skynetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="map"]').click();
  for (var i = 0; i < 80 && !document.querySelector('.skynet-card-terms'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('.skynet-card-terms').click();
  for (var j = 0; j < 30 && document.querySelectorAll('#remote-sessions tr').length < 3; j++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('#remote-sessions [data-open="watch"]')) throw new Error('skynet: terminal picker missing');
})();`,
			SettleMS: 300,
		},
		{
			Key:     "skynet-remote-terminal-watch",
			Title:   "A peer's agent terminal, watch-only",
			Caption: "Opening a watch-only session: the same browser terminal as a local one, rendered at the peer's pinned size, with a watch-only badge naming the peer; keystrokes and image paste are not sent.",
			InitJS:  skynetRemoteTerminalJS + skynetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="map"]').click();
  for (var i = 0; i < 80 && !document.querySelector('.skynet-card-terms'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('.skynet-card-terms').click();
  for (var j = 0; j < 30 && !document.querySelector('#remote-sessions [data-open="watch"]'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('#remote-sessions [data-open="watch"]').click();
  for (var k = 0; k < 60 && !document.querySelector('#term-session-modal .term-remote-badge.watch'); k++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('#term-session-modal .term-remote-badge.watch')) throw new Error('skynet: watch-only badge missing');
})();`,
			SettleMS: 600,
		},
		{
			Key:     "skynet-remote-terminal-interactive",
			Title:   "A peer's agent terminal, interactive",
			Caption: "Opening a session the peer lets this node type into (sessions.attach): the badge turns amber, keystrokes and image paste reach the peer's agent, and the peer shows REMOTE INPUT on its pane.",
			InitJS:  skynetRemoteTerminalJS + skynetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="map"]').click();
  for (var i = 0; i < 80 && !document.querySelector('.skynet-card-terms'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('.skynet-card-terms').click();
  for (var j = 0; j < 30 && !document.querySelector('#remote-sessions [data-open="interactive"]'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('#remote-sessions [data-open="interactive"]').click();
  for (var k = 0; k < 60 && !document.querySelector('#term-session-modal .term-remote-badge.interactive'); k++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('#term-session-modal .term-remote-badge.interactive')) throw new Error('skynet: interactive badge missing');
})();`,
			SettleMS: 600,
		},
		{
			Key:     "skynet-fleet-viewers",
			Title:   "Who is watching this node's terminals",
			Caption: "Fleet → Peers shows, while any peer is viewing an agent terminal here, who watches or drives which agent (interactive viewers can type, including answering harness prompts) and for how long; Disconnect… closes one view and says the peer can reopen it while it still holds the grant.",
			InitJS:  "window.__dashsnapViewers = true;" + skynetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('#fleet-viewers'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('#fleet-viewers')) throw new Error('skynet: viewers panel missing');
})();`,
			SettleMS: 300,
		},
		{
			Key:     "skynet-fleet-key-transition",
			Title:   "A peer's signing-key transition",
			Caption: "Fleet → Peers marks a peer whose signing key is changing in its fingerprint cell: forge's new key is pending (information only), lab has competing successors. The dialog shows both fingerprints in full and, for the conflict, the recover-peer preview and the apply to run only after verifying the fingerprint with the peer's operator; recovery stays in the CLI.",
			InitJS:  "window.__dashsnapKeyConflict = true; window.__dashsnapKeyPending = true;" + skynetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('[data-peer="inst_2p6ym4ke"] [data-fa="key"]'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('[data-peer="inst_hn3cxq7a"] [data-fa="key"]')) throw new Error('skynet: pending key marker missing');
  document.querySelector('[data-peer="inst_2p6ym4ke"] [data-fa="key"]').click();
  for (var j = 0; j < 30 && !document.querySelector('#fleet-key-preview'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('#fleet-key-apply')) throw new Error('skynet: recover commands missing');
})();`,
			SettleMS: 300,
		},
		{
			Key:     "skynet-map-key-transition",
			Title:   "Map cards with signing-key transitions",
			Caption: "The map notes a pending new signing key (amber, not yet accepted) and competing keys that need recovery (red) on the peer cards, pointing to Fleet → Peers.",
			InitJS:  "window.__dashsnapKeyConflict = true; window.__dashsnapKeyPending = true;" + skynetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="map"]').click();
  for (var i = 0; i < 80 && document.querySelectorAll('.skynet-card-key').length < 2; i++) await new Promise(function(r){ setTimeout(r, 100); });
  if (document.querySelectorAll('.skynet-card-key').length !== 2) throw new Error('skynet: key transition notes missing on the map');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "skynet-fleet-move-dialog",
			Title:   "Moving an agent to a peer",
			Caption: "Fleet → Moves → Move an agent to a peer…: pick a local agent, a trusted peer and the peer's receiving group; the confirm spells out that the agent retires here once the peer runs its copy, and starts focus on Cancel.",
			InitJS:  skynetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('.fa-subtab'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  [].slice.call(document.querySelectorAll('.fa-subtab')).filter(function(b){ return /Moves/.test(b.textContent); })[0].click();
  for (var j = 0; j < 30 && !document.querySelector('#fleet-move-open'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('#fleet-move-open').click();
  for (var k = 0; k < 30 && !document.querySelector('#fleet-move-agent'); k++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('#fleet-move-agent')) throw new Error('skynet: move dialog did not open');
})();`,
			SettleMS: 300,
		},
		{
			Key:     "skynet-fleet-job-live",
			Title:   "Live output of a running job",
			Caption: "Fleet → Jobs & repos → Live output on a running job this node sent: stdout and stderr as they arrive, read every 2 s only while the dialog is open, Fleet is shown and the tab is visible; the stored output replaces it once the job ends.",
			InitJS:  "window.__dashsnapLiveJob = true;" + skynetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('.fa-subtab'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  [].slice.call(document.querySelectorAll('.fa-subtab')).filter(function(b){ return b.textContent === 'Jobs & repos'; })[0].click();
  for (var j = 0; j < 30 && !document.querySelector('[data-job="job_live7k"] [data-fa="follow"]'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('[data-job="job_live7k"] [data-fa="follow"]').click();
  for (var k = 0; k < 30 && !(document.querySelector('#fleet-job-logs pre') && document.querySelector('#fleet-job-logs pre').textContent); k++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!/TestHubRelayBackpressure/.test(document.querySelector('#fleet-job-logs').textContent)) throw new Error('skynet: live output missing');
})();`,
			SettleMS: 300,
		},
		{
			Key:     "skynet-fleet-offers",
			Title:   "Bundle offers",
			Caption: "Fleet → Offers: config and agent bundles peers offered this node (from, kind — moves and teleports flagged —, summary, size, expiry, state; Preview… and Decline…) and the ones this node sent with their outcome, plus Offer my config… / Offer an agent… / Offer a profile's config….",
			InitJS:  skynetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('.fa-subtab'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  [].slice.call(document.querySelectorAll('.fa-subtab')).filter(function(b){ return b.textContent === 'Offers'; })[0].click();
  for (var j = 0; j < 30 && !document.querySelector('#fleet-offers-out'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  if (document.querySelectorAll('#fleet-offers-in tbody tr').length !== 3) throw new Error('skynet: incoming offers missing');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "skynet-fleet-offer-import",
			Title:   "Previewing a config offer",
			Caption: "Fleet → Offers → Preview…: the offer's size, expiry and full sha256, one row per item (new, overwrites yours, unchanged; security-relevant items marked) with a tick to include it, the values the offer needs on this node, and Apply… — blocked until conflicts are unticked or overwriting is chosen and the placeholders are filled, and confirmed with what changes.",
			InitJS:  skynetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('.fa-subtab'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  [].slice.call(document.querySelectorAll('.fa-subtab')).filter(function(b){ return b.textContent === 'Offers'; })[0].click();
  for (var j = 0; j < 30 && !document.querySelector('[data-offer="off_9c4r"] [data-fa="preview"]'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('[data-offer="off_9c4r"] [data-fa="preview"]').click();
  for (var k = 0; k < 30 && !document.querySelector('#fleet-offer-changes'); k++) await new Promise(function(r){ setTimeout(r, 100); });
  if (document.querySelectorAll('#fleet-offer-changes tbody tr').length !== 3) throw new Error('skynet: preview rows missing');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "skynet-fleet-jobs",
			Title:   "Remote jobs and repositories",
			Caption: "Fleet → Jobs & repos: jobs peers sent here (⇠) and jobs this node sent (⇢) with repo@ref, group, command and state — Approve… for jobs a manual-approval grant holds, Cancel…, Resend… for uncertain delivery, Acknowledge stopped… for a job this node lost track of, Output for finished ones — plus Run a job… and the repositories peers may run jobs in here (Allow…, Edit…, Disable…).",
			InitJS:  skynetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('.fa-subtab'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  [].slice.call(document.querySelectorAll('.fa-subtab')).filter(function(b){ return b.textContent === 'Jobs & repos'; })[0].click();
  for (var j = 0; j < 30 && !(document.querySelector('#fleet-jobs') && document.querySelector('#fleet-repos')); j++) await new Promise(function(r){ setTimeout(r, 100); });
  if (document.querySelectorAll('#fleet-jobs tbody tr').length !== 4) throw new Error('skynet: jobs missing');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "skynet-fleet-models",
			Title:   "Model gateways",
			Caption: "Fleet → Model gateways: the all-gateways switch, each gateway's state (a policy missing an allowlist, budget or cap refuses every request and says so), models and limits (daily for all peers, per peer, per session; per-request caps), peers blocked on it with unblock and a block picker, requester-paid leases with Revoke, and the day's usage per gateway, peer and model. Turning a gateway or a peer off revokes the matching leases, and each confirm says so.",
			InitJS:  skynetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('.fa-subtab'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  Array.from(document.querySelectorAll('.fa-subtab')).find(function(b){ return /Model gateways/.test(b.textContent); }).click();
  for (var j = 0; j < 30 && !document.querySelector('#fleet-model-usage'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  if (document.querySelectorAll('#fleet-model-gateways tbody tr').length !== 2) throw new Error('skynet: gateways missing');
  if (document.querySelectorAll('#fleet-model-leases [data-fa="revoke-lease"]').length !== 1) throw new Error('skynet: revoked lease should not be revocable');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "skynet-fleet-hub-run",
			Title:   "Run a script on the hub",
			Caption: "Fleet → Hub → run a script on the hub…: with the host-set accept switch on and hub.exec held, the hub is a target on the Run page, preselected; it runs as the hub's service user, the confirm adds the hub threat note, and the pane shows its live output tail and exit status.",
			InitJS:  "window.__dashsnapHubRun = true;" + skynetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
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
  if (!/tclaude-hub\.service/.test((document.querySelector('#fleet-run-results [data-node="hub"]') || {}).textContent || '')) throw new Error('skynet: hub run output missing');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "skynet-fleet-hub-update",
			Title:   "Hub self-update in progress",
			Caption: "Fleet → Hub → update…: the hub's version, its supervisor and the update job restarting into its health check, with the deadline after which it rolls back on its own.",
			InitJS:  "window.__dashsnapHubUpdate = 'running';" + skynetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('.fa-subtab'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  [].slice.call(document.querySelectorAll('.fa-subtab')).filter(function(b){ return b.textContent === 'Hub'; })[0].click();
  for (var j = 0; j < 30 && !document.querySelector('#fleet-hub-update-open'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('#fleet-hub-update-open').click();
  for (var k = 0; k < 30 && !document.querySelector('#fleet-node-update-job'); k++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('#fleet-node-update-job')) throw new Error('skynet: hub update state missing');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "skynet-fleet-hub-update-rolled-back",
			Title:   "Hub update rolled back",
			Caption: "A hub update whose candidate failed its health check: the hub restored the previous version and the outcome stays visible with the reason.",
			InitJS:  "window.__dashsnapHubUpdate = 'rolled_back';" + skynetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('.fa-subtab'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  [].slice.call(document.querySelectorAll('.fa-subtab')).filter(function(b){ return b.textContent === 'Hub'; })[0].click();
  for (var j = 0; j < 30 && !document.querySelector('#fleet-hub-update-open'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('#fleet-hub-update-open').click();
  for (var k = 0; k < 30 && !document.querySelector('#fleet-node-update-job.fa-danger'); k++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('#fleet-node-update-job.fa-danger')) throw new Error('skynet: hub update state missing');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "skynet-fleet-hub-update-unsupervised",
			Title:   "Hub update refused: not supervised",
			Caption: "A hub that is not running under systemd or launchd cannot restart itself or roll back safely, so update is refused with the reason and the update button is disabled.",
			InitJS:  "window.__dashsnapHubUpdate = 'unsupervised';" + skynetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('.fa-subtab'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  [].slice.call(document.querySelectorAll('.fa-subtab')).filter(function(b){ return b.textContent === 'Hub'; })[0].click();
  for (var j = 0; j < 30 && !document.querySelector('#fleet-hub-update-open'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('#fleet-hub-update-open').click();
  for (var k = 0; k < 30 && !document.querySelector('#fleet-hub-update-blocked'); k++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('#fleet-hub-update-blocked')) throw new Error('skynet: hub update state missing');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "skynet-fleet-run",
			Title:   "Run a script on nodes",
			Caption: "Fleet → Run scripts: this node's receiving switch (off, with its limits), the node picker (a peer must grant node.exec and accept remote scripts; offline peers are skipped), the script editor with its size and timeout, and one result pane per node with state, exit code, duration and output tail; failed nodes can be re-run.",
			InitJS:  skynetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
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
  if (document.querySelectorAll('#fleet-run-results .fa-run-pane').length !== 2) throw new Error('skynet: run panes missing');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "skynet-fleet-harnesses",
			Title:   "Harnesses across the fleet",
			Caption: "Fleet → Harnesses: every node's harnesses in one table (version, ↑ when an update is available, whether a login is present), this node plus each trusted peer through the peer proxy; an offline or unshared peer says why. Each cell opens install/update and the login push/backup/restore controls.",
			InitJS:  skynetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('.fa-subtab'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  [].slice.call(document.querySelectorAll('.fa-subtab')).filter(function(b){ return /Harnesses/.test(b.textContent); })[0].click();
  for (var j = 0; j < 30 && document.querySelectorAll('#fleet-harnesses [data-cell]').length < 10; j++) await new Promise(function(r){ setTimeout(r, 100); });
  if (document.querySelectorAll('#fleet-harnesses [data-cell]').length < 10) throw new Error('skynet: harness cells missing');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "skynet-fleet-harness-dialog",
			Title:   "Harness on a peer: install and logins",
			Caption: "Opening a harness on a peer: install or update with the exact command it runs, and the login files: push my login (warns that agents there act as you), back up now, and restore a backup.",
			InitJS:  skynetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('.fa-subtab'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  [].slice.call(document.querySelectorAll('.fa-subtab')).filter(function(b){ return /Harnesses/.test(b.textContent); })[0].click();
  for (var j = 0; j < 30 && !document.querySelector('#fleet-harnesses [data-node="inst_hn3cxq7a"] [data-cell="claude"]'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('#fleet-harnesses [data-node="inst_hn3cxq7a"] [data-cell="claude"]').click();
  for (var k = 0; k < 30 && !document.querySelector('#fleet-harness-backups'); k++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('#fleet-harness-push')) throw new Error('skynet: push missing');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "skynet-fleet-trust",
			Title:   "Trust dialog (unrestricted)",
			Caption: "Trusting a waiting instance previews first: the instance ID and the fingerprint the daemon will pin, in full, and an out-of-band check the operator must tick. Choosing Unrestricted repeats what it implies — every peer permission on all groups, including ones created later.",
			InitJS:  skynetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('#fleet-waiting [data-fa="trust"]'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('#fleet-waiting [data-fa="trust"]').click();
  for (var j = 0; j < 30 && !document.querySelector('#fleet-trust-modal input[name="fa-level"]'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelectorAll('#fleet-trust-modal input[name="fa-level"]')[1].click();
  for (var k = 0; k < 20 && !document.querySelector('#fleet-trust-modal .fa-consequence'); k++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('#fleet-trust-modal .fa-consequence')) throw new Error('skynet: unrestricted consequence not shown');
  if (document.querySelector('#fleet-trust-modal').textContent.indexOf('w5ze-a3nq-7m1p-kd42-xr8c-0fv6') < 0) throw new Error('skynet: full fingerprint not shown');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "skynet-fleet-unrestrict",
			Title:   "Make unrestricted dialog",
			Caption: "Raising a trusted peer to unrestricted shows its full fingerprint, repeats what unrestricted grants (every peer permission on all groups, including later ones) and needs the out-of-band check before the button enables.",
			InitJS:  skynetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('#fleet-trusted [data-fa="unrestrict"]'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('#fleet-trusted [data-fa="unrestrict"]').click();
  for (var j = 0; j < 30 && !document.querySelector('#fleet-level-modal .fa-consequence'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('#fleet-level-modal .fa-consequence')) throw new Error('skynet: unrestrict consequence not shown');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "skynet-fleet-untrust-confirm",
			Title:   "Untrust confirm",
			Caption: "Untrust… goes through the shared confirm, which says what the peer loses — every grant, pool grants, catalog and access — before anything changes.",
			InitJS:  skynetFederationStubJS,
			JS: `return (async function(){
  for (var w = 0; w < 50 && !document.querySelector('#node-chips-root .node-chip'); w++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('nav [data-tab="fleet-admin"]').click();
  for (var i = 0; i < 50 && !document.querySelector('#fleet-trusted [data-fa="unrestrict"]'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('#fleet-trusted [data-fa="untrust"]').click();
  for (var j = 0; j < 30 && !document.querySelector('#confirm-modal.show'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('#confirm-modal.show')) throw new Error('skynet: untrust confirm not shown');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "skynet-group-links",
			Title:   "Linked-group marker",
			Caption: "A group linked to federation peers carries a 🌐 marker after its header chips (green dot: a linked node is live). The popover lists each link — direct or pool grant, or route mirror — with what it allows and a jump to that node's dashboard.",
			InitJS:  skynetGroupLinksJS + skynetFederationStubJS,
			JS: showGroups + `return (async function(){
  for (var i = 0; i < 50 && !document.querySelector('.group-federation-chip'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  var chip = document.querySelector('.group-federation-chip');
  if (!chip) throw new Error('skynet: no federation marker');
  if (document.querySelectorAll('.group-federation-chip').length !== 1) throw new Error('skynet: marker on an unlinked group');
  chip.click();
  for (var j = 0; j < 20 && !document.querySelector('.group-federation-pop'); j++) await new Promise(function(r){ setTimeout(r, 50); });
  if (!document.querySelector('.group-federation-pop')) throw new Error('skynet: popover did not open');
})();`,
			SettleMS: 300,
		},
		{
			Key:     "skynet-request-access",
			Title:   "Request access from a peer",
			Caption: "In a peer view, each feature the peer does not share has a request… link in the peer-view pill. It opens a dialog that asks the peer's operator for that permission: how long and a reason (group permissions without a group cover every group on the peer, which its operator may narrow). Local-only features such as terminals have no link. Once sent, the dialog follows the request until the peer's operator decides.",
			InitJS:  skynetRemoteViewJS + skynetFederationStubJS,
			JS: showGroups + `return (async function(){
  for (var i = 0; i < 50 && !document.querySelector('.remote-node-pill'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('.remote-node-pill').click();
  for (var k = 0; k < 20 && !document.querySelector('.rnp-ask[data-perm="costs.read"]'); k++) await new Promise(function(r){ setTimeout(r, 50); });
  var ask = document.querySelector('.rnp-ask[data-perm="costs.read"]');
  if (document.querySelector('.rnp-ask[data-perm="sessions.attach"]')) throw new Error('skynet: terminals offered a request link');
  if (!ask) throw new Error('skynet: no request link for an unshared feature');
  ask.click();
  for (var j = 0; j < 20 && !document.querySelector('#peer-access-reason'); j++) await new Promise(function(r){ setTimeout(r, 50); });
  var reason = document.querySelector('#peer-access-reason');
  if (!reason) throw new Error('skynet: request dialog did not open');
  reason.value = 'Pair on the flaky deploy test with ada';
  reason.dispatchEvent(new Event('input', { bubbles: true }));
  var ttl = document.querySelector('#peer-access-ttl');
  ttl.value = '28800'; ttl.dispatchEvent(new Event('change', { bubbles: true }));
})();`,
			SettleMS: 300,
		},
		{
			Key:     "skynet-peer-access-decide",
			Title:   "A peer operator's access request",
			Caption: "Messages → Access requests on the receiving node: a request from a peer's operator names the peer, the permission, the scope and how long it asked for, plus its reason. Only this node's operator decides (never an away cover). An any-group request covers every group on this node, including future ones; approval can shorten the grant or narrow it to one group the peer already holds grants in, and there is no \"always\" for peers.",
			InitJS:  skynetGroupLinksJS + `window.__peerAccessRow = { id: 'par_8kq2', origin_peer: 'inst_hn3cxq7a', perm: 'sessions.watch', conv_title: 'operator@inst_hn3cxq7a', caller_state: 'operator', title_status: 'available', grant_ttl_seconds: 86400, body: 'Pair on the flaky deploy test with ada', body_label: 'Reason', path: '/api/peer-access-requests', scope_display: 'group_id=0', created_at: new Date(Date.now() - 40000).toISOString(), deadline: new Date(Date.now() + 260000).toISOString() };` + skynetFederationStubJS,
			JS: `return (async function(){
  document.querySelector('nav [data-tab="messages"]').click();
  for (var i = 0; i < 50 && !document.querySelector('[data-id="access-requests"]'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  var box = document.querySelector('[data-id="access-requests"]');
  if (!box) throw new Error('skynet: no access requests folder');
  box.click();
  for (var j = 0; j < 50 && !document.querySelector('.access-row-wrap'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  var row = document.querySelector('.access-row-wrap');
  if (!row) throw new Error('skynet: no peer access request row');
  row.click();
  for (var k = 0; k < 50 && !document.querySelector('.access-peer-group'); k++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('.access-peer-ttl')) throw new Error('skynet: no peer decide controls');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "skynet-peer-actions",
			Title:   "Acting on a peer's agent",
			Caption: "On a peer view where forge granted lifecycle actions, an agent's retire, clone and stop controls open the peer action dialog instead of the local one: the actions forge shares (stop, retire, clone, move here, teleport here), the consequence spelled out, and the CLI equivalent. Move and teleport always bring the agent to a group on this node.",
			InitJS:  "window.__dashsnapPeerActions = true;" + skynetRemoteViewJS + skynetFederationStubJS,
			JS: showGroups + `return (async function(){
  for (var i = 0; i < 50 && !document.querySelector('.remote-node-pill'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  for (var j = 0; j < 50 && !document.querySelector('[data-act="retire-agent"][data-stable-agent^="agt_"]'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  var btn = document.querySelector('[data-act="retire-agent"][data-stable-agent^="agt_"]');
  if (!btn) throw new Error('skynet: no retire control with an agent ID');
  btn.click();
  for (var k = 0; k < 30 && !document.querySelector('#peer-action-modal'); k++) await new Promise(function(r){ setTimeout(r, 100); });
  if (document.querySelectorAll('#peer-action-modal .peer-action-choices label').length !== 5) throw new Error('skynet: peer actions missing');
})();`,
			SettleMS: 400,
		},
		{
			Key:     "skynet-remote-marker-pop",
			Title:   "What a peer shares with you",
			Caption: "On a peer view, the peer-view pill next to the node name opens what this peer shares and what it does not (each with request… where a permission would open it), its trust level, whether it is live, and the way back to this node.",
			InitJS:  skynetRemoteViewJS + skynetFederationStubJS,
			JS: showGroups + `return (async function(){
  for (var i = 0; i < 50 && !document.querySelector('.remote-node-pill'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  for (var j = 0; j < 50 && !document.querySelector('.remote-node-pill.live'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  document.querySelector('.remote-node-pill').click();
  for (var k = 0; k < 30 && !document.querySelector('.remote-node-pop'); k++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('.remote-node-pop')) throw new Error('skynet: remote marker popover did not open');
})();`,
			SettleMS: 300,
		},
		{
			Key:     "skynet-remote-view",
			Title:   "Peer view of a node",
			Caption: "The whole per-node UI showing the peer forge through the local proxy: forge's name replaces the title, a 2–3px line in forge's colour runs along the top edge, forge's chip is current, and the peer-view pill lists what forge shares. Tabs and actions forge does not offer are greyed in place (costs, config, new group…), and the header says usage and costs are not shared. The harness asserts the header, tab bar and main area keep today's offsets.",
			InitJS:  skynetRemoteViewJS + skynetFederationStubJS,
			JS: showGroups + `return (async function(){
  for (var i = 0; i < 50 && !document.querySelector('.remote-node-pill'); i++) await new Promise(function(r){ setTimeout(r, 100); });
  if (!document.querySelector('.remote-node-pill')) throw new Error('skynet: no remote marker');
  for (var j = 0; j < 50 && !document.querySelector('.node-chip.active[aria-current="page"]'); j++) await new Promise(function(r){ setTimeout(r, 100); });
  var cur = document.querySelector('.node-chip[aria-current="page"]');
  if (!cur || cur.textContent.indexOf('forge') < 0) throw new Error('skynet: forge chip not current');
  // What forge does not share stays in place, greyed; a click on a mutation
  // control is stopped before the control's own handler runs.
  if (!document.querySelector('nav [data-tab="costs"].pv-off')) throw new Error('skynet: costs tab not greyed');
  if (!document.querySelector('nav [data-tab="config"].pv-off')) throw new Error('skynet: config tab not greyed');
  if (document.querySelector('nav [data-tab="groups"].pv-off')) throw new Error('skynet: groups tab greyed');
  if (!document.querySelector('#usage.peer-view-na')) throw new Error('skynet: header usage does not say it is not shared');
  var create = document.getElementById('group-create-open'); var reached = false;
  create.addEventListener('click', function(){ reached = true; });
  create.click();
  if (reached) throw new Error('skynet: a mutation control ran on a peer view');
  document.querySelector('.remote-node-pill').click();
  for (var k = 0; k < 20 && !document.querySelector('.remote-node-pop'); k++) await new Promise(function(r){ setTimeout(r, 50); });
  if (!document.querySelector('.remote-node-pop')) throw new Error('skynet: peer view popover did not open');
})();`,
			SettleMS: 300,
		},
	}
}
