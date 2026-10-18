// node-trust.js — whether a node lets this one act there as its own
// operator does: this node itself, or a peer whose snapshot says it trusts
// this one unrestricted (peer_view.level). A blank node, or the page's own,
// is the page's: on a peer's page, that peer.
import { isPageNode, nodeSnapshot, selfNodeID } from './node-api.js';
import { dashboardState } from './snapshot-store.js';

export function unrestrictedOn(node) {
  // This node's own agent, from a peer's page, is this operator's own.
  if (node && node === selfNodeID()) return true;
  const snap = !isPageNode(node) ? nodeSnapshot(node) : globalThis.__tclaudeRemoteNode?.id ? dashboardState.snapshot.value : null;
  return snap?.peer_view?.level === 'unrestricted';
}
