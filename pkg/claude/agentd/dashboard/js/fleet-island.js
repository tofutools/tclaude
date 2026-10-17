import { Fragment, h, render } from 'preact';
import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { NodeUpdateDialog } from './node-update.js';
import { RequestAccessDialog } from './peer-access.js';
import { RemoteSessionsDialog } from './remote-terminal.js';
import { PeerActionHost } from './peer-action.js';
import { shellConfirm, shellToast } from './shell-state.js';
import { LOCAL_COLOR, STATUS_POLL_MS, cardView, fmtAge, hubOffReason, nodeColor, nodeHref, peerViewSummary, pollDelay, remoteHealthView, remoteNodeID, staggerOffset, switchOrder } from './fleet-model.js';
import { FUSED_TABS, filterNodes, fleetNodes, fusedParam, isTicked, selectorKind, tickedNodes, toggleNode, withFused } from './fleet-scope.js';
import { shortFingerprint, shortID } from './fleet-admin-model.js';
import { dashboardState } from './snapshot-store.js';
import { TOP_LEVEL_TABS } from './fleet-state.js';

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
  // pane, Debug switched off); Groups is always there. Processes' anchor is
  // always hidden (it is reached through Automations), so only its feature
  // flag decides.
  const hidden = tab === 'processes'
    ? document.body.classList.contains('hide-processes')
    : !anchor || anchor.offsetParent === null;
  if (!TOP_LEVEL_TABS.has(tab) && hidden) anchor = document.querySelector('nav [data-tab="groups"]');
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
// Without a fleet only this node can be picked: ⌂ still leaves a top-level
// view or a peer's view.
function pickNode(state, id, { navigate, remote, switchNode }) {
  const fleet = state.fleet.value;
  const selfID = fleet?.self.id ?? state.node.value?.self.id ?? '';
  if (!fleet && id !== selfID) return;
  const wasFused = !!state.fused.value;
  state.setFused(null);
  writeScopeURL(null);
  if (id === (remote || selfID)) {
    if (state.view.value.topLevel || !wasFused) navigate(state.lastLocalTab());
  } else switchNode(id === selfID ? '' : id);
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
// clicking a node's name (or its "only") shows that node alone. A connected
// node with no peers yet still opens it, to list itself and say how to link.
function FusedDropdown({ state, fleet, fused, pageNode, onPick, onFuse }) {
  const rootRef = useRef(null);
  const [open, setOpen, popStyle] = usePopover(rootRef);
  const [query, setQuery] = useState('');
  const nodes = fleetNodes(fleet);
  const ticked = fused ? tickedNodes(fleet, fused) : nodes;
  const list = selectorKind(fleet) === 'dropdown' ? filterNodes(fleet, query) : nodes;
  const alone = nodes.length < 2;
  const toggle = (id) => {
    if (alone) return;
    const next = toggleNode(fleet, fused || 'all', id);
    if (next.only !== undefined) { setOpen(false); onPick(next.only); } else onFuse(next);
  };
  const onButton = () => {
    if (alone) setOpen(!open);
    else if (!fused) { onFuse('all'); setOpen(true); } else setOpen(!open);
  };
  const primary = nodes.find((n) => n.id === pageNode) || fleet.self;
  return html`<span class="node-pick" ref=${rootRef}>
    <button type="button" class=${`node-chip node-fuse${fused ? ' active' : ''}`} aria-haspopup="dialog" aria-expanded=${open ? 'true' : 'false'}
      aria-label=${fused ? `Fused view of ${ticked.length} of ${nodes.length} nodes. Change the nodes` : 'Show several nodes together'}
      title=${fused ? `Groups and Terminals show ${ticked.length} of ${nodes.length} nodes` : alone ? NO_PEERS : 'Show several nodes together in Groups and Terminals'} onClick=${onButton}>
      <span class="node-fuse-stack" aria-hidden="true">${nodes.slice(0, 4).map((n) => html`<b key=${n.id} class=${isTicked(fused || 'all', n.id) ? '' : 'off'} style=${`--c:${n.color}`}></b>`)}</span>
      <span class="node-chip-name">${fused ? `${ticked.length}/${nodes.length}` : 'nodes'}</span><span class="node-pick-caret" aria-hidden="true">▾</span>
    </button>
    ${open && html`<div class="node-pop node-fuse-pop" role="dialog" aria-label="Fuse nodes" style=${popStyle}>
      <div class="node-pop-head"><span>Fuse nodes</span>${!alone && fused !== 'all' && html`<button type="button" class="node-pop-link" data-fuse="all" onClick=${() => onFuse('all')}>all</button>`}</div>
      ${list !== nodes && html`<label class="node-pop-filter"><input type="text" value=${query} placeholder="type to filter" aria-label="Filter nodes" onInput=${(e) => setQuery(e.currentTarget.value)} /></label>`}
      ${list.map((node) => html`<div key=${node.id} class=${`node-pop-row${node.online || node.local ? '' : ' offline'}`} style=${`--nc:${node.color}`}>
        <input type="checkbox" checked=${isTicked(fused || 'all', node.id)} disabled=${alone} aria-label=${`Include ${node.name}`} data-fuse-node=${node.id} onChange=${() => toggle(node.id)} />
        <span class="node-pop-swatch" aria-hidden="true"></span>
        <button type="button" class="node-pop-name" title=${`Show ${node.name} alone`} onClick=${() => { setOpen(false); onPick(node.id); }}>${node.local ? '⌂ ' : ''}${node.name}</button>
        <button type="button" class="node-pop-only" tabindex="-1" onClick=${() => { setOpen(false); onPick(node.id); }}>only</button>
        <span class="node-pop-meta">${node.local ? '' : html`<span class="node-pop-dot" aria-hidden="true"></span>`}${nodeMeta(node)}${node.id === primary.id ? ' · primary' : ''}</span>
      </div>`)}
      ${alone ? html`<div class="node-pop-foot">${NO_PEERS}</div>` : html`<div class="node-pop-foot"><b>Groups</b> and <b>Terminals</b> show the ticked nodes together. Other tabs show one node, the primary: <b>${primary.local ? '⌂ ' : ''}${primary.name}</b>.</div>`}
    </div>`}
  </span>`;
}

// NO_PEERS says why there is nothing to fuse on a connected node that has no
// linked nodes yet.
const NO_PEERS = 'No linked nodes yet: trust a peer in ⚙ Fleet config';
// The map can trust peers itself once its "Show untrusted" box is ticked.
const MAP_NO_PEERS = 'No linked nodes yet: tick "Show untrusted" to trust a peer from the map, or use ⚙ Fleet config';
const NO_STRANGERS = 'No linked nodes yet, and no other instance is visible on the hub';

// FleetChip is the right end's third scope, beside ⌂ this node and the nodes
// dropdown: the whole fleet. It is lit on every fleet view and opens the one
// used last (the map, or Fleet config), or Fleet config while the hub is not
// connected, since that is where the hub and the first peer are set up. It is
// never greyed out on this node. In a peer's view it can only open the map,
// so it hides while there is none. Until the setup guide has been opened once,
// a node that was never set up says so on the chip.
function FleetChip({ state, node, off, remote, navigate }) {
  const current = state.view.value;
  const mapOK = !off && !!current.mapFleet;
  if (remote && !mapOK) return null;
  const active = current.topLevel;
  const target = remote || (mapOK && state.lastFleetTab() !== 'fleet-admin') ? 'map' : 'fleet-admin';
  const hint = !remote && !!node && !node.configured && !current.setupSeen;
  const title = off
    ? `Fleet: ${node?.configured ? 'the hub connection and peers' : 'link this node with your other machines or colleagues'} (${off.replace(/^Not connected to a federation hub: /, 'not connected: ')})`
    : !current.fleet ? 'Fleet: trust a peer to link nodes'
      : `Fleet: every linked node (${target === 'map' ? 'the map' : 'Fleet config'})`;
  // The separator comes with the chip, so a row without it ends cleanly.
  return html`<span class="node-chips-sep" aria-hidden="true"></span>
  <button type="button" class=${`node-chip fleet-entry${active ? ' active' : ''}`} data-fleet-entry data-fleet-target=${target}
    aria-current=${active ? 'page' : undefined} aria-label=${`Fleet${hint ? ': set up' : ''}`} title=${title}
    onClick=${() => { if (!active) navigate(target); }}>
    <${MapGlyph} /><span class="node-chip-name">Fleet</span>${hint && html`<span class="node-chip-hint">set up</span>`}
  </button>`;
}

// IdleChips is the chip row before there is a fleet to pick from: this node's
// ⌂ and the fused dropdown, greyed out until the hub is connected and then
// listing this node alone. ⌂ is usable once the hub is connected, and
// always where it leads somewhere: home from a peer's view, or back from the
// map or Fleet administration.
function IdleChips({ state, node, off, remote, onPick, navigate }) {
  const current = state.view.value;
  const self = node?.self || { id: '', name: 'this node', color: LOCAL_COLOR, local: true };
  const homeOff = off && !remote && !current.topLevel;
  const isCurrent = !remote && !current.topLevel;
  return html`<span class=${`node-chips${off ? ' hub-off' : ' has-map'}`} role="group" aria-label="Nodes" title=${off || undefined}>
    <button type="button" class=${`node-chip local${isCurrent ? ' active' : ''}`} style=${`--nc:${self.color}`} disabled=${homeOff}
      aria-current=${isCurrent ? 'page' : undefined} aria-label=${`${self.name} (this node)${homeOff ? `. ${off}` : ''}`}
      title=${homeOff ? off : `${self.name}: this node · Alt+1`} onClick=${() => onPick(self.id)}>
      <span class="node-chip-home" aria-hidden="true">⌂</span><span class="node-chip-name">${self.name}</span>
    </button>
    <span class="node-chips-sep" aria-hidden="true"></span>
    ${current.mapFleet && !off ? html`<${FusedDropdown} state=${state} fleet=${current.mapFleet} fused=${null} pageNode=${remote || self.id} onPick=${onPick} onFuse=${() => {}} />`
    : html`<button type="button" class="node-chip node-fuse" disabled aria-label=${`Show several nodes together. ${off || NO_PEERS}`} title=${off || NO_PEERS}>
      <span class="node-chip-name">nodes</span><span class="node-pick-caret" aria-hidden="true">▾</span>
    </button>`}
    <${FleetChip} state=${state} node=${node} off=${off} remote=${remote} navigate=${navigate} />
  </span>`;
}

// NodeChips is the tab-bar node selector: a button per node (or one node
// dropdown past BUTTON_LIMIT nodes), then the fused-view dropdown. The row is
// always there, greyed out until this node's hub connection is up (and again
// when it drops); peers only add buttons. ⌘ Fleet ends the row. The static
// nav[data-tab="map"] anchor stays the map's routing target, hidden.
export function NodeChips({ state, navigate = defaultNavigate, remote = remoteNodeID(), switchNode = defaultSwitchNode }) {
  const current = state.view.value;
  const fleet = current.fleet;
  const off = hubOffReason(current.node);
  const onPick = (id) => pickNode(state, id, { navigate, remote, switchNode });
  // A fused view already on keeps its row, so it can be left.
  if (!fleet || (off && !current.fused)) return html`<${IdleChips} state=${state} node=${current.node} off=${off} remote=${remote} onPick=${onPick} navigate=${navigate} />`;
  const fused = current.fused;
  const pageNode = remote || fleet.self.id;
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
  return html`<span class="node-chips has-fleet" role="group" aria-label="Nodes">
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
    <${FleetChip} state=${state} node=${current.node} off=${off} remote=${remote} navigate=${navigate} />
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
  const fleet = current.mapFleet;
  if (!current.topLevel || !fleet) return null;
  const n = fleet.peers.length + 1;
  const hub = fleet.hub.state;
  // "Groups · all nodes" is the fused Groups view of every node.
  const go = (tab) => {
    if (tab !== 'groups-fused') { navigate(tab); return; }
    state.setFused('all'); writeScopeURL('all'); navigate('groups');
  };
  const seg = (tab, on, label) => html`<button type="button" class=${`fleet-seg-btn${on ? ' on' : ''}`} aria-current=${on ? 'page' : undefined} onClick=${() => { if (!on) go(tab); }}>${label}</button>`;
  return html`<span class="fleet-toplevel-bar">
    <span class="fleet-seg" role="group" aria-label="Fleet views">${seg('groups-fused', false, '▦ Groups · all nodes')}${seg('map', current.mapActive, html`<${MapGlyph} /> Map`)}${seg('fleet-admin', current.adminActive, '⚙ Fleet config')}</span>
    <span class="fleet-toplevel-note">${n} node${n === 1 ? '' : 's'} · hub ${hub}</span>
  </span>`;
}

function Meter({ label, value }) {
  return html`<span class="fleet-meter" title=${`${label} ${value}%`}>${label}<span class="fleet-meter-bar"><b style=${`width:${value}%`}></b></span>${value}%</span>`;
}

function CardRows({ card, node, onUpdate }) {
  const f = card.failure;
  if (card.sharedAgents == null) {
    let text = 'Loading summary…';
    if (f?.kind === 'unavailable') text = 'This node does not offer a map summary yet.';
    else if (f?.kind === 'not_trusted') text = 'Not trusted any more.';
    else if (f?.kind === 'offline' || f?.kind === 'timeout' || card.presence === 'offline') text = card.lastSeen ? `Unreachable. Last seen ${new Date(card.lastSeen).toLocaleString()}.` : 'Unreachable.';
    else if (f) text = `Summary failed: ${f.message || f.kind}`;
    return html`<div class="fleet-card-empty">${text}</div>`;
  }
  const r = card.resources;
  return html`<dl class="fleet-card-body">
    <dt>Agents</dt><dd>${card.onlineAgents ?? '?'} of ${card.sharedAgents} online · ${card.sharedGroups} group${card.sharedGroups === 1 ? '' : 's'}${node.local ? '' : html` <span class="muted">shared</span>`}</dd>
    <dt>Attention</dt><dd>${card.waiting ? html`<span class="fleet-attn">❓ ${card.waiting} waiting for input</span>` : html`<span class="muted">none</span>`}</dd>
    <dt>Resources</dt><dd>${r ? html`${r.cpu != null && html`<${Meter} label="cpu" value=${r.cpu} />`}${r.mem != null && html`<${Meter} label="mem" value=${r.mem} />`}${r.disk != null && html`<${Meter} label="disk" value=${r.disk} />`}` : html`<span class="muted">not shared</span>`}</dd>
    ${card.version && html`<dt>tclaude</dt><dd>${card.version}${card.updateAvailable ? html` <span class="fleet-update" title=${`tclaude ${card.latestVersion} is available`}>↑ ${card.latestVersion}</span>` : ''}${onUpdate && html` <button type="button" class="fleet-link" data-fleet="node-update" onClick=${onUpdate}>${card.updateAvailable ? 'update…' : 'manage…'}</button>`}</dd>`}
    ${card.health && html`<dt>Health</dt><dd class=${`fleet-card-health ${card.health}`}>${card.health === 'current' ? '✓ current' : card.health}</dd>`}
    ${card.omitted.length > 0 && html`<dt>Not shared</dt><dd class="muted">${card.omitted.join(', ')}</dd>`}
  </dl>`;
}

function NodeCard({ node, card, focused, onOpen, onUpdate, onTerminals, cardRef, shown }) {
  const kind = node.local ? 'this node' : node.level === 'unrestricted' ? '⚠ unrestricted peer' : 'restricted peer';
  const presence = card.presence === 'online' ? (node.local ? 'online' : 'online') : card.presence === 'offline' ? 'offline' : card.presence === 'error' ? 'error' : '…';
  return html`<article ref=${cardRef} data-node-id=${node.id} class=${`fleet-card${node.local ? ' local' : ''}${card.stale || card.presence === 'offline' ? ' stale' : ''}${focused ? ' focused' : ''}`}
    style=${`--nc:${node.color}`} aria-label=${`${node.name} node`}>
    <div class="fleet-card-head"><span class="fleet-card-name">${node.local ? '⌂ ' : ''}${node.name}</span>
      <span class=${`fleet-card-kind${node.level === 'unrestricted' ? ' unrestricted' : ''}`}>${kind}</span>
      <span class=${`fleet-card-presence ${card.presence}`}><i aria-hidden="true"></i>${presence}${card.stale && card.ageMs != null ? html` <span class="muted">· data ${fmtAge(card.ageMs)} old</span>` : ''}</span></div>
    <${CardRows} card=${card} node=${node} onUpdate=${onUpdate} />
    ${node.keyTransition && html`<div class=${`fleet-card-key ${node.keyTransition.state}`} title="See Fleet config → Peers">${node.keyTransition.state === 'conflict' ? '⚠ competing new signing keys: needs recovery (Fleet config → Peers)' : '🔑 new signing key pending: not yet accepted'}</div>`}
    ${onOpen && html`<div class="fleet-card-foot"><button type="button" class=${shown ? 'primary' : ''} onClick=${onOpen}>${shown ? 'Back to its dashboard' : 'Open dashboard'}</button>${onTerminals && html` <button type="button" class="fleet-card-terms" title=${`Watch or type into ${node.name}'s agents in the browser, as it shares them`} onClick=${onTerminals}>Terminals…</button>`}</div>`}
  </article>`;
}

// HubCard is the federation hub between this node and its peers: every link
// to a peer is relayed through it.
function HubCard({ hub, now }) {
  const connected = hub.state === 'connected';
  const shortID = hub.id.length > 16 ? `${hub.id.slice(0, 16)}…` : hub.id;
  return html`<article class=${`fleet-card hub${connected ? '' : ' stale'}`} data-node-id=":hub" aria-label="federation hub">
    <div class="fleet-card-head"><span class="fleet-card-name">hub</span><span class="fleet-card-kind">relay</span>
      <span class=${`fleet-card-presence ${connected ? 'online' : 'offline'}`}><i aria-hidden="true"></i>${hub.state}</span></div>
    <dl class="fleet-card-body">
      ${hub.url && html`<dt>Address</dt><dd class="fleet-card-hub-url" title=${hub.url}>${hub.url}</dd>`}
      ${hub.id && html`<dt>Hub ID</dt><dd title=${hub.id}>${shortID}</dd>`}
      ${hub.spaces?.length > 0 && html`<dt>Spaces</dt><dd>${hub.spaces.join(', ')}</dd>`}
      ${hub.since != null && html`<dt>${connected ? 'Up' : 'Since'}</dt><dd>${fmtAge(Math.max(0, now - hub.since))}</dd>`}
    </dl>
  </article>`;
}

// StrangerCard is a hub-visible instance this node has not trusted. Its name
// is the instance's own claim, so the card leads with the fingerprint, and
// Trust… opens the same fingerprint-checked dialog as Fleet config → Peers.
function StrangerCard({ row, now, onTrust }) {
  const seen = row.online ? 'online' : row.lastSeen ? `seen ${fmtAge(Math.max(0, now - Date.parse(row.lastSeen)))} ago` : 'offline';
  return html`<article data-node-id=${row.id} class=${`fleet-card untrusted${row.online ? '' : ' stale'}`} aria-label=${`${row.label} untrusted instance`}>
    <div class="fleet-card-head"><span class="fleet-card-name" title="Name reported by the instance itself">${row.name || html`<span class="muted">unnamed</span>`}</span>
      <span class="fleet-card-kind">not trusted</span>
      <span class=${`fleet-card-presence ${row.online ? 'online' : 'offline'}`}><i aria-hidden="true"></i>${seen}</span></div>
    <dl class="fleet-card-body">
      <dt>Instance</dt><dd title=${row.id}><code>${shortID(row.id)}</code></dd>
      <dt>Fingerprint</dt><dd title=${row.fingerprint}><code>${shortFingerprint(row.fingerprint) || '—'}</code></dd>
    </dl>
    ${onTrust && html`<div class="fleet-card-foot"><button type="button" class="primary" data-fleet="trust" title="Compare its fingerprint with its operator, then pick a trust level" onClick=${onTrust}>Trust…</button></div>`}
  </article>`;
}

// loadTrustDialog fetches Fleet config's trust dialog and actions on first
// use, so the map does not load the whole administration view up front.
async function defaultLoadTrust() {
  const [{ TrustDialog }, { createFleetAdminActions }] = await Promise.all([import('./fleet-admin-island.js'), import('./fleet-admin-actions.js')]);
  return { TrustDialog, actions: createFleetAdminActions() };
}

// edgePath draws a gentle horizontal S-curve between two anchor points.
function edgePath(a, b) {
  const dx = Math.max(40, (b.x - a.x) / 2);
  return `M${a.x} ${a.y} C${a.x + dx} ${a.y} ${b.x - dx} ${b.y} ${b.x} ${b.y}`;
}

// measureEdges routes the links as they run: this node to the hub, and the
// hub to each peer. Without a hub card every peer links to this node directly.
export function measureEdges(mapEl) {
  if (!mapEl) return [];
  const box = mapEl.getBoundingClientRect();
  const local = mapEl.querySelector('.fleet-card.local');
  if (!local) return [];
  const side = (el, right) => { const r = el.getBoundingClientRect(); return { x: (right ? r.right : r.left) - box.left, y: r.top - box.top + r.height / 2 }; };
  const edge = (id, from, to) => ({ id, d: edgePath(from, to), mid: { x: (from.x + to.x) / 2, y: (from.y + to.y) / 2 } });
  const hub = mapEl.querySelector('.fleet-card.hub');
  const from = hub ? side(hub, true) : side(local, true);
  const peers = [...mapEl.querySelectorAll('.fleet-card:not(.local):not(.hub)')].map((el) => edge(el.dataset.nodeId, from, side(el, false)));
  return hub ? [edge(':hub', side(local, true), side(hub, false)), ...peers] : peers;
}

export function FleetMap({ state, actions, navigate = defaultNavigate, timers = globalThis, now = () => Date.now(), remote = remoteNodeID(), switchNode = defaultSwitchNode, confirm = shellConfirm, toast = shellToast, updateActions, loadTrust = defaultLoadTrust }) {
  const current = state.view.value;
  const fleet = current.mapFleet;
  const mapRef = useRef(null);
  const [edges, setEdges] = useState([]);
  const [updating, setUpdating] = useState(null);
  const [terminals, setTerminals] = useState(null);
  // trusting is { row, TrustDialog, actions } once the dialog has loaded.
  const [trusting, setTrusting] = useState(null);
  const fleetKey = fleet ? [fleet.self.id, ...fleet.peers.map((p) => p.id)].join(',') : '';

  // Never strand the operator on an empty map: once the node list has loaded
  // without a map to show (a /map deep link on a node off any hub, or the hub
  // dropping while the map is open), return to the per-node tab. Fleet
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
  }, [current.mapActive, fleetKey, current.summaries, fleet?.hub.state, current.strangers]);

  if (!fleet) return html`<div class="empty">Not connected to a federation hub. Set it up in ⚙ Fleet or with <code>tclaude federation</code> (see docs/federation.md).</div>`;
  const t = now();
  const peerById = new Map(fleet.peers.map((p) => [p.id, p]));
  const card = (node) => cardView(node, current.summaries[node.id], t);
  // Each card opens its node's per-node view; the node already shown returns
  // to the tab the operator left for the map.
  const open = (node) => () => pickNode(state, node.id, { navigate: () => navigate(state.lastLocalTab()), remote, switchNode });
  // Trusting is this node's own decision, made on this node's page only.
  const strangers = remote ? [] : current.strangers;
  const toggleUntrusted = (e) => { state.setShowUntrusted(e.currentTarget.checked); actions.loadStatus?.(); };
  const trust = (row) => () => loadTrust()
    .then((loaded) => setTrusting({ row, ...loaded }))
    .catch((e) => toast(`Trust: ${e?.message || e}`, true));
  const trusted = (msg) => { setTrusting(null); toast(msg, false); actions.loadStatus?.(); };
  return html`<div class="fleet-map" ref=${mapRef}>
    <svg class="fleet-edges" aria-hidden="true">
      ${edges.map((e) => { const p = peerById.get(e.id); const live = e.id === ':hub' ? fleet.hub.state === 'connected' : p && card(p).presence === 'online'; return html`<path key=${e.id} d=${e.d} class=${`fleet-edge${e.id !== ':hub' && !p ? ' untrusted' : live ? ' live' : ' stale'}`} />`; })}
    </svg>
    ${edges.map((e) => { const p = peerById.get(e.id); return p && html`<span key=${`l-${e.id}`} class="fleet-edge-label" style=${`left:${e.mid.x}px;top:${e.mid.y}px`}>${p.level === 'unrestricted' ? '⚠ unrestricted' : 'restricted'} link</span>`; })}
    <div class="fleet-map-local"><${NodeCard} node=${fleet.self} card=${card(fleet.self)} focused=${current.focused === fleet.self.id} shown=${!remote} onOpen=${open(fleet.self)} onUpdate=${() => setUpdating(fleet.self)} /></div>
    <div class="fleet-map-hub"><${HubCard} hub=${fleet.hub} now=${t} /></div>
    <div class=${`fleet-map-peers${fleet.peers.length + strangers.length > 4 ? ' wide' : ''}`}>${fleet.peers.length === 0 && !strangers.length && html`<div class="fleet-map-alone">${remote ? NO_PEERS : current.showUntrusted ? NO_STRANGERS : MAP_NO_PEERS}</div>`}${fleet.peers.map((peer) => html`<${NodeCard} key=${peer.id} node=${peer} card=${card(peer)} focused=${current.focused === peer.id} shown=${remote === peer.id} onOpen=${open(peer)} onUpdate=${() => setUpdating(peer)} onTerminals=${() => setTerminals(peer)} />`)}${strangers.map((row) => html`<${StrangerCard} key=${row.id} row=${row} now=${t} onTrust=${trust(row)} />`)}</div>
    ${trusting && html`<${trusting.TrustDialog} row=${trusting.row} actions=${trusting.actions} onClose=${() => setTrusting(null)} onDone=${trusted} />`}
    ${terminals && html`<${RemoteSessionsDialog} node=${terminals} toast=${toast} onClose=${() => setTerminals(null)} />`}
    ${updating && html`<${NodeUpdateDialog} node=${{ ...updating, label: updating.name }} confirm=${confirm} toast=${toast} timers=${timers} actions=${updateActions}
      onClose=${() => { setUpdating(null); actions.loadSummary(updating); }} />`}
    <div class="fleet-legend"><span aria-hidden="true"><i class="live"></i>linked, reachable</span><span aria-hidden="true"><i class="stale"></i>unreachable or stale</span>${strangers.length > 0 && html`<span aria-hidden="true"><i class="untrusted"></i>on the hub, not trusted</span>`}<span aria-hidden="true">Counts show only what each node shares with you.</span>
      ${!remote && html`<label class="fleet-untrusted-toggle" title="Also show instances on the hub that this node has not trusted, to trust them from here"><input type="checkbox" data-fleet="show-untrusted" checked=${current.showUntrusted} onChange=${toggleUntrusted} /> Show untrusted</label>`}</div>
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

// HUB_POLL_MS is the status cadence while federation is on, so the chips follow
// a hub connecting or dropping within seconds.
const HUB_POLL_MS = 5000;

// StatusPoller keeps the chip row's node list fresh at a relaxed cadence and
// pauses while the page is hidden. It renders nothing.
function StatusPoller({ state, actions, timers = globalThis }) {
  useEffect(() => {
    let disposed = false; let timer = null;
    const loop = async () => {
      if (disposed) return;
      const fleet = globalThis.document?.hidden ? undefined : await actions.loadStatus();
      // The read is local. A node with federation on keeps the chips fresh,
      // so they follow the hub connection; federation off (or an older daemon
      // without the route) rechecks rarely.
      const hub = state.node.value?.hub;
      const live = hub ? hub !== 'off' : !!fleet;
      if (!disposed) timer = timers.setTimeout(loop, pollDelay({ base: live ? HUB_POLL_MS : fleet === undefined ? STATUS_POLL_MS : STATUS_POLL_MS * 4 }));
    };
    loop();
    return () => { disposed = true; timers.clearTimeout(timer); };
  }, []);
  return null;
}

export function mountFleetIsland({ chipsHost, barHost, mapHost, remoteHost, bannerHost, state, actions, registerCleanup, navigate, timers }) {
  render(html`<${Fragment}><${StatusPoller} state=${state} actions=${actions} timers=${timers} /><${NodeSwitchKeys} state=${state} navigate=${navigate} /><${ScopeSync} state=${state} navigate=${navigate} /><${NodeChips} state=${state} navigate=${navigate} /></${Fragment}>`, chipsHost);
  if (bannerHost) render(html`<${ScopeBanner} state=${state} />`, bannerHost);
  // A peer view also hosts the peer action dialog (stop/retire/clone/move/
  // teleport/spawn through the peer's action routes).
  if (remoteHost) render(html`<${RemoteMarker} state=${state} /><${PeerActionHost} snapshot=${dashboardState.snapshot} />`, remoteHost);
  render(html`<${TopLevelBar} state=${state} navigate=${navigate} />`, barHost);
  render(html`<${FleetMap} state=${state} actions=${actions} navigate=${navigate} timers=${timers} />`, mapHost);
  registerCleanup(() => { render(null, chipsHost); render(null, barHost); render(null, mapHost); if (remoteHost) render(null, remoteHost); if (bannerHost) render(null, bannerHost); });
}
