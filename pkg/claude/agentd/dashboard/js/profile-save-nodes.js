// profile-save-nodes.js — where the spawn dialog's "Save as profile…" may
// save: the node the spawn is on and the page's own node, each where this
// operator may create profiles (this node, a peer trusting it unrestricted,
// or a peer granting node.spawn.profiles.create). The profile editor offers
// them as its "Save on" choice.
import { isPageNode, nodeSnapshot, selfNodeID } from './node-api.js';
import { unrestrictedOn } from './node-trust.js';
import { featureState } from './peer-view-limits.js';
import { dashboardState } from './snapshot-store.js';

const FEATURE = 'spawn.profiles.create';

// ownNode reports this node itself: blank (the page's) off a peer's page.
function ownNode(node) {
  return node ? node === selfNodeID() : !globalThis.__tclaudeRemoteNode?.id;
}

function peerViewOf(node) {
  return node ? nodeSnapshot(node)?.peer_view : dashboardState.snapshot.value?.peer_view;
}

// profileSaveNodes lists {id, name, operatorOnly} for a spawn on spawnNode
// (blank: the page's node), the spawn's node first. id is blank for the
// page's node. operatorOnly marks a restricted peer, which saves the
// profile operator-only (peer_node_manage.go servePeerProfileCreate).
export function profileSaveNodes(spawnNode = '', spawnName = '') {
  const remotePage = !!globalThis.__tclaudeRemoteNode?.id;
  const pageName = remotePage ? globalThis.document?.documentElement?.dataset?.remoteNodeName || 'this peer' : 'this node';
  const nodes = [];
  if (spawnNode && !isPageNode(spawnNode)) nodes.push({ id: spawnNode, name: ownNode(spawnNode) ? 'this node' : spawnName || 'that node' });
  nodes.push({ id: '', name: pageName });
  return nodes.flatMap((node) => {
    if (ownNode(node.id) || unrestrictedOn(node.id)) return [{ ...node, operatorOnly: false }];
    return featureState(FEATURE, peerViewOf(node.id)) === 'shared' ? [{ ...node, operatorOnly: true }] : [];
  });
}
