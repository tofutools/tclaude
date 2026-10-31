import { Fragment, h, render } from 'preact';
import { openUsageRulesManager } from './modal-usage-rules.js';
import { FleetUsageChip, peerManagesUsage } from './usage-profile-chip.js';
import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import htm from 'htm';
import { NodeUpdateDialog } from './node-update.js';
import { RequestAccessDialog, USAGE_PERM } from './peer-access.js';
import { PeerActionHost } from './peer-action.js';
import { shellConfirm, shellToast } from './shell-state.js';
import { LOCAL_COLOR, STATUS_POLL_MS, cardView, fmtAge, hubOffReason, linkView, nodeColor, nodeHref, peerViewSummary, pollDelay, remoteHealthView, remoteNodeID, staggerOffset, switchOrder } from './fleet-model.js';
import { FUSED_TABS, filterNodes, fleetNodes, fusedParam, isTicked, selectorKind, tickedNodes, toggleNode, withFused } from './fleet-scope.js';
import { shortFingerprint, shortID } from './fleet-admin-model.js';
import { dashboardState } from './snapshot-store.js';
import { TOP_LEVEL_TABS } from './fleet-state.js';
import { requestFleetPage } from './fleet-move-drop.js';
import { fleetAdminLocation } from './fleet-admin-page.js';
import { isModifiedClick } from './helpers.js';
import { toPath } from './nav-history-core.js';
import { LockReason } from './lock-reason.js';
import { nodeSnapshot } from './node-api.js';
import { holdsText, useDismissablePopover } from './hover-popover.js';

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

// defaultSwitchNode shows node id's per-node view ('' = this node), on path
// when given, else on the page's own path. It is a page navigation:
// remote-node.js routes the per-node API only from page load, so one node's
// data can never be painted under another node's marker.
function defaultSwitchNode(id, path) {
  const loc = globalThis.location;
  globalThis.location.assign(nodeHref(id, path ? { pathname: path, search: loc.search } : loc));
}

// openNodeView opens a node's per-node view from elsewhere in the dashboard,
// e.g. a linked group's federation marker. A node alone leaves any fused view
// (a fused set is always this node's page; remote-node.js).
export function openNodeView(id, loc = globalThis.location, assign = (url) => globalThis.location.assign(url)) {
  assign(nodeHref(id, { pathname: loc.pathname, search: withFused(loc.search, null) }));
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
// view or a peer's view. From a top-level view (the map, Fleet config) the
// other node opens on tab, by default the per-node tab the operator left:
// keeping the page's path would load that node's map instead.
function pickNode(state, id, { navigate, remote, switchNode, tab }) {
  const fleet = state.fleet.value;
  const selfID = fleet?.self.id ?? state.node.value?.self.id ?? '';
  if (!fleet && id !== selfID) return;
  const wasFused = !!state.fused.value;
  state.setFused(null);
  writeScopeURL(null);
  if (id === (remote || selfID)) {
    if (state.view.value.topLevel || !wasFused) navigate(state.lastLocalTab());
  } else if (state.view.value.topLevel) switchNode(id === selfID ? '' : id, toPath({ tab: tab || state.lastLocalTab() }));
  else switchNode(id === selfID ? '' : id);
}

// POPOVER_LEAVE_MS is how long a node popover stays once the pointer has
// left its chip and popover; it then fades out over POPOVER_FADE_MS (the
// .node-pop.closing transition), focus or not.
export const POPOVER_LEAVE_MS = 1000;
const POPOVER_FADE_MS = 200;

// usePopover closes a popover on a click outside it, Escape, or a while after
// the pointer leaves it (hover's handlers go on the root). The popover is
// fixed under its button (the chip host clips), right-aligned to it.
function usePopover(rootRef, { timers = globalThis } = {}) {
  const [open, setOpenRaw] = useState(false);
  const [closing, setClosing] = useState(false);
  const [pos, setPos] = useState(null);
  const leaveTimer = useRef(0);
  const cancelLeave = () => { timers.clearTimeout(leaveTimer.current); leaveTimer.current = 0; setClosing(false); };
  const setOpen = (next) => {
    cancelLeave();
    const r = next && rootRef.current?.getBoundingClientRect?.();
    if (r) setPos({ top: Math.round(r.bottom + 6), right: Math.max(8, Math.round((globalThis.innerWidth || r.right) - r.right)) });
    setOpenRaw(next);
  };
  useEffect(() => () => timers.clearTimeout(leaveTimer.current), []);
  const hover = {
    closing,
    onMouseEnter: () => { if (leaveTimer.current || closing) cancelLeave(); },
    onMouseLeave: () => {
      if (!open) return;
      timers.clearTimeout(leaveTimer.current);
      leaveTimer.current = timers.setTimeout(() => {
        leaveTimer.current = 0;
        if (holdsText(rootRef.current)) return;
        setClosing(true);
        leaveTimer.current = timers.setTimeout(() => {
          leaveTimer.current = 0;
          setClosing(false);
          const focused = rootRef.current?.contains?.(globalThis.document?.activeElement);
          setOpenRaw(false);
          if (focused) rootRef.current.querySelector('button')?.focus();
        }, POPOVER_FADE_MS);
      }, POPOVER_LEAVE_MS);
    },
  };
  useEffect(() => {
    if (!open) return undefined;
    const onDown = (event) => { if (!rootRef.current?.contains(event.target)) setOpen(false); };
    const onKey = (event) => { if (event.key === 'Escape') { event.stopPropagation(); setOpen(false); } };
    document.addEventListener('mousedown', onDown);
    document.addEventListener('keydown', onKey, true);
    return () => { document.removeEventListener('mousedown', onDown); document.removeEventListener('keydown', onKey, true); };
  }, [open]);
  return [open, setOpen, pos ? `top:${pos.top}px;right:${pos.right}px` : '', hover];
}

// closedNode says why an offline peer cannot be opened, or '' when it can:
// its dashboard is served through the peer, so there is nothing to show until
// it reconnects. The node already on screen stays pickable (it only leaves a
// fused view).
function closedNode(node, pageNode) {
  if (node.local || node.online || node.id === pageNode) return '';
  return `${node.name} is ${nodeMeta(node)}: its dashboard opens again once it reconnects`;
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
  const [open, setOpen, popStyle, hover] = usePopover(rootRef);
  const [query, setQuery] = useState('');
  const [hl, setHl] = useState(0);
  useEffect(() => { if (open) { setQuery(''); setHl(0); inputRef.current?.focus(); } }, [open]);
  const shown = fleetNodes(fleet).find((n) => n.id === pageNode) || fleet.self;
  const list = filterNodes(fleet, query);
  const pick = (node) => { if (closedNode(node, pageNode)) return; setOpen(false); onPick(node.id); };
  const onKey = (event) => {
    if (event.key === 'ArrowDown') { event.preventDefault(); setHl((i) => Math.min(list.length - 1, i + 1)); }
    else if (event.key === 'ArrowUp') { event.preventDefault(); setHl((i) => Math.max(0, i - 1)); }
    else if (event.key === 'Enter' && list[hl]) { event.preventDefault(); pick(list[hl]); }
  };
  const row = (node, i) => html`<button type="button" key=${node.id} role="option" aria-selected=${i === hl ? 'true' : 'false'}
    class=${`node-pop-row${node.online || node.local ? '' : ' offline'}${i === hl ? ' hl' : ''}${!fused && node.id === pageNode ? ' sel' : ''}`}
    style=${`--nc:${node.color}`} disabled=${!!closedNode(node, pageNode)} title=${closedNode(node, pageNode) || undefined}
    onMouseEnter=${() => setHl(i)} onClick=${() => pick(node)}>
    <span class="node-pop-dot" aria-hidden="true"></span><span class="node-pop-name">${node.local ? '⌂ ' : ''}${node.name}</span>
    <span class="node-pop-meta">${nodeMeta(node)}${!fused && node.id === pageNode ? ' · shown' : ''}</span>
  </button>`;
  const self = list[0]?.local ? list[0] : null;
  return html`<span class="node-pick" ref=${rootRef} onMouseEnter=${hover.onMouseEnter} onMouseLeave=${hover.onMouseLeave}>
    <button type="button" class=${`node-chip node-pick-btn${fused ? (isTicked(fused, shown.id) ? ' ticked' : '') : ' active'}${shown.local || shown.online ? '' : ' offline'}`} style=${`--nc:${shown.color}`}
      aria-haspopup="listbox" aria-expanded=${open ? 'true' : 'false'} aria-label=${`Node: ${shown.name}. Choose a node`} title=${`${shown.name}: pick a node to show`}
      onClick=${() => setOpen(!open)}>
      ${shown.local && html`<span class="node-chip-home" aria-hidden="true">⌂</span>`}<span class="node-chip-name">${shown.name}</span><span class="node-pick-caret" aria-hidden="true">▾</span><span class="node-chip-dot"></span>
    </button>
    ${open && html`<div class=${`node-pop${hover.closing ? ' closing' : ''}`} role="dialog" aria-label="Choose a node" style=${popStyle}>
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
  const [open, setOpen, popStyle, hover] = usePopover(rootRef);
  const [query, setQuery] = useState('');
  useEffect(() => { if (open) rootRef.current?.querySelector('.node-pop-filter input')?.focus(); }, [open]);
  const nodes = fleetNodes(fleet);
  const ticked = fused ? tickedNodes(fleet, fused) : nodes;
  const list = selectorKind(fleet) === 'dropdown' ? filterNodes(fleet, query) : nodes;
  const alone = nodes.length < 2;
  const toggle = (id) => {
    if (alone) return;
    const next = toggleNode(fleet, fused || 'all', id);
    // Unticking down to one offline peer would open it alone: keep the set.
    if (next.only !== undefined) {
      const only = nodes.find((n) => n.id === next.only);
      if (only && closedNode(only, pageNode)) return;
      setOpen(false); onPick(next.only);
    } else onFuse(next);
  };
  const onButton = () => {
    if (alone) setOpen(!open);
    else if (!fused) { onFuse('all'); setOpen(true); } else setOpen(!open);
  };
  return html`<span class="node-pick" ref=${rootRef} onMouseEnter=${hover.onMouseEnter} onMouseLeave=${hover.onMouseLeave}>
    <button type="button" class=${`node-chip node-fuse${fused ? ' active' : ''}`} aria-haspopup="dialog" aria-expanded=${open ? 'true' : 'false'}
      aria-label=${fused ? `Fused view of ${ticked.length} of ${nodes.length} nodes. Change the nodes` : 'Show several nodes together'}
      title=${fused ? `Groups and Terminals show ${ticked.length} of ${nodes.length} nodes` : alone ? NO_PEERS : 'Show several nodes together in Groups and Terminals'} onClick=${onButton}>
      <span class="node-fuse-stack" aria-hidden="true">${nodes.slice(0, 4).map((n) => html`<b key=${n.id} class=${isTicked(fused || 'all', n.id) ? '' : 'off'} style=${`--c:${n.color}`}></b>`)}</span>
      <span class="node-chip-name">${fused ? `${ticked.length}/${nodes.length}` : 'nodes'}</span><span class="node-pick-caret" aria-hidden="true">▾</span>
    </button>
    ${open && html`<div class=${`node-pop node-fuse-pop${hover.closing ? ' closing' : ''}`} role="dialog" aria-label="Fuse nodes" style=${popStyle}>
      <div class="node-pop-head"><span>Fuse nodes</span>${!alone && fused !== 'all' && html`<button type="button" class="node-pop-link" data-fuse="all" onClick=${() => onFuse('all')}>all</button>`}</div>
      ${list !== nodes && html`<label class="node-pop-filter"><input type="text" value=${query} placeholder="type to filter" aria-label="Filter nodes" onInput=${(e) => setQuery(e.currentTarget.value)} /></label>`}
      ${list.map((node) => html`<div key=${node.id} class=${`node-pop-row${node.online || node.local ? '' : ' offline'}`} style=${`--nc:${node.color}`}>
        <input type="checkbox" checked=${isTicked(fused || 'all', node.id)} disabled=${alone} aria-label=${`Include ${node.name}`} data-fuse-node=${node.id} onChange=${() => toggle(node.id)} />
        <span class="node-pop-swatch" aria-hidden="true"></span>
        <button type="button" class="node-pop-name" disabled=${!!closedNode(node, pageNode)} title=${closedNode(node, pageNode) || `Show ${node.name} alone`} onClick=${() => { setOpen(false); onPick(node.id); }}>${node.local ? '⌂ ' : ''}${node.name}</button>
        <button type="button" class="node-pop-only" tabindex="-1" disabled=${!!closedNode(node, pageNode)} title=${closedNode(node, pageNode) || undefined} onClick=${() => { setOpen(false); onPick(node.id); }}>only</button>
        <span class="node-pop-meta">${node.local ? '' : html`<span class="node-pop-dot" aria-hidden="true"></span>`}${nodeMeta(node)}</span>
      </div>`)}
      ${alone ? html`<div class="node-pop-foot">${NO_PEERS}</div>` : html`<div class="node-pop-foot"><b>Groups</b>, <b>Terminals</b> and <b>Messages</b> show the ticked nodes together, as this node's page. Other tabs show this node: <b>⌂ ${fleet.self.name}</b>.</div>`}
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
// a node that was never set up says so on the chip. It lights up while the
// hub is connected, with a status dot like a node's; a node set up but not
// connected gets a grey dot.
function FleetChip({ state, node, off, remote, navigate }) {
  const current = state.view.value;
  const mapOK = !off && !!current.mapFleet;
  if (remote && !mapOK) return null;
  const active = current.topLevel;
  const target = remote || (mapOK && state.lastFleetTab() !== 'fleet-admin') ? 'map' : 'fleet-admin';
  const hint = !remote && !!node && !node.configured && !current.setupSeen;
  const connected = !off;
  const title = off
    ? `Fleet: ${node?.configured ? 'the hub connection and peers' : 'link this node with your other machines or colleagues'} (${off.replace(/^Not connected to a federation hub: /, 'not connected: ')})`
    : !current.fleet ? 'Fleet: hub connected · trust a peer to link nodes'
      : `Fleet: hub connected · every linked node (${target === 'map' ? 'the map' : 'Fleet config'})`;
  // The separator comes with the chip, so a row without it ends cleanly.
  return html`<span class="node-chips-sep" aria-hidden="true"></span>
  <button type="button" class=${`node-chip fleet-entry${active ? ' active' : ''}${connected ? ' connected' : ' offline'}`} data-fleet-entry data-fleet-target=${target}
    aria-current=${active ? 'page' : undefined} aria-label=${`Fleet${hint ? ': set up' : ''}`} title=${title}
    onClick=${() => { if (!active) navigate(target); }}>
    <${MapGlyph} /><span class="node-chip-name">Fleet</span>${hint && html`<span class="node-chip-hint">set up</span>`}
    ${(connected || node?.configured) && html`<span class="node-chip-dot"></span>`}
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
    // A fused view is this node's page: from a peer's page, go home with it.
    if (value && remote) { switchNode('', state.view.value.topLevel ? toPath({ tab: 'groups' }) : undefined); return; }
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
    ${fleet.peers.map((peer, i) => html`<button key=${peer.id} type="button" class=${cls(peer)} style=${`--nc:${peer.color}`} disabled=${!!closedNode(peer, pageNode)}
      aria-current=${isCurrent(peer.id) ? 'page' : undefined} aria-label=${`${peer.name}, ${presenceLabel(peer)}, ${peer.level} peer`}
      title=${closedNode(peer, pageNode) || `${peer.name}: ${presenceLabel(peer)} · ${peer.level} peer · open its dashboard${i < 8 ? ` · Alt+${i + 2}` : ''}`} onClick=${() => onPick(peer.id)}>
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
// shown node's colour. It drops a fused set once a status read says no linked
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

// MACHINE_TABS hold one machine's own settings, so a fused view keeps them on
// this node. Their strip says how each ticked peer's own is reached: Access
// opens on a peer's per-node view under unrestricted trust; Config is never
// served to a peer (each node's is set on that node).
const MACHINE_TABS = Object.freeze({ access: 'unrestricted', config: 'never' });

// peerTabReach is how the strip offers a ticked peer's own copy of a machine
// tab: { href } to open it, or { why } it cannot be opened from here. theirs
// is the trust the peer grants this node (peer.level is the other direction):
// a peer known to grant less than unrestricted is locked; one not known yet
// is offered, and its page says if it refuses.
function peerTabReach(peer, tab, loc, theirs = '') {
  if (MACHINE_TABS[tab] === 'never') return { why: `${peer.name}'s ${tab} is set on ${peer.name} itself` };
  if (!peer.online) return { why: `${peer.name} is offline` };
  if (theirs && theirs !== 'unrestricted') return { why: `${peer.name} does not share its ${tab} with you (needs unrestricted trust from ${peer.name})` };
  return { href: nodeHref(peer.id, { pathname: toPath({ tab }), search: withFused(loc?.search || '', null) }) };
}

// ScopeBanner tops a per-node tab while a fused view is on: that tab shows
// this node only (a fused view is always this node's page), and the strip
// says so. On a machine tab (MACHINE_TABS) it also offers each ticked peer's
// own; elsewhere a peer's own view opens from its node chip.
export function ScopeBanner({ state, doc = globalThis.document, loc = globalThis.location, peerSnapshot = nodeSnapshot }) {
  const current = state.view.value;
  const fleet = current.fleet;
  if (!fleet || !current.fused || current.topLevel || FUSED_TABS.has(current.activeTab)) return null;
  const self = fleet.self;
  const tab = current.activeTab;
  const label = doc?.querySelector?.(`nav [data-tab="${tab}"] .tab-label-regular`)?.textContent?.trim() || 'This tab';
  const peers = MACHINE_TABS[tab] ? tickedNodes(fleet, current.fused).filter((n) => !n.local) : [];
  // The fused poll's peer snapshot, else the map's node summary, carries it.
  const theirLevel = (id) => peerSnapshot(id)?.peer_view?.level || current.summaries?.[id]?.summary?.peer_view?.level || '';
  return html`<div class="scope-banner" role="status" style=${`--nc:${self.color}`}>
    <span>${label} shows this node, <span class="scope-pill" style=${`--nc:${self.color}`}>⌂ ${self.name}</span>.${peers.length ? '' : " Open another node's own view from its chip."}</span>
    ${peers.length ? html`<span class="scope-peers">${peers.map((peer) => {
      const reach = peerTabReach(peer, tab, loc, theirLevel(peer.id));
      return reach.href
        ? html`<a key=${peer.id} class="scope-pill scope-peer-link" style=${`--nc:${peer.color}`} href=${reach.href}>${peer.name}'s ${label} ↗</a>`
        : html`<span key=${peer.id} class="scope-pill scope-peer-locked" style=${`--nc:${peer.color}`}>${peer.name}<${LockReason} why=${reach.why} /></span>`;
    })}</span>` : null}
  </div>`;
}

// TopLevelBar replaces the tab strip while a top-level, multi-node view (the
// map or Fleet config) is active, and switches between the two. CSS
// hides the per-node tabs in that mode; the row keeps the same height so
// nothing below moves.
export function TopLevelBar({ state, navigate = defaultNavigate }) {
  const current = state.view.value;
  const fleet = current.mapFleet;
  if (!current.topLevel || !fleet) return null;
  const n = fleet.peers.length + 1;
  const hub = fleet.hub.state;
  const asks = remoteNodeID() ? 0 : current.strangers.filter((s) => s.request).length;
  const seg = (tab, on, label) => html`<button type="button" class=${`fleet-seg-btn${on ? ' on' : ''}`} aria-current=${on ? 'page' : undefined} onClick=${() => { if (!on) navigate(tab); }}>${label}</button>`;
  return html`<span class="fleet-toplevel-bar">
    <span class="fleet-seg" role="group" aria-label="Fleet views">${seg('map', current.mapActive, html`<${MapGlyph} /> Map`)}${seg('fleet-admin', current.adminActive, '⚙ Fleet config')}</span>
    <span class="fleet-toplevel-note">${n} node${n === 1 ? '' : 's'} · hub ${hub}${asks > 0 && html` · <span class="fleet-map-asking">🤝 ${asks} asking to be trusted</span>`}</span>
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
    ${card.version && html`<dt>tclaude</dt><dd>${card.version}${card.health && html` <span class=${`fleet-card-health ${card.health}`}>${card.health === 'current' ? '✓ current' : card.health}</span>`}${card.updateAvailable ? html` <span class="fleet-update" title=${`tclaude ${card.latestVersion} is available`}>↑ ${card.latestVersion}</span>` : ''}${onUpdate && html` <button type="button" class="fleet-link" data-fleet="node-update" onClick=${onUpdate}>${card.updateAvailable ? 'update…' : 'manage…'}</button>`}</dd>`}
    ${!card.version && card.health && html`<dt>Health</dt><dd class=${`fleet-card-health ${card.health}`}>${card.health === 'current' ? '✓ current' : card.health}</dd>`}
    ${card.omitted.length > 0 && html`<dt>Not shared</dt><dd class="muted" title=${card.omitted.join(', ')}>${card.omitted.length} feature${card.omitted.length === 1 ? '' : 's'}</dd>`}
  </dl>`;
}

// TrustRows names both directions of a peer's trust on its card. The card's
// other rows (and "Hidden from you") are what the peer shares with this node,
// so its own trust level is never shown as a bare "unrestricted peer" badge.
// Your side links to where it is set: the trust level in Fleet config →
// Peers, and while restricted the grants that say what the peer may use.
// configHref is the address of a Fleet config section, for the map's
// shortcuts to it.
function configHref(page, target = '') {
  return toPath(fleetAdminLocation(page, target));
}

function TrustRows({ node, card, selfName, onConfigure }) {
  const link = linkView(selfName, node, card);
  const theirs = link.out.level;
  const restricted = link.in.level !== 'unrestricted';
  return html`<dl class="fleet-card-body fleet-card-trust">
    <dt>You trust it</dt><dd><span class=${`fleet-trust ${link.in.level}`} title=${`What ${node.name} may do on ${selfName}. You set this in Fleet config.`}>${restricted ? 'restricted' : '⚠ unrestricted'}</span>${onConfigure && html`<span class="fleet-card-acts">${restricted && html`<a class="fleet-chip-link" data-fleet="grants" href=${configHref('grants', node.id)} title=${`Edit what ${node.name} may use on ${selfName} (Fleet config → Peer grants)`} onClick=${onConfigure('grants', node.id)}>grants…</a>`}<a class="fleet-chip-link" data-fleet="trust-level" href=${configHref('peers')} title=${`Change how ${selfName} trusts ${node.name} (Fleet config → Peers)`} onClick=${onConfigure('peers')}>trust…</a></span>`}</dd>
    <dt>It trusts you</dt><dd class=${`fleet-trust ${theirs || 'unknown'}`} title=${`What ${selfName} may do on ${node.name}. ${node.name}'s operator sets this.`}>${theirs || 'not reported'}</dd>
  </dl>`;
}

// OfflineNoticeRow is an offline peer's operator-notice state: when the next
// Messages notice is due, with a button to silence this disconnection.
function OfflineNoticeRow({ notice, now, onSilence }) {
  if (notice.silenced) return html`<div class="fleet-card-notice silenced" title="Notices resume if it disconnects again after reconnecting">🔕 Offline notices silenced until it reconnects</div>`;
  const next = notice.nextAt != null ? `next notice in ${fmtAge(Math.max(0, notice.nextAt - now))}` : '';
  const sent = notice.notices > 0 ? `${notice.notices} notice${notice.notices === 1 ? '' : 's'} sent` : 'no notice sent yet';
  return html`<div class="fleet-card-notice" title="Offline notices go to Messages after 5 min, 30 min, 4 h, then daily">🔔 ${sent}${next && ` · ${next}`}${onSilence && html` <button type="button" class="fleet-link" data-fleet="silence-offline" onClick=${onSilence}>silence</button>`}</div>`;
}

// OfflineAlertsRow switches a peer's offline/back notices: "notify" escalates
// to Messages, "part-time" never does, for a node that is often off on purpose.
function OfflineAlertsRow({ node, onOfflineAlerts }) {
  const seg = (on, label, title) => {
    const active = node.partTime !== on;
    return html`<button type="button" class=${active ? 'on' : ''} aria-pressed=${active ? 'true' : 'false'} title=${title}
      data-fleet=${on ? 'offline-alerts-notify' : 'offline-alerts-part-time'} onClick=${() => { if (!active) onOfflineAlerts(on); }}>${label}</button>`;
  };
  return html`<dl class="fleet-card-body fleet-card-alerts">
    <dt>Offline alerts</dt><dd><span class="fleet-card-seg" role="group" aria-label=${`Offline alerts for ${node.name}`}>${seg(true, '🔔 notify', 'Messages after 5 min, 30 min, 4 h offline, then daily, and when it is back')}${seg(false, '💤 part-time', 'Often off on purpose: no offline or back-online messages')}</span></dd>
  </dl>`;
}

function NodeCard({ node, card, selfName, focused, onOpen, onUpdate, onSilence, onOfflineAlerts, onConfigure, now, cardRef, shown }) {
  const kind = node.local ? 'this node' : 'peer';
  const asleep = node.partTime && card.presence === 'offline';
  const presence = card.presence === 'online' ? (node.local ? 'online' : 'online') : asleep ? 'asleep' : card.presence === 'offline' ? 'offline' : card.presence === 'error' ? 'error' : '…';
  return html`<article ref=${cardRef} data-node-id=${node.id} class=${`fleet-card${node.local ? ' local' : ''}${card.stale || card.presence === 'offline' ? ' stale' : ''}${node.partTime ? ' part-time' : ''}${focused ? ' focused' : ''}`}
    style=${`--nc:${node.color}`} aria-label=${`${node.name} node`}>
    <div class="fleet-card-head"><span class="fleet-card-name">${node.local ? '⌂ ' : ''}${node.name}</span>
      <span class="fleet-card-kind">${kind}</span>${node.partTime && html`<span class="fleet-card-kind part-time" title="Often off on purpose: no offline or back-online messages">part-time</span>`}
      <span class=${`fleet-card-presence ${asleep ? 'asleep' : card.presence}`}><i aria-hidden="true"></i>${presence}${card.stale && card.ageMs != null ? html` <span class="muted">· data ${fmtAge(card.ageMs)} old</span>` : ''}</span>
      ${onConfigure && html`<a class="fleet-card-cfg" data-fleet="node-config" href=${configHref(node.local ? '' : 'peers')} aria-label=${node.local ? 'This node in Fleet config' : `${node.name} in Fleet config`} title=${node.local ? 'Fleet config' : `${node.name} in Fleet config → Peers`} onClick=${onConfigure(node.local ? '' : 'peers')}>⚙</a>`}</div>
    <${CardRows} card=${card} node=${node} onUpdate=${onUpdate} />
    ${!node.local && html`<${TrustRows} node=${node} card=${card} selfName=${selfName} onConfigure=${onConfigure} />`}
    ${!node.local && onOfflineAlerts && html`<${OfflineAlertsRow} node=${node} onOfflineAlerts=${onOfflineAlerts} />`}
    ${!node.local && !node.partTime && node.offlineNotice && !node.online && html`<${OfflineNoticeRow} notice=${node.offlineNotice} now=${now ?? Date.now()} onSilence=${onSilence} />`}
    ${node.keyTransition && html`<div class=${`fleet-card-key ${node.keyTransition.state}`} title="See Fleet config → Peers">${node.keyTransition.state === 'conflict' ? '⚠ competing new signing keys: needs recovery (Fleet config → Peers)' : '🔑 new signing key pending: not yet accepted'}</div>`}
    ${onOpen && html`<div class="fleet-card-foot"><button type="button" class=${shown ? 'primary' : ''} onClick=${onOpen}>${shown ? 'Back to its dashboard' : 'Open dashboard'}</button></div>`}
  </article>`;
}

// HubCard is the federation hub between this node and its peers: every link
// to a peer is relayed through it.
// admin is whether this node holds hub admin (null while unknown); Manage
// opens Fleet config → Hub either way, where a non-admin can claim.
function HubCard({ hub, now, admin, onConfigure }) {
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
    ${onConfigure && html`<div class="fleet-card-hub-admin">${admin === true ? html`<span class="fleet-hub-admin yes">★ hub admin</span>` : admin === false ? html`<span class="muted">not a hub admin</span>` : ''}
      <a class="fleet-chip-link" data-fleet="hub-config" href=${configHref('hub')} title="Fleet config → Hub" onClick=${onConfigure('hub')}>${admin === true ? 'Manage hub…' : 'Hub page…'}</a></div>`}
  </article>`;
}

// terse drops a label's hidden count: the peer's card shows it.
const terse = (short) => short.split(' · ')[0];

// LinkBadge labels a peer's link on one line with both directions of trust:
// → what this node may do on the peer, ← what the peer may do here. Each
// names its nodes in its tooltip, and the popover spells out who can do what.
function LinkBadge({ link, at, open, onToggle, onConfigure }) {
  const sentence = (dir, ours) => {
    if (dir.level === 'unrestricted') return html`<b>${dir.from} has full peer access to ${dir.to}</b>: every permission a peer can be granted. Administration stays local.`;
    if (dir.level === 'restricted') return ours ? html`<b>${dir.from} can use only what ${dir.to} grants it.</b>` : html`<b>${dir.from} can use only what ${dir.to} shares.</b>`;
    return html`<b>${dir.to} has not reported how it trusts ${dir.from}</b> (it may be unreachable, still loading, or on an older tclaude).`;
  };
  const tag = (level) => html`<span class=${`fleet-link-tag ${level || 'unknown'}`}>${level || 'unknown'}</span>`;
  return html`<span class=${`fleet-edge-link${open ? ' open' : ''}`} style=${`left:${at.x}px;top:${at.y}px`}>
    <button type="button" class=${`fleet-edge-label${link.asymmetric ? ' asymmetric' : ''}`} aria-haspopup="dialog" aria-expanded=${open ? 'true' : 'false'}
      aria-label=${`Trust between ${link.in.from} and ${link.in.to}`} onClick=${onToggle}>
      <span class=${`fleet-link-row ${link.out.level || 'unknown'}`} title=${`${link.out.from} → ${link.out.to}: ${link.out.short}`}><span class="fleet-link-dir">→</span>${link.out.level === 'unrestricted' ? '⚠ ' : ''}${terse(link.out.short)}</span>
      <span class=${`fleet-link-row ${link.in.level}`} title=${`${link.in.from} → ${link.in.to}: ${link.in.short}`}><span class="fleet-link-dir">←</span>${link.in.level === 'unrestricted' ? '⚠ ' : ''}${terse(link.in.short)}</span>
    </button>
    ${open && html`<div class="fleet-link-pop" role="dialog" aria-label=${`Trust between ${link.in.from} and ${link.in.to}`}>
      <div class="fleet-link-pop-head">${link.asymmetric ? '⇄ Asymmetric link: each side trusts the other differently' : `Trust between ${link.in.from} and ${link.in.to}`}</div>
      <p>${tag(link.in.level)} ${sentence(link.in, true)} You set this. <a class="fleet-link-act" href=${configHref('peers')} onClick=${onConfigure}>Change in Fleet config…</a></p>
      <p>${tag(link.out.level)} ${sentence(link.out, false)}${link.out.level ? ` ${link.out.to}'s operator set this.` : ''}${link.out.hidden.length > 0 ? html` <span class="muted">Hidden from you: ${link.out.hidden.join(', ')}.</span>` : ''}</p>
    </div>`}
  </span>`;
}

// TRUST_NOTE_MAX mirrors proto.MaxTrustRequestNote.
const TRUST_NOTE_MAX = 200;
// ASK_COOLDOWN_MS mirrors the daemon's one request per instance a minute.
const ASK_COOLDOWN_MS = 60000;

// TrustAsk is the card's request row: a request this instance sent (its note
// is its own words, shown as such), or this node's own "Ask to be trusted…"
// with an optional note.
function TrustAsk({ row, now, onAsk, onDismiss }) {
  const [open, setOpen] = useState(false);
  const [note, setNote] = useState('');
  const [busy, setBusy] = useState(false);
  const run = (fn) => async () => { if (busy) return; setBusy(true); try { if (await fn()) { setOpen(false); setNote(''); } } finally { setBusy(false); } };
  const req = row.request;
  const asked = row.requestedAt != null ? Math.max(0, now - row.requestedAt) : null;
  return html`${req && html`<div class="fleet-card-ask" data-fleet="trust-request">
      <b>🤝 ${req.trustsYou ? 'Trusts this node, asks to be trusted back' : 'Asks to be trusted'}</b>${req.receivedAt != null && html` <span class="muted">· ${fmtAge(Math.max(0, now - req.receivedAt))} ago</span>`}
      ${req.note && html`<div class="fleet-card-ask-note" title="Written by the instance itself">“${req.note}”</div>`}
    </div>`}
    ${open && html`<div class="fleet-card-ask-form">
      <input type="text" data-fleet="ask-note" maxlength=${TRUST_NOTE_MAX} placeholder="optional note, e.g. who you are" value=${note} autocomplete="off" spellcheck="false"
        onInput=${(e) => setNote(e.currentTarget.value)} onKeyDown=${(e) => { if (e.key === 'Enter' && !e.isComposing) { e.preventDefault(); run(() => onAsk(note))(); } else if (e.key === 'Escape') setOpen(false); }} />
      <button type="button" class="primary" data-fleet="ask-send" disabled=${busy} onClick=${run(() => onAsk(note))}>${busy ? 'Sending…' : 'Send'}</button>
      <button type="button" disabled=${busy} onClick=${() => setOpen(false)}>Cancel</button>
    </div>`}
    ${!open && html`<div class="fleet-card-foot">
      <button type="button" class="primary" data-fleet="trust" title="Compare its fingerprint with its operator, then pick a trust level" onClick=${() => row.onTrust()}>Trust…</button>
      ${req
        ? html`<button type="button" data-fleet="dismiss" title="Forget this request; the instance stays visible on the hub" disabled=${busy} onClick=${run(onDismiss)}>Dismiss</button>`
        : html`<button type="button" data-fleet="ask" title="Ask its operator to trust this node; they still compare fingerprints" disabled=${asked != null && asked < ASK_COOLDOWN_MS} onClick=${() => setOpen(true)}>Ask to be trusted…</button>`}
      ${asked != null && html`<span class="muted fleet-card-asked">asked ${fmtAge(asked)} ago</span>`}
    </div>`}`;
}

// StrangerCard is a hub-visible instance this node has not trusted. Its name
// is the instance's own claim, so the card leads with the fingerprint, and
// Trust… opens the same fingerprint-checked dialog as Fleet config → Peers.
function StrangerCard({ row, now, onTrust, onAsk, onDismiss }) {
  const seen = row.online ? 'online' : row.lastSeen ? `seen ${fmtAge(Math.max(0, now - Date.parse(row.lastSeen)))} ago` : 'offline';
  return html`<article data-node-id=${row.id} class=${`fleet-card untrusted${row.online ? '' : ' stale'}${row.request ? ' asks' : ''}`} aria-label=${`${row.label} untrusted instance`}>
    <div class="fleet-card-head"><span class="fleet-card-name" title="Name reported by the instance itself">${row.name || html`<span class="muted">unnamed</span>`}</span>
      <span class="fleet-card-kind">not trusted</span>
      <span class=${`fleet-card-presence ${row.online ? 'online' : 'offline'}`}><i aria-hidden="true"></i>${seen}</span></div>
    <dl class="fleet-card-body">
      <dt>Instance</dt><dd title=${row.id}><code>${shortID(row.id)}</code></dd>
      <dt>Fingerprint</dt><dd title=${row.fingerprint}><code>${shortFingerprint(row.fingerprint) || '—'}</code></dd>
    </dl>
    <${TrustAsk} row=${{ ...row, onTrust }} now=${now} onAsk=${onAsk} onDismiss=${onDismiss} />
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
// Untrusted instances share one link to the section that encloses them.
// scale is the map's zoom: rects are measured on screen, the edges drawn in
// the map's own coordinates.
export function measureEdges(mapEl, scale = 1) {
  if (!mapEl) return [];
  const box = mapEl.getBoundingClientRect();
  const local = mapEl.querySelector('.fleet-card.local');
  if (!local) return [];
  const side = (el, right) => { const r = el.getBoundingClientRect(); return { x: ((right ? r.right : r.left) - box.left) / scale, y: (r.top - box.top + r.height / 2) / scale }; };
  const edge = (id, from, to) => ({ id, d: edgePath(from, to), mid: { x: (from.x + to.x) / 2, y: (from.y + to.y) / 2 } });
  const hub = mapEl.querySelector('.fleet-card.hub');
  const from = hub ? side(hub, true) : side(local, true);
  const peers = [...mapEl.querySelectorAll('.fleet-card:not(.local):not(.hub):not(.untrusted)')].map((el) => edge(el.dataset.nodeId, from, side(el, false)));
  const strangers = mapEl.querySelector('.fleet-map-strangers');
  if (strangers) peers.push(edge(':strangers', from, side(strangers, false)));
  return hub ? [edge(':hub', side(local, true), side(hub, false)), ...peers] : peers;
}

// defaultLoadHubStatus reads whether this node is a hub admin, through Fleet
// config's actions, loaded on first use like the trust dialog.
async function defaultLoadHubStatus() {
  const { createFleetAdminActions } = await import('./fleet-admin-actions.js');
  return createFleetAdminActions().hubStatus();
}

// The map zooms between ZOOM_MIN and ZOOM_MAX; buttons step by ZOOM_STEP.
const ZOOM_MIN = 0.25;
const ZOOM_MAX = 2;
const ZOOM_STEP = 1.2;
const clampZoom = (z) => Math.min(ZOOM_MAX, Math.max(ZOOM_MIN, z));

// zoomAt scales view by k about the stage point (px, py), keeping that point
// under the cursor.
export function zoomAt(view, k, px, py) {
  const s = clampZoom(view.s * k);
  const r = s / view.s;
  return { s, x: px - (px - view.x) * r, y: py - (py - view.y) * r };
}

// fitView scales content of size (w, h) to fit a (sw, sh) stage, never past
// 100%, centred.
export function fitView(w, h, sw, sh) {
  if (!w || !h || !sw || !sh) return { s: 1, x: 0, y: 0 };
  const s = clampZoom(Math.min(1, sw / w, sh / h));
  return { s, x: Math.max(0, (sw - w * s) / 2), y: Math.max(0, (sh - h * s) / 2) };
}

// PAN_EXEMPT are the targets a press on starts no pan: cards (their text stays
// selectable) and controls.
const PAN_EXEMPT = '.fleet-card, .fleet-edge-link, button, a, input, label, select, textarea';

// useZoomPan zooms the map with the wheel (a trackpad pinch arrives as a
// ctrl+wheel), a two-finger pinch, or the toolbar; a drag on the background
// pans. touched flips on the first gesture, so an automatic fit never undoes
// the operator's own view. ready says the stage is rendered.
function useZoomPan(stageRef, mapRef, ready) {
  const [view, setViewRaw] = useState({ s: 1, x: 0, y: 0 });
  const viewRef = useRef(view);
  const touched = useRef(false);
  const setView = (next, byUser = true) => { if (byUser) touched.current = true; viewRef.current = next; setViewRaw(next); };
  const center = () => { const el = stageRef.current; return [(el?.clientWidth || 0) / 2, (el?.clientHeight || 0) / 2]; };
  const zoomBy = (k) => { const [cx, cy] = center(); setView(zoomAt(viewRef.current, k, cx, cy)); };
  const fit = (byUser = true) => {
    const map = mapRef.current; const stage = stageRef.current;
    if (!map || !stage) return;
    setView(fitView(map.scrollWidth, map.scrollHeight, stage.clientWidth, stage.clientHeight), byUser);
  };
  const reset = () => setView({ s: 1, x: 0, y: 0 });
  // fitIfOverflowing fits a map that outgrows its stage until the operator
  // zooms or pans themselves.
  const fitIfOverflowing = () => {
    const map = mapRef.current; const stage = stageRef.current;
    if (touched.current || !map || !stage || globalThis.matchMedia?.('(max-width: 720px)').matches) return;
    if (map.scrollWidth > stage.clientWidth + 1 || map.scrollHeight > stage.clientHeight + 1) fit(false);
  };
  useEffect(() => {
    const stage = stageRef.current;
    if (!stage) return undefined;
    const local = (e) => { const r = stage.getBoundingClientRect(); return [e.clientX - r.left, e.clientY - r.top]; };
    // The stacked phone layout (dashboard.css, max-width 720px) scrolls with the page.
    const stacked = () => !!globalThis.matchMedia?.('(max-width: 720px)').matches;
    const onWheel = (e) => {
      if (stacked()) return;
      e.preventDefault();
      const dy = e.deltaY * (e.deltaMode === 1 ? 16 : e.deltaMode === 2 ? 400 : 1);
      const [px, py] = local(e);
      setView(zoomAt(viewRef.current, Math.exp(-dy * (e.ctrlKey ? 0.01 : 0.0015)), px, py));
    };
    // pointers holds each active pointer's last stage position.
    const pointers = new Map();
    let pinch = 0;
    const spread = () => { const [a, b] = [...pointers.values()]; return [Math.hypot(a[0] - b[0], a[1] - b[1]), (a[0] + b[0]) / 2, (a[1] + b[1]) / 2]; };
    const onDown = (e) => {
      if ((e.button !== 0 && e.pointerType === 'mouse') || stacked()) return;
      if (!pointers.size && e.target?.closest?.(PAN_EXEMPT)) return;
      if (pointers.size >= 2) return;
      pointers.set(e.pointerId, local(e));
      stage.setPointerCapture?.(e.pointerId);
      if (pointers.size === 2) pinch = spread()[0];
      stage.classList.add('panning');
    };
    const onMove = (e) => {
      const last = pointers.get(e.pointerId);
      if (!last) return;
      const p = local(e);
      if (pointers.size === 2) {
        const [, mx0, my0] = spread();
        pointers.set(e.pointerId, p);
        const [d, mx, my] = spread();
        const v = viewRef.current;
        const zoomed = pinch > 0 ? zoomAt(v, d / pinch, mx, my) : v;
        pinch = d;
        setView({ ...zoomed, x: zoomed.x + mx - mx0, y: zoomed.y + my - my0 });
        return;
      }
      pointers.set(e.pointerId, p);
      const v = viewRef.current;
      setView({ ...v, x: v.x + p[0] - last[0], y: v.y + p[1] - last[1] });
    };
    const onUp = (e) => {
      if (!pointers.delete(e.pointerId)) return;
      pinch = pointers.size === 2 ? spread()[0] : 0;
      if (!pointers.size) stage.classList.remove('panning');
    };
    stage.addEventListener('wheel', onWheel, { passive: false });
    stage.addEventListener('pointerdown', onDown);
    stage.addEventListener('pointermove', onMove);
    stage.addEventListener('pointerup', onUp);
    stage.addEventListener('pointercancel', onUp);
    return () => {
      stage.removeEventListener('wheel', onWheel);
      stage.removeEventListener('pointerdown', onDown);
      stage.removeEventListener('pointermove', onMove);
      stage.removeEventListener('pointerup', onUp);
      stage.removeEventListener('pointercancel', onUp);
    };
  }, [ready]);
  return { view, viewRef, zoomBy, fit, reset, fitIfOverflowing };
}

export function FleetMap({ state, actions, navigate = defaultNavigate, timers = globalThis, now = () => Date.now(), remote = remoteNodeID(), switchNode = defaultSwitchNode, confirm = shellConfirm, toast = shellToast, updateActions, loadTrust = defaultLoadTrust, loadHubStatus = defaultLoadHubStatus }) {
  const current = state.view.value;
  const fleet = current.mapFleet;
  const mapRef = useRef(null);
  const stageRef = useRef(null);
  const frameRef = useRef(null);
  const zoom = useZoomPan(stageRef, mapRef, !!fleet);
  const showHub = current.showHub;
  const [hubAdmin, setHubAdmin] = useState(null);
  const [edges, setEdges] = useState([]);
  const [updating, setUpdating] = useState(null);
  const [linkOpen, setLinkOpen] = useState('');
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
    const update = () => { setEdges(measureEdges(mapRef.current, zoom.viewRef.current.s)); zoom.fitIfOverflowing(); };
    update();
    if (typeof ResizeObserver !== 'function' || !mapRef.current) return undefined;
    const ro = new ResizeObserver(update);
    ro.observe(mapRef.current);
    return () => ro.disconnect();
  }, [current.mapActive, fleetKey, current.summaries, fleet?.hub.state, current.strangers, showHub]);

  // The frame fills the window below the top bar and above the footer, so the
  // toolbar and legend stay on screen while the map zooms inside.
  useLayoutEffect(() => {
    if (!current.mapActive || !fleet) return undefined;
    const win = document.defaultView || globalThis;
    const fit = () => {
      const el = frameRef.current;
      if (!el || !win.innerHeight || win.matchMedia?.('(max-width: 720px)').matches) { if (el) el.style.height = ''; return; }
      const footer = document.querySelector('footer')?.offsetHeight || 0;
      el.style.height = `${Math.max(320, Math.floor(win.innerHeight - el.getBoundingClientRect().top - (win.scrollY || 0) - footer))}px`;
    };
    fit();
    win.addEventListener?.('resize', fit);
    return () => win.removeEventListener?.('resize', fit);
  }, [current.mapActive, !!fleet]);

  // The hub card says whether this node is a hub admin: read once per
  // connection while the card is shown. A peer view's map is this node's,
  // and its Fleet config lives on this node's page, so it does not ask.
  useEffect(() => {
    if (fleet?.hub.state !== 'connected') setHubAdmin(null);
    if (!current.mapActive || !fleet || remote || !showHub || fleet.hub.state !== 'connected') return undefined;
    let off = false;
    Promise.resolve().then(loadHubStatus)
      .then((s) => { if (!off) setHubAdmin(typeof s?.admin === 'boolean' ? s.admin : null); })
      .catch(() => { if (!off) setHubAdmin(null); });
    return () => { off = true; };
  }, [current.mapActive, fleet?.hub.state, showHub, remote]);

  useDismissablePopover(() => mapRef.current?.querySelector('.fleet-edge-link.open'), linkOpen, () => setLinkOpen(''));

  if (!fleet) return html`<div class="empty">Not connected to a federation hub. Set it up in ⚙ Fleet or with <code>tclaude federation</code> (see docs/federation.md).</div>`;
  const t = now();
  const peerById = new Map(fleet.peers.map((p) => [p.id, p]));
  const card = (node) => cardView(node, current.summaries[node.id], t);
  // Each card opens its node's Groups page; the node already shown returns
  // to the tab the operator left for the map.
  const open = (node) => () => pickNode(state, node.id, { navigate: () => navigate(state.lastLocalTab()), remote, switchNode, tab: 'groups' });
  // Trusting is this node's own decision, made on this node's page only.
  const strangers = remote ? [] : current.strangers;
  const toggleUntrusted = (e) => { state.setShowUntrusted(e.currentTarget.checked); actions.loadStatus?.(); };
  const trust = (row) => () => loadTrust()
    .then((loaded) => setTrusting({ row, ...loaded }))
    .catch((e) => toast(`Trust: ${e?.message || e}`, true));
  // Trusting an instance that does not trust this node yet also asks it to
  // trust back; its operator still compares fingerprints on their side.
  const trusted = async (msg) => {
    const row = trusting?.row;
    setTrusting(null);
    let back = '';
    if (row && !row.request?.trustsYou && actions.requestTrust) {
      try { await actions.requestTrust(row.id); back = ` · asked ${row.label} to trust this node back`; } catch { /* the trust itself succeeded */ }
    }
    toast(msg + back, false);
    actions.loadStatus?.();
  };
  // Each returns true on success so the card's form closes.
  const ask = (row) => async (note) => {
    try { await actions.requestTrust(row.id, note.trim()); toast(`Asked ${row.label} to trust this node`, false); actions.loadStatus?.(); return true; } catch (e) { toast(`Ask ${row.label}: ${e?.message || e}`, true); return false; }
  };
  const dismiss = (row) => async () => {
    try { await actions.dismissTrustRequest(row.id); actions.loadStatus?.(); return true; } catch (e) { toast(`Dismiss: ${e?.message || e}`, true); return false; }
  };
  // Silencing is this node's own notice state; a peer view never silences.
  const silence = (peer) => (remote || !actions.silenceOffline ? undefined : async () => {
    try { await actions.silenceOffline(peer.id); toast(`Offline notices for ${peer.name} silenced until it reconnects`, false); actions.loadStatus?.(); } catch (e) { toast(`Silence ${peer.name}: ${e?.message || e}`, true); }
  });
  const offlineAlerts = (peer) => (remote || !actions.setOfflineAlerts ? undefined : async (on) => {
    try { await actions.setOfflineAlerts(peer.id, on); toast(on ? `Offline alerts for ${peer.name} on` : `${peer.name} is part-time: no offline or back-online messages`, false); actions.loadStatus?.(); } catch (e) { toast(`Offline alerts for ${peer.name}: ${e?.message || e}`, true); }
  });
  // configure opens a Fleet config section ('' = its landing page), with
  // the peer a section should open on. A peer view reaches Fleet config only
  // through a page load home, which drops the section, so it shows no links.
  // Each is a real link (configLink) so a modified or middle click opens the
  // section in a new browser tab; a plain click switches in place.
  const configure = remote ? undefined : (page, target = '') => (e) => {
    if (e && isModifiedClick(e)) return;
    e?.preventDefault?.();
    setLinkOpen(''); if (page) requestFleetPage(page, target); navigate('fleet-admin');
  };
  const configurePeers = configure ? configure('peers') : (e) => { e?.preventDefault?.(); setLinkOpen(''); navigate('fleet-admin'); };
  const asking = strangers.filter((s) => s.request).length;
  const hubState = fleet.hub.state;
  const pct = Math.round(zoom.view.s * 100);
  return html`<div class="fleet-map-frame" ref=${frameRef}>
    <div class="fleet-map-tools">
      <label class="fleet-map-check" title="Show the hub between the nodes; hidden, links run node to node"><input type="checkbox" data-fleet="show-hub" checked=${showHub} onChange=${(e) => state.setShowHub(e.currentTarget.checked)} /> Show hub</label>
      ${!showHub && html`<span class=${`fleet-map-hubstate ${hubState === 'connected' ? 'online' : ''}`}><i aria-hidden="true"></i>hub ${hubState}</span>`}
      <span class="fleet-zoom" role="group" aria-label="Zoom">
        <button type="button" aria-label="Zoom out" disabled=${zoom.view.s <= ZOOM_MIN} onClick=${() => zoom.zoomBy(1 / ZOOM_STEP)}>−</button>
        <button type="button" class="fleet-zoom-level" title="Reset to 100%" onClick=${zoom.reset}>${pct}%</button>
        <button type="button" aria-label="Zoom in" disabled=${zoom.view.s >= ZOOM_MAX} onClick=${() => zoom.zoomBy(ZOOM_STEP)}>+</button>
        <button type="button" aria-label="Fit to screen" title="Fit to screen" onClick=${() => zoom.fit()}>⤢</button>
      </span>
      <span class="fleet-map-hint">scroll or pinch to zoom · drag the background to pan</span>
      <${FleetUsageChip} className="fleet-map-usage"/>
    </div>
    <div class="fleet-map-stage" ref=${stageRef}>
    <div class="fleet-map" ref=${mapRef} style=${`transform:translate(${zoom.view.x}px,${zoom.view.y}px) scale(${zoom.view.s})`}>
    <svg class="fleet-edges" aria-hidden="true">
      ${edges.map((e) => { const p = peerById.get(e.id); const live = e.id === ':hub' ? fleet.hub.state === 'connected' : p && card(p).presence === 'online'; return html`<path key=${e.id} d=${e.d} class=${`fleet-edge${e.id !== ':hub' && !p ? ' untrusted' : live ? ' live' : ' stale'}`} />`; })}
    </svg>
    ${edges.map((e) => { const p = peerById.get(e.id); return p && html`<${LinkBadge} key=${`l-${e.id}`} link=${linkView(fleet.self.name, p, card(p))} at=${e.mid} open=${linkOpen === e.id}
      onToggle=${() => setLinkOpen(linkOpen === e.id ? '' : e.id)} onConfigure=${configurePeers} />`; })}
    <div class="fleet-map-local"><${NodeCard} node=${fleet.self} card=${card(fleet.self)} focused=${current.focused === fleet.self.id} shown=${!remote} onOpen=${open(fleet.self)} onUpdate=${() => setUpdating(fleet.self)} onConfigure=${configure} /></div>
    <div class="fleet-map-hub">${showHub && html`<${HubCard} hub=${fleet.hub} now=${t} admin=${remote ? null : hubAdmin} onConfigure=${configure} />`}</div>
    <div class=${`fleet-map-peers${fleet.peers.length > 4 || strangers.length > 2 ? ' wide' : ''}`}>${fleet.peers.length === 0 && !strangers.length && html`<div class="fleet-map-alone">${remote ? NO_PEERS : current.showUntrusted ? NO_STRANGERS : MAP_NO_PEERS}</div>`}${fleet.peers.map((peer) => html`<${NodeCard} key=${peer.id} node=${peer} card=${card(peer)} selfName=${fleet.self.name} focused=${current.focused === peer.id} shown=${remote === peer.id} onOpen=${open(peer)} onUpdate=${() => setUpdating(peer)} onSilence=${silence(peer)} onOfflineAlerts=${offlineAlerts(peer)} onConfigure=${configure} now=${t} />`)}${strangers.length > 0 && html`<div class="fleet-map-strangers" role="group" aria-label="untrusted instances on the hub">
      <h4>On the hub, not trusted <span class="muted">${strangers.length}</span>${asking > 0 && html` <span class="fleet-map-asking">· ${asking} asking to be trusted</span>`}</h4>
      <div class="fleet-map-strangers-cards">${strangers.map((row) => html`<${StrangerCard} key=${row.id} row=${row} now=${t} onTrust=${trust(row)} onAsk=${ask(row)} onDismiss=${dismiss(row)} />`)}</div>
    </div>`}</div>
    </div>
    </div>
    ${trusting && html`<${trusting.TrustDialog} row=${trusting.row} actions=${trusting.actions} onClose=${() => setTrusting(null)} onDone=${trusted} />`}
    ${updating && html`<${NodeUpdateDialog} node=${{ ...updating, label: updating.name }} confirm=${confirm} toast=${toast} timers=${timers} actions=${updateActions}
      onClose=${() => { setUpdating(null); actions.loadSummary(updating); }} />`}
    <div class="fleet-legend"><span aria-hidden="true"><i class="live"></i>linked, reachable</span><span aria-hidden="true"><i class="stale"></i>unreachable or stale</span>${strangers.length > 0 && html`<span aria-hidden="true"><i class="untrusted"></i>on the hub, not trusted</span>`}<span aria-hidden="true">Counts show only what each node shares with you. A link's label shows its trust in each direction (→ what you may do there, ← what it may do here); click it for details.</span>
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
      // Swallow an offline peer's key too, or the browser switches tabs.
      event.preventDefault();
      if (closedNode(node, remote || fleet.self.id)) return;
      if (node.id === (remote || fleet.self.id) && !state.fused.value) { if (state.view.value.topLevel) navigate(state.lastLocalTab()); return; }
      pickNode(state, node.id, { navigate, remote, switchNode });
    };
    document.addEventListener('keydown', onKey);
    return () => document.removeEventListener('keydown', onKey);
  }, []);
  return null;
}

// RemoteMarker marks a peer's per-node view without moving any component: the
// title reads "tclaude @ <node>" in the node's colour, a thin line
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
  useDismissablePopover(rootRef, open, () => setOpen(false));
  if (!remote) return null;
  const name = peer?.name || remote.slice(0, 13);
  const hv = remoteHealthView(health, now());
  const pv = peerViewSummary(snapshot.value?.peer_view);
  const level = peer?.level || '';
  return html`<span class="remote-node-marker" ref=${rootRef} style=${`--nc:${color}`}>
    <span class="remote-node-at" aria-hidden="true">@</span>
    <span class="remote-node-title" title=${`Peer view of ${name} (${remote})`}>${name}</span>
    <button type="button" class=${`remote-node-pill ${hv.state}`} aria-haspopup="dialog" aria-expanded=${open ? 'true' : 'false'}
      onClick=${() => setOpen(!open)} title="What this peer shares with you">
      peer view${hv.label ? html` <span class="remote-node-health">· ${hv.label}</span>` : ''}
    </button>
    ${open && html`<div class="remote-node-pop" role="dialog" aria-label=${`Peer view of ${name}`}>
      <div class="rnp-head">Peer view of <b>${name}</b></div>
      ${level && html`<div class="rnp-row"><span class="rnp-k">You trust it</span> ${level === 'unrestricted' ? '⚠ unrestricted' : 'restricted'} · <span class="rnp-k">It trusts you</span> ${pv?.level || 'not reported'}</div>`}
      <div class="rnp-row">${hv.state === 'live' ? 'Live: the peer answers through this node.' : `The peer is ${hv.label}. The data on screen is what it last shared.`}</div>
      ${pv && pv.included.length > 0 && html`<div class="rnp-row"><span class="rnp-k">Shared</span> ${pv.included.join(', ')}</div>`}
      ${pv && pv.omitted.length > 0 && html`<div class="rnp-row"><span class="rnp-k">Hidden from you</span> ${pv.omitted.map((o, i) => html`${i ? ', ' : ''}<span title=${o.requires ? `needs ${o.requires}` : ''}>${o.feature}</span>${requestable(o) ? html` <button type="button" class="rnp-ask" data-perm=${o.requires} title=${`Ask ${name}'s operator for ${o.requires}`} onClick=${() => { setAsking(o.requires); setOpen(false); }}>request…</button>` : ''}`)}</div>`}
      ${!pv && html`<div class="rnp-row muted">What the peer shares shows once it answers.</div>`}
      ${peerManagesUsage(snapshot.value) && html`<div class="rnp-row"><span class="rnp-k">Usage rules</span> <button type="button" class="rnp-ask" id="rnp-usage-rules" title=${`${name} lets you manage its usage limits and budgets (usage-rules.manage)`} onClick=${() => { setOpen(false); openUsageRulesManager({ node: remote, nodeLabel: name }); }}>manage its usage rules…</button></div>`}
      <div class="rnp-row"><span class="rnp-k">Usage</span> <button type="button" class="rnp-ask" id="rnp-ask-usage" title=${`Ask ${name}'s operator to pay for your agents' model calls through its model gateway`} onClick=${() => { setAsking(USAGE_PERM); setOpen(false); }}>ask for usage…</button></div>
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
