import { Fragment, h, render } from 'preact';
import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { STATUS_POLL_MS, cardView, fmtAge, pollDelay, staggerOffset, visibleChips } from './skynet-model.js';

const html = htm.bind(h);

// navigateTab routes through the real nav anchors so tab activation, history
// and per-tab side effects stay owned by refresh.js / nav-history.js.
function defaultNavigate(tab) {
  document.querySelector(`nav [data-tab="${tab}"]`)?.click();
}

const MapGlyph = () => html`<svg width="14" height="14" viewBox="0 0 14 14" fill="none" stroke="currentColor" stroke-width="1.4" aria-hidden="true"><circle cx="3" cy="3.5" r="2"/><circle cx="11" cy="3" r="2"/><circle cx="7" cy="11" r="2"/><path d="M4.8 4.3 5.9 9.3M9.4 4.3 7.9 9.3M5 3.4 9 3.1"/></svg>`;

function presenceLabel(node) {
  if (node.local) return 'this node';
  return node.online ? 'online' : 'offline';
}

// NodeChips is the tab-bar node row: this node, then trusted peers, then an
// overflow chip. The map entry is the static nav[data-tab="map"] anchor right
// after this host, so tab routing keeps one owner.
export function NodeChips({ state, navigate = defaultNavigate }) {
  const current = state.view.value;
  const fleet = current.fleet;
  if (!fleet) return null;
  const { shown, overflow } = visibleChips(fleet.peers);
  const openPeer = (peer) => { state.setFocused(peer.id); navigate('map'); };
  const self = fleet.self;
  return html`<span class="node-chips" role="group" aria-label="Nodes">
    <button type="button" class=${`node-chip local${current.mapActive ? '' : ' active'}`} style=${`--nc:${self.color}`}
      aria-current=${current.mapActive ? undefined : 'page'} aria-label=${`${self.name} (this node)`}
      title=${`${self.name}: this node`} onClick=${() => navigate(state.lastLocalTab())}>
      <span class="node-chip-home" aria-hidden="true">⌂</span><span class="node-chip-name">${self.name}</span><span class="node-chip-dot"></span>
    </button>
    ${shown.map((peer) => html`<button key=${peer.id} type="button" class=${`node-chip${peer.online ? '' : ' offline'}`} style=${`--nc:${peer.color}`}
      aria-label=${`${peer.name}, ${presenceLabel(peer)}, ${peer.level} peer`}
      title=${`${peer.name}: ${presenceLabel(peer)} · ${peer.level} peer · show on the map`} onClick=${() => openPeer(peer)}>
      <span class="node-chip-name">${peer.name}</span><span class="node-chip-dot"></span>
    </button>`)}
    ${overflow > 0 && html`<button type="button" class="node-chip more" aria-label=${`${overflow} more nodes on the map`} title=${`${overflow} more nodes: open the map`} onClick=${() => navigate('map')}>+${overflow}</button>`}
    <span class="node-chips-sep" aria-hidden="true"></span>
  </span>`;
}

// TopLevelBar replaces the tab strip while the map (a top-level, multi-node
// view) is active. CSS hides the per-node tabs in that mode; the row keeps the
// same height so nothing below moves.
export function TopLevelBar({ state }) {
  const current = state.view.value;
  if (!current.mapActive || !current.fleet) return null;
  const n = current.fleet.peers.length + 1;
  const hub = current.fleet.hub.state;
  return html`<span class="skynet-toplevel-bar">
    <span class="skynet-seg" role="group" aria-label="Skynet views"><span class="skynet-seg-btn on" aria-current="page"><${MapGlyph} /> Map</span></span>
    <span class="skynet-toplevel-note">Skynet · ${n} nodes · hub ${hub} · pick ⌂ ${current.fleet.self.name} to return to its dashboard</span>
  </span>`;
}

function Meter({ label, value }) {
  return html`<span class="skynet-meter" title=${`${label} ${value}%`}>${label}<span class="skynet-meter-bar"><b style=${`width:${value}%`}></b></span>${value}%</span>`;
}

function CardRows({ card, node }) {
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
    ${card.health && html`<dt>Health</dt><dd class=${`skynet-health ${card.health}`}>${card.health === 'current' ? '✓ current' : card.health}</dd>`}
    ${card.omitted.length > 0 && html`<dt>Not shared</dt><dd class="muted">${card.omitted.join(', ')}</dd>`}
  </dl>`;
}

function NodeCard({ node, card, focused, onOpen, cardRef }) {
  const kind = node.local ? 'this node' : node.level === 'unrestricted' ? '⚠ unrestricted peer' : 'restricted peer';
  const presence = card.presence === 'online' ? (node.local ? 'online' : 'online') : card.presence === 'offline' ? 'offline' : card.presence === 'error' ? 'error' : '…';
  return html`<article ref=${cardRef} data-node-id=${node.id} class=${`skynet-card${node.local ? ' local' : ''}${card.stale || card.presence === 'offline' ? ' stale' : ''}${focused ? ' focused' : ''}`}
    style=${`--nc:${node.color}`} aria-label=${`${node.name} node`}>
    <div class="skynet-card-head"><span class="skynet-card-name">${node.local ? '⌂ ' : ''}${node.name}</span>
      <span class=${`skynet-card-kind${node.level === 'unrestricted' ? ' unrestricted' : ''}`}>${kind}</span>
      <span class=${`skynet-card-presence ${card.presence}`}><i aria-hidden="true"></i>${presence}${card.stale && card.ageMs != null ? html` <span class="muted">· data ${fmtAge(card.ageMs)} old</span>` : ''}</span></div>
    <${CardRows} card=${card} node=${node} />
    ${node.local && html`<div class="skynet-card-foot"><button type="button" class="primary" onClick=${onOpen}>Open dashboard</button></div>`}
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

export function SkynetMap({ state, actions, navigate = defaultNavigate, timers = globalThis, now = () => Date.now() }) {
  const current = state.view.value;
  const fleet = current.fleet;
  const mapRef = useRef(null);
  const [edges, setEdges] = useState([]);
  const fleetKey = fleet ? [fleet.self.id, ...fleet.peers.map((p) => p.id)].join(',') : '';

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
  return html`<div class="skynet-map" ref=${mapRef}>
    <svg class="skynet-edges" aria-hidden="true">
      ${edges.map((e) => { const p = peerById.get(e.id); const live = p && card(p).presence === 'online'; return html`<path key=${e.id} d=${e.d} class=${`skynet-edge${live ? ' live' : ' stale'}`} />`; })}
    </svg>
    ${edges.map((e) => { const p = peerById.get(e.id); return p && html`<span key=${`l-${e.id}`} class="skynet-edge-label" style=${`left:${e.mid.x}px;top:${e.mid.y}px`}>${p.level === 'unrestricted' ? '⚠ unrestricted' : 'restricted'} link</span>`; })}
    <div class="skynet-map-local"><${NodeCard} node=${fleet.self} card=${card(fleet.self)} focused=${current.focused === fleet.self.id} onOpen=${() => navigate(state.lastLocalTab())} /></div>
    <div class=${`skynet-map-peers${fleet.peers.length > 4 ? ' wide' : ''}`}>${fleet.peers.map((peer) => html`<${NodeCard} key=${peer.id} node=${peer} card=${card(peer)} focused=${current.focused === peer.id} />`)}</div>
    <div class="skynet-legend" aria-hidden="true"><span><i class="live"></i>linked, reachable</span><span><i class="stale"></i>unreachable or stale</span><span>Counts show only what each node shares with you.</span></div>
  </div>`;
}

// StatusPoller keeps the chip row's node list fresh at a relaxed cadence and
// pauses while the page is hidden. It renders nothing.
function StatusPoller({ actions, timers = globalThis }) {
  useEffect(() => {
    let disposed = false; let timer = null;
    const loop = async () => {
      if (disposed) return;
      if (!globalThis.document?.hidden) await actions.loadStatus();
      if (!disposed) timer = timers.setTimeout(loop, pollDelay({ base: STATUS_POLL_MS }));
    };
    loop();
    return () => { disposed = true; timers.clearTimeout(timer); };
  }, []);
  return null;
}

export function mountSkynetIsland({ chipsHost, barHost, mapHost, state, actions, registerCleanup, navigate, timers }) {
  render(html`<${Fragment}><${StatusPoller} actions=${actions} timers=${timers} /><${NodeChips} state=${state} navigate=${navigate} /></${Fragment}>`, chipsHost);
  render(html`<${TopLevelBar} state=${state} />`, barHost);
  render(html`<${SkynetMap} state=${state} actions=${actions} navigate=${navigate} timers=${timers} />`, mapHost);
  registerCleanup(() => { render(null, chipsHost); render(null, barHost); render(null, mapHost); });
}
