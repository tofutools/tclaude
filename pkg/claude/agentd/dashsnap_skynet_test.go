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
      { id: 'nprof_7h2k', name: 'test-rig', revision: 3, definition: { trust_level: 'restricted', pools: ['pool_1'], peer_grants: [{ slug: 'message.direct' }, { slug: 'groups.roster.read' }], labels: ['gpu', 'ci'] } },
      { id: 'nprof_9x1q', name: 'build-farm', revision: 1, definition: { trust_level: 'restricted', pools: [], peer_grants: [{ slug: 'jobs.run' }], labels: ['linux'] } }
    ], default: { id: 'nprof_7h2k', name: 'test-rig' } });
    if (path === '/api/federation/enroll-tokens') return json({ tokens: [
      { id: 'etok_4mz81c', public_token: '', max_uses: 3, used: 1, revoked: false, expires_at: '2026-10-11T09:00:00Z' },
      { id: 'etok_q2v7tn', public_token: '', max_uses: 1, used: 1, revoked: false, expires_at: '2026-10-17T09:00:00Z' },
      { id: 'etok_8kd0rw', public_token: '', max_uses: 1, used: 0, revoked: true, expires_at: '2026-10-12T09:00:00Z' }
    ] });
    if (path === '/api/federation/enrollments') return json({ enrollments: [{ direction: 'issuer', token_id: 'etok_q2v7tn', peer: 'inst_hn3cxq7a', retired: false }] });
    if (path === '/api/federation/enroll/preview') return json({ claims: { master: 'inst_w5zea3nq', profile_name: 'worker', profile_id: 'nprof_c4r0l', profile_revision: 2, trust_level: 'restricted', expires_at: '2026-10-11T09:00:00Z' }, preview_token: 'pv', master_fingerprint: 'w5ze-a3nq-7m1p-kd42-xr8c-0fv6', node_fingerprint: 'q4w7-pjf2-kx3m-z6bt-y5nd-8c1e', consent: 'Running enroll trusts the pinned master at the displayed level. Its profile controls this node\'s authority on the master. No default profile or config offer is applied locally.' });
    if (path === '/api/federation/peers/trust') return json({ instance_id: 'inst_w5zea3nq', fingerprint: 'w5ze-a3nq-7m1p-kd42-xr8c-0fv6', level: 'restricted', profile: null, plan: null, applied: false });
    if (path === '/api/node-summary') return json({ presence: 'online', shared_groups: 2, shared_agents: 10, online_agents: 8, waiting_for_input: 1, resources: res, health: 'current' }, 200, { ETag: '"local"' });
    if (path === '/api/peer/inst_hn3cxq7a/node-summary') return json({ presence: 'online', shared_groups: 2, shared_agents: 9, online_agents: 7, waiting_for_input: 1, peer_view: { peer: 'desk', included: [], omitted: [{ feature: 'costs', requires: 'costs.read' }, { feature: 'terminals', requires: 'sessions.watch' }] } }, 200, { ETag: '"forge"' });
    if (path === '/api/peer/inst_2p6ym4ke/node-summary') return json({ error: 'peer offline', code: 'peer_unreachable', reason: 'peer_offline', last_seen: '2026-10-09T20:37:00Z' }, 502);
    // A peer view of forge: serve this daemon's own per-node data as if forge
    // answered through the proxy, with forge's peer_view metadata on the snapshot.
    var forgePrefix = '/api/peer/inst_hn3cxq7a/';
    if (path.indexOf(forgePrefix) === 0) {
      var u = new URL(url, location.href);
      var local = '/api/' + path.slice(forgePrefix.length) + u.search;
      if (path === forgePrefix + 'snapshot') return realFetch(local, init).then(function(r){
        return r.json().then(function(snap){
          snap.peer_view = { peer: 'desk', included: ['agents.status', 'groups', 'messaging'], omitted: [{ feature: 'costs', requires: 'costs.read' }, { feature: 'spawn', requires: 'groups.members.spawn' }, { feature: 'terminals', requires: 'sessions.attach' }] };
          delete snap.assets_version;
          snap.usage = { available: false }; // what filterPeerFields leaves behind
          return new Response(JSON.stringify(snap), { status: 200, headers: { 'Content-Type': 'application/json' } });
        });
      });
      return realFetch(local, init);
    }
    if (path === '/api/snapshot' && window.__skynetGroupLinks) return realFetch(input, init).then(function(r){
      return r.clone().json().then(function(snap){
        var g = (snap.groups || [])[0];
        if (g) g.federation_links = window.__skynetGroupLinks;
        return new Response(JSON.stringify(snap), { status: r.status, headers: r.headers });
      }, function(){ return r; });
    });
    return realFetch(input, init);
  };
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
