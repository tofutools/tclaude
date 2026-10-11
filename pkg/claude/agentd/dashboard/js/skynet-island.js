import { Fragment, h, render } from 'preact';
import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { NodeUpdateDialog } from './node-update.js';
import { RequestAccessDialog } from './peer-access.js';
import { RemoteSessionsDialog } from './remote-terminal.js';
import { PeerActionHost } from './peer-action.js';
import { shellConfirm, shellToast } from './shell-state.js';
import { STATUS_POLL_MS, cardView, fmtAge, nodeColor, nodeHref, peerViewSummary, pollDelay, remoteHealthView, remoteNodeID, staggerOffset, switchOrder } from './skynet-model.js';
import { FUSED_TABS, filterNodes, fleetNodes, fusedParam, isTicked, selectorKind, tickedNodes, toggleNode, withFused } from './skynet-scope.js';
import { dashboardState } from './snapshot-store.js';
import { TOP_LEVEL_TABS } from './skynet-state.js';

const html = htm.bind(h);

// SAFE_SLUG admits a permission slug from the peer's metadata into a request.
const SAFE_SLUG = /^[a-z][a-z0-9._-]{0,63}$/;
// Features a peer can never open through an access request: local-only
// transports (the dashboard shell, terminals, inline spawn) and the access
// requests themselves. The daemon refuses these as not requestable.
const UNREQUESTABLE = new Set(['local_dashboard', 'terminals', 'spawn.inline', 'permissions.requests']);
// Newer daemons say so per entry (requestable); older ones fall back to the list.
const requestable = (o) => !!o.requires && SAFE_SLUG.test(o.requires)
  && (typeof o.requestable === 'boolean' ? o.requestable : !UNREQUESTABLE.has(o.feature) && o.requires !== 'local_only' && o.requires !== 'peer_access');

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

// writeScopeURL keeps ?nodes= in the address bar in step with the fused set
// without a page load; nav-history.js carries it across tab pushes. Callers
// that navigate right after (switchNode) need it written synchronously.
export function writeScopeURL(fused, win = globalThis) {
  const loc = win.location;
  if (!loc || !win.history?.replaceState) return;
  const next = loc.pathname + withFused(loc.search, fused) + (loc.hash || '');
  if (next !== loc.pathname + loc.search + (loc.hash || '')) win.history.replaceState(win.history.state, '', next);
}

// pickNode shows one node on its own: it leaves any fused view, then shows
// that node's per-node view (a page load for another node, as before).
function pickNode(state, id, { navigate, remote, switchNode }) {
  const fleet = state.fleet.value;
  if (!fleet) return;
  const wasFused = !!state.fused.value;
  state.setFused(null);
  writeScopeURL(null);
  if (id === (remote || fleet.self.id)) {
    if (state.view.value.topLevel || !wasFused) navigate(state.lastLocalTab());
  } else switchNode(id === fleet.self.id ? '' : id);
}

// usePopover closes a popover on a click outside it or Escape. The popover is
// fixed under its button (the chip host clips), right-aligned to it.
function usePopover(rootRef) {
  const [open, setOpenRaw] = useState(false);
  const [pos, setPos] = useState(null);
  const setOpen = (next) => {
    const r = next && rootRef.current?.getBoundingClientRect?.();
    if (r) setPos({ top: Math.round(r.bottom + 6), right: Math.max(8, Math.round((globalThis.innerWidth || r.right) - r.right)) });
    setOpenRaw(next);
  };
  useEffect(() => {
    if (!open) return undefined;
    const onDown = (event) => { if (!rootRef.current?.contains(event.target)) setOpen(false); };
    const onKey = (event) => { if (event.key === 'Escape') { event.stopPropagation(); setOpen(false); } };
    document.addEventListener('mousedown', onDown);
    document.addEventListener('keydown', onKey, true);
    return () => { document.removeEventListener('mousedown', onDown); document.removeEventListener('keydown', onKey, true); };
  }, [open]);
  return [open, setOpen, pos ? `top:${pos.top}px;right:${pos.right}px` : ''];
}

function nodeMeta(node) {
  if (node.local) return 'this node';
  if (node.online) return 'connected';
  return node.lastSeen ? `offline · seen ${fmtAge(Math.max(0, Date.now() - new Date(node.lastSeen).getTime()))} ago` : 'offline';
}

// NodeDropdown stands in for the node buttons past BUTTON_LIMIT nodes: one
// button naming the node on screen, opening a type-to-filter list with this
// node pinned first and a status dot per node.
function NodeDropdown({ state, fleet, pageNode, fused, onPick }) {
  const rootRef = useRef(null);
  const inputRef = useRef(null);
  const [open, setOpen, popStyle] = usePopover(rootRef);
  const [query, setQuery] = useState('');
  const [hl, setHl] = useState(0);
  useEffect(() => { if (open) { setQuery(''); setHl(0); inputRef.current?.focus(); } }, [open]);
  const shown = fleetNodes(fleet).find((n) => n.id === pageNode) || fleet.self;
  const list = filterNodes(fleet, query);
  const pick = (node) => { setOpen(false); onPick(node.id); };
  const onKey = (event) => {
    if (event.key === 'ArrowDown') { event.preventDefault(); setHl((i) => Math.min(list.length - 1, i + 1)); }
    else if (event.key === 'ArrowUp') { event.preventDefault(); setHl((i) => Math.max(0, i - 1)); }
    else if (event.key === 'Enter' && list[hl]) { event.preventDefault(); pick(list[hl]); }
  };
  const row = (node, i) => html`<button type="button" key=${node.id} role="option" aria-selected=${i === hl ? 'true' : 'false'}
    class=${`node-pop-row${node.online || node.local ? '' : ' offline'}${i === hl ? ' hl' : ''}${!fused && node.id === pageNode ? ' sel' : ''}`}
    style=${`--nc:${node.color}`} onMouseEnter=${() => setHl(i)} onClick=${() => pick(node)}>
    <span class="node-pop-dot" aria-hidden="true"></span><span class="node-pop-name">${node.local ? '⌂ ' : ''}${node.name}</span>
    <span class="node-pop-meta">${nodeMeta(node)}${!fused && node.id === pageNode ? ' · shown' : ''}</span>
  </button>`;
  const self = list[0]?.local ? list[0] : null;
  return html`<span class="node-pick" ref=${rootRef}>
    <button type="button" class=${`node-chip node-pick-btn${fused ? (isTicked(fused, shown.id) ? ' ticked' : '') : ' active'}${shown.local || shown.online ? '' : ' offline'}`} style=${`--nc:${shown.color}`}
      aria-haspopup="listbox" aria-expanded=${open ? 'true' : 'false'} aria-label=${`Node: ${shown.name}. Choose a node`} title=${`${shown.name}: pick a node to show`}
      onClick=${() => setOpen(!open)}>
      ${shown.local && html`<span class="node-chip-home" aria-hidden="true">⌂</span>`}<span class="node-chip-name">${shown.name}</span><span class="node-pick-caret" aria-hidden="true">▾</span><span class="node-chip-dot"></span>
    </button>
    ${open && html`<div class="node-pop" role="dialog" aria-label="Choose a node" style=${popStyle}>
      <label class="node-pop-filter"><input ref=${inputRef} type="text" value=${query} placeholder="type to filter" aria-label="Filter nodes"
        onInput=${(e) => { setQuery(e.currentTarget.value); setHl(0); }} onKeyDown=${onKey} /></label>
      <div role="listbox" aria-label="Nodes">
        ${list.map((node, i) => html`${row(node, i)}${self && i === 0 && list.length > 1 ? html`<hr class="node-pop-sep" />` : ''}`)}
      </div>
      <div class="node-pop-foot">${list.length} of ${fleetNodes(fleet).length} · ↑↓ ↵ · Alt+1 this node</div>
    </div>`}
  </span>`;
}

// FusedDropdown is the fused-view control: a checkbox per node, defaulting to
// all. Ticking two or more shows them together in Groups and Terminals;
// clicking a node's name (or its "only") shows that node alone.
function FusedDropdown({ state, fleet, fused, pageNode, onPick, onFuse }) {
  const rootRef = useRef(null);
  const [open, setOpen, popStyle] = usePopover(rootRef);
  const [query, setQuery] = useState('');
  const nodes = fleetNodes(fleet);
  const ticked = fused ? tickedNodes(fleet, fused) : nodes;
  const list = selectorKind(fleet) === 'dropdown' ? filterNodes(fleet, query) : nodes;
  const toggle = (id) => {
    const next = toggleNode(fleet, fused || 'all', id);
    if (next.only !== undefined) { setOpen(false); onPick(next.only); } else onFuse(next);
  };
  const onButton = () => {
    if (!fused) { onFuse('all'); setOpen(true); } else setOpen(!open);
  };
  const primary = nodes.find((n) => n.id === pageNode) || fleet.self;
  return html`<span class="node-pick" ref=${rootRef}>
    <button type="button" class=${`node-chip node-fuse${fused ? ' active' : ''}`} aria-haspopup="dialog" aria-expanded=${open ? 'true' : 'false'}
      aria-label=${fused ? `Fused view of ${ticked.length} of ${nodes.length} nodes. Change the nodes` : 'Show several nodes together'}
      title=${fused ? `Groups and Terminals show ${ticked.length} of ${nodes.length} nodes` : 'Show several nodes together in Groups and Terminals'} onClick=${onButton}>
      <span class="node-fuse-stack" aria-hidden="true">${nodes.slice(0, 4).map((n) => html`<b key=${n.id} class=${isTicked(fused || 'all', n.id) ? '' : 'off'} style=${`--c:${n.color}`}></b>`)}</span>
      <span class="node-chip-name">${fused ? `${ticked.length}/${nodes.length}` : 'nodes'}</span><span class="node-pick-caret" aria-hidden="true">▾</span>
    </button>
    ${open && html`<div class="node-pop node-fuse-pop" role="dialog" aria-label="Fuse nodes" style=${popStyle}>
      <div class="node-pop-head"><span>Fuse nodes</span>${fused !== 'all' && html`<button type="button" class="node-pop-link" data-fuse="all" onClick=${() => onFuse('all')}>all</button>`}</div>
      ${list !== nodes && html`<label class="node-pop-filter"><input type="text" value=${query} placeholder="type to filter" aria-label="Filter nodes" onInput=${(e) => setQuery(e.currentTarget.value)} /></label>`}
      ${list.map((node) => html`<div key=${node.id} class=${`node-pop-row${node.online || node.local ? '' : ' offline'}`} style=${`--nc:${node.color}`}>
        <input type="checkbox" checked=${isTicked(fused || 'all', node.id)} aria-label=${`Include ${node.name}`} data-fuse-node=${node.id} onChange=${() => toggle(node.id)} />
        <span class="node-pop-swatch" aria-hidden="true"></span>
        <button type="button" class="node-pop-name" title=${`Show ${node.name} alone`} onClick=${() => { setOpen(false); onPick(node.id); }}>${node.local ? '⌂ ' : ''}${node.name}</button>
        <button type="button" class="node-pop-only" tabindex="-1" onClick=${() => { setOpen(false); onPick(node.id); }}>only</button>
        <span class="node-pop-meta">${node.local ? '' : html`<span class="node-pop-dot" aria-hidden="true"></span>`}${nodeMeta(node)}${node.id === primary.id ? ' · primary' : ''}</span>
      </div>`)}
      <div class="node-pop-foot"><b>Groups</b> and <b>Terminals</b> show the ticked nodes together. Other tabs show one node, the primary: <b>${primary.local ? '⌂ ' : ''}${primary.name}</b>.</div>
    </div>`}
  </span>`;
}

// NodeChips is the tab-bar node selector: a button per node (or one node
// dropdown past BUTTON_LIMIT nodes), then the fused-view dropdown. The map
// entry is the static nav[data-tab="map"] anchor right after this host, so tab
// routing keeps one owner.
export function NodeChips({ state, navigate = defaultNavigate, remote = remoteNodeID(), switchNode = defaultSwitchNode }) {
  const current = state.view.value;
  const fleet = current.fleet;
  if (!fleet) return null;
  const fused = current.fused;
  const pageNode = remote || fleet.self.id;
  const onPick = (id) => pickNode(state, id, { navigate, remote, switchNode });
  const onFuse = (value) => {
    state.setFused(value);
    writeScopeURL(value);
    if (state.view.value.topLevel) navigate('groups');
  };
  // The shown node's button is current while its per-node view is on screen;
  // in a fused view every ticked node's button is outlined instead.
  const isCurrent = (id) => !fused && !current.topLevel && id === pageNode;
  const cls = (node) => `node-chip${node.local ? ' local' : ''}${node.local || node.online ? '' : ' offline'}${isCurrent(node.id) ? ' active' : ''}${fused && isTicked(fused, node.id) ? ' ticked' : ''}`;
  const self = fleet.self;
  const buttons = selectorKind(fleet) === 'buttons';
  return html`<span class="node-chips" role="group" aria-label="Nodes">
    ${buttons ? html`
    <button type="button" class=${cls(self)} style=${`--nc:${self.color}`}
      aria-current=${isCurrent(self.id) ? 'page' : undefined} aria-label=${`${self.name} (this node)`}
      title=${`${self.name}: this node · Alt+1`} onClick=${() => onPick(self.id)}>
      <span class="node-chip-home" aria-hidden="true">⌂</span><span class="node-chip-name">${self.name}</span><span class="node-chip-dot"></span>
    </button>
    ${fleet.peers.map((peer, i) => html`<button key=${peer.id} type="button" class=${cls(peer)} style=${`--nc:${peer.color}`}
      aria-current=${isCurrent(peer.id) ? 'page' : undefined} aria-label=${`${peer.name}, ${presenceLabel(peer)}, ${peer.level} peer`}
      title=${`${peer.name}: ${presenceLabel(peer)} · ${peer.level} peer · open its dashboard${i < 8 ? ` · Alt+${i + 2}` : ''}`} onClick=${() => onPick(peer.id)}>
      <span class="node-chip-name">${peer.name}</span><span class="node-chip-dot"></span>
    </button>`)}`
    : html`<${NodeDropdown} state=${state} fleet=${fleet} pageNode=${pageNode} fused=${fused} onPick=${onPick} />`}
    <span class="node-chips-sep" aria-hidden="true"></span>
    <${FusedDropdown} state=${state} fleet=${fleet} fused=${fused} pageNode=${pageNode} onPick=${onPick} onFuse=${onFuse} />
  </span>`;
}

// ScopeSync mirrors the fused set onto the page: the html class the CSS keys
// on, the ?nodes= value nav-history.js carries across tab pushes, and the
// primary's colour. It drops a fused set once a status read says no linked
// nodes are left (a failed read keeps it).
export function ScopeSync({ state, navigate = defaultNavigate, remote = remoteNodeID() }) {
  const current = state.view.value;
  const fused = current.fused;
  const fleet = current.fleet;
  useEffect(() => {
    const root = document.documentElement;
    root.classList.toggle('scope-fused', !!fused);
    if (fused) root.dataset.scopeNodes = fusedParam(fused); else delete root.dataset.scopeNodes;
    writeScopeURL(fused);
  }, [fusedParam(fused)]);
  useEffect(() => {
    const primary = fleet && (fleetNodes(fleet).find((n) => n.id === (remote || fleet.self.id)) || fleet.self);
    if (primary) document.documentElement.style.setProperty('--scope-primary', primary.color);
  }, [fleet, remote]);
  useEffect(() => {
    if (fused && current.noFleet) { state.setFused(null); writeScopeURL(null); }
  }, [!!fused, current.noFleet]);
  return null;
}

// ScopeBanner tops a per-node tab while a fused view is on: that tab shows one
// node, the primary, and the strip says which and can switch it (a page load,
// as any node switch; the fused set stays).
export function ScopeBanner({ state, remote = remoteNodeID(), switchNode = defaultSwitchNode, doc = globalThis.document }) {
  const current = state.view.value;
  const fleet = current.fleet;
  if (!fleet || !current.fused || current.topLevel || FUSED_TABS.has(current.activeTab)) return null;
  const ticked = tickedNodes(fleet, current.fused);
  const pageNode = remote || fleet.self.id;
  const primary = fleetNodes(fleet).find((n) => n.id === pageNode) || fleet.self;
  const label = doc?.querySelector?.(`nav [data-tab="${current.activeTab}"] .tab-label-regular`)?.textContent?.trim() || 'This tab';
  const pill = (n) => html`<span class="scope-pill" style=${`--nc:${n.color}`}>${n.local ? '⌂ ' : ''}${n.name}</span>`;
  return html`<div class="scope-banner" role="status" style=${`--nc:${primary.color}`}>
    <span>${label} shows one node. Showing ${pill(primary)}, the primary of your ${ticked.length} fused nodes.</span>
    <span class="scope-banner-seg" role="group" aria-label="Primary node">
      ${ticked.map((n) => html`<button type="button" key=${n.id} class=${n.id === pageNode ? 'on' : ''} aria-pressed=${n.id === pageNode ? 'true' : 'false'}
        onClick=${() => { if (n.id !== pageNode) switchNode(n.local ? '' : n.id); }}>${n.local ? '⌂ ' : ''}${n.name}</button>`)}
    </span>
  </div>`;
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
  // "Groups · all nodes" is the fused Groups view of every node.
  const go = (tab) => {
    if (tab !== 'groups-fused') { navigate(tab); return; }
    state.setFused('all'); writeScopeURL('all'); navigate('groups');
  };
  const seg = (tab, on, label) => html`<button type="button" class=${`skynet-seg-btn${on ? ' on' : ''}`} aria-current=${on ? 'page' : undefined} onClick=${() => { if (!on) go(tab); }}>${label}</button>`;
  return html`<span class="skynet-toplevel-bar">
    <span class="skynet-seg" role="group" aria-label="Skynet views">${seg('map', current.mapActive, html`<${MapGlyph} /> Map`)}${seg('groups-fused', false, 'Groups · all nodes')}${seg('fleet-admin', current.adminActive, '⚙ Fleet')}</span>
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

function NodeCard({ node, card, focused, onOpen, onUpdate, onTerminals, cardRef, shown }) {
  const kind = node.local ? 'this node' : node.level === 'unrestricted' ? '⚠ unrestricted peer' : 'restricted peer';
  const presence = card.presence === 'online' ? (node.local ? 'online' : 'online') : card.presence === 'offline' ? 'offline' : card.presence === 'error' ? 'error' : '…';
  return html`<article ref=${cardRef} data-node-id=${node.id} class=${`skynet-card${node.local ? ' local' : ''}${card.stale || card.presence === 'offline' ? ' stale' : ''}${focused ? ' focused' : ''}`}
    style=${`--nc:${node.color}`} aria-label=${`${node.name} node`}>
    <div class="skynet-card-head"><span class="skynet-card-name">${node.local ? '⌂ ' : ''}${node.name}</span>
      <span class=${`skynet-card-kind${node.level === 'unrestricted' ? ' unrestricted' : ''}`}>${kind}</span>
      <span class=${`skynet-card-presence ${card.presence}`}><i aria-hidden="true"></i>${presence}${card.stale && card.ageMs != null ? html` <span class="muted">· data ${fmtAge(card.ageMs)} old</span>` : ''}</span></div>
    <${CardRows} card=${card} node=${node} onUpdate=${onUpdate} />
    ${node.keyTransition && html`<div class=${`skynet-card-key ${node.keyTransition.state}`} title="See Fleet → Peers">${node.keyTransition.state === 'conflict' ? '⚠ competing new signing keys: needs recovery (Fleet → Peers)' : '🔑 new signing key pending: not yet accepted'}</div>`}
    ${onOpen && html`<div class="skynet-card-foot"><button type="button" class=${shown ? 'primary' : ''} onClick=${onOpen}>${shown ? 'Back to its dashboard' : 'Open dashboard'}</button>${onTerminals && html` <button type="button" class="skynet-card-terms" title=${`Watch or type into ${node.name}'s agents in the browser, as it shares them`} onClick=${onTerminals}>Terminals…</button>`}</div>`}
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
  const [terminals, setTerminals] = useState(null);
  const fleetKey = fleet ? [fleet.self.id, ...fleet.peers.map((p) => p.id)].join(',') : '';

  // Never strand the operator on an empty map: once the node list has loaded
  // without linked nodes (a /map deep link on an unlinked node, or the last
  // peer untrusted while the map is open), return to the per-node tab. Fleet
  // administration stays: it is where the first peer gets trusted.
  const multiNode = current.mapActive;
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
  const open = (node) => () => pickNode(state, node.id, { navigate: () => navigate(state.lastLocalTab()), remote, switchNode });
  return html`<div class="skynet-map" ref=${mapRef}>
    <svg class="skynet-edges" aria-hidden="true">
      ${edges.map((e) => { const p = peerById.get(e.id); const live = p && card(p).presence === 'online'; return html`<path key=${e.id} d=${e.d} class=${`skynet-edge${live ? ' live' : ' stale'}`} />`; })}
    </svg>
    ${edges.map((e) => { const p = peerById.get(e.id); return p && html`<span key=${`l-${e.id}`} class="skynet-edge-label" style=${`left:${e.mid.x}px;top:${e.mid.y}px`}>${p.level === 'unrestricted' ? '⚠ unrestricted' : 'restricted'} link</span>`; })}
    <div class="skynet-map-local"><${NodeCard} node=${fleet.self} card=${card(fleet.self)} focused=${current.focused === fleet.self.id} shown=${!remote} onOpen=${open(fleet.self)} onUpdate=${() => setUpdating(fleet.self)} /></div>
    <div class=${`skynet-map-peers${fleet.peers.length > 4 ? ' wide' : ''}`}>${fleet.peers.map((peer) => html`<${NodeCard} key=${peer.id} node=${peer} card=${card(peer)} focused=${current.focused === peer.id} shown=${remote === peer.id} onOpen=${open(peer)} onUpdate=${() => setUpdating(peer)} onTerminals=${() => setTerminals(peer)} />`)}</div>
    ${terminals && html`<${RemoteSessionsDialog} node=${terminals} toast=${toast} onClose=${() => setTerminals(null)} />`}
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
      if (node.id === (remote || fleet.self.id) && !state.fused.value) { if (state.view.value.topLevel) navigate(state.lastLocalTab()); return; }
      pickNode(state, node.id, { navigate, remote, switchNode });
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

export function mountSkynetIsland({ chipsHost, barHost, mapHost, remoteHost, bannerHost, state, actions, registerCleanup, navigate, timers }) {
  render(html`<${Fragment}><${StatusPoller} actions=${actions} timers=${timers} /><${NodeSwitchKeys} state=${state} navigate=${navigate} /><${ScopeSync} state=${state} navigate=${navigate} /><${NodeChips} state=${state} navigate=${navigate} /></${Fragment}>`, chipsHost);
  if (bannerHost) render(html`<${ScopeBanner} state=${state} />`, bannerHost);
  // A peer view also hosts the peer action dialog (stop/retire/clone/move/
  // teleport/spawn through the peer's action routes).
  if (remoteHost) render(html`<${RemoteMarker} state=${state} /><${PeerActionHost} snapshot=${dashboardState.snapshot} />`, remoteHost);
  render(html`<${TopLevelBar} state=${state} navigate=${navigate} />`, barHost);
  render(html`<${SkynetMap} state=${state} actions=${actions} navigate=${navigate} timers=${timers} />`, mapHost);
  registerCleanup(() => { render(null, chipsHost); render(null, barHost); render(null, mapHost); if (remoteHost) render(null, remoteHost); if (bannerHost) render(null, bannerHost); });
}
