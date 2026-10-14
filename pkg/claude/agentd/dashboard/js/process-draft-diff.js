// process-draft-diff.js -- pure three-way diff + merge for reviewing another
// author's saved template generation (typically the docked process scribe's)
// against the human's live editor draft (E4, tcl-v7byug).
//
//   base   = the generation the editor draft was loaded from / last saved as
//   ours   = the live editor draft (possibly with unsaved edits)
//   theirs = the newer saved generation under review
//
// A change is one base→theirs difference on a stable identity: a node id, an
// edge (from, outcome) key -- the start pseudo edge included -- or the template
// settings as a whole. Accepting a change copies theirs onto ours for that
// identity only; rejecting leaves ours untouched. A change conflicts when the
// human ALSO changed that identity locally (ours ≠ base) to something other
// than theirs: the UI must force an explicit per-change choice for those.
//
// No DOM, no fetch: jstest/process-draft-diff.test.mjs runs this exact file.
// Template content is untrusted; labels here are bounded display copy and are
// rendered as text only.

import { graphEdgeID } from './process-edit-model.js';

export const DRAFT_DIFF_LABEL_MAX = 80;
// A review is a deliberate, human-paced step. A proposal touching more than
// this many identities is better applied via reload than stepped through.
export const DRAFT_DIFF_MAX_CHANGES = 500;

const ORDER = { template: 0, node: 1, edge: 2 };
const OP_ORDER = { added: 0, changed: 1, removed: 2 };

function stable(value) {
  if (Array.isArray(value)) return value.map(stable);
  if (!value || typeof value !== 'object') return value;
  return Object.fromEntries(Object.keys(value).sort().map((key) => [key, stable(value[key])]));
}

function same(a, b) {
  return JSON.stringify(stable(a)) === JSON.stringify(stable(b));
}

function clone(value) {
  return value === undefined ? undefined : structuredClone(value);
}

function boundedLabel(value) {
  const text = String(value ?? '');
  return text.length <= DRAFT_DIFF_LABEL_MAX ? text : `${text.slice(0, DRAFT_DIFF_LABEL_MAX - 1)}…`;
}

// node.next is a server-side projection of the edges array; it never carries
// meaning of its own in the editor, so it never counts as a change.
function semanticNode(node) {
  if (!node || typeof node !== 'object') return node;
  const { next: _next, ...rest } = node;
  return rest;
}

// Template settings: every top-level field except identity, the node map,
// start (projected from the start pseudo edge) and the legacy layout slot.
function templateSettings(template = {}) {
  const { id: _id, nodes: _nodes, start: _start, layout: _layout, ...rest } = template || {};
  // The editor model materializes an empty params map; the wire may omit it.
  if (rest.params && typeof rest.params === 'object' && !Object.keys(rest.params).length) delete rest.params;
  return rest;
}

export function draftEdgeKey(from, outcome) {
  return `${encodeURIComponent(String(from ?? ''))}:${encodeURIComponent(String(outcome ?? ''))}`;
}

function edgeMap(edges) {
  const map = new Map();
  for (const edge of edges || []) {
    if (!edge || typeof edge !== 'object') continue;
    map.set(draftEdgeKey(edge.from, edge.outcome), { from: String(edge.from ?? ''), outcome: String(edge.outcome ?? ''), to: String(edge.to ?? '') });
  }
  return map;
}

function opFor(had, has) {
  if (!had && has) return 'added';
  if (had && !has) return 'removed';
  return 'changed';
}

function changedFields(a = {}, b = {}) {
  const keys = new Set([...Object.keys(a || {}), ...Object.keys(b || {})]);
  return [...keys].filter((key) => !same(a?.[key], b?.[key])).sort();
}

function normalizedView(view = {}) {
  const template = view?.template || {};
  return {
    template,
    nodes: template.nodes || {},
    edges: edgeMap(view?.edges),
    layout: view?.layout || {},
  };
}

function nodeLabel(id, ...nodes) {
  const named = nodes.find((node) => node?.name);
  return boundedLabel(named ? `${named.name} (${id})` : id);
}

function edgeLabel(edge) {
  if (!edge.from) return boundedLabel(`start → ${edge.to}`);
  return boundedLabel(`${edge.from} —${edge.outcome}→ ${edge.to}`);
}

// diffTemplateDrafts lists every base→theirs change that ours does not
// already match. A missing base (an unknown loaded generation) degrades to a
// two-way diff with ours as the base, which can never report a conflict.
export function diffTemplateDrafts({ base, ours, theirs } = {}) {
  const O = normalizedView(ours);
  const T = normalizedView(theirs);
  const B = base ? normalizedView(base) : O;
  const changes = [];

  const baseSettings = templateSettings(B.template);
  const theirSettings = templateSettings(T.template);
  const ourSettings = templateSettings(O.template);
  if (!same(baseSettings, theirSettings) && !same(ourSettings, theirSettings)) {
    changes.push({
      id: 'template', kind: 'template', op: 'changed', label: 'template settings',
      fields: changedFields(baseSettings, theirSettings),
      conflict: !same(ourSettings, baseSettings),
      requires: [],
    });
  }

  const nodeIDs = new Set([...Object.keys(B.nodes), ...Object.keys(T.nodes)]);
  for (const id of nodeIDs) {
    const before = semanticNode(B.nodes[id]); const after = semanticNode(T.nodes[id]);
    const mine = semanticNode(O.nodes[id]);
    if (same(before, after) || same(mine, after)) continue;
    const op = opFor(!!before, !!after);
    changes.push({
      id: `node:${id}`, kind: 'node', op, nodeId: id,
      label: nodeLabel(id, after, mine, before),
      nodeType: String((after || mine || before)?.type || 'task'),
      fields: op === 'changed' ? changedFields(before, after) : [],
      conflict: !same(mine, before),
      requires: [],
    });
  }

  const edgeKeys = new Set([...B.edges.keys(), ...T.edges.keys()]);
  for (const key of edgeKeys) {
    const before = B.edges.get(key); const after = T.edges.get(key); const mine = O.edges.get(key);
    if (same(before, after) || same(mine, after)) continue;
    const op = opFor(!!before, !!after);
    const shown = after || mine || before;
    changes.push({
      id: `edge:${key}`, kind: 'edge', op,
      edge: { from: shown.from, outcome: shown.outcome, to: shown.to },
      ...(mine && after && mine.to !== after.to ? { previousTo: mine.to } : {}),
      label: edgeLabel(shown),
      fields: op === 'changed' ? ['to'] : [],
      conflict: !same(mine, before),
      requires: [],
    });
  }

  // Dependencies keep every partial decision set applicable without dangling
  // references: an added/retargeted edge needs its added endpoint nodes, and
  // removing a node needs every theirs-side edge change touching it.
  const byID = new Map(changes.map((change) => [change.id, change]));
  const nodeChange = (id) => byID.get(`node:${id}`);
  for (const change of changes) {
    if (change.kind !== 'edge' || change.op === 'removed') continue;
    for (const endpoint of [change.edge.from, change.edge.to]) {
      const dependency = endpoint && nodeChange(endpoint);
      if (dependency && dependency.op === 'added') change.requires.push(dependency.id);
    }
  }
  for (const change of changes) {
    if (change.kind !== 'node' || change.op !== 'removed') continue;
    for (const edgeChange of changes) {
      if (edgeChange.kind !== 'edge') continue;
      const before = B.edges.get(edgeChange.id.slice(5));
      const touches = [edgeChange.edge, before].some((edge) => edge && (edge.from === change.nodeId || edge.to === change.nodeId));
      if (touches) change.requires.push(edgeChange.id);
    }
    // A removal would also drop connectors the human drew to this node
    // locally. That is unsaved human work, so it counts as a conflict.
    for (const [key, edge] of O.edges) {
      if (B.edges.has(key) || (edge.from !== change.nodeId && edge.to !== change.nodeId)) continue;
      change.conflict = true;
    }
  }

  changes.sort((a, b) => (ORDER[a.kind] - ORDER[b.kind]) || (OP_ORDER[a.op] - OP_ORDER[b.op])
    || a.id.localeCompare(b.id, 'en'));
  return changes;
}

export function draftDiffCounts(changes = [], decisions = {}) {
  const counts = { total: changes.length, added: 0, changed: 0, removed: 0, conflicts: 0, pending: 0, accepted: 0, rejected: 0 };
  for (const change of changes) {
    counts[change.op] += 1;
    if (change.conflict) counts.conflicts += 1;
    const verdict = decisions[change.id];
    if (verdict === 'accept') counts.accepted += 1;
    else if (verdict === 'reject') counts.rejected += 1;
    else counts.pending += 1;
  }
  return counts;
}

function closure(changes, start, next) {
  const seen = new Set();
  const queue = [start];
  while (queue.length) {
    const id = queue.shift();
    if (seen.has(id)) continue;
    seen.add(id);
    queue.push(...next(id));
  }
  return seen;
}

// decideDraftChange records a verdict for one change and keeps the decision
// set consistent: accepting pulls in its requirements, rejecting pushes out
// every change that requires it. Returns a new decisions object plus the ids
// that moved, so a caller can tell the human about the linked changes.
export function decideDraftChange(changes = [], decisions = {}, id, verdict) {
  const byID = new Map(changes.map((change) => [change.id, change]));
  if (!byID.has(id)) return { decisions, affected: [] };
  const next = { ...decisions };
  let affected;
  if (verdict === 'accept') {
    affected = closure(changes, id, (current) => byID.get(current)?.requires || []);
  } else if (verdict === 'reject') {
    const dependents = new Map();
    for (const change of changes) {
      for (const required of change.requires) {
        if (!dependents.has(required)) dependents.set(required, []);
        dependents.get(required).push(change.id);
      }
    }
    affected = closure(changes, id, (current) => dependents.get(current) || []);
  } else {
    // Undo a verdict. Only the change itself returns to pending; the linked
    // changes keep their verdicts so the set stays applicable.
    delete next[id];
    return { decisions: next, affected: [id] };
  }
  for (const changeID of affected) next[changeID] = verdict;
  return { decisions: next, affected: [...affected] };
}

// decideAllDraftChanges settles every pending change. Conflicts are never
// settled implicitly: pass includeConflicts only after an explicit human
// choice that covers them.
export function decideAllDraftChanges(changes = [], decisions = {}, verdict, { includeConflicts = false } = {}) {
  let next = { ...decisions };
  for (const change of changes) {
    if (next[change.id] || (change.conflict && !includeConflicts)) continue;
    next = decideDraftChange(changes, next, change.id, verdict).decisions;
  }
  return next;
}

export function pendingDraftChanges(changes = [], decisions = {}) {
  return changes.filter((change) => !decisions[change.id]);
}

// mergeDraftDecisions applies the accepted changes to a copy of ours and
// returns the editor content ({ template, edges, layout }). Layout is
// editor-owned: ours keeps its positions, an accepted added node adopts
// theirs' position when theirs has one, and stale edge pins are pruned.
export function mergeDraftDecisions({ ours, theirs, changes = [], decisions = {} } = {}) {
  const template = clone(ours?.template) || {};
  template.nodes = template.nodes || {};
  let edges = clone(ours?.edges) || [];
  const layout = clone(ours?.layout) || {};
  layout.nodes = layout.nodes || {};
  const T = normalizedView(theirs);
  const accepted = changes.filter((change) => decisions[change.id] === 'accept');

  for (const change of accepted.filter((item) => item.kind === 'template')) {
    const settings = templateSettings(template);
    for (const key of Object.keys(settings)) delete template[key];
    Object.assign(template, clone(templateSettings(T.template)));
  }
  for (const change of accepted.filter((item) => item.kind === 'node')) {
    const id = change.nodeId;
    if (change.op === 'removed') {
      delete template.nodes[id];
      delete layout.nodes[id];
      edges = edges.filter((edge) => edge.from !== id && edge.to !== id);
      continue;
    }
    const next = semanticNode(clone(T.nodes[id]));
    template.nodes[id] = next;
    if (change.op === 'added' && T.layout.nodes?.[id] && !layout.nodes[id]) {
      layout.nodes[id] = clone(T.layout.nodes[id]);
    }
  }
  for (const change of accepted.filter((item) => item.kind === 'edge')) {
    const key = change.id.slice(5);
    edges = edges.filter((edge) => draftEdgeKey(edge.from, edge.outcome) !== key);
    if (change.op === 'removed') continue;
    const edge = T.edges.get(key);
    if (edge) edges.push({ from: edge.from, outcome: edge.outcome, to: edge.to });
  }
  // Start is a projection of the start pseudo edge (mirrors the model).
  const start = edges.find((edge) => edge.from === '' && edge.outcome === 'start');
  if (start) template.start = start.to;
  else delete template.start;
  // Defensive: never hand the editor a connector to a missing node. The
  // dependency closure makes this unreachable for consistent decision sets.
  edges = edges.filter((edge) => (edge.from === '' || template.nodes[edge.from]) && template.nodes[edge.to]);
  if (layout.edges) {
    const live = new Set(edges.map((edge) => `${edge.from}\u0000${edge.outcome}`));
    for (const [from, byOutcome] of Object.entries(layout.edges)) {
      for (const outcome of Object.keys(byOutcome || {})) {
        if (!live.has(`${from}\u0000${outcome}`)) delete byOutcome[outcome];
      }
      if (!Object.keys(byOutcome || {}).length) delete layout.edges[from];
    }
    if (!Object.keys(layout.edges).length) delete layout.edges;
  }
  return { template, edges, layout };
}

// sameDraftContent compares merged editor content to a saved view: semantic
// template + edges, plus node positions for every surviving node.
export function sameDraftContent(a = {}, b = {}) {
  const nodesOf = (view) => Object.fromEntries(Object.entries(view?.template?.nodes || {})
    .map(([id, node]) => [id, semanticNode(node)]));
  const edgesOf = (view) => [...edgeMap(view?.edges).entries()].sort(([x], [y]) => x.localeCompare(y, 'en'));
  return same(templateSettings(a.template), templateSettings(b.template))
    && same(nodesOf(a), nodesOf(b))
    && same(edgesOf(a), edgesOf(b))
    && same(a.layout?.nodes || {}, b.layout?.nodes || {})
    && same(a.layout?.edges || {}, b.layout?.edges || {});
}

function changeState(change, decisions, cursorID) {
  return {
    op: change.op,
    state: decisions[change.id] === 'accept' ? 'accepted' : decisions[change.id] === 'reject' ? 'rejected' : 'pending',
    conflict: !!change.conflict,
    current: change.id === cursorID,
    tag: change.op === 'added' ? '+ added' : change.op === 'removed' ? '− removed'
      : boundedLabel(`~ ${change.fields?.length ? change.fields.join(', ') : 'changed'}`),
  };
}

// overlayDraftDiff projects the review onto a ProcessGraph input built from
// ours (model.graph(), possibly validation-decorated). Theirs-only nodes and
// edges are added as proposals; ours-only ones stay and are marked removed;
// a retargeted edge shows its proposed route plus a removed ghost of the
// current one. Every decorated item carries `diff` for the graph core.
export function overlayDraftDiff(graph, { theirs, changes = [], decisions = {}, cursor = '' } = {}) {
  const out = { nodes: (graph?.nodes || []).map((node) => ({ ...node })), edges: (graph?.edges || []).map((edge) => ({ ...edge })) };
  const T = normalizedView(theirs);
  const nodes = new Map(out.nodes.map((node) => [node.id, node]));
  for (const change of changes) {
    if (change.kind !== 'node') continue;
    const diff = changeState(change, decisions, cursor);
    let node = nodes.get(change.nodeId);
    if (!node && change.op !== 'removed') {
      const source = T.nodes[change.nodeId] || {};
      node = {
        id: change.nodeId, type: source.type || 'task', label: source.name || change.nodeId,
        pinned: T.layout.nodes?.[change.nodeId] ? { ...T.layout.nodes[change.nodeId] } : undefined,
        proposed: true,
      };
      out.nodes.push(node);
      nodes.set(node.id, node);
    }
    if (!node) continue;
    if (change.op === 'changed' && T.nodes[change.nodeId]?.name && diff.state !== 'rejected') {
      node.label = T.nodes[change.nodeId].name;
    }
    node.diff = diff;
  }
  const edgeIndex = new Map(out.edges.map((edge) => [draftEdgeKey(edge.from, edge.outcome), edge]));
  for (const change of changes) {
    if (change.kind !== 'edge' || !change.edge.from) continue;
    const diff = changeState(change, decisions, cursor);
    const key = change.id.slice(5);
    const proposal = T.edges.get(key);
    const existing = edgeIndex.get(key);
    if (change.op === 'removed') {
      if (existing) existing.diff = diff;
      continue;
    }
    if (!proposal || !nodes.has(proposal.from) || !nodes.has(proposal.to)) continue;
    if (existing && existing.to !== proposal.to) {
      existing.id = `${existing.id}~current`;
      existing.diff = { ...diff, op: 'removed', ghost: true, tag: '' };
      existing.ghostOf = change.id;
    }
    if (existing && existing.to === proposal.to) {
      existing.diff = diff;
      continue;
    }
    out.edges.push({
      id: graphEdgeID(proposal.from, proposal.outcome), from: proposal.from, to: proposal.to,
      outcome: proposal.outcome, pinned: true, proposed: true, diff,
    });
  }
  return out;
}

// draftChangeAt resolves which change a canvas click on a decorated item
// refers to (node id or the edge's from/outcome).
export function draftChangeForNode(changes = [], nodeId) {
  return changes.find((change) => change.kind === 'node' && change.nodeId === nodeId) || null;
}

export function draftChangeForEdge(changes = [], from, outcome) {
  const id = `edge:${draftEdgeKey(from, outcome)}`;
  return changes.find((change) => change.id === id) || null;
}
