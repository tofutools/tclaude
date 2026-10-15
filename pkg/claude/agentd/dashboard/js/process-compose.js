// process-compose.js -- composition on the canvas (E2, tcl-lf1vlh): splice a
// node into a connector, wrap a selection as a ▣ subprocess with ports
// derived from its boundary connectors, and unwrap a ▣ node back into its
// parent. Each operation builds the next scope state from clones and lands it
// through model.commitScope, so it is exactly one undo step; anything invalid
// throws before the model is touched.
//
// Pure model code: no DOM, no fetch.

import {
  START_OUTCOME, SCOPE_SEPARATOR, MAX_SUBPROCESS_DEPTH, defaultNode, foldBody, scopeEdges, scopeNodes,
  isInlineSubprocess,
} from './process-edit-model.js';

const PARAM_REF = /\{\{\s*params\.([A-Za-z_][A-Za-z0-9_-]*)\s*\}\}/g;

function clone(value) {
  return value === undefined ? undefined : structuredClone(value);
}

function scopeState(model) {
  return { template: clone(model.template), edges: clone(model.edges), layout: clone(model.layout) };
}

function uniqueID(taken, base) {
  let candidate = base;
  for (let n = 2; taken.has(candidate); n += 1) candidate = `${base}-${n}`;
  taken.add(candidate);
  return candidate;
}

function slug(value) {
  return String(value || '').toLowerCase().replace(/[^a-z0-9._-]+/g, '-').replace(/^[^a-z0-9]+|-+$/g, '').slice(0, 48);
}

// ownScope splits a node into what this scope's params can reach and the
// nested body that has params of its own: a ▣ node's inputs bind from this
// scope, but its body's nodes and params refer to the body's params.
function ownScope(node) {
  if (node?.type !== 'subprocess' || !node.subprocess) return { own: node, nested: null };
  const { nodes, params, ...ports } = node.subprocess;
  return { own: { ...node, subprocess: ports }, nested: { nodes, params } };
}

function withNested(own, nested) {
  if (!nested) return own;
  const out = { ...own, subprocess: { ...own.subprocess } };
  if (nested.params !== undefined) out.subprocess.params = nested.params;
  if (nested.nodes !== undefined) out.subprocess.nodes = nested.nodes;
  return out;
}

// subprocessDepth is how many body levels a node brings along (0 for any
// non-▣ node or a referenced body).
function subprocessDepth(node) {
  if (node?.type !== 'subprocess' || !isInlineSubprocess(node.subprocess)) return 0;
  return 1 + Math.max(0, ...Object.values(node.subprocess.nodes || {}).map(subprocessDepth));
}

// rekeyLayout moves layout pins (and the namespaced pins of nested bodies)
// from one id to another; ids absent from `moves` keep theirs.
function rekeyLayout(layout, moves) {
  const out = { ...layout, nodes: {} };
  const target = (key) => {
    for (const [from, to] of moves) {
      if (key === from) return to;
      if (key.startsWith(from + SCOPE_SEPARATOR)) return to + key.slice(from.length);
    }
    return key;
  };
  for (const [key, value] of Object.entries(layout.nodes || {})) out.nodes[target(key)] = value;
  if (layout.edges) {
    out.edges = {};
    for (const [key, value] of Object.entries(layout.edges)) out.edges[target(key)] = value;
  }
  return out;
}

// ---- insert / splice ----------------------------------------------------------

// insertNode adds a node of `type` (init customizes it, e.g. a template
// reference) as one undo step and returns its id.
export function insertNode(model, type, { x, y, name, init } = {}) {
  model.assertCanInsert();
  model.assertCanNest(type);
  const next = scopeState(model);
  const id = model.uniqueNodeID(slug(name) || type);
  const node = defaultNode(type);
  if (name) node.name = name;
  init?.(node);
  next.template.nodes[id] = node;
  if (Number.isFinite(x) && Number.isFinite(y)) next.layout.nodes[id] = { x, y };
  model.commitScope(next);
  return id;
}

// spliceNode drops a new node onto a connector: from --outcome--> new
// --pass--> old target. The incoming connector keeps its identity (and its
// label pin). Connectors inside an inline-expanded body splice inside it.
export function spliceNode(model, { from, outcome }, type, { x, y, name, init } = {}) {
  const nested = model.nestedTarget(from);
  if (nested) {
    const { dx, dy } = model.expansionOffset(nested.owner);
    const id = model.withinBody(nested.owner, () => spliceNode(model, { from: nested.inner, outcome }, type, {
      x: Number.isFinite(x) ? x - dx : x, y: Number.isFinite(y) ? y - dy : y, name, init,
    }));
    return nested.owner + SCOPE_SEPARATOR + id;
  }
  if (type === 'end' || type === 'start') {
    throw new Error(`A ${type} node cannot be spliced into a connector: it has no way ${type === 'end' ? 'out' : 'in'}.`);
  }
  model.assertCanInsert();
  model.assertCanNest(type);
  const edge = model.findEdge(from, outcome);
  if (!edge) throw new Error(`unknown connector ${from} (${outcome})`);
  model.assertEdgeEditable(edge);
  const id = model.uniqueNodeID(slug(name) || type);
  const node = defaultNode(type);
  if (name) node.name = name;
  init?.(node);
  const planned = { ...model.template.nodes, [id]: node };
  for (const [source, target] of [[edge.from, id], [id, edge.to]]) {
    const message = model.edgePortRejection(source, target, planned);
    if (message) throw new Error(message);
  }
  const outEdge = { from: id, outcome: 'pass', to: edge.to };
  model.assertEdgeEditable(outEdge);
  const next = scopeState(model);
  next.template.nodes[id] = node;
  next.edges.find((candidate) => candidate.from === from && candidate.outcome === outcome).to = id;
  next.edges.push(outEdge);
  if (Number.isFinite(x) && Number.isFinite(y)) next.layout.nodes[id] = { x, y };
  model.commitScope(next);
  return id;
}

// ---- wrap ---------------------------------------------------------------------

// wrapPlan derives the ▣ node a selection would collapse into. The region
// must be entered through one node (the body's start); every connector
// leaving it becomes an out-port: one body end per outside target, mapped
// onto a ▣ outcome named after the first connector that left for it. Params
// the wrapped nodes reference become body params bound from the parent's.
export function wrapPlan(model, ids) {
  const selected = new Set((ids || []).filter((id) => model.template.nodes[id]));
  if (!selected.size) throw new Error('Select the nodes to wrap first.');
  for (const id of ids || []) {
    if (model.nestedTarget(id)) throw new Error('Collapse the expanded subprocess before wrapping its inner nodes.');
  }
  for (const id of selected) model.assertNodeEditable(id);
  const depth = model.frames.length + 1 + Math.max(0, ...[...selected].map((id) => subprocessDepth(model.template.nodes[id])));
  if (depth > MAX_SUBPROCESS_DEPTH) throw new Error(`subprocesses nest at most ${MAX_SUBPROCESS_DEPTH} levels deep`);

  const incoming = model.edges.filter((edge) => !selected.has(edge.from) && selected.has(edge.to));
  const outgoing = model.edges.filter((edge) => selected.has(edge.from) && !selected.has(edge.to));
  const internal = model.edges.filter((edge) => selected.has(edge.from) && selected.has(edge.to));
  const entries = [...new Set(incoming.map((edge) => edge.to))].sort();
  if (entries.length > 1) {
    throw new Error(`The selection is entered at ${entries.length} nodes (${entries.join(', ')}); a subprocess has one start. Select a region entered through one node.`);
  }
  // An island (no connector in) starts at a node nothing inside leads to.
  const entry = entries[0] || [...selected].sort().find((id) => !internal.some((edge) => edge.to === id)) || [...selected].sort()[0];
  for (const edge of [...incoming, ...outgoing]) model.assertEdgeEditable(edge);

  const bodyIDs = new Set(selected);
  const ends = [];
  const exits = new Map();
  for (const edge of [...outgoing].sort((a, b) => `${a.from}\0${a.outcome}`.localeCompare(`${b.from}\0${b.outcome}`, 'en'))) {
    if (!exits.has(edge.to)) {
      const target = model.template.nodes[edge.to];
      exits.set(edge.to, {
        target: edge.to, outcome: edge.outcome,
        end: uniqueID(bodyIDs, `to-${edge.to}`),
        name: `→ ${target?.name || edge.to}`,
        edges: [],
      });
    }
    exits.get(edge.to).edges.push(edge);
  }
  const outcomes = new Set();
  for (const exit of exits.values()) {
    exit.outcome = uniqueID(outcomes, exit.outcome);
    ends.push(exit);
  }
  const refs = new Set();
  for (const id of selected) {
    for (const match of JSON.stringify(ownScope(model.template.nodes[id]).own).matchAll(PARAM_REF)) refs.add(match[1]);
  }
  return { selected, entry, incoming, outgoing, internal, exits: ends, params: [...refs].sort() };
}

// wrapSelection collapses the selection into one ▣ node with an inline body
// (ports derived by wrapPlan) and returns its id. position is where the ▣
// node lands (the editor passes the selection's drawn center).
export function wrapSelection(model, ids, { name = '', position } = {}) {
  model.assertCanInsert();
  const plan = wrapPlan(model, ids);
  const label = String(name || '').trim() || 'Subprocess';
  const id = model.uniqueNodeID(slug(label) || 'subprocess');
  const next = scopeState(model);

  // Connectors live in edges; a root node's next map is the stale view the
  // server sent and must not resurrect into the body (scopeNodes strips it).
  const bodyNodes = scopeNodes({ nodes: Object.fromEntries([...plan.selected].map((nodeID) => [nodeID, model.template.nodes[nodeID]])) });
  const bodyEdges = [{ from: '', outcome: START_OUTCOME, to: plan.entry }, ...clone(plan.internal)];
  const outputs = {};
  for (const exit of plan.exits) {
    bodyNodes[exit.end] = { type: 'end', result: 'success', name: exit.name };
    for (const edge of exit.edges) bodyEdges.push({ from: edge.from, outcome: edge.outcome, to: exit.end });
    outputs[exit.end] = exit.outcome;
  }
  const params = {};
  const inputs = {};
  for (const param of plan.params) {
    params[param] = clone(model.template.params?.[param]) || { type: 'string' };
    inputs[param] = `{{ params.${param} }}`;
  }
  const body = foldBody({}, { nodes: bodyNodes, params }, bodyEdges);
  if (Object.keys(inputs).length) body.inputs = inputs;
  if (Object.keys(outputs).length) body.outputs = outputs;

  for (const nodeID of plan.selected) delete next.template.nodes[nodeID];
  next.template.nodes[id] = { type: 'subprocess', name: label, subprocess: body };
  const touched = (edge) => plan.selected.has(edge.from) || plan.selected.has(edge.to);
  next.edges = next.edges.filter((edge) => !touched(edge));
  for (const edge of plan.incoming) next.edges.push({ ...edge, to: id });
  for (const exit of plan.exits) next.edges.push({ from: id, outcome: exit.outcome, to: exit.target });

  // Pins move into the body's namespace; the ▣ node lands at position or
  // at the wrapped pins' centroid.
  const pins = [...plan.selected].map((nodeID) => model.layout.nodes[nodeID]).filter(Boolean);
  next.layout = rekeyLayout(next.layout, [...plan.selected].map((nodeID) => [nodeID, id + SCOPE_SEPARATOR + nodeID]));
  const at = position || (pins.length ? {
    x: Math.round(pins.reduce((sum, pin) => sum + pin.x, 0) / pins.length),
    y: Math.round(pins.reduce((sum, pin) => sum + pin.y, 0) / pins.length),
  } : null);
  if (at && Number.isFinite(at.x) && Number.isFinite(at.y)) next.layout.nodes[id] = { x: at.x, y: at.y };
  // New end nodes sit just right of the region, beside the node that leaves
  // for them, so an expanded or drilled-in body stays compact.
  if (pins.length) {
    const right = Math.max(...pins.map((pin) => pin.x)) + 220;
    const used = new Set();
    for (const exit of plan.exits) {
      const source = model.layout.nodes[exit.edges[0].from];
      let y = Math.round(source ? source.y : pins[0].y);
      while (used.has(y)) y += 100;
      used.add(y);
      next.layout.nodes[id + SCOPE_SEPARATOR + exit.end] = { x: right, y };
    }
  }
  // Label pins of connectors that now leave the ▣ node instead belong to
  // connectors that no longer exist in this scope.
  if (next.layout.edges) {
    for (const [from] of Object.entries(next.layout.edges)) {
      if (!from.includes(SCOPE_SEPARATOR) && !next.edges.some((edge) => edge.from === from)) delete next.layout.edges[from];
    }
    if (!Object.keys(next.layout.edges).length) delete next.layout.edges;
  }
  model.commitScope(next);
  return id;
}

// ---- unwrap -------------------------------------------------------------------

// unwrapSubprocess inlines a ▣ node's body back into this scope: the body's
// start takes the ▣ node's incoming connectors, each body end mapped onto a
// ▣ outcome dissolves into that outcome's target, and bound params are
// substituted by their bindings. A wrap followed by an unwrap restores the
// original topology. Returns the inlined node ids.
export function unwrapSubprocess(model, id, { origin } = {}) {
  if (model.nestedTarget(id) || !model.canEnterScope(id)) throw new Error(`node ${id} has no inline subprocess body to unwrap`);
  model.assertCanInsert();
  model.assertNodeEditable(id);
  const node = model.template.nodes[id];
  const sub = node.subprocess;
  const bodyNodes = scopeNodes(sub);
  let bodyEdges = scopeEdges(sub);
  for (const edge of [...model.incomingEdges(id), ...model.outgoingEdges(id)]) model.assertEdgeEditable(edge);

  // A body that opens on a bare start marker (a fresh ▣) enters at its
  // successor: a start node in the middle of a graph would not be a start.
  let entry = sub.start || '';
  const marker = bodyNodes[entry];
  const markerOut = bodyEdges.filter((edge) => edge.from === entry);
  if (marker?.type === 'start' && markerOut.length === 1) {
    delete bodyNodes[entry];
    bodyEdges = bodyEdges.filter((edge) => edge.from !== entry);
    entry = markerOut[0].to;
  }

  const taken = new Set(Object.keys(model.template.nodes).filter((candidate) => candidate !== id));
  const rename = new Map(Object.keys(bodyNodes).sort().map((inner) => [inner, uniqueID(taken, inner)]));

  const parentOut = model.outgoingEdges(id);
  const outcomeTarget = new Map(parentOut.map((edge) => [edge.outcome, edge.to]));
  const outputs = sub.outputs || {};
  const dissolve = new Map();
  for (const [inner, child] of Object.entries(bodyNodes)) {
    if (child?.type !== 'end') continue;
    const outcome = outputs[inner] || (parentOut.length === 1 ? parentOut[0].outcome : '');
    const target = outcomeTarget.get(outcome);
    if (target) dissolve.set(inner, target);
  }

  // Bound params become their bindings; unbound ones join the parent's.
  const params = clone(model.template.params) || {};
  const substitutions = new Map();
  for (const [param, spec] of Object.entries(sub.params || {})) {
    const binding = sub.inputs?.[param];
    if (binding !== undefined) {
      if (binding !== `{{ params.${param} }}`) substitutions.set(param, String(binding));
    } else if (!params[param]) {
      params[param] = clone(spec);
    }
  }
  const substitute = (value) => {
    if (!substitutions.size) return value;
    const { own, nested } = ownScope(value);
    return withNested(JSON.parse(JSON.stringify(own).replace(PARAM_REF, (match, param) => (
      substitutions.has(param) ? JSON.stringify(substitutions.get(param)).slice(1, -1) : match))), nested);
  };

  const next = scopeState(model);
  next.template.params = params;
  delete next.template.nodes[id];
  for (const [inner, child] of Object.entries(bodyNodes)) {
    if (dissolve.has(inner)) continue;
    next.template.nodes[rename.get(inner)] = substitute(child);
  }
  const mapped = (inner) => dissolve.get(inner) || rename.get(inner);
  next.edges = next.edges.filter((edge) => edge.from !== id).map((edge) => (edge.to === id
    ? { ...edge, to: mapped(entry) } : edge)).filter((edge) => edge.to);
  for (const edge of bodyEdges) {
    if (!edge.from || dissolve.has(edge.from)) continue;
    next.edges.push({ from: rename.get(edge.from), outcome: edge.outcome, to: mapped(edge.to) });
  }

  // Inlined nodes land where the body was drawn: around the ▣ node, or
  // exactly where an inline expansion showed them.
  const prefix = id + SCOPE_SEPARATOR;
  const positions = model.bodyPositions(id);
  const expanded = model.isExpanded(id);
  let shift;
  if (expanded) {
    const { dx, dy } = model.expansionOffset(id);
    shift = { dx, dy };
  } else {
    const at = origin || model.layout.nodes[id] || { x: 0, y: 0 };
    // A wrapped region keeps its pins in parent coordinates, with the ▣ node
    // inside their span: put them back exactly. Anything else is centered
    // on the ▣ node.
    const pins = Object.keys(bodyNodes).map((inner) => model.layout.nodes[prefix + inner]).filter(Boolean);
    const spans = pins.length && Math.min(...pins.map((p) => p.x)) <= at.x && at.x <= Math.max(...pins.map((p) => p.x))
      && Math.min(...pins.map((p) => p.y)) <= at.y && at.y <= Math.max(...pins.map((p) => p.y));
    const all = spans ? [] : [...positions.values()];
    const cx = all.length ? (Math.min(...all.map((p) => p.x)) + Math.max(...all.map((p) => p.x))) / 2 : 0;
    const cy = all.length ? (Math.min(...all.map((p) => p.y)) + Math.max(...all.map((p) => p.y))) / 2 : 0;
    shift = spans ? { dx: 0, dy: 0 } : { dx: at.x - cx, dy: at.y - cy };
  }
  const layout = { ...next.layout, nodes: {} };
  for (const [key, value] of Object.entries(next.layout.nodes || {})) {
    if (key !== id && !key.startsWith(prefix)) layout.nodes[key] = value;
  }
  for (const [inner, newID] of rename) {
    if (dissolve.has(inner) || !positions.has(inner)) continue;
    const p = positions.get(inner);
    layout.nodes[newID] = { x: Math.round(p.x + shift.dx), y: Math.round(p.y + shift.dy) };
    // Deeper bodies' pins follow their ▣ node to its new id.
    for (const [key, value] of Object.entries(next.layout.nodes || {})) {
      if (key.startsWith(prefix + inner + SCOPE_SEPARATOR)) layout.nodes[newID + key.slice((prefix + inner).length)] = value;
    }
  }
  if (next.layout.edges) {
    layout.edges = {};
    for (const [key, value] of Object.entries(next.layout.edges)) {
      if (key === id) continue;
      if (!key.startsWith(prefix)) { layout.edges[key] = value; continue; }
      const rest = key.slice(prefix.length);
      const inner = rest.split(SCOPE_SEPARATOR)[0];
      if (rename.has(inner) && !dissolve.has(inner)) layout.edges[rename.get(inner) + rest.slice(inner.length)] = value;
    }
    if (!Object.keys(layout.edges).length) delete layout.edges;
  }
  next.layout = layout;
  model.collapseSubprocess(id);
  model.commitScope(next);
  return [...rename].filter(([inner]) => !dissolve.has(inner)).map(([, newID]) => newID);
}
