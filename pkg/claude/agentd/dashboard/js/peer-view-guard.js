import { shellToast } from './shell-state.js';

// refuseInPeerView stops a browser-native /api/ navigation (a download anchor)
// on a peer's per-node view. remote-node.js routes fetch only; an anchor would
// reach this node's agentd and save local data under the peer's marker.
export function refuseInPeerView(what, toast = shellToast) {
  if (!globalThis.__tclaudeRemoteNode?.id) return false;
  toast(`${what} is not available in a peer view yet`, true);
  return true;
}
