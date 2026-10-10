// skynet-scope.js — DOM-free model of the node scope (tcl-sb4grp): which nodes
// the dashboard shows. One node is today's per-node view (this node, or a peer
// through ?node=). Fusing ticks several nodes: Groups and Terminals show them
// together, every other tab keeps showing one node, the primary (the page's
// own node), and says so.
//
// The fused set is a property of the page like ?node=, carried in ?nodes=:
// "all" (every linked node, including ones linked later) or a comma-separated
// list of instance IDs.

// FUSED_TABS show every ticked node; every other per-node tab shows the primary.
export const FUSED_TABS = new Set(['groups', 'terminals', 'messages']);

// BUTTON_LIMIT is how many nodes (this one included) still get a button each;
// past it the buttons fold into one node dropdown.
export const BUTTON_LIMIT = 4;

const ID_RE = /^inst_[a-z0-9]{4,64}$/;

// parseFused reads ?nodes= from a query string: null (not fused), 'all', or
// the listed instance IDs (deduplicated; anything malformed is dropped).
export function parseFused(search) {
  const raw = new URLSearchParams(search || '').get('nodes');
  if (raw == null) return null;
  if (raw === 'all' || raw === '') return 'all';
  const ids = [...new Set(raw.split(',').filter((id) => ID_RE.test(id)))];
  return ids.length ? ids : 'all';
}

// fusedParam is the ?nodes= value for a fused set ('' when not fused).
export function fusedParam(fused) {
  if (!fused) return '';
  return fused === 'all' ? 'all' : fused.join(',');
}

// withFused returns a query string with ?nodes= set for fused (or removed).
export function withFused(search, fused) {
  const params = new URLSearchParams(search || '');
  const v = fusedParam(fused);
  if (v) params.set('nodes', v); else params.delete('nodes');
  const q = params.toString();
  return q ? `?${q}` : '';
}

// fleetNodes lists this node first, then its peers in chip order.
export function fleetNodes(fleet) {
  return fleet ? [fleet.self, ...fleet.peers] : [];
}

// isTicked reports whether a node is in the fused set.
export function isTicked(fused, id) {
  return fused === 'all' || (Array.isArray(fused) && fused.includes(id));
}

// tickedNodes lists the fleet's nodes in the fused set, in fleet order. IDs no
// longer in the fleet (a peer untrusted since) are ignored.
export function tickedNodes(fleet, fused) {
  if (!fused) return [];
  return fleetNodes(fleet).filter((n) => isTicked(fused, n.id));
}

// toggleNode ticks or unticks one node. It returns the new fused set, or
// { only: id } when a single node is left: one ticked node is no longer a
// fused view but that node's own view. Ticking every node is 'all'.
export function toggleNode(fleet, fused, id) {
  const all = fleetNodes(fleet).map((n) => n.id);
  const cur = new Set(fused === 'all' ? all : (fused || []).filter((x) => all.includes(x)));
  if (cur.has(id)) cur.delete(id); else cur.add(id);
  const next = all.filter((x) => cur.has(x));
  if (next.length <= 1) return { only: next[0] || '' };
  return next.length === all.length ? 'all' : next;
}

// selectorKind says how the node selector draws the nodes: a button each up
// to BUTTON_LIMIT nodes, one dropdown past it.
export function selectorKind(fleet, limit = BUTTON_LIMIT) {
  return fleetNodes(fleet).length > limit ? 'dropdown' : 'buttons';
}

// filterNodes narrows the dropdown by a typed query (name or instance ID,
// case-insensitive). This node always stays listed, so getting home is one
// click whatever the filter says.
export function filterNodes(fleet, query) {
  const q = String(query || '').trim().toLowerCase();
  const nodes = fleetNodes(fleet);
  if (!q) return nodes;
  return nodes.filter((n) => n.local || n.name.toLowerCase().includes(q) || n.id.toLowerCase().includes(q));
}
