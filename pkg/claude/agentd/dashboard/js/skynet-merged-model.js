// skynet-merged-model.js — DOM-free merge of several nodes' dashboard
// snapshots into one Groups listing for the top-level "Groups · all nodes"
// view. Every node keeps its own snapshot schema (a restricted peer's is the
// peer-view projection), so the existing Groups renderer draws the result.

import { fmtAge } from './skynet-model.js';

// MERGED_POLL_MS paces each peer's snapshot read while the merged view is on
// screen: slower than a per-node view (2 s), faster than the map's summaries.
export const MERGED_POLL_MS = 5000;

// STALE_AFTER_MS is how old a node's last good snapshot may get before its
// groups read as stale even without a failed read.
export const STALE_AFTER_MS = 30000;

function suffix(name, node) {
  return name ? `${name}@${node.name}` : name;
}

// nodeState describes one node's data for the merged view.
export function nodeState(node, entry, now = Date.now()) {
  const receivedAt = entry?.receivedAt ?? null;
  const ageMs = receivedAt != null ? Math.max(0, now - receivedAt) : null;
  const failed = !!entry?.failure;
  const stale = !node.local && (failed || (ageMs != null && ageMs > STALE_AFTER_MS));
  // A peer the hub reports offline that never answered: say so, rather than
  // "loading…" before its first poll or a bare "stale" after it fails.
  if (!node.local && !entry?.snapshot && node.online === false) return { stale: true, ageMs, label: 'offline', loaded: false };
  let label = '';
  if (stale) label = ageMs != null ? `stale · data ${fmtAge(ageMs)} old` : 'stale';
  return { stale, ageMs, label, loaded: !!entry?.snapshot };
}

// uniqueNodeNames gives every node a distinct display name for the @node
// suffix. A peer's name can be self-reported and so can collide with this
// node's or another peer's; a collision gets a short instance ID appended so
// two nodes' groups never share a name (the Groups tree keys on it). An '@'
// in a node name is replaced, so 'ops@a' on node 'b' and 'ops' on node 'a@b'
// cannot both become 'ops@a@b'.
export function uniqueNodeNames(nodes) {
  const base = (node) => String(node.name || node.id).replace(/@/g, '_');
  const count = new Map();
  for (const { node } of nodes) count.set(base(node), (count.get(base(node)) || 0) + 1);
  const out = new Map();
  for (const { node } of nodes) {
    const dup = count.get(base(node)) > 1 && !node.local;
    out.set(node.id, dup ? `${base(node)}~${String(node.id).replace(/^inst_/, '').slice(0, 6)}` : base(node));
  }
  return out;
}

// mergeSnapshots returns a snapshot-shaped object whose groups and agents come
// from every node. Group names (and parent references) carry @node so names
// never collide across nodes; each group also carries fleet_node for the
// renderer's node colour and stale treatment. The local snapshot provides the
// presentation settings (theme, attachments mode, …) since the view is local.
export function mergeSnapshots(input, now = Date.now()) {
  const names = uniqueNodeNames(input);
  const nodes = input.map(({ node, entry }) => ({ node: { ...node, name: names.get(node.id) }, entry }));
  const local = nodes.find((n) => n.node.local)?.entry?.snapshot || {};
  const groups = [];
  const agents = [];
  const seenAgents = new Set();
  const status = [];
  for (const { node, entry } of nodes) {
    const snap = entry?.snapshot;
    const st = nodeState(node, entry, now);
    status.push({ node, ...st, failure: entry?.failure || null });
    if (!snap) continue;
    for (const g of snap.groups || []) {
      groups.push({
        ...g,
        name: suffix(g.name, node),
        key: `${node.id}/${g.key || g.name}`,
        parent: g.parent ? suffix(g.parent, node) : g.parent,
        fleet_node: { id: node.id, name: node.name, color: node.color, local: !!node.local, stale: st.stale, label: st.label },
        // Links and attachments describe one node's configuration; the
        // merged overview does not carry them across.
        federation_links: undefined,
      });
    }
    for (const a of snap.agents || []) {
      const id = a.agent_id || a.conv_id;
      if (!id || seenAgents.has(id)) continue;
      seenAgents.add(id);
      agents.push(a);
    }
  }
  return {
    ...local,
    groups,
    agents,
    ungrouped: [],
    pending: [],
    links: [],
    peer_view: undefined,
    fleet_nodes: status,
  };
}
