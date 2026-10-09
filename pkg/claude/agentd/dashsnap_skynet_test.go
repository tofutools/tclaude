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
  var status = { enabled: true, instance_id: 'inst_q4w7pjf2kx3mz6bty5nd', name: 'desk', hub_url: 'wss://hub.example:8470', hub: { state: 'connected' },
    peers: [
      { instance_id: 'inst_hn3cxq7a', label: 'forge', trusted: true, online: true, level: 'restricted' },
      { instance_id: 'inst_2p6ym4ke', label: 'lab', trusted: true, online: false, level: 'unrestricted', last_seen: '2026-10-09T20:37:00Z' },
      { instance_id: 'inst_w5zea3nq', name: 'carol@buildbox', trusted: false, online: true }
    ] };
  var res = { status: 'current', cpu: { logical_cores: 8, load_average: [2.7, 2, 1] }, ram: { total_bytes: 32e9, available_bytes: 12e9, available_estimated: false }, data_disk: { total_bytes: 500e9, available_bytes: 210e9 } };
  window.fetch = function(input, init) {
    var url = typeof input === 'string' ? input : input.url;
    var path = new URL(url, location.href).pathname;
    if (path === '/api/federation/status') return json(status);
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
