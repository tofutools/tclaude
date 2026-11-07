// peer-drawing-boards.js -- a linked peer's collab drawing boards on that peer's dashboard
// page (tcl-ug8bb8). The peer shares a drawing board through the drawing board's group
// (peer_drawing_boards.go): groups.resources.read there reads it, .write edits it;
// a drawing board in no group is not shared. The page reaches them through the peer
// proxy (remote-node.js rewrites /api/*), and polls: the live socket stays
// with the drawing board's own node.
//
// The Collab panel lists them under their groups (placement A), and each
// group of the peer's Groups tab lists its own (placement B); both open one
// editor (drawing-board-island.js) under the same remote-drawing-board banner.

import { signal, effect } from '@preact/signals';
import { onPeerPage } from './node-api.js';
import { featureState } from './peer-view-limits.js';
import { dashboardState } from './snapshot-store.js';

const READ = 'groups.resources.read';
const WRITE = 'groups.resources.write';

const peerView = (snapshotValue) => snapshotValue?.peer_view || null;
const unrestricted = (pv) => pv?.level === 'unrestricted';

// peerDrawingBoardsShared says whether this page is a peer's that shares drawing boards
// (in some group). Off a peer page it is false: this node's drawing boards are not
// "peer drawing boards".
export function peerDrawingBoardsShared(snapshotValue = dashboardState.snapshot.value, peer = onPeerPage()) {
  if (!peer) return false;
  const pv = peerView(snapshotValue);
  if (!pv) return false;
  return unrestricted(pv) || featureState('drawing-boards', pv) === 'shared';
}

// peerDrawingBoardAccess is what this node may do with the peer's drawing boards in group:
// 'write', 'read', or '' (none, or not a peer page).
export function peerDrawingBoardAccess(group, snapshotValue = dashboardState.snapshot.value, peer = onPeerPage()) {
  if (!peer || !group) return '';
  const pv = peerView(snapshotValue);
  if (!pv) return '';
  if (unrestricted(pv)) return 'write';
  const held = Array.isArray(pv.groups?.[group]) ? pv.groups[group] : [];
  if (held.includes(WRITE)) return 'write';
  return held.includes(READ) ? 'read' : '';
}

// peerDrawingBoards is the peer's drawing boards this node may read, as GET /api/drawing-boards
// lists them ({ id, title, group, version, updated_at, agents_locked, … }),
// or null before the first answer.
export const peerDrawingBoards = signal(null);

// peerDrawingBoardsPollMs is how often, at most, the list is read again. It rides
// the dashboard's snapshot poll, as a peer group's resources do.
export const peerDrawingBoardsPollMs = 5000;

// loadPeerDrawingBoards reads the list once into peerDrawingBoards; a refusal empties it.
export async function loadPeerDrawingBoards(fetchImpl = (...args) => globalThis.fetch(...args)) {
  try {
    const response = await fetchImpl('/api/drawing-boards', { credentials: 'same-origin' });
    const body = await response.json().catch(() => ({}));
    peerDrawingBoards.value = response.ok && Array.isArray(body.drawingBoards) ? body.drawingBoards.filter((b) => b && b.id && b.group) : [];
  } catch {
    // Offline: keep the last list; the editor says the node is out of reach.
  }
  return peerDrawingBoards.value;
}

let stopPolling = null;

// startPeerDrawingBoards keeps peerDrawingBoards fresh while this is a peer page that
// shares drawing boards; it returns the stop. Called once by the page shell.
export function startPeerDrawingBoards({ tick = dashboardState.lastRefreshAt, pollMs = peerDrawingBoardsPollMs, fetchImpl } = {}) {
  stopPolling?.();
  let due = 0;
  const stop = effect(() => {
    const at = tick.value;
    if (!peerDrawingBoardsShared(dashboardState.snapshot.value)) {
      if (peerDrawingBoards.peek() !== null) peerDrawingBoards.value = null;
      return;
    }
    if (at == null || at < due || globalThis.document?.visibilityState === 'hidden') return;
    due = at + pollMs;
    void loadPeerDrawingBoards(fetchImpl);
  });
  stopPolling = () => { stop(); stopPolling = null; };
  return stopPolling;
}

// peerDrawingBoardResources are the peer's drawing boards as the Collab panel's model
// takes resources (collab-rail-model.js): by the id of the group the peer's
// snapshot lists by that name, closed (a drawing board has nothing that runs).
export function peerDrawingBoardResources(drawingBoards = peerDrawingBoards.value, snapshotValue = dashboardState.snapshot.value) {
  const ids = new Map((snapshotValue?.groups || []).map((g) => [g.name, g.id]));
  return (drawingBoards || []).filter((b) => ids.has(b.group)).map((b) => ({
    kind: 'drawing_board', id: b.id, group_id: ids.get(b.group), title: b.title || b.id, state: 'closed', locked: !!b.agents_locked, peer: true,
  }));
}

// peerGroupDrawingBoards are the peer's drawing boards in group, for its Groups tab card.
export function peerGroupDrawingBoards(group, drawingBoards = peerDrawingBoards.value) {
  return (drawingBoards || []).filter((b) => b.group === group);
}

// peerName is how the banner names the peer: the name the page's remote
// marker publishes (fleet-island.js), else its id.
export function peerName(remote = globalThis.__tclaudeRemoteNode, doc = globalThis.document) {
  return doc?.documentElement?.dataset?.remoteNodeName || (remote?.id ? String(remote.id).slice(0, 13) : 'the peer');
}
