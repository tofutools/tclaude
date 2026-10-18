// dock-nodes.js — the right panel over several nodes (the fused Groups view).
//
// On one node's page the dock lists that node's catalogs. When the Groups
// view fuses several nodes, the fused view (fleet-merged-island.js) publishes
// its ticked nodes and their snapshots here, and the dock lists every shown
// node's catalogs merged: one card per name, with a pill per node saying
// whether that node holds it (present), holds a different version (differs)
// or lacks it (missing). A "Nodes" filter row narrows which nodes the dock
// shows, starting from the fused view's ticked nodes. A node that withholds a
// section (a restricted peer without node.spawn.read) is named in the
// section's header with the reason.

import { signal } from '@preact/signals';
import { featureNeeds, featureState } from './peer-view-limits.js';

// fusedDock is null off the fused view; else { nodes, snapshots, page }:
// nodes are the ticked nodes in fleet order ({ id, name, color }), snapshots
// maps a polled node's ID to its latest snapshot, and page is the page's own
// node, whose snapshot is the page's.
export const fusedDock = signal(null);

export function publishFusedDock(value) {
  fusedDock.value = value || null;
}

// hiddenNodes are the nodes the dock's filter row leaves out, on this page.
export const hiddenNodes = signal([]);

export function toggleHiddenNode(id, hidden = hiddenNodes) {
  const cur = hidden.value;
  hidden.value = cur.includes(id) ? cur.filter((x) => x !== id) : [...cur, id];
}

// shownNodes are the fused nodes the filter keeps.
export function shownNodes(fused, hidden = []) {
  return (fused?.nodes || []).filter((n) => !hidden.includes(n.id));
}

// nodeSnap is a fused node's snapshot: the page's own for the page node.
export function nodeSnap(fused, id, pageSnapshot) {
  if (!fused) return null;
  return (id === fused.page ? pageSnapshot : fused.snapshots?.[id]) || null;
}

// The catalogs ride the spawn catalog read (node.spawn.read): a peer view
// that does not include it sends them empty.
const CATALOG_FEATURE = 'spawn.catalog';

// catalogWithheld is why a node lists no catalogs here: '' when it shares
// them (this node, an unrestricted peer, or a peer granting the read), a
// reason otherwise. A node not read yet is 'loading'.
export function catalogWithheld(node, snapshot) {
  if (!snapshot) return 'loading';
  const pv = snapshot.peer_view;
  if (!pv || pv.level === 'unrestricted') return '';
  if (featureState(CATALOG_FEATURE, pv) === 'shared') return '';
  const needs = featureNeeds(CATALOG_FEATURE, pv) || 'node.spawn.read';
  return `${node.name} keeps its catalogs to itself: it needs ${needs}.`;
}

// nodeTrustLock is the 🔒 reason of a change to a node's catalog item from
// here: blank where the node lets this operator act as its own (this node,
// whose snapshot carries no peer view, or a peer trusting it unrestricted).
export function nodeTrustLock(node, snapshot, what) {
  const pv = snapshot?.peer_view;
  if (!pv || pv.level === 'unrestricted') return '';
  return `${node.name} keeps ${what} to its own operator: it needs unrestricted trust from it.`;
}

// VOLATILE_KEYS do not make two nodes' copies of an item different: store
// identities, timestamps and revisions differ between nodes by nature.
const VOLATILE_KEYS = new Set(['id', 'created_at', 'updated_at', 'revision', 'rev', 'version', 'source_hash']);

// canonical is an item's comparable form: its JSON with sorted keys and the
// volatile keys left out.
export function canonical(value) {
  if (Array.isArray(value)) return `[${value.map(canonical).join(',')}]`;
  if (value && typeof value === 'object') {
    return `{${Object.keys(value).filter((k) => !VOLATILE_KEYS.has(k)).sort()
      .map((k) => `${JSON.stringify(k)}:${canonical(value[k])}`).join(',')}}`;
  }
  return JSON.stringify(value ?? null);
}

// mergeCatalog merges the nodes' items of one section by name. Each entry has
// the name, byNode (node ID -> item), ref (the node whose copy the card
// shows: the page node when it holds one, else the first holder in node
// order) and state (node ID -> 'present' | 'differs' | 'missing', for every
// node in itemsByNode). A copy differs when it is not the ref's.
export function mergeCatalog(nodes, itemsByNode, nameOf, page = '') {
  const merged = new Map();
  for (const node of nodes) {
    const items = itemsByNode[node.id];
    if (!items) continue;
    for (const item of items) {
      const name = nameOf(item);
      if (!name) continue;
      if (!merged.has(name)) merged.set(name, { name, byNode: {} });
      merged.get(name).byNode[node.id] ??= item;
    }
  }
  const listed = nodes.filter((n) => itemsByNode[n.id]);
  return [...merged.values()].sort((a, b) => a.name.localeCompare(b.name)).map((entry) => {
    const ref = entry.byNode[page] ? page : listed.find((n) => entry.byNode[n.id])?.id;
    const want = canonical(entry.byNode[ref]);
    const state = {};
    for (const n of listed) {
      const item = entry.byNode[n.id];
      state[n.id] = !item ? 'missing' : canonical(item) === want ? 'present' : 'differs';
    }
    return { ...entry, ref, state };
  });
}
