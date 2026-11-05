// feature-availability.js -- whether the snapshot's node offers Processes and
// Triggers. Both are always on for this node. A peer's snapshot (it carries
// peer_view) still says per feature: a restricted peer never shares them, and
// an older peer may have had them switched off by its retired feature flag.
// Before a snapshot arrives, this node's chrome shows.

function peerSnapshot(snapshot) {
  return !!snapshot?.peer_view;
}

export function processesAvailable(snapshot) {
  return !peerSnapshot(snapshot) || snapshot.processes_enabled === true;
}

export function triggersAvailable(snapshot) {
  return !peerSnapshot(snapshot) || snapshot.triggers_enabled === true;
}
