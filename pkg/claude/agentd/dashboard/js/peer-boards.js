// peer-boards.js -- a linked peer's collab boards on that peer's dashboard
// page (tcl-ug8bb8). The peer shares a board through the board's group
// (peer_boards.go): groups.resources.read there reads it, .write edits it;
// a board in no group is not shared. The page reaches them through the peer
// proxy (remote-node.js rewrites /api/*), and polls: the live socket stays
// with the board's own node.
//
// The Collab panel lists them under their groups (placement A), and each
// group of the peer's Groups tab lists its own (placement B); both open one
// editor (board-island.js) under the same remote-board banner.

import { signal, effect } from '@preact/signals';
import { onPeerPage } from './node-api.js';
import { featureState } from './peer-view-limits.js';
import { dashboardState } from './snapshot-store.js';

const READ = 'groups.resources.read';
const WRITE = 'groups.resources.write';

const peerView = (snapshotValue) => snapshotValue?.peer_view || null;
const unrestricted = (pv) => pv?.level === 'unrestricted';

// peerBoardsShared says whether this page is a peer's that shares boards
// (in some group). Off a peer page it is false: this node's boards are not
// "peer boards".
export function peerBoardsShared(snapshotValue = dashboardState.snapshot.value, peer = onPeerPage()) {
  if (!peer) return false;
  const pv = peerView(snapshotValue);
  if (!pv) return false;
  return unrestricted(pv) || featureState('boards', pv) === 'shared';
}

// peerBoardAccess is what this node may do with the peer's boards in group:
// 'write', 'read', or '' (none, or not a peer page).
export function peerBoardAccess(group, snapshotValue = dashboardState.snapshot.value, peer = onPeerPage()) {
  if (!peer || !group) return '';
  const pv = peerView(snapshotValue);
  if (!pv) return '';
  if (unrestricted(pv)) return 'write';
  const held = Array.isArray(pv.groups?.[group]) ? pv.groups[group] : [];
  if (held.includes(WRITE)) return 'write';
  return held.includes(READ) ? 'read' : '';
}

// peerBoards is the peer's boards this node may read, as GET /api/boards
// lists them ({ id, title, group, version, updated_at, agents_locked, … }),
// or null before the first answer.
export const peerBoards = signal(null);

// peerBoardsPollMs is how often, at most, the list is read again. It rides
// the dashboard's snapshot poll, as a peer group's resources do.
export const peerBoardsPollMs = 5000;

// loadPeerBoards reads the list once into peerBoards; a refusal empties it.
export async function loadPeerBoards(fetchImpl = (...args) => globalThis.fetch(...args)) {
  try {
    const response = await fetchImpl('/api/boards', { credentials: 'same-origin' });
    const body = await response.json().catch(() => ({}));
    peerBoards.value = response.ok && Array.isArray(body.boards) ? body.boards.filter((b) => b && b.id && b.group) : [];
  } catch {
    // Offline: keep the last list; the editor says the node is out of reach.
  }
  return peerBoards.value;
}

let stopPolling = null;

// startPeerBoards keeps peerBoards fresh while this is a peer page that
// shares boards; it returns the stop. Called once by the page shell.
export function startPeerBoards({ tick = dashboardState.lastRefreshAt, pollMs = peerBoardsPollMs, fetchImpl } = {}) {
  stopPolling?.();
  let due = 0;
  const stop = effect(() => {
    const at = tick.value;
    if (!peerBoardsShared(dashboardState.snapshot.value)) {
      if (peerBoards.peek() !== null) peerBoards.value = null;
      return;
    }
    if (at == null || at < due || globalThis.document?.visibilityState === 'hidden') return;
    due = at + pollMs;
    void loadPeerBoards(fetchImpl);
  });
  stopPolling = () => { stop(); stopPolling = null; };
  return stopPolling;
}

// peerBoardResources are the peer's boards as the Collab panel's model
// takes resources (collab-rail-model.js): by the id of the group the peer's
// snapshot lists by that name, closed (a board has nothing that runs).
export function peerBoardResources(boards = peerBoards.value, snapshotValue = dashboardState.snapshot.value) {
  const ids = new Map((snapshotValue?.groups || []).map((g) => [g.name, g.id]));
  return (boards || []).filter((b) => ids.has(b.group)).map((b) => ({
    kind: 'board', id: b.id, group_id: ids.get(b.group), title: b.title || b.id, state: 'closed', locked: !!b.agents_locked, peer: true,
  }));
}

// peerGroupBoards are the peer's boards in group, for its Groups tab card.
export function peerGroupBoards(group, boards = peerBoards.value) {
  return (boards || []).filter((b) => b.group === group);
}

// peerName is how the banner names the peer: the name the page's remote
// marker publishes (fleet-island.js), else its id.
export function peerName(remote = globalThis.__tclaudeRemoteNode, doc = globalThis.document) {
  return doc?.documentElement?.dataset?.remoteNodeName || (remote?.id ? String(remote.id).slice(0, 13) : 'the peer');
}
