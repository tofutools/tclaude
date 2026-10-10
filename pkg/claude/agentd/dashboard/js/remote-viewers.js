import { h } from 'preact';
import { signal } from '@preact/signals';
import htm from 'htm';
import { remoteNodeID } from './skynet-model.js';
import { dashboardState } from './snapshot-store.js';
import { skynetState } from './skynet-state.js';

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

// viewersOpened counts badge clicks, so a second click on the same agent's
// badge still brings Fleet back to Peers.
export const viewersOpened = signal(0);

const VIEWERLESS_TABS = new Set(['map', 'fleet-admin']);

let lastRead = -Infinity;

// claimViewersRead says whether this tick should read the viewers, and if so
// claims the slot so an overlapping tick does not read again: only for this
// node's own dashboard (a peer's per-node view shows the peer's agents, whose
// viewers are not ours), only once this node is known to be federated, and at
// most every VIEWERS_EVERY_MS. A node that leaves the federation drops its
// badges. The map shows no agent rows and Fleet → Peers polls its own viewers
// panel, so those two skip the read; the merged Groups view renders this
// node's rows and keeps reading.
export function claimViewersRead(now = Date.now(), { remote = remoteNodeID(), fleet = skynetState.fleet.value, tab = dashboardState.activeTab.value } = {}) {
  if (remote || !fleet) {
    if (remoteViewers.value.length) remoteViewers.value = [];
    return false;
  }
  if (VIEWERLESS_TABS.has(tab)) return false;
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
  viewersOpened.value = 0;
}

// agentIDOf resolves a terminal selector, which may be a conversation ID (a
// pane opened from the palette or a jump), to the agent ID the viewers rows
// carry, through the current snapshot.
export function agentIDOf(selector, snapshot = dashboardState.snapshot.value) {
  if (!selector || selector.startsWith('agt_')) return selector;
  const lists = [snapshot?.agents, snapshot?.ungrouped, ...(snapshot?.groups || []).map((g) => g?.members)];
  for (const list of lists) {
    const hit = (list || []).find((m) => m && m.conv_id === selector && m.agent_id);
    if (hit) return hit.agent_id;
  }
  return selector;
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
  viewersOpened.value += 1;
  doc?.querySelector('nav [data-tab="fleet-admin"]')?.click();
}

// ViewersBadge marks an agent that a peer is watching (👁) or typing into (⌨)
// right now, naming the peer. Peer labels and instance IDs are rendered as
// text only.
export function ViewersBadge({ agentId: selector, session = '', rows = remoteViewers.value, label = peerLabel, open = openViewers, className = '' }) {
  const agentId = agentIDOf(selector);
  const s = viewerSummary(viewersOf(rows, agentId, session), label);
  if (!s) return null;
  const onClick = (e) => { e.preventDefault(); e.stopPropagation(); open(agentId || session); };
  return html`<button type="button" class=${`remote-viewers-badge${s.typing ? ' typing' : ''}${className ? ` ${className}` : ''}`}
    data-viewers=${agentId || session} title=${s.title} aria-label=${s.title} onClick=${onClick}>${s.typing ? '⌨' : '👁'} ${s.text}</button>`;
}
