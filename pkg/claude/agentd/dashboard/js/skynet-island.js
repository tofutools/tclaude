import { Fragment, h, render } from 'preact';
import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { NodeUpdateDialog } from './node-update.js';
import { RequestAccessDialog } from './peer-access.js';
import { PeerActionHost } from './peer-action.js';
import { shellConfirm, shellToast } from './shell-state.js';
import { STATUS_POLL_MS, cardView, fmtAge, nodeColor, nodeHref, peerViewSummary, pollDelay, remoteHealthView, remoteNodeID, staggerOffset, switchOrder, visibleChips } from './skynet-model.js';
import { dashboardState } from './snapshot-store.js';
import { TOP_LEVEL_TABS } from './skynet-state.js';

const html = htm.bind(h);

// SAFE_SLUG admits a permission slug from the peer's metadata into a request.
const SAFE_SLUG = /^[a-z][a-z0-9._-]{0,63}$/;
// Features a peer can never open through an access request: local-only
// transports (the dashboard shell, terminals, inline spawn) and the access
// requests themselves. The daemon refuses these as not requestable.
const UNREQUESTABLE = new Set(['local_dashboard', 'terminals', 'spawn.inline', 'permissions.requests']);
const requestable = (o) => o.requires && SAFE_SLUG.test(o.requires) && !UNREQUESTABLE.has(o.feature) && o.requires !== 'local_only' && o.requires !== 'peer_access';

// navigateTab routes through the real nav anchors so tab activation, history
// and per-tab side effects stay owned by refresh.js / nav-history.js.
function defaultNavigate(tab) {
  let anchor = document.querySelector(`nav [data-tab="${tab}"]`);
  // A remembered per-node tab can have been hidden since (Terminals with no
  // pane, Debug switched off); Groups is always there.
  if (!TOP_LEVEL_TABS.has(tab) && (!anchor || anchor.offsetParent === null)) anchor = document.querySelector('nav [data-tab="groups"]');
  anchor?.click();
}

// defaultSwitchNode shows node id's per-node view ('' = this node). It is a
// page navigation: remote-node.js routes the per-node API only from page load,
// so one node's data can never be painted under another node's marker.
function defaultSwitchNode(id) {
  globalThis.location.assign(nodeHref(id));
}

// openNodeView opens a node's per-node view from elsewhere in the dashboard,
// e.g. a linked group's federation marker.
export function openNodeView(id) {
  defaultSwitchNode(id);
}

const MapGlyph = () => html`<svg width="14" height="14" viewBox="0 0 14 14" fill="none" stroke="currentColor" stroke-width="1.4" aria-hidden="true"><circle cx="3" cy="3.5" r="2"/><circle cx="11" cy="3" r="2"/><circle cx="7" cy="11" r="2"/><path d="M4.8 4.3 5.9 9.3M9.4 4.3 7.9 9.3M5 3.4 9 3.1"/></svg>`;

function presenceLabel(node) {
  if (node.local) return 'this node';
  return node.online ? 'online' : 'offline';
}

// NodeChips is the tab-bar node row: this node, then trusted peers, then an
// overflow chip. The map entry is the static nav[data-tab="map"] anchor right
// after this host, so tab routing keeps one owner.
export function NodeChips({ state, navigate = defaultNavigate, remote = remoteNodeID(), switchNode = defaultSwitchNode }) {
  const current = state.view.value;
  const fleet = current.fleet;
  if (!fleet) return null;
  const { shown, overflow } = visibleChips(fleet.peers);
  // A peer view of a node past the chip budget swaps it into the last slot, so
  // the node on screen always has its chip highlighted.
  const remotePeer = remote && !shown.some((p) => p.id === remote) ? fleet.peers.find((p) => p.id === remote) : null;
  if (remotePeer && shown.length) shown[shown.length - 1] = remotePeer;
  // The shown node's chip is current while its per-node view is on screen.
  const isCurrent = (id) => !current.topLevel && (remote ? remote === id : id === fleet.self.id);
  const openNode = (id) => {
    if (id === (remote || fleet.self.id)) navigate(state.lastLocalTab());
    else switchNode(id === fleet.self.id ? '' : id);
  };
  const self = fleet.self;
  return html`<span class="node-chips" role="group" aria-label="Nodes">
    <button type="button" class=${`node-chip local${isCurrent(self.id) ? ' active' : ''}`} style=${`--nc:${self.color}`}
      aria-current=${isCurrent(self.id) ? 'page' : undefined} aria-label=${`${self.name} (this node)`}
      title=${`${self.name}: this node · Alt+1`} onClick=${() => openNode(self.id)}>
      <span class="node-chip-home" aria-hidden="true">⌂</span><span class="node-chip-name">${self.name}</span><span class="node-chip-dot"></span>
    </button>
    ${shown.map((peer, i) => html`<button key=${peer.id} type="button" class=${`node-chip${peer.online ? '' : ' offline'}${isCurrent(peer.id) ? ' active' : ''}`} style=${`--nc:${peer.color}`}
      aria-current=${isCurrent(peer.id) ? 'page' : undefined} aria-label=${`${peer.name}, ${presenceLabel(peer)}, ${peer.level} peer`}
      title=${`${peer.name}: ${presenceLabel(peer)} · ${peer.level} peer · open its dashboard${i < 8 ? ` · Alt+${i + 2}` : ''}`} onClick=${() => openNode(peer.id)}>
      <span class="node-chip-name">${peer.name}</span><span class="node-chip-dot"></span>
    </button>`)}
    ${overflow > 0 && html`<button type="button" class="node-chip more" aria-label=${`${overflow} more nodes on the map`} title=${`${overflow} more nodes: open the map`} onClick=${() => navigate('map')}>+${overflow}</button>`}
    <span class="node-chips-sep" aria-hidden="true"></span>
  </span>`;
}

// TopLevelBar replaces the tab strip while a top-level, multi-node view (the
// map or the merged Groups view) is active, and switches between the two. CSS
// hides the per-node tabs in that mode; the row keeps the same height so
// nothing below moves.
export function TopLevelBar({ state, navigate = defaultNavigate }) {
  const current = state.view.value;
  if (!current.topLevel || !current.fleet) return null;
  const n = current.fleet.peers.length + 1;
  const hub = current.fleet.hub.state;
  const seg = (tab, on, label) => html`<button type="button" class=${`skynet-seg-btn${on ? ' on' : ''}`} aria-current=${on ? 'page' : undefined} onClick=${() => { if (!on) navigate(tab); }}>${label}</button>`;
  return html`<span class="skynet-toplevel-bar">
    <span class="skynet-seg" role="group" aria-label="Skynet views">${seg('map', current.mapActive, html`<${MapGlyph} /> Map`)}${seg('fleet', current.fleetActive, 'Groups · all nodes')}${seg('fleet-admin', current.adminActive, '⚙ Fleet')}</span>
    <span class="skynet-toplevel-note">Skynet · ${n} nodes · hub ${hub} · pick ⌂ ${current.fleet.self.name} to return to its dashboard</span>
  </span>`;
}

function Meter({ label, value }) {
  return html`<span class="skynet-meter" title=${`${label} ${value}%`}>${label}<span class="skynet-meter-bar"><b style=${`width:${value}%`}></b></span>${value}%</span>`;
}

function CardRows({ card, node, onUpdate }) {
  const f = card.failure;
  if (card.sharedAgents == null) {
    let text = 'Loading summary…';
    if (f?.kind === 'unavailable') text = 'This node does not offer a map summary yet.';
    else if (f?.kind === 'not_trusted') text = 'Not trusted any more.';
    else if (f?.kind === 'offline' || f?.kind === 'timeout' || card.presence === 'offline') text = card.lastSeen ? `Unreachable. Last seen ${new Date(card.lastSeen).toLocaleString()}.` : 'Unreachable.';
    else if (f) text = `Summary failed: ${f.message || f.kind}`;
    return html`<div class="skynet-card-empty">${text}</div>`;
  }
  const r = card.resources;
  return html`<dl class="skynet-card-body">
    <dt>Agents</dt><dd>${card.onlineAgents ?? '?'} of ${card.sharedAgents} online · ${card.sharedGroups} group${card.sharedGroups === 1 ? '' : 's'}${node.local ? '' : html` <span class="muted">shared</span>`}</dd>
    <dt>Attention</dt><dd>${card.waiting ? html`<span class="skynet-attn">❓ ${card.waiting} waiting for input</span>` : html`<span class="muted">none</span>`}</dd>
    <dt>Resources</dt><dd>${r ? html`${r.cpu != null && html`<${Meter} label="cpu" value=${r.cpu} />`}${r.mem != null && html`<${Meter} label="mem" value=${r.mem} />`}${r.disk != null && html`<${Meter} label="disk" value=${r.disk} />`}` : html`<span class="muted">not shared</span>`}</dd>
    ${card.version && html`<dt>tclaude</dt><dd>${card.version}${card.updateAvailable ? html` <span class="skynet-update" title=${`tclaude ${card.latestVersion} is available`}>↑ ${card.latestVersion}</span>` : ''}${onUpdate && html` <button type="button" class="skynet-link" data-skynet="node-update" onClick=${onUpdate}>${card.updateAvailable ? 'update…' : 'manage…'}</button>`}</dd>`}
    ${card.health && html`<dt>Health</dt><dd class=${`skynet-health ${card.health}`}>${card.health === 'current' ? '✓ current' : card.health}</dd>`}
    ${card.omitted.length > 0 && html`<dt>Not shared</dt><dd class="muted">${card.omitted.join(', ')}</dd>`}
  </dl>`;
}

function NodeCard({ node, card, focused, onOpen, onUpdate, cardRef, shown }) {
  const kind = node.local ? 'this node' : node.level === 'unrestricted' ? '⚠ unrestricted peer' : 'restricted peer';
  const presence = card.presence === 'online' ? (node.local ? 'online' : 'online') : card.presence === 'offline' ? 'offline' : card.presence === 'error' ? 'error' : '…';
  return html`<article ref=${cardRef} data-node-id=${node.id} class=${`skynet-card${node.local ? ' local' : ''}${card.stale || card.presence === 'offline' ? ' stale' : ''}${focused ? ' focused' : ''}`}
    style=${`--nc:${node.color}`} aria-label=${`${node.name} node`}>
    <div class="skynet-card-head"><span class="skynet-card-name">${node.local ? '⌂ ' : ''}${node.name}</span>
      <span class=${`skynet-card-kind${node.level === 'unrestricted' ? ' unrestricted' : ''}`}>${kind}</span>
      <span class=${`skynet-card-presence ${card.presence}`}><i aria-hidden="true"></i>${presence}${card.stale && card.ageMs != null ? html` <span class="muted">· data ${fmtAge(card.ageMs)} old</span>` : ''}</span></div>
    <${CardRows} card=${card} node=${node} onUpdate=${onUpdate} />
    ${onOpen && html`<div class="skynet-card-foot"><button type="button" class=${shown ? 'primary' : ''} onClick=${onOpen}>${shown ? 'Back to its dashboard' : 'Open dashboard'}</button></div>`}
  </article>`;
}

// edgePath draws a gentle horizontal S-curve between two anchor points.
function edgePath(a, b) {
  const dx = Math.max(40, (b.x - a.x) / 2);
  return `M${a.x} ${a.y} C${a.x + dx} ${a.y} ${b.x - dx} ${b.y} ${b.x} ${b.y}`;
}

export function measureEdges(mapEl) {
  if (!mapEl) return [];
  const box = mapEl.getBoundingClientRect();
  const local = mapEl.querySelector('.skynet-card.local');
  if (!local) return [];
  const lr = local.getBoundingClientRect();
  const from = { x: lr.right - box.left, y: lr.top - box.top + lr.height / 2 };
  return [...mapEl.querySelectorAll('.skynet-card:not(.local)')].map((el) => {
    const r = el.getBoundingClientRect();
    const to = { x: r.left - box.left, y: r.top - box.top + r.height / 2 };
    return { id: el.dataset.nodeId, d: edgePath(from, to), mid: { x: (from.x + to.x) / 2, y: (from.y + to.y) / 2 } };
  });
}

export function SkynetMap({ state, actions, navigate = defaultNavigate, timers = globalThis, now = () => Date.now(), remote = remoteNodeID(), switchNode = defaultSwitchNode, confirm = shellConfirm, toast = shellToast, updateActions }) {
  const current = state.view.value;
  const fleet = current.fleet;
  const mapRef = useRef(null);
  const [edges, setEdges] = useState([]);
  const [updating, setUpdating] = useState(null);
  const fleetKey = fleet ? [fleet.self.id, ...fleet.peers.map((p) => p.id)].join(',') : '';

  // Never strand the operator on an empty map: once the node list has loaded
  // without linked nodes (a /map deep link on an unlinked node, or the last
  // peer untrusted while the map is open), return to the per-node tab. Fleet
  // administration stays: it is where the first peer gets trusted.
  const multiNode = current.mapActive || current.fleetActive;
  useEffect(() => {
    if (multiNode && current.statusLoaded && !fleet) navigate(state.lastLocalTab());
  }, [multiNode, current.statusLoaded, fleetKey]);

  // [ / ] and ←/→ cycling have no per-node tab to move to while the map hides
  // them; refresh.js hands the keystroke here and the map steps back out.
  useEffect(() => {
    const leave = () => { if (state.view.value.topLevel) navigate(state.lastLocalTab()); };
    document.addEventListener('tclaude:leave-map', leave);
    return () => document.removeEventListener('tclaude:leave-map', leave);
  }, []);

  // Poll each node's summary only while the map is on screen: staggered first
  // reads, a relaxed jittered interval, failure backoff, and never two reads in
  // flight for one node.
  useEffect(() => {
    if (!current.mapActive || !fleet) return undefined;
    const nodes = [fleet.self, ...fleet.peers];
    let disposed = false;
    const pending = new Map();
    const inflight = new Set();
    const schedule = (node, delay) => { pending.set(node.id, timers.setTimeout(() => tick(node), delay)); };
    async function tick(node) {
      if (disposed || inflight.has(node.id)) return;
      if (globalThis.document?.hidden) { schedule(node, pollDelay()); return; }
      inflight.add(node.id);
      try { await actions.loadSummary(node); } finally { inflight.delete(node.id); }
      if (!disposed) schedule(node, pollDelay({ failures: state.entry(node.id)?.failures || 0 }));
    }
    nodes.forEach((node, i) => schedule(node, staggerOffset(i, nodes.length)));
    return () => { disposed = true; pending.forEach((t) => timers.clearTimeout(t)); };
  }, [current.mapActive, fleetKey]);

  // Edges follow the measured card positions; re-measure on resize and when
  // card contents change height.
  useLayoutEffect(() => {
    if (!current.mapActive || !fleet) return undefined;
    const update = () => setEdges(measureEdges(mapRef.current));
    update();
    if (typeof ResizeObserver !== 'function' || !mapRef.current) return undefined;
    const ro = new ResizeObserver(update);
    ro.observe(mapRef.current);
    return () => ro.disconnect();
  }, [current.mapActive, fleetKey, current.summaries]);

  if (!fleet) return html`<div class="empty">No linked nodes. Link this node to others with <code>tclaude federation</code> (see docs/federation.md).</div>`;
  const t = now();
  const peerById = new Map(fleet.peers.map((p) => [p.id, p]));
  const card = (node) => cardView(node, current.summaries[node.id], t);
  // Each card opens its node's per-node view; the node already shown returns
  // to the tab the operator left for the map.
  const open = (node) => () => {
    if (node.id === (remote || fleet.self.id)) navigate(state.lastLocalTab());
    else switchNode(node.local ? '' : node.id);
  };
  return html`<div class="skynet-map" ref=${mapRef}>
    <svg class="skynet-edges" aria-hidden="true">
      ${edges.map((e) => { const p = peerById.get(e.id); const live = p && card(p).presence === 'online'; return html`<path key=${e.id} d=${e.d} class=${`skynet-edge${live ? ' live' : ' stale'}`} />`; })}
    </svg>
    ${edges.map((e) => { const p = peerById.get(e.id); return p && html`<span key=${`l-${e.id}`} class="skynet-edge-label" style=${`left:${e.mid.x}px;top:${e.mid.y}px`}>${p.level === 'unrestricted' ? '⚠ unrestricted' : 'restricted'} link</span>`; })}
    <div class="skynet-map-local"><${NodeCard} node=${fleet.self} card=${card(fleet.self)} focused=${current.focused === fleet.self.id} shown=${!remote} onOpen=${open(fleet.self)} onUpdate=${() => setUpdating(fleet.self)} /></div>
    <div class=${`skynet-map-peers${fleet.peers.length > 4 ? ' wide' : ''}`}>${fleet.peers.map((peer) => html`<${NodeCard} key=${peer.id} node=${peer} card=${card(peer)} focused=${current.focused === peer.id} shown=${remote === peer.id} onOpen=${open(peer)} onUpdate=${() => setUpdating(peer)} />`)}</div>
    ${updating && html`<${NodeUpdateDialog} node=${{ ...updating, label: updating.name }} confirm=${confirm} toast=${toast} timers=${timers} actions=${updateActions}
      onClose=${() => { setUpdating(null); actions.loadSummary(updating); }} />`}
    <div class="skynet-legend" aria-hidden="true"><span><i class="live"></i>linked, reachable</span><span><i class="stale"></i>unreachable or stale</span><span>Counts show only what each node shares with you.</span></div>
  </div>`;
}

// switchKeyTarget reports whether an Alt+digit press may switch nodes: never
// while typing, and never while a web terminal has focus (the shell owns the
// keyboard there, Alt+digit included).
export function switchKeyAllowed(event) {
  if (!event.altKey || event.ctrlKey || event.metaKey || event.shiftKey || event.repeat) return false;
  if (!/^Digit[1-9]$/.test(event.code || '')) return false;
  const t = event.target;
  if (t?.closest?.('.xterm, .terminal-view, [data-terminal], input, textarea, select, [contenteditable=""], [contenteditable="true"]')) return false;
  return true;
}

// NodeSwitchKeys binds Alt+1 (this node) and Alt+2..9 (peers in chip order).
function NodeSwitchKeys({ state, navigate = defaultNavigate, remote = remoteNodeID(), switchNode = defaultSwitchNode }) {
  useEffect(() => {
    const onKey = (event) => {
      if (!switchKeyAllowed(event)) return;
      const fleet = state.fleet.value;
      const node = switchOrder(fleet)[Number(event.code.slice(5)) - 1];
      if (!node) return;
      event.preventDefault();
      if (node.id === (remote || fleet.self.id)) { if (state.view.value.topLevel) navigate(state.lastLocalTab()); return; }
      switchNode(node.local ? '' : node.id);
    };
    document.addEventListener('keydown', onKey);
    return () => document.removeEventListener('keydown', onKey);
  }, []);
  return null;
}

// RemoteMarker marks a peer's per-node view without moving any component: the
// node's name replaces the dashboard title (CSS hides "tclaude"), a thin line
// in the node's colour runs along the top edge, and a "peer view" pill lists
// what the peer shares and whether the data is live.
export function RemoteMarker({ state, remote = remoteNodeID(), snapshot = dashboardState.snapshot, now = () => Date.now(), switchNode = defaultSwitchNode }) {
  const [health, setHealth] = useState(globalThis.__tclaudeRemoteNode?.health ? { ...globalThis.__tclaudeRemoteNode.health } : null);
  const [open, setOpen] = useState(false);
  const [asking, setAsking] = useState('');
  const rootRef = useRef(null);
  const fleet = state.view.value.fleet;
  const peer = fleet?.peers.find((p) => p.id === remote) || null;
  const color = peer?.color || nodeColor(remote);
  useEffect(() => {
    if (!remote) return undefined;
    const win = document.defaultView || globalThis;
    const onHealth = (event) => setHealth(event.detail);
    win.addEventListener('tclaude:remote-health', onHealth);
    // A poll that settled between the first render and this effect.
    if (globalThis.__tclaudeRemoteNode?.health) setHealth({ ...globalThis.__tclaudeRemoteNode.health });
    return () => win.removeEventListener('tclaude:remote-health', onHealth);
  }, [remote]);
  useEffect(() => {
    if (!remote) return;
    document.documentElement.style.setProperty('--remote-node-color', color);
    // peer-view-limits.js names the node in its hints.
    document.documentElement.dataset.remoteNodeName = peer?.name || remote.slice(0, 13);
  }, [remote, color, peer?.name]);
  useEffect(() => {
    if (!open) return undefined;
    const onDown = (event) => { if (!rootRef.current?.contains(event.target)) setOpen(false); };
    const onKey = (event) => { if (event.key === 'Escape') setOpen(false); };
    document.addEventListener('mousedown', onDown);
    document.addEventListener('keydown', onKey);
    return () => { document.removeEventListener('mousedown', onDown); document.removeEventListener('keydown', onKey); };
  }, [open]);
  if (!remote) return null;
  const name = peer?.name || remote.slice(0, 13);
  const hv = remoteHealthView(health, now());
  const pv = peerViewSummary(snapshot.value?.peer_view);
  const level = peer?.level || '';
  return html`<span class="remote-node-marker" ref=${rootRef} style=${`--nc:${color}`}>
    <span class="remote-node-title" title=${`Peer view of ${name} (${remote})`}>${name}</span>
    <button type="button" class=${`remote-node-pill ${hv.state}`} aria-haspopup="dialog" aria-expanded=${open ? 'true' : 'false'}
      onClick=${() => setOpen(!open)} title="What this peer shares with you">
      peer view${hv.label ? html` <span class="remote-node-health">· ${hv.label}</span>` : ''}
    </button>
    ${open && html`<div class="remote-node-pop" role="dialog" aria-label=${`Peer view of ${name}`}>
      <div class="rnp-head">Peer view of <b>${name}</b>${level ? html` · ${level === 'unrestricted' ? '⚠ unrestricted' : 'restricted'} peer` : ''}</div>
      <div class="rnp-row">${hv.state === 'live' ? 'Live: the peer answers through this node.' : `The peer is ${hv.label}. The data on screen is what it last shared.`}</div>
      ${pv && pv.included.length > 0 && html`<div class="rnp-row"><span class="rnp-k">Shared</span> ${pv.included.join(', ')}</div>`}
      ${pv && pv.omitted.length > 0 && html`<div class="rnp-row"><span class="rnp-k">Not shared</span> ${pv.omitted.map((o, i) => html`${i ? ', ' : ''}<span title=${o.requires ? `needs ${o.requires}` : ''}>${o.feature}</span>${requestable(o) ? html` <button type="button" class="rnp-ask" data-perm=${o.requires} title=${`Ask ${name}'s operator for ${o.requires}`} onClick=${() => { setAsking(o.requires); setOpen(false); }}>request…</button>` : ''}`)}</div>`}
      ${!pv && html`<div class="rnp-row muted">What the peer shares shows once it answers.</div>`}
      <div class="rnp-foot"><button type="button" onClick=${() => switchNode('')}>⌂ Back to ${fleet?.self.name || 'this node'}</button></div>
    </div>`}
    ${asking && html`<${RequestAccessDialog} node=${name} perm=${asking} groups=${(snapshot.value?.groups || []).filter((g) => g?.id && g.name)} onClose=${() => setAsking('')} />`}
  </span>`;
}

// StatusPoller keeps the chip row's node list fresh at a relaxed cadence and
// pauses while the page is hidden. It renders nothing.
function StatusPoller({ actions, timers = globalThis }) {
  useEffect(() => {
    let disposed = false; let timer = null;
    const loop = async () => {
      if (disposed) return;
      const fleet = globalThis.document?.hidden ? undefined : await actions.loadStatus();
      // A node without linked peers (or an older daemon without the route)
      // rechecks rarely; a linked one keeps the chips fresh.
      if (!disposed) timer = timers.setTimeout(loop, pollDelay({ base: fleet === null ? STATUS_POLL_MS * 4 : STATUS_POLL_MS }));
    };
    loop();
    return () => { disposed = true; timers.clearTimeout(timer); };
  }, []);
  return null;
}

export function mountSkynetIsland({ chipsHost, barHost, mapHost, remoteHost, state, actions, registerCleanup, navigate, timers }) {
  render(html`<${Fragment}><${StatusPoller} actions=${actions} timers=${timers} /><${NodeSwitchKeys} state=${state} navigate=${navigate} /><${NodeChips} state=${state} navigate=${navigate} /></${Fragment}>`, chipsHost);
  // A peer view also hosts the peer action dialog (stop/retire/clone/move/
  // teleport/spawn through the peer's action routes).
  if (remoteHost) render(html`<${RemoteMarker} state=${state} /><${PeerActionHost} snapshot=${dashboardState.snapshot} />`, remoteHost);
  render(html`<${TopLevelBar} state=${state} navigate=${navigate} />`, barHost);
  render(html`<${SkynetMap} state=${state} actions=${actions} navigate=${navigate} timers=${timers} />`, mapHost);
  registerCleanup(() => { render(null, chipsHost); render(null, barHost); render(null, mapHost); if (remoteHost) render(null, remoteHost); });
}
