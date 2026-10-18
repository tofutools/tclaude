import { prepareRemoteSpawn } from './agent-spawn-remote.js';
import { dashboardState } from './snapshot-store.js';
import { shellToast } from './shell-state.js';
import { selfNodeID } from './node-api.js';

let controller = null;

export function registerAgentSpawnController(value) {
  controller = value;
  return () => {
    if (controller === value) controller = null;
  };
}

function requireController() {
  if (!controller) throw new Error('agent spawn UI is not ready');
  return controller;
}

// openAgentSpawnModal opens the spawn dialog. On a per-node peer view every
// spawn targets the peer, so it opens on the peer there whatever opened it.
export function openAgentSpawnModal(options = {}) {
  const page = globalThis.__tclaudeRemoteNode;
  if (!options?.remote && !options?.target && page?.id) {
    const node = globalThis.document?.documentElement?.dataset?.remoteNodeName || '';
    const snapshot = dashboardState.snapshot.value;
    if (nodeMode(page.id, snapshot)) return requireController().open({ ...options, target: { node: page.id, nodeId: page.id, name: node, snapshot } });
    const fallback = (snapshot?.groups || []).some((g) => g?.name === options?.defaultGroup) ? options.defaultGroup : '';
    return openRemoteAgentSpawn({ nodeId: page.id, groupName: options?.groupName || fallback, node, snapshot });
  }
  return requireController().open(options);
}

// nodeMode says whether a spawn on nodeId runs the regular dialog against
// that node, as its own dashboard would: this node always, and a peer that
// trusts this one unrestricted. Any other peer takes a spawn request.
function nodeMode(nodeId, snapshot) {
  return (!!nodeId && nodeId === selfNodeID()) || snapshot?.peer_view?.level === 'unrestricted';
}

// openRemoteAgentSpawn opens the regular spawn dialog against a peer's group,
// addressed through /api/peer/{nodeId}/ (which a per-node peer view leaves
// local). It reads the peer's allowances first, so the dialog opens locked.
export async function openRemoteAgentSpawn({ nodeId = '', node = '', groupName, snapshot = null, toast = shellToast, prepare = prepareRemoteSpawn } = {}) {
  if (!groupName) {
    toast('Pick a group on the peer to spawn into (its ＋ button)', true);
    return null;
  }
  if (nodeMode(nodeId, snapshot)) {
    const self = nodeId === selfNodeID();
    return requireController().open({ groupName, target: { node: nodeId, nodeId: self ? '' : nodeId, name: node, snapshot } });
  }
  let remote;
  try {
    remote = await prepare({ nodeId, node, group: groupName, snapshot });
  } catch (error) {
    toast(`Could not ask ${node || 'the peer'} what a spawn may set: ${error?.message || error}`, true);
    return null;
  }
  // The grant's pinned profile is the peer's default for this requester: it
  // outranks the group's default profile, as on the peer.
  return requireController().open({ groupName, remote, profileName: remote.options?.defaults?.profile || '' });
}

export function refreshAgentSpawnSandboxPolicy() {
  return controller?.refreshSandboxPolicy();
}
