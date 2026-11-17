// phone-node-picker.js -- the phone's node chip and node picker. The chip
// (⌂ name ▾) says which node's dashboard is on screen; with linked nodes it
// opens a sheet listing this node and its peers, and picking one opens that
// node's dashboard. Switching is a page load, as on the desktop chips
// (fleet-island.js openNodeView, which openNode mirrors without loading the
// Fleet island), so no node's data is ever drawn under
// another node's name.
//
// It reads the fleet state the desktop Fleet island keeps (fleet-state.js):
// fleet is null until this node has trusted peers, node is this node alone.

import { h } from 'preact';
import htm from 'htm';
import { nodeHref, remoteNodeID } from './fleet-model.js';
import { fleetNodes, withFused } from './fleet-scope.js';
import { openPhoneSheet } from './phone-sheet.js';
import { SheetHead } from './phone-ui.js';

const html = htm.bind(h);

// openNode opens node id's dashboard ('' this node) on the page's path,
// leaving any fused view.
export function openNode(id, loc = globalThis.location) {
  loc.assign(nodeHref(id, { pathname: loc.pathname, search: withFused(loc.search, null) }));
}

// nodeChipView is what the chip shows: the node on screen (a peer's page
// names that peer) and whether there is any other node to pick.
export function nodeChipView(fleetView, remote = remoteNodeID()) {
  const fleet = fleetView?.fleet || null;
  const self = fleet?.self || fleetView?.node?.self || null;
  if (!self) return null;
  const nodes = fleetNodes(fleet);
  const shown = (remote && nodes.find((n) => n.id === remote)) || self;
  return { name: shown.name, color: shown.color, local: !!shown.local, current: shown.id, pickable: nodes.length > 1 };
}

// nodeRows lists the nodes the picker offers: this node first, then peers.
// A peer that is offline cannot be opened (its dashboard is served through
// it), unless it is the one on screen.
export function nodeRows(fleetView, current) {
  return fleetNodes(fleetView?.fleet || null).map((node) => ({
    id: node.id,
    name: node.name,
    color: node.color,
    local: !!node.local,
    online: node.local || !!node.online,
    current: node.id === current,
    disabled: !node.local && !node.online && node.id !== current,
    meta: node.local ? 'this node' : node.online ? `${node.level || 'restricted'} peer` : 'offline',
  }));
}

function NodePicker({ fleet, current, open, close }) {
  const rows = nodeRows(fleet.view.value, current);
  const pick = (row) => {
    close();
    if (row.current) return;
    open(row.local ? '' : row.id);
  };
  return html`<${SheetHead}>Nodes<//>
    <ul class="phone-menu">
      ${rows.map((row) => html`<li key=${row.id}>
        <button data-ui-exempt type="button" class="phone-menu-item phone-node-row" style=${`--nc:${row.color}`}
          aria-current=${row.current ? 'page' : undefined} disabled=${row.disabled} onClick=${() => pick(row)}>
          <span class="phone-menu-icon phone-node-dot" data-online=${row.online ? 'true' : 'false'} aria-hidden="true">${row.local ? '⌂' : '●'}</span>
          <span class="phone-node-name">${row.name}</span>
          <span class="phone-node-meta">${row.current ? 'showing' : row.meta}</span>
        </button>
      </li>`)}
    </ul>`;
}

// openNodePicker opens the node picker sheet. open(id) opens a node's
// dashboard ('' this node); by default a page load (openNode).
export function openNodePicker({ fleet, remote = remoteNodeID(), open = openNode }) {
  const view = nodeChipView(fleet.view.value, remote);
  if (!view?.pickable) return;
  openPhoneSheet({ label: 'Nodes', Body: NodePicker, props: { fleet, current: view.current, open } });
}

// NodeChip is the app bar's node chip. Without linked nodes it only names
// this node; with them it opens the picker.
export function NodeChip({ fleet, remote = remoteNodeID(), open = openNode }) {
  const view = nodeChipView(fleet?.view?.value, remote);
  if (!view) return null;
  const face = html`<span aria-hidden="true">${view.local ? '⌂' : '●'}</span><span class="phone-node-chip-name">${view.name}</span>`;
  if (!view.pickable) return html`<span class="phone-node-chip" style=${`--nc:${view.color}`} title=${`${view.name} (this node)`}>${face}</span>`;
  return html`<button data-ui-exempt type="button" class="phone-node-chip" style=${`--nc:${view.color}`} aria-haspopup="dialog"
    aria-label=${`Node: ${view.name}. Pick a node`} onClick=${() => openNodePicker({ fleet, remote, open })}>
    ${face}<span class="phone-node-chip-caret" aria-hidden="true">▾</span>
  </button>`;
}
