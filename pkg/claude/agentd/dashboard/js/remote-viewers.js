import { h } from 'preact';
import { signal } from '@preact/signals';
import htm from 'htm';
import { remoteNodeID } from './skynet-model.js';
import { dashboardState } from './snapshot-store.js';
import { TOP_LEVEL_TABS, skynetState } from './skynet-state.js';

const html = htm.bind(h);

// Peers watching or typing into this node's agent terminals, read from
// /api/federation/viewers. The snapshot poll (refresh.js) is the only reader:
// it rides its own tick no more often than VIEWERS_EVERY_MS, so the agent rows
// and terminal headers add no request of their own and no timer.
export const VIEWERS_EVERY_MS = 5000;

// remoteViewers holds the incoming views from the last read: [{ id, peer,
// agent, session, group, read_only, started }].
export const remoteViewers = signal([]);

// viewersFocus is the agent a viewer badge was clicked for: Fleet → Peers
// filters its viewers panel to it until the operator clears the filter.
export const viewersFocus = signal('');

let lastRead = -Infinity;

// claimViewersRead says whether this tick should read the viewers, and if so
// claims the slot so an overlapping tick does not read again: only for this
// node's own dashboard (a peer's per-node view shows the peer's agents, whose
// viewers are not ours), only once this node is known to be federated, and at
// most every VIEWERS_EVERY_MS. A node that leaves the federation drops its
// badges. The multi-node views (map, merged Groups, Fleet) show no local agent
// rows or terminal headers, and Fleet → Peers polls its own viewers panel, so
// they skip the read.
export function claimViewersRead(now = Date.now(), { remote = remoteNodeID(), fleet = skynetState.fleet.value, tab = dashboardState.activeTab.value } = {}) {
  if (remote || !fleet) {
    if (remoteViewers.value.length) remoteViewers.value = [];
    return false;
  }
  if (TOP_LEVEL_TABS.has(tab)) return false;
  if (now - lastRead < VIEWERS_EVERY_MS) return false;
  lastRead = now;
  return true;
}

// noteViewersRead publishes a read; a failed one (null) keeps the previous
// list, so a blip never hides a live viewer's badge.
export function noteViewersRead(rows) {
  if (Array.isArray(rows)) remoteViewers.value = rows.filter((v) => v && v.incoming !== false);
}

// resetViewersForTest clears the module state between tests.
export function resetViewersForTest() {
  lastRead = -Infinity;
  remoteViewers.value = [];
  viewersFocus.value = '';
}

// viewersOf lists the incoming views of one agent, matched by agent ID or by
// its tmux session name.
export function viewersOf(rows, agentId, session = '') {
  if (!agentId && !session) return [];
  return rows.filter((v) => (agentId && v.agent === agentId) || (session && v.session === session));
}

// peerLabel names a viewing peer as the chip row does; an unknown peer shows
// its instance ID.
export function peerLabel(id, fleet = skynetState.fleet.value) {
  return fleet?.peers?.find((p) => p.id === id)?.name || id;
}

// viewerSummary is what a badge shows: typing outranks watching, the first
// peer is named and any others counted.
export function viewerSummary(views, label = peerLabel) {
  if (!views.length) return null;
  const typing = views.filter((v) => !v.read_only);
  const lead = (typing[0] || views[0]);
  const names = [...new Set(views.map((v) => label(v.peer)))];
  const mode = typing.length ? 'typing' : 'watching';
  const detail = views.map((v) => `${label(v.peer)} ${v.read_only ? 'watching' : 'typing (interactive)'}`).join(', ');
  return {
    typing: typing.length > 0,
    text: `${label(lead.peer)}${names.length > 1 ? ` +${names.length - 1}` : ''}`,
    title: `Remote ${mode}: ${detail}. Click to see who is viewing in Fleet → Peers, where you can disconnect them.`,
  };
}

// openViewers shows Fleet → Peers with the viewers panel filtered to agentId,
// through the real nav anchor so tab activation stays owned by refresh.js.
export function openViewers(agentId, doc = globalThis.document) {
  viewersFocus.value = agentId || '';
  doc?.querySelector('nav [data-tab="fleet-admin"]')?.click();
}

// ViewersBadge marks an agent that a peer is watching (👁) or typing into (⌨)
// right now, naming the peer. Peer labels and instance IDs are rendered as
// text only.
export function ViewersBadge({ agentId, session = '', rows = remoteViewers.value, label = peerLabel, open = openViewers, className = '' }) {
  const s = viewerSummary(viewersOf(rows, agentId, session), label);
  if (!s) return null;
  const onClick = (e) => { e.preventDefault(); e.stopPropagation(); open(agentId || session); };
  return html`<button type="button" class=${`remote-viewers-badge${s.typing ? ' typing' : ''}${className ? ` ${className}` : ''}`}
    data-viewers=${agentId || session} title=${s.title} aria-label=${s.title} onClick=${onClick}>${s.typing ? '⌨' : '👁'} ${s.text}</button>`;
}
