// node-api.js — which node a dashboard request goes to.
//
// A dashboard page belongs to one node: this node, or a peer whose per-node
// view remote-node.js routes every /api/ request to. The fused Groups view
// shows several nodes' groups on one page, so a control acting on one of
// them names its node, and its requests go through nodeFetch(node): the
// page's own fetch for the page's node, this node's daemon for this node
// (from a peer's page), and the peer proxy for any other peer. The peer
// decides what it allows (its peer-view grants); this only addresses it.

const NODE_ID = /^inst_[A-Za-z0-9]{4,64}$/;

// selfNode is this node's instance ID, which fleet-state.js reports once the
// federation status names it.
let selfNode = '';

export function setSelfNode(id) {
  selfNode = NODE_ID.test(id || '') ? id : '';
}

export function selfNodeID() {
  return selfNode;
}

// peerPath addresses an /api/ path on a peer through this node's proxy.
export function peerPath(node, path) {
  const url = new URL(String(path), 'http://node.invalid');
  if (!url.pathname.startsWith('/api/') || url.pathname.startsWith('/api/peer/')) {
    throw new Error(`not a node API path: ${url.pathname}`);
  }
  return `/api/peer/${encodeURIComponent(node)}/${url.pathname.slice('/api/'.length)}${url.search}`;
}

// isPageNode reports whether node is the page's own node (blank included):
// its requests are the page's plain fetch, and its data the page snapshot.
export function isPageNode(node, { self = selfNode, remote = globalThis.__tclaudeRemoteNode } = {}) {
  return !node || node === (remote?.id || self);
}

// nodeFetch returns a fetch whose /api/ requests reach node. self is this
// node's instance ID (needed only to reach it from a peer's page); a blank
// node, or the page's own node, is the page's fetch.
export function nodeFetch(node, {
  self = selfNode, remote = globalThis.__tclaudeRemoteNode, fetchImpl = (...args) => globalThis.fetch(...args),
} = {}) {
  if (isPageNode(node, { self, remote })) return fetchImpl;
  if (node === self && remote?.localFetch) return remote.localFetch;
  if (!NODE_ID.test(node)) throw new Error(`invalid node: ${node}`);
  return (input, init) => {
    if (input instanceof Request) return fetchImpl(new Request(peerPath(node, new URL(input.url).pathname + new URL(input.url).search), input), init);
    return fetchImpl(peerPath(node, input), init);
  };
}

// nodeDownloadPath addresses one of node's file downloads (a snapshot's or
// an agent's .paf, a group archive, a template). The peer view carries JSON
// replies only, so another node's file comes through this node's
// /api/peer-download/{node}, which reads it from that node in chunks; on a
// peer's page that is the page's own node too. It is a path on this node's
// daemon, which a peer's page reaches through localFetch (or an anchor).
export function nodeDownloadPath(node, path, { self = selfNode, remote = globalThis.__tclaudeRemoteNode } = {}) {
  const target = node || remote?.id || '';
  if (!target || target === self) return path;
  if (!NODE_ID.test(target)) throw new Error(`invalid node: ${target}`);
  return `/api/peer-download/${encodeURIComponent(target)}?path=${encodeURIComponent(path)}`;
}

// nodeDownload fetches one of node's file downloads (see nodeDownloadPath).
export function nodeDownload(node, path, {
  self = selfNode, remote = globalThis.__tclaudeRemoteNode, fetchImpl = (...args) => globalThis.fetch(...args),
} = {}) {
  return (remote?.localFetch || fetchImpl)(nodeDownloadPath(node, path, { self, remote }), { credentials: 'same-origin' });
}

// nodeUpload posts a file upload (a spawn's attachments) to node: this node's
// route directly, another node's (the page's own on a peer's page) through
// this node's /api/peer-upload/{node}, which sends it on in chunks (the peer
// view carries 32 KiB requests otherwise).
export function nodeUpload(node, path, init = {}, {
  self = selfNode, remote = globalThis.__tclaudeRemoteNode, fetchImpl = (...args) => globalThis.fetch(...args),
} = {}) {
  const target = node || remote?.id || '';
  const local = remote?.localFetch || fetchImpl;
  const options = { credentials: 'same-origin', ...init, method: 'POST' };
  if (!target || target === self) return local(path, options);
  if (!NODE_ID.test(target)) throw new Error(`invalid node: ${target}`);
  return local(`/api/peer-upload/${encodeURIComponent(target)}?path=${encodeURIComponent(path)}`, options);
}

// onNodeChanged tells views polling a node (the fused Groups view) that a
// control just changed it, so they read it again now. It returns the
// unsubscribe.
const nodeListeners = new Set();

export function onNodeChanged(listener) {
  nodeListeners.add(listener);
  return () => nodeListeners.delete(listener);
}

export function announceNodeChanged(node) {
  if (!node) return;
  for (const listener of [...nodeListeners]) {
    try { listener(node); } catch (_) { /* one view's failure is not the change's */ }
  }
}

// nodeSnapshot is the latest snapshot of another node's dashboard, which a
// dialog acting on that node's group or agent reads instead of the page's
// (null for the page's own node: callers use the page snapshot). The fused
// Groups view, which polls those nodes, is the source.
let snapshotSource = () => null;

export function setNodeSnapshotSource(source) {
  snapshotSource = source;
  return () => { if (snapshotSource === source) snapshotSource = () => null; };
}

export function nodeSnapshot(node) {
  if (isPageNode(node)) return null;
  try { return snapshotSource(node) || null; } catch (_) { return null; }
}
