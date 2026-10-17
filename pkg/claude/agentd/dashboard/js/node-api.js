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

// peerPath addresses an /api/ path on a peer through this node's proxy.
export function peerPath(node, path) {
  const url = new URL(String(path), 'http://node.invalid');
  if (!url.pathname.startsWith('/api/') || url.pathname.startsWith('/api/peer/')) {
    throw new Error(`not a node API path: ${url.pathname}`);
  }
  return `/api/peer/${encodeURIComponent(node)}/${url.pathname.slice('/api/'.length)}${url.search}`;
}

// nodeFetch returns a fetch whose /api/ requests reach node. self is this
// node's instance ID (needed only to reach it from a peer's page); a blank
// node, or the page's own node, is the page's fetch.
export function nodeFetch(node, {
  self = selfNode, remote = globalThis.__tclaudeRemoteNode, fetchImpl = (...args) => globalThis.fetch(...args),
} = {}) {
  const page = remote?.id || self;
  if (!node || node === page) return fetchImpl;
  if (node === self && remote?.localFetch) return remote.localFetch;
  if (!NODE_ID.test(node)) throw new Error(`invalid node: ${node}`);
  return (input, init) => {
    if (input instanceof Request) return fetchImpl(new Request(peerPath(node, new URL(input.url).pathname + new URL(input.url).search), input), init);
    return fetchImpl(peerPath(node, input), init);
  };
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
