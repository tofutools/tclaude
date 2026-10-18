import { h } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import htm from 'htm';
import { PEER_ACTION_EVENT } from './peer-view-limits.js';
import { openRemoteAgentSpawn } from './agent-spawn-controller.js';
import { shellConfirm, shellToast } from './shell-state.js';
import { openMessageCreateModal } from './message-access-dialog-controller.js';
import { isPageNode } from './node-api.js';

const html = htm.bind(h);

export class PeerActionError extends Error {
  constructor(status, body) {
    super(body?.error || `HTTP ${status}`);
    this.status = status;
    this.code = body?.code || '';
  }
}

// createPeerActionActions sends the peer action routes. On a peer view
// remote-node.js forwards /api/* to /api/peer/{id}/*, so these are the
// peer's routes; the merged all-nodes view passes node to address
// /api/peer/{node}/* itself.
export function createPeerActionActions({ fetchImpl = (...a) => globalThis.fetch(...a), node = '' } = {}) {
  const api = node ? `/api/peer/${encodeURIComponent(node)}/` : '/api/';
  async function call(method, url, body) {
    const init = { method, credentials: 'same-origin', headers: {} };
    if (body !== undefined) {
      init.headers['Content-Type'] = 'application/json';
      init.body = JSON.stringify(body);
    }
    const res = await fetchImpl(url, init);
    let data = null;
    try { data = await res.json(); } catch (_) { data = null; }
    if (!res.ok) throw new PeerActionError(res.status, data);
    return data;
  }
  const agent = (id, tail) => `${api}agents/${encodeURIComponent(id)}/${tail}`;
  return Object.freeze({
    move: (id, group) => call('POST', agent(id, 'move'), { group }),
    // moveDirect lets the receiving node (this one) land it at once when its
    // permissions allow; otherwise it waits as an offer (tcl-u4yodp).
    moveDirect: (id, body) => call('POST', agent(id, 'move'), body),
    // teleport asks the peer to teleport (or, with clone, copy) its agent to
    // this node: {group, clone?, note?}.
    teleport: (id, body) => call('POST', agent(id, 'teleport'), body),
    message: (to, { subject = '', body }) => call('POST', `${api}operator-message`, { to, subject, body }),
  });
}

let sharedActions = null;
function defaultActions() { return (sharedActions ||= createPeerActionActions()); }

// MessageOpener hands a message request to the regular message dialog
// (message-access-dialog-island.js) and closes itself: messaging a peer's
// group uses the same dialog as here, with what the peer withholds locked.
function MessageOpener({ req, openMessage, onClose }) {
  useEffect(() => {
    openMessage({ targetMode: 'group', groupName: req.group }, req.page || isPageNode(req.nodeId) ? '' : req.nodeId, req.node);
    onClose();
  }, []);
  return null;
}

// RemoteSpawnOpener hands a spawn request to the regular spawn dialog in
// remote mode (agent-spawn-remote.js) and closes itself: a spawn on a peer
// uses the same dialog as here, with what the peer decides disabled.
function RemoteSpawnOpener({ req, snapshot, toast, openSpawn, onClose }) {
  useEffect(() => {
    void openSpawn({ nodeId: req.nodeId, node: req.node, groupName: req.group, snapshot, toast });
    onClose();
  }, []);
  return null;
}

// PeerActionDialog shows the dialog for one peer action request; req carries
// the node's name and ID. peerView says what the peer shares, groups are its
// snapshot's groups (message recipients), snapshot its whole snapshot (the
// spawn dialog's groups and harnesses).
export function PeerActionDialog({ req, peerView, groups, snapshot = null, actions, confirm = shellConfirm, toast = shellToast, openSpawn = openRemoteAgentSpawn, openMessage = openMessageCreateModal, onClose }) {
  if (req.action === 'spawn') return html`<${RemoteSpawnOpener} req=${req} snapshot=${snapshot || { groups }} toast=${toast} openSpawn=${openSpawn} onClose=${onClose} />`;
  if (req.action === 'message') return html`<${MessageOpener} req=${req} openMessage=${openMessage} onClose=${onClose} />`;
  return null;
}

// PeerActionHost listens for peer action clicks on a peer view and shows the
// matching dialog. snapshot supplies peer_view (what the peer shares).
export function PeerActionHost({ snapshot, remote = globalThis.__tclaudeRemoteNode, actions = defaultActions(), confirm = shellConfirm, toast = shellToast, openSpawn = openRemoteAgentSpawn, openMessage = openMessageCreateModal, doc = globalThis.document }) {
  const [req, setReq] = useState(null);
  useEffect(() => {
    if (!remote?.id || !doc) return undefined;
    const on = (event) => setReq(event.detail || null);
    doc.addEventListener(PEER_ACTION_EVENT, on);
    return () => doc.removeEventListener(PEER_ACTION_EVENT, on);
  }, [remote?.id]);
  if (!req || !remote?.id) return null;
  const node = doc?.documentElement?.dataset?.remoteNodeName || remote.id.slice(0, 13);
  const snap = snapshot?.value;
  return html`<${PeerActionDialog} req=${{ ...req, node, nodeId: remote.id, page: true }} peerView=${snap?.peer_view} groups=${snap?.groups} snapshot=${snap} actions=${actions} confirm=${confirm} toast=${toast} openSpawn=${openSpawn} openMessage=${openMessage} onClose=${() => setReq(null)} />`;
}
