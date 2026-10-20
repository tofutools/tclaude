// toolbar-node.js — which node the Groups toolbar's node-wide controls (the
// 🧠 global default spawn profile, the 🛡 global sandbox profile and
// + new group) show and set.
//
// One node's page: that node, and nothing here is set. The fused Groups view
// lists several nodes' groups on one page, so it publishes those nodes
// (setToolbarNodes) and the toolbar follows the node of the group last
// interacted with there (followToolbarNode), or the one picked in the
// toolbar's node chip (pickToolbarNode). Config
// dashboard.fused_toolbar_fixed_node turns the following off. Every node's
// own setup cannot fit the toolbar; the chip says whose it shows.
import { computed, signal } from '@preact/signals';
import { managedHint } from './peer-view-limits.js';

// toolbarNodes are the fused view's nodes, in its order: { id, name, color,
// local, page, profile, sandbox, peerView }. page marks the page's own node,
// whose values the toolbar's usual snapshot path paints; another node's come
// from its snapshot (profile, sandbox) as the fused view polls it.
export const toolbarNodes = signal([]);
const picked = signal('');

// toolbarNode is the node the toolbar acts on: the picked one while listed,
// else the page's own. null outside a fused view.
export const toolbarNode = computed(() => {
  const list = toolbarNodes.value;
  return list.find((n) => n.id === picked.value) || list.find((n) => n.page) || list[0] || null;
});

const nodeKey = (n) => [n.id, n.name, n.color, n.local, n.page, n.profile, n.sandbox, JSON.stringify(n.peerView || null)].join('\u0000');

// setToolbarNodes publishes the fused view's nodes (empty: no fused view, so
// the toolbar is the page's again and the next fused view starts there).
export function setToolbarNodes(list = []) {
  const next = list.map((n) => Object.freeze({ ...n }));
  if (!next.length) picked.value = '';
  const prev = toolbarNodes.value;
  if (prev.length === next.length && prev.every((n, i) => nodeKey(n) === nodeKey(next[i]))) return;
  toolbarNodes.value = Object.freeze(next);
}

// pickToolbarNode makes a listed node the toolbar's.
export function pickToolbarNode(id) {
  if (id && toolbarNodes.value.some((n) => n.id === id)) picked.value = id;
}

// followToolbarNode moves the toolbar to the node of a group acted on in the
// fused view, unless config dashboard.fused_toolbar_fixed_node keeps it on
// the chip's pick.
let fixedNode = false;
export function setToolbarFixedNode(value) {
  fixedNode = !!value;
}
export function followToolbarNode(id) {
  if (!fixedNode) pickToolbarNode(id);
}

// toolbarTarget is the node a toolbar control's requests go to: blank for the
// page's own node (the page's fetch and gating), else the node's ID.
export function toolbarTarget(node = toolbarNode.value) {
  return node && !node.page ? node.id : '';
}

// toolbarRefusal is why the toolbar's control (a MANAGED_CONTROLS selector in
// peer-view-limits.js) is refused on node, or ''. The page's own node is
// gated as before (by the peer page's limits); this node, reached from a
// peer's page, takes every control.
export function toolbarRefusal(node, selector) {
  if (!node || node.page || node.local) return '';
  return managedHint(selector, node.peerView, node.name);
}
