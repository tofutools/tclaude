// feature-availability.js -- whether the snapshot's node offers Processes and
// Triggers. Both are always on for this node. A peer's snapshot (it carries
// peer_view) still says per feature: a restricted peer never shares them, and
// an older peer may have had them switched off by its retired feature flag.
// Before a snapshot arrives, this node's chrome shows.

import { nodeShares } from './peer-view-limits.js';

function peerSnapshot(snapshot) {
  return !!snapshot?.peer_view;
}

export function processesAvailable(snapshot) {
  return !peerSnapshot(snapshot) || snapshot.processes_enabled === true;
}

export function triggersAvailable(snapshot) {
  return !peerSnapshot(snapshot) || snapshot.triggers_enabled === true;
}

// triggersReadable reports whether a page can read a node's trigger rules
// (/api/triggers) through its snapshot: this node, or a peer that offers
// Triggers and shares its local dashboard routes (unrestricted trust). To any
// other peer the route is local-only (peer_view_local_only), so the page does
// not ask. Like every nodeShares gate it waits for the snapshot: before it
// arrives a peer page cannot tell (tk_56c664c0).
export function triggersReadable(snapshot) {
  if (!nodeShares(snapshot, 'local_dashboard')) return false;
  return !peerSnapshot(snapshot) || snapshot.triggers_enabled === true;
}
