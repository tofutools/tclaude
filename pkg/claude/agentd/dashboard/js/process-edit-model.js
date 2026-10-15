// process-edit-model.js -- pure edit-model state for the process template
// editor (TCL-296). No DOM, no fetch: Node's test runner exercises the exact
// file shipped to the browser (jstest/process-edit-model.test.mjs).
//
// The model mirrors the REST edit view from GET /v1/process/templates/{id}:
//   { template, edges[], layout, sourceHash, semanticHash, currentRef }
// Semantics (template + edges) and authoring metadata (layout) stay separate,
// matching the server: layout never contributes to semanticHash, and edges are
// the normalized truth — node.next is never touched client-side; the server
// rebuilds it from the edges array on save.
//
// Every mutation goes through one undo gate, so the bounded undo/redo stack
// covers add/delete/move/edge/label/join ops uniformly. Dirty state is a
// revision comparison (rev vs savedRev), so undoing back to the last saved
// point reads as clean again.
//
// Forward-compat seam (design §9): the constructor takes a config object with
// per-node/per-edge editability predicates. A later run-editing surface opens
// the same model over a run with completed nodes locked; nothing in here may
// assume template-only editing beyond the defaults.

import { PROCESS_NODE_TYPES } from './process-node-types.js';
import { layoutProcessGraph } from './process-layout.js';
import {
  processEdgeMutationMessage, processEdgePortAvailability, processNodePortAvailability,
} from './process-port-availability.js';
import {
  PROCESS_CLIPBOARD_MAX_COORDINATE, PROCESS_CLIPBOARD_MAX_EDGES,
  PROCESS_CLIPBOARD_MAX_ID, PROCESS_CLIPBOARD_MAX_NODES, ProcessClipboardError,
  validateProcessSelectionPayload,
} from './process-editor-clipboard.js';

export const MAX_UNDO = 50;

// The start-of-process marker is a pseudo edge with an empty `from` and the
// reserved outcome "start" — exactly what model.NormalizeEdges emits and what
// assembleProcessEditModel reads back into template.start on save.
export const START_OUTCOME = 'start';

export const PALETTE_PRIMITIVES = PROCESS_NODE_TYPES;

const NODE_TYPES = new Set(['task', 'decision', 'parallel', 'wait', 'start', 'end', 'subprocess']);

// ---- subprocess scopes (E1) --------------------------------------------------
//
// A subprocess (▣) node carries a nested graph in node.subprocess: inline
// (start, nodes with their own `next`, params) or a templateRef. The editor
// drills into an inline body by projecting it as the model's active scope —
// template/edges/layout become the body's — while the parents wait in
// `frames`. Undo snapshots, Save and the outline always see the ROOT, folded
// back from every open scope, so one history and one CAS save cover the whole
// tree. Body layout lives in the root layout under namespaced keys
// ("qa/unit"), matching the runtime's namespaced node ids.

export const SCOPE_SEPARATOR = '/';
export const MAX_SUBPROCESS_DEPTH = 4;

export function isInlineSubprocess(sub) {
  return !!sub && typeof sub === 'object' && !sub.templateRef
    && (!!sub.start || Object.keys(sub.nodes || {}).length > 0 || Object.keys(sub.params || {}).length > 0);
}

// A fresh ▣ node: an inline body that runs straight from start to end.
export function blankSubprocessBody() {
  return {
    start: 'start',
    nodes: { start: { type: 'start', next: { pass: 'end' } }, end: { type: 'end', result: 'success' } },
  };
}

export function defaultNode(type) {
  const node = { type };
  if (type === 'end') node.result = 'success';
  if (type === 'subprocess') node.subprocess = blankSubprocessBody();
  return node;
}

// scopeEdges normalizes a body's nodes[*].next into the editor's edge list,
// the client-side twin of the server's NormalizeEdges.
export function scopeEdges(body = {}) {
  const edges = [];
  if (body.start) edges.push({ from: '', outcome: START_OUTCOME, to: body.start });
  for (const id of Object.keys(body.nodes || {}).sort()) {
    const next = body.nodes[id]?.next || {};
    for (const outcome of Object.keys(next).sort()) edges.push({ from: id, outcome, to: next[outcome] });
  }
  return edges;
}

export function scopeNodes(body = {}) {
  return Object.fromEntries(Object.entries(clone(body.nodes) || {}).map(([id, node]) => {
    const { next: _next, ...rest } = node || {};
    return [id, rest];
  }));
}

// foldBody writes a scope's nodes/edges/params back into its subprocess
// object, keeping the node's other fields (inputs, outputs).
export function foldBody(sub = {}, template = {}, edges = []) {
  const { start: _start, nodes: _nodes, params: _params, ...rest } = clone(sub) || {};
  const nodes = clone(template.nodes) || {};
  let start = '';
  for (const edge of edges) {
    if (edge.from === '') {
      if (edge.outcome === START_OUTCOME) start = edge.to;
      continue;
    }
    const node = nodes[edge.from];
    if (!node) continue;
    node.next = { ...(node.next || {}), [edge.outcome]: edge.to };
  }
  const out = { ...rest };
  if (template.params && Object.keys(template.params).length) out.params = clone(template.params);
  if (start) out.start = start;
  out.nodes = nodes;
  return out;
}

function scopedLayout(layout = {}, prefix) {
  const out = { nodes: {} };
  for (const [key, value] of Object.entries(layout.nodes || {})) {
    if (key.startsWith(prefix)) out.nodes[key.slice(prefix.length)] = clone(value);
  }
  for (const [from, value] of Object.entries(layout.edges || {})) {
    if (!from.startsWith(prefix)) continue;
    out.edges ||= {};
    out.edges[from.slice(prefix.length)] = clone(value);
  }
  return out;
}

function withoutScope(layout = {}, prefix) {
  const out = clone(layout) || {};
  out.nodes ||= {};
  for (const key of Object.keys(out.nodes)) if (key.startsWith(prefix)) delete out.nodes[key];
  for (const key of Object.keys(out.edges || {})) if (key.startsWith(prefix)) delete out.edges[key];
  if (out.edges && !Object.keys(out.edges).length) delete out.edges;
  return out;
}

function foldFrame(frame, child) {
  const template = clone(frame.template);
  const node = template.nodes[frame.nodeId];
  node.subprocess = foldBody(node.subprocess, child.template, child.edges);
  const prefix = frame.nodeId + SCOPE_SEPARATOR;
  const layout = withoutScope(frame.layout, prefix);
  for (const [key, value] of Object.entries(child.layout?.nodes || {})) layout.nodes[prefix + key] = clone(value);
  for (const [key, value] of Object.entries(child.layout?.edges || {})) {
    layout.edges ||= {};
    layout.edges[prefix + key] = clone(value);
  }
  return { template, edges: clone(frame.edges), layout };
}

// processOutline is the bounded tree the outline panel lists: every node of
// the root graph, with inline bodies nested under their ▣ node.
export function processOutline(template = {}, depth = 0) {
  return Object.entries(template.nodes || {}).sort(([a], [b]) => a.localeCompare(b, 'en')).map(([id, node]) => {
    const entry = { id, name: String(node?.name || id), type: String(node?.type || 'task') };
    if (node?.type === 'subprocess') {
      const sub = node.subprocess || {};
      if (sub.templateRef) entry.ref = String(sub.templateRef);
      else if (depth < MAX_SUBPROCESS_DEPTH) entry.children = processOutline({ nodes: sub.nodes || {} }, depth + 1);
    }
    return entry;
  });
}

function clone(value) {
  return value === undefined ? undefined : structuredClone(value);
}

// processSelectionRenderedCenter resolves the center of the exact node
// rectangles rendered by ProcessGraph. Pins are the clipboard coordinates;
// edges and ports deliberately stay out of these bounds. Sharing the layout
// boundary keeps task/decision/wait/start/end dimensions in one authority.
export function processSelectionRenderedCenter(selection) {
  const layout = layoutProcessGraph({
    nodes: selection.nodes.map((entry) => ({
      id: entry.id,
      type: entry.node.type,
      label: entry.node.name || entry.id,
      pinned: entry.position,
    })),
    edges: [],
  });
  const left = Math.min(...layout.nodes.map((node) => node.x - node.width / 2));
  const right = Math.max(...layout.nodes.map((node) => node.x + node.width / 2));
  const top = Math.min(...layout.nodes.map((node) => node.y - node.height / 2));
  const bottom = Math.max(...layout.nodes.map((node) => node.y + node.height / 2));
  return { x: (left + right) / 2, y: (top + bottom) / 2 };
}

// graphEdgeID is the stable identity handed to the graph core (and used by
// the editor's selection). Components are URI-encoded and joined with ':',
// which encodeURIComponent always escapes inside a component -- so arbitrary
// node ids/outcomes can never collide on the separator.
export function graphEdgeID(from, outcome) {
  return `${encodeURIComponent(from)}:${encodeURIComponent(outcome)}`;
}

// A blank draft carries NO id: ids are minted by the store when the draft is
// first saved (POST /v1/process/templates). The old 'new-process' default was
// a permanent key handed out by accident -- accepting it named the template
// forever, and the second template created that way collided.
export function blankEditView(name) {
  return {
    template: {
      apiVersion: 'tclaude.dev/v1alpha1',
      kind: 'ProcessTemplate',
      id: '',
      name: name || '',
      params: {},
      start: 'start',
      nodes: {
        start: { type: 'start' },
      },
    },
    edges: [
      { from: '', outcome: START_OUTCOME, to: 'start' },
    ],
    layout: { nodes: { start: { x: 120, y: 90 } } },
    sourceHash: '',
    semanticHash: '',
    currentRef: '',
  };
}

// templateNeedsCreate reports whether a draft has never been persisted, so the
// next save must mint an id through the collection endpoint rather than PUT
// itself back over an existing key. A blank shell stops needing creation as
// soon as conflict resolution adopts an existing head as the CAS base, even if
// the retry has not saved successfully. Keeping this predicate pure lets the
// model and the save path agree.
export function templateNeedsCreate(blank, sourceHash) {
  return !!blank && !sourceHash;
}

export class ProcessEditModel {
  constructor(view, config = {}) {
    const v = view || blankEditView();
    this.template = clone(v.template) || {};
    if (!this.template.nodes) this.template.nodes = {};
    if (!this.template.params) this.template.params = {};
    this.edges = clone(v.edges) || [];
    this.layout = clone(v.layout) || {};
    if (!this.layout.nodes) this.layout.nodes = {};
    this.sourceHash = v.sourceHash || '';
    this.semanticHash = v.semanticHash || '';
    this.currentRef = v.currentRef || '';
    this.savedTemplateID = this.template.id || '';
    this.diagnostics = clone(v.diagnostics) || [];
    this.config = {
      mode: config.mode || 'template',
      nodeEditable: typeof config.nodeEditable === 'function' ? config.nodeEditable : () => true,
      edgeEditable: typeof config.edgeEditable === 'function' ? config.edgeEditable : () => true,
      // canInsert is the mode-level permission for growing the topology (new
      // nodes, snippet compounds). Per-node/edge predicates lock EXISTING
      // items; whether the view may add material at all is a mode property —
      // a future run view sets canInsert: false wholesale.
      canInsert: config.canInsert !== false,
      maxUndo: Number.isInteger(config.maxUndo) && config.maxUndo > 0 ? config.maxUndo : MAX_UNDO,
    };
    // Open subprocess scopes, outermost first: each holds the PARENT state
    // ({ nodeId, template, edges, layout }) as of entering the child.
    this.frames = [];
    // Inline expansions (E2) of the CURRENT scope's ▣ nodes: id → the
    // translation { dx, dy } from body layout coordinates to this canvas.
    // View state only: never saved, never in history.
    this.expansions = new Map();
    this.undoStack = [];
    this.redoStack = [];
    // rev identifies the CURRENT state; serial mints revs and never decreases,
    // so a rev is never reused. Undo restores an old rev, but a new mutation
    // after an undo gets a fresh serial -- it can never collide with savedRev
    // and read as clean while the content diverges from the saved version.
    this.rev = 0;
    this.serial = 0;
    this.savedRev = 0;
  }

  get dirty() {
    return this.rev !== this.savedRev || (this.template.id || '') !== this.savedTemplateID;
  }

  get canUndo() {
    return this.undoStack.length > 0;
  }

  get canRedo() {
    return this.redoStack.length > 0;
  }

  // Undo snapshots always hold the ROOT, so history stays one timeline across
  // drill-ins; restoring re-enters the open scope path as far as it survives.
  snapshotState() {
    return { ...this.rootState(), rev: this.rev };
  }

  restoreState(snapshot) {
    // Identity lives outside graph/content undo. This keeps a draft id stable
    // while topology history moves, and prevents pre-save snapshots from
    // resurrecting another store key after the identity becomes pinned.
    const id = this.template.id;
    const path = this.scopePath();
    this.expansions = this.frames[0]?.expansions || this.expansions;
    this.frames = [];
    this.template = snapshot.template;
    this.template.id = id;
    this.edges = snapshot.edges;
    this.layout = snapshot.layout;
    this.rev = snapshot.rev;
    this.reenterScopes(path);
  }

  // ---- subprocess scopes ----------------------------------------------------

  get inScope() {
    return this.frames.length > 0;
  }

  scopePath() {
    return this.frames.map((frame) => frame.nodeId);
  }

  // rootState folds every open scope back into a fresh copy of the root.
  rootState() {
    let state = { template: clone(this.template), edges: clone(this.edges), layout: clone(this.layout) };
    for (let i = this.frames.length - 1; i >= 0; i -= 1) state = foldFrame(this.frames[i], state);
    state.template.id = this.template.id;
    return state;
  }

  scopeTrail() {
    const root = this.frames[0]?.template || this.template;
    return [
      { id: '', label: String(root.name || root.id || 'process') },
      ...this.frames.map((frame) => ({ id: frame.nodeId, label: String(frame.template.nodes[frame.nodeId]?.name || frame.nodeId) })),
    ];
  }

  // assertCanNest refuses a ▣ node whose body would exceed the server's
  // nesting limit (bodies nest at most MAX_SUBPROCESS_DEPTH levels).
  assertCanNest(type) {
    if (type === 'subprocess' && this.frames.length >= MAX_SUBPROCESS_DEPTH) {
      throw new Error(`subprocesses nest at most ${MAX_SUBPROCESS_DEPTH} levels deep`);
    }
  }

  canEnterScope(id) {
    const node = this.template.nodes[id];
    return node?.type === 'subprocess' && isInlineSubprocess(node.subprocess) && this.frames.length < MAX_SUBPROCESS_DEPTH;
  }

  // enterScope is navigation, not an edit: no undo step, no rev change.
  enterScope(id) {
    if (!this.canEnterScope(id)) throw new Error(`node ${id} has no inline subprocess body to enter`);
    const node = this.template.nodes[id];
    const body = node.subprocess;
    this.frames.push({ nodeId: id, template: this.template, edges: this.edges, layout: this.layout, expansions: this.expansions });
    this.expansions = new Map();
    this.template = {
      id: this.template.id, name: String(node.name || id),
      params: clone(body.params) || {}, start: body.start || '', nodes: scopeNodes(body),
    };
    this.edges = scopeEdges(body);
    this.layout = scopedLayout(this.frames.at(-1).layout, id + SCOPE_SEPARATOR);
    return true;
  }

  exitScope() {
    const frame = this.frames.pop();
    if (!frame) return '';
    this.expansions = frame.expansions || new Map();
    const id = this.template.id;
    const state = foldFrame(frame, { template: this.template, edges: this.edges, layout: this.layout });
    this.template = state.template;
    this.template.id = id;
    this.edges = state.edges;
    this.layout = state.layout;
    return frame.nodeId;
  }

  exitToRoot() {
    while (this.frames.length) this.exitScope();
  }

  reenterScopes(path = []) {
    for (const id of path) {
      if (!this.canEnterScope(id)) break;
      this.enterScope(id);
    }
  }

  // scopePorts describes the open scope's contract with its parent: body
  // params are the in-ports (bound by inputs), and body end nodes leave
  // through out-ports (outputs) onto the parent node's outcomes.
  scopePorts() {
    const frame = this.frames.at(-1);
    if (!frame) return null;
    const sub = frame.template.nodes[frame.nodeId]?.subprocess || {};
    const outcomes = frame.edges.filter((edge) => edge.from === frame.nodeId).map((edge) => edge.outcome).sort();
    const ends = Object.entries(this.template.nodes).filter(([, node]) => node?.type === 'end').map(([id]) => id).sort();
    return {
      nodeId: frame.nodeId,
      params: Object.keys(this.template.params || {}).sort(),
      parentParams: Object.keys((this.frames[0]?.template || this.template).params || {}).sort(),
      inputs: { ...(sub.inputs || {}) },
      outputs: { ...(sub.outputs || {}) },
      outcomes, ends,
      start: this.template.start || '',
    };
  }

  // setScopeInput / setScopeOutput edit the open scope's ports on the parent
  // ▣ node — one undoable step each. An empty value unbinds.
  setScopeInput(param, expression) {
    return this.editScopePorts('inputs', param, String(expression ?? '').trim());
  }

  setScopeOutput(endID, outcome) {
    return this.editScopePorts('outputs', endID, String(outcome ?? '').trim());
  }

  editScopePorts(field, key, value) {
    const frame = this.frames.at(-1);
    if (!frame) throw new Error('not inside a subprocess');
    if (!key) throw new Error('port name is required');
    this.assertNodeEditable(frame.nodeId);
    const sub = frame.template.nodes[frame.nodeId].subprocess;
    if ((sub[field]?.[key] || '') === value) return false;
    this.begin();
    const next = { ...(sub[field] || {}) };
    if (value) next[key] = value;
    else delete next[key];
    if (Object.keys(next).length) sub[field] = next;
    else delete sub[field];
    return true;
  }

  // replaceSubprocessWithRef swaps an inline body (a node of the CURRENT
  // scope) for a reference to the template it was extracted into, keeping
  // the ports.
  replaceSubprocessWithRef(id, templateRef) {
    const node = this.template.nodes[id];
    if (node?.type !== 'subprocess' || !isInlineSubprocess(node.subprocess)) throw new Error(`node ${id} has no inline body`);
    if (!templateRef) throw new Error('template id is required');
    this.assertNodeEditable(id);
    this.begin();
    const { inputs, outputs } = node.subprocess;
    node.subprocess = { templateRef, ...(inputs ? { inputs } : {}), ...(outputs ? { outputs } : {}) };
    this.layout = withoutScope(this.layout, id + SCOPE_SEPARATOR);
    return true;
  }

  // extractableBody returns the open scope as a standalone template edit
  // view (for POST /v1/process/templates).
  extractableBody(name) {
    if (!this.inScope) throw new Error('enter a subprocess to extract it');
    const template = clone(this.template);
    delete template.id;
    template.apiVersion = this.frames[0].template.apiVersion || 'tclaude.dev/v1alpha1';
    template.kind = this.frames[0].template.kind || 'ProcessTemplate';
    template.name = String(name || template.name || '').trim();
    if (!Object.keys(template.params || {}).length) delete template.params;
    return { template, edges: clone(this.edges), layout: clone(this.layout) };
  }

  outline() {
    return processOutline(this.rootState().template);
  }

  // begin() is the single undo gate: every mutation snapshots the pre-state,
  // bumps the revision, and invalidates the redo branch.
  begin() {
    this.undoStack.push(this.snapshotState());
    if (this.undoStack.length > this.config.maxUndo) this.undoStack.shift();
    this.redoStack = [];
    this.serial += 1;
    this.rev = this.serial;
  }

  undo() {
    const snapshot = this.undoStack.pop();
    if (!snapshot) return false;
    this.redoStack.push(this.snapshotState());
    this.restoreState(snapshot);
    return true;
  }

  redo() {
    const snapshot = this.redoStack.pop();
    if (!snapshot) return false;
    this.undoStack.push(this.snapshotState());
    if (this.undoStack.length > this.config.maxUndo) this.undoStack.shift();
    this.restoreState(snapshot);
    return true;
  }

  assertNodeEditable(id) {
    if (!this.config.nodeEditable(id)) throw new Error(`node ${id} is read-only in this view`);
  }

  assertEdgeEditable(edge) {
    if (!this.config.edgeEditable(edge)) throw new Error(`edge ${edge.from} -> ${edge.to} is read-only in this view`);
  }

  // edgePortRejection returns the operator-facing reason a prospective edge may
  // not be created, or '' when the endpoints are allowed. `context` names the
  // mutation and the edge to blame, which is what turns a bare port sentence
  // into recovery guidance; callers with nothing better to say omit it.
  edgePortRejection(from, to, nodes = this.template.nodes, context = null) {
    const availability = processEdgePortAvailability(nodes?.[from], nodes?.[to]);
    if (availability.enabled) return '';
    if (!context) return availability.message;
    // Never let composition empty the message: this gate rejects on a non-empty
    // string, so it must not depend on guidance copy being present.
    return processEdgeMutationMessage(context.operation, context.edge || { from, to }, availability.message)
      || availability.message;
  }

  assertNewEdgePortsAvailable(from, to, nodes = this.template.nodes, context = null) {
    const message = this.edgePortRejection(from, to, nodes, context);
    if (message) throw new Error(message);
  }

  assertCanInsert() {
    if (!this.config.canInsert) throw new Error('adding nodes is not allowed in this view');
  }

  node(id) {
    return this.template.nodes[id];
  }

  findEdge(from, outcome) {
    return this.edges.find((edge) => edge.from === from && edge.outcome === outcome);
  }

  incomingEdges(id) {
    return this.edges.filter((edge) => edge.to === id && edge.from !== '');
  }

  outgoingEdges(id) {
    return this.edges.filter((edge) => edge.from === id);
  }

  uniqueNodeID(base) {
    const taken = new Set(Object.keys(this.template.nodes));
    let candidate = base;
    for (let n = 2; taken.has(candidate); n += 1) candidate = `${base}-${n}`;
    return candidate;
  }

  // freeOutcome picks the first outcome name not already used by `from`,
  // honoring the server's unique-(from,outcome) invariant.
  // ---- connector label pinning ---------------------------------------------------
  //
  // Pin state lives in layout.edges (authoring metadata, stripped before the
  // semantic hash) so decluttering a label travels with the template without
  // changing what the process means.

  // edgePinned returns true, false, or undefined. Undefined is not the same as
  // false: it means the author has no opinion and the default rule decides,
  // whereas false hides a label the default would have drawn.
  edgePinned(from, outcome) {
    return this.layout?.edges?.[from]?.[outcome]?.pinned;
  }

  // setEdgePinned records or clears an author opinion. Passing undefined
  // returns the edge to the default rule rather than storing a false, and prunes
  // the empty containers so an untouched template never grows a layout.edges
  // block it does not need.
  setEdgePinned(from, outcome, pinned) {
    const edge = this.findEdge(from, outcome);
    if (!edge) throw new Error(`unknown edge ${from} -> ${outcome}`);
    this.assertEdgeEditable(edge);
    if (this.edgePinned(from, outcome) === pinned) return false;
    this.begin();
    this.writeEdgePin(from, outcome, pinned);
    return true;
  }

  // pruneEdgePins drops any stored opinion whose (from, outcome) no longer names
  // a live edge. Called after every structural edge mutation rather than
  // per-path: the previous per-path cleanup missed deleteItems, which is the
  // editor's ONLY delete route, so pins outlived their connectors and were
  // silently inherited by the next edge minted with the same outcome --
  // freeOutcome reuses a name as soon as it is free. It also covers incoming
  // edges dropped by a node delete, which no per-path cleanup handled.
  pruneEdgePins() {
    if (!this.layout.edges) return;
    const live = new Set(this.edges.map((edge) => `${edge.from}\u0000${edge.outcome}`));
    for (const [from, byOutcome] of Object.entries(this.layout.edges)) {
      // Namespaced keys ("qa/unit") pin edges inside nested bodies; their own
      // scope prunes them, and withoutScope drops them with the ▣ node.
      if (from.includes(SCOPE_SEPARATOR)) continue;
      for (const outcome of Object.keys(byOutcome)) {
        if (!live.has(`${from}\u0000${outcome}`)) delete byOutcome[outcome];
      }
      if (!Object.keys(byOutcome).length) delete this.layout.edges[from];
    }
    if (!Object.keys(this.layout.edges).length) delete this.layout.edges;
  }

  // writeEdgePin is the raw container maintenance, shared with the rename and
  // delete paths, which must move or drop pin state with the edge it describes.
  // It deliberately does not snapshot undo: callers own that gate.
  writeEdgePin(from, outcome, pinned) {
    if (pinned === undefined) {
      const byOutcome = this.layout.edges?.[from];
      if (!byOutcome) return;
      delete byOutcome[outcome];
      if (!Object.keys(byOutcome).length) delete this.layout.edges[from];
      if (!Object.keys(this.layout.edges).length) delete this.layout.edges;
      return;
    }
    if (!this.layout.edges) this.layout.edges = {};
    if (!this.layout.edges[from]) this.layout.edges[from] = {};
    this.layout.edges[from][outcome] = { pinned };
  }

  freeOutcome(from, base = 'pass') {
    const taken = new Set(this.outgoingEdges(from).map((edge) => edge.outcome));
    let candidate = base;
    for (let n = 2; taken.has(candidate); n += 1) candidate = `${base}-${n}`;
    return candidate;
  }

  addNode(type, { x, y, id, name } = {}) {
    this.assertCanInsert();
    this.assertCanNest(type);
    const nodeType = NODE_TYPES.has(type) ? type : 'task';
    const nodeID = this.uniqueNodeID(id || nodeType);
    this.begin();
    const node = defaultNode(nodeType);
    if (name) node.name = name;
    this.template.nodes[nodeID] = node;
    if (Number.isFinite(x) && Number.isFinite(y)) this.layout.nodes[nodeID] = { x, y };
    return nodeID;
  }

  // addConnectedNode is the connector-drop transaction: validate every
  // insertion/edge constraint before begin(), then add both topology records
  // behind one undo snapshot. Exactly one of connectFrom (existing → new) or
  // connectTo (new → existing) identifies the side of the dragged port.
  addConnectedNode(type, { x, y, connectFrom = '', connectTo = '' } = {}) {
    this.assertCanInsert();
    this.assertCanNest(type);
    if (!!connectFrom === !!connectTo) throw new Error('connected node requires exactly one existing endpoint');
    const existingID = connectFrom || connectTo;
    if (!this.template.nodes[existingID]) throw new Error(`unknown node ${existingID}`);
    const nodeType = NODE_TYPES.has(type) ? type : 'task';
    const nodeID = this.uniqueNodeID(nodeType);
    const from = connectFrom || nodeID;
    const to = connectTo || nodeID;
    const plannedNodes = { ...this.template.nodes, [nodeID]: { type: nodeType } };
    this.assertNewEdgePortsAvailable(from, to, plannedNodes);
    const outcome = this.freeOutcome(from, 'pass');
    const edge = { from, outcome, to };
    this.assertEdgeEditable(edge);

    this.begin();
    this.template.nodes[nodeID] = defaultNode(nodeType);
    if (Number.isFinite(x) && Number.isFinite(y)) this.layout.nodes[nodeID] = { x, y };
    this.edges.push(edge);
    return { id: nodeID, edge };
  }

  // deleteNode removes the node, its layout pin, and every touching edge.
  // With rewire=true each incoming edge is redirected to the deleted node's
  // primary successor (its first outgoing edge in stable outcome order) instead
  // of being dropped — one target per incoming edge, because an incoming edge
  // keeps its (from, outcome) identity and that pair must stay unique.
  deleteNode(id, { rewire = false } = {}) {
    if (!this.template.nodes[id]) throw new Error(`unknown node ${id}`);
    this.assertNodeEditable(id);
    const incoming = this.incomingEdges(id);
    const outgoing = this.outgoingEdges(id);
    for (const edge of [...incoming, ...outgoing]) this.assertEdgeEditable(edge);
    const successor = rewire
      ? [...outgoing].sort((a, b) => String(a.outcome).localeCompare(String(b.outcome), 'en'))[0]?.to
      : undefined;
    if (rewire && successor && successor !== id) {
      for (const edge of incoming) {
        this.assertNewEdgePortsAvailable(edge.from, successor, this.template.nodes,
          { operation: 'delete-rewire', edge: { from: edge.from, outcome: edge.outcome, to: successor } });
      }
    }
    this.begin();
    delete this.template.nodes[id];
    delete this.layout.nodes[id];
    this.layout = withoutScope(this.layout, id + SCOPE_SEPARATOR);
    this.edges = this.edges.filter((edge) => edge.from !== id && edge.to !== id);
    if (rewire && successor && successor !== id) {
      for (const edge of incoming) {
        this.edges.push({ from: edge.from, outcome: edge.outcome, to: successor });
      }
    }
    // After rewiring, so a bridged edge that kept its (from, outcome) keeps its
    // pin rather than having it collected.
    this.pruneEdgePins();
    // A start pseudo edge that pointed at the deleted node is gone with the
    // filter above; the template keeps rendering (start is advisory until save).
  }

  moveNode(id, x, y) {
    return this.moveNodes([{ id, x, y }]);
  }

  // moveNodes keeps a multi-selection drag atomic: every coordinate and
  // editability gate is checked before one undo snapshot is consumed.
  moveNodes(moves) {
    const unique = new Map((moves || []).map((move) => [move.id, move]));
    if (!unique.size) return false;
    for (const { id, x, y } of unique.values()) {
      if (!this.template.nodes[id]) throw new Error(`unknown node ${id}`);
      this.assertNodeEditable(id);
      if (!Number.isFinite(x) || !Number.isFinite(y)) throw new Error('moveNodes needs finite coordinates');
    }
    this.begin();
    for (const { id, x, y } of unique.values()) this.layout.nodes[id] = { x, y };
    return true;
  }

  // duplicateNodes copies selected node definitions plus edges wholly inside
  // that selection. Placement is injected by the editor from the laid-out
  // graph, keeping this model DOM-free and making the operation one undo step.
  duplicateNodes(ids, { positions = {}, offset = { x: 36, y: 36 } } = {}) {
    this.assertCanInsert();
    const sourceIDs = [...new Set(ids || [])].filter((id) => this.template.nodes[id]);
    if (!sourceIDs.length) return new Map();
    const idMap = new Map();
    const taken = new Set(Object.keys(this.template.nodes));
    for (const sourceID of sourceIDs) {
      let cloneID = sourceID;
      for (let suffix = 2; taken.has(cloneID); suffix += 1) cloneID = `${sourceID}-${suffix}`;
      taken.add(cloneID);
      idMap.set(sourceID, cloneID);
    }
    const plannedNodes = { ...this.template.nodes };
    for (const sourceID of sourceIDs) plannedNodes[idMap.get(sourceID)] = this.template.nodes[sourceID];
    const internalEdges = this.edges.filter((edge) => idMap.has(edge.from) && idMap.has(edge.to));
    // Blame the source edge, not the clone ids: it is the source edge the
    // operator can actually deselect or delete to unblock the duplicate.
    for (const edge of internalEdges) {
      this.assertNewEdgePortsAvailable(idMap.get(edge.from), idMap.get(edge.to), plannedNodes,
        { operation: 'duplicate', edge });
    }
    this.begin();
    for (const sourceID of sourceIDs) {
      const cloneID = idMap.get(sourceID);
      this.template.nodes[cloneID] = clone(this.template.nodes[sourceID]);
      const point = positions[sourceID] || this.layout.nodes[sourceID];
      if (Number.isFinite(point?.x) && Number.isFinite(point?.y)) {
        this.layout.nodes[cloneID] = {
          x: point.x + (Number(offset?.x) || 0),
          y: point.y + (Number(offset?.y) || 0),
        };
      }
    }
    for (const edge of internalEdges) {
      this.edges.push({ from: idMap.get(edge.from), outcome: edge.outcome, to: idMap.get(edge.to) });
    }
    return idMap;
  }

  // insertClipboardSelection imports one already-bounded clipboard subgraph as
  // a single undoable transaction. Validation, capacity, deterministic ids,
  // remapped edges, and every destination coordinate are planned before
  // begin(), so a stale or hostile payload can never partially mutate state.
  // `operation` names the surface for rejection messages: the custom-snippet
  // palette shares this transaction with paste, and telling a snippet user to
  // "copy the selection again" would point at an action they never took.
  insertClipboardSelection(payload, { center = { x: 0, y: 0 }, offset = { x: 0, y: 0 }, operation = 'paste' } = {}) {
    const selection = validateProcessSelectionPayload(payload);
    this.assertCanInsert();
    if (Object.keys(this.template.nodes).length + selection.nodes.length > PROCESS_CLIPBOARD_MAX_NODES
        || this.edges.length + selection.edges.length > PROCESS_CLIPBOARD_MAX_EDGES) {
      throw new ProcessClipboardError('destination_limit', 'Pasting this selection would exceed the process graph limits.');
    }
    if (!Number.isFinite(center?.x) || !Number.isFinite(center?.y)
        || !Number.isFinite(offset?.x) || !Number.isFinite(offset?.y)) {
      throw new ProcessClipboardError('position', 'The paste target has an invalid position.');
    }

    const sourceCenter = processSelectionRenderedCenter(selection);
    const taken = new Set(Object.keys(this.template.nodes));
    const idMap = new Map();
    for (const entry of selection.nodes) {
      let candidate = entry.id;
      for (let suffix = 2; taken.has(candidate); suffix += 1) {
        const ending = `-${suffix}`;
        candidate = `${entry.id.slice(0, PROCESS_CLIPBOARD_MAX_ID - ending.length)}${ending}`;
      }
      taken.add(candidate);
      idMap.set(entry.id, candidate);
    }
    const nodes = selection.nodes.map((entry) => {
      const position = {
        x: center.x + offset.x + entry.position.x - sourceCenter.x,
        y: center.y + offset.y + entry.position.y - sourceCenter.y,
      };
      if (!Number.isFinite(position.x) || !Number.isFinite(position.y)
          || Math.abs(position.x) > PROCESS_CLIPBOARD_MAX_COORDINATE
          || Math.abs(position.y) > PROCESS_CLIPBOARD_MAX_COORDINATE) {
        throw new ProcessClipboardError('position', 'Pasted node positions exceed the editor coordinate limits.');
      }
      return { id: idMap.get(entry.id), node: clone(entry.node), position };
    });
    const edges = selection.edges.map((edge) => ({
      from: idMap.get(edge.from), outcome: edge.outcome, to: idMap.get(edge.to),
    }));
    const plannedNodes = Object.fromEntries(nodes.map((entry) => [entry.id, entry.node]));
    // Report the payload's own ids rather than the remapped destination ids,
    // and stay inside the clipboard error type so the paste handler surfaces
    // this instead of collapsing it to the generic clipboard failure.
    for (let index = 0; index < edges.length; index += 1) {
      const rejection = this.edgePortRejection(edges[index].from, edges[index].to, plannedNodes,
        { operation, edge: selection.edges[index] });
      if (rejection) throw new ProcessClipboardError('port', rejection);
    }

    this.begin();
    for (const entry of nodes) {
      this.template.nodes[entry.id] = entry.node;
      this.layout.nodes[entry.id] = entry.position;
    }
    this.edges.push(...edges);
    return idMap;
  }

  // deleteItems is the atomic multi-selection counterpart to deleteNode /
  // deleteEdge. With rewire=true, incoming edges from outside the selected
  // node set cross the selected subgraph to its first stable surviving exit.
  deleteItems(items, { rewire = false } = {}) {
    const nodes = new Set((items || []).filter((item) => item.type === 'node').map((item) => item.id));
    const edgeKeys = new Set((items || []).filter((item) => item.type === 'edge')
      .map((item) => graphEdgeID(item.from, item.outcome)));
    for (const id of nodes) {
      if (!this.template.nodes[id]) throw new Error(`unknown node ${id}`);
      this.assertNodeEditable(id);
    }
    const selectedEdge = (edge) => edgeKeys.has(graphEdgeID(edge.from, edge.outcome));
    const affected = this.edges.filter((edge) => selectedEdge(edge) || nodes.has(edge.from) || nodes.has(edge.to));
    for (const edge of affected) this.assertEdgeEditable(edge);
    if (!nodes.size && !affected.length) return false;

    const successor = (start) => {
      const seen = new Set();
      const queue = [start];
      while (queue.length) {
        const id = queue.shift();
        if (seen.has(id)) continue;
        seen.add(id);
        const outgoing = this.outgoingEdges(id)
          .filter((edge) => !selectedEdge(edge))
          .sort((a, b) => String(a.outcome).localeCompare(String(b.outcome), 'en'));
        for (const edge of outgoing) {
          if (!nodes.has(edge.to)) return edge.to;
          queue.push(edge.to);
        }
      }
      return null;
    };
    const bridges = rewire ? this.edges
      .filter((edge) => !nodes.has(edge.from) && nodes.has(edge.to) && !selectedEdge(edge))
      .map((edge) => ({ ...edge, to: successor(edge.to) }))
      .filter((edge) => edge.to && !nodes.has(edge.to)) : [];
    // Template.Start is a persisted pseudo-edge, not an ordinary connector:
    // preserve its existing delete-through rewire without requiring a source
    // port. Every ordinary bridge still passes the shared endpoint authority.
    for (const edge of bridges) {
      if (edge.from !== '') {
        this.assertNewEdgePortsAvailable(edge.from, edge.to, this.template.nodes,
          { operation: 'delete-rewire', edge });
      }
    }

    this.begin();
    for (const id of nodes) {
      delete this.template.nodes[id];
      delete this.layout.nodes[id];
      this.layout = withoutScope(this.layout, id + SCOPE_SEPARATOR);
    }
    this.edges = this.edges.filter((edge) => !selectedEdge(edge) && !nodes.has(edge.from) && !nodes.has(edge.to));
    this.edges.push(...bridges);
    this.pruneEdgePins();
    if (nodes.has(this.template.start)) {
      const start = this.edges.find((edge) => edge.from === '' && edge.outcome === START_OUTCOME);
      this.template.start = start?.to || '';
    }
    return true;
  }

  // updateNode is the node dialogs' single mutation gate (TCL-298): the
  // mutator receives a DRAFT clone of the node, so a thrown error can never
  // half-apply, and a mutation that leaves the node deep-equal to the current
  // value is a no-op — no dirty, no undo slot (the same discipline as
  // renameNode/setJoin). Structural mutations (stages, checks, performers,
  // retry, captures) all flow through here; topology stays with the
  // dedicated edge/node ops above.
  updateNode(id, mutate) {
    const node = this.template.nodes[id];
    if (!node) throw new Error(`unknown node ${id}`);
    this.assertNodeEditable(id);
    const draft = clone(node);
    mutate(draft);
    // Key insertion order is preserved by structuredClone and in-place field
    // assignment, so serialized equality is a faithful no-op test here.
    if (JSON.stringify(draft) === JSON.stringify(node)) return false;
    this.begin();
    this.template.nodes[id] = draft;
    return true;
  }

  renameNode(id, name) {
    const node = this.template.nodes[id];
    if (!node) throw new Error(`unknown node ${id}`);
    this.assertNodeEditable(id);
    // No-op renames (e.g. an inline editor blur without a change) must not
    // dirty the model or burn an undo slot.
    if ((name || undefined) === node.name) return;
    this.begin();
    if (name) node.name = name;
    else delete node.name;
  }

  // setJoin records typed fan-in semantics on the TARGET node. A recognized
  // legacy metadata.join is removed so editor saves converge on canonical
  // first-class authoring instead of carrying two competing fields.
  setJoin(id, join) {
    const node = this.template.nodes[id];
    if (!node) throw new Error(`unknown node ${id}`);
    this.assertNodeEditable(id);
    if (join && join !== 'all' && join !== 'any') throw new Error(`invalid join ${join}`);
    if ((node.join || null) === (join || null) && !Object.hasOwn(node.metadata || {}, 'join')) return;
    this.begin();
    if (join) node.join = join;
    else delete node.join;
    if (node.metadata) {
      delete node.metadata.join;
      if (!Object.keys(node.metadata).length) delete node.metadata;
    }
  }

  addEdge(from, outcome, to) {
    if (!this.template.nodes[from]) throw new Error(`unknown node ${from}`);
    if (!this.template.nodes[to]) throw new Error(`unknown node ${to}`);
    if (!outcome) throw new Error('edge outcome is required');
    // v1 templates are acyclic: validateAcyclic makes every self-loop a
    // graph_cycle ERROR (the sanctioned retry loop is engine-recognized,
    // never hand-drawn), and advisory-save semantics would let the doomed
    // version through silently — refuse at the source.
    if (from === to) throw new Error('self-loop edges are not supported (v1 processes are acyclic)');
    if (this.findEdge(from, outcome)) throw new Error(`${from} already has a connector labelled "${outcome}". Outcome labels are the keys that pick which connector a run takes, so they must be unique per node.`);
    this.assertNewEdgePortsAvailable(from, to);
    const edge = { from, outcome, to };
    this.assertEdgeEditable(edge);
    this.begin();
    this.edges.push(edge);
    return edge;
  }

  deleteEdge(from, outcome) {
    const edge = this.findEdge(from, outcome);
    if (!edge) throw new Error(`unknown edge ${from} (${outcome})`);
    this.assertEdgeEditable(edge);
    this.begin();
    this.edges = this.edges.filter((candidate) => candidate !== edge);
    this.pruneEdgePins();
  }

  setEdgeOutcome(from, oldOutcome, newOutcome) {
    const edge = this.findEdge(from, oldOutcome);
    if (!edge) throw new Error(`unknown edge ${from} (${oldOutcome})`);
    if (!newOutcome) throw new Error('edge outcome is required');
    if (newOutcome === oldOutcome) return;
    if (this.findEdge(from, newOutcome)) throw new Error(`${from} already has a connector labelled "${newOutcome}". Outcome labels are the keys that pick which connector a run takes, so they must be unique per node.`);
    this.assertEdgeEditable(edge);
    this.begin();
    const pinned = this.edgePinned(from, oldOutcome);
    edge.outcome = newOutcome;
    // The opinion belongs to the connector, not to the name it used to have.
    if (pinned !== undefined) {
      this.writeEdgePin(from, oldOutcome, undefined);
      this.writeEdgePin(from, newOutcome, pinned);
    }
  }

  setEdgeTarget(from, outcome, to) {
    const edge = this.findEdge(from, outcome);
    if (!edge) throw new Error(`unknown edge ${from} (${outcome})`);
    if (!this.template.nodes[to]) throw new Error(`unknown node ${to}`);
    this.assertEdgeEditable(edge);
    this.assertNewEdgePortsAvailable(from, to);
    this.begin();
    edge.to = to;
  }

  setStart(to) {
    if (!this.template.nodes[to]) throw new Error(`unknown node ${to}`);
    // Repointing the start is an edge mutation on the start pseudo edge —
    // guard both the edge being replaced and its replacement.
    const current = this.edges.find((edge) => edge.from === '' && edge.outcome === START_OUTCOME);
    if (current) this.assertEdgeEditable(current);
    this.assertEdgeEditable({ from: '', outcome: START_OUTCOME, to });
    this.begin();
    this.edges = this.edges.filter((edge) => !(edge.from === '' && edge.outcome === START_OUTCOME));
    this.edges.push({ from: '', outcome: START_OUTCOME, to });
    this.template.start = to;
  }

  setTemplateMeta({ name, description, doc } = {}) {
    if (this.inScope) throw new Error('template settings belong to the root process; exit the subprocess first');
    // Same no-op discipline as renameNode/setJoin/setEdgeOutcome: a change
    // event that commits the current value must not dirty the model.
    const changed = (name !== undefined && (name || undefined) !== this.template.name)
      || (description !== undefined && (description || undefined) !== this.template.description)
      || (doc !== undefined && (doc || undefined) !== this.template.doc);
    if (!changed) return;
    this.begin();
    if (name !== undefined) {
      if (name) this.template.name = name;
      else delete this.template.name;
    }
    if (description !== undefined) {
      if (description) this.template.description = description;
      else delete this.template.description;
    }
    if (doc !== undefined) {
      if (doc) this.template.doc = doc;
      else delete this.template.doc;
    }
  }

  setParams(params) {
    const next = clone(params) || {};
    if (JSON.stringify(next) === JSON.stringify(this.template.params || {})) return false;
    this.begin();
    this.template.params = next;
    return true;
  }

  // insertSnippet clones a preconfigured compound (nodes + internal edges) at a
  // drop point. Node ids are uniquified against the current template and every
  // internal edge is remapped through the same id map; relative layout offsets
  // pin each clone near the drop point. Gated as a whole on the mode-level
  // canInsert permission: every inserted node and edge is NEW material, so the
  // per-item predicates (which lock existing items by id) do not apply.
  insertSnippet(snippet, { x = 0, y = 0 } = {}) {
    this.assertCanInsert();
    const idMap = new Map();
    const taken = new Set(Object.keys(this.template.nodes));
    for (const [nodeID, node] of Object.entries(snippet.nodes)) {
      // The planned taken set covers both current and earlier snippet ids, so
      // intra-snippet uniqueness holds before the transaction begins.
      let cloneID = nodeID;
      for (let suffix = 2; taken.has(cloneID); suffix += 1) cloneID = `${nodeID}-${suffix}`;
      taken.add(cloneID);
      idMap.set(nodeID, cloneID);
    }
    const plannedNodes = { ...this.template.nodes };
    for (const [nodeID, node] of Object.entries(snippet.nodes)) plannedNodes[idMap.get(nodeID)] = node;
    const edges = [];
    for (const edge of snippet.edges || []) {
      const from = idMap.get(edge.from);
      const to = idMap.get(edge.to);
      if (!from || !to) continue;
      this.assertNewEdgePortsAvailable(from, to, plannedNodes);
      edges.push({ from, outcome: edge.outcome, to });
    }
    this.begin();
    for (const [nodeID, node] of Object.entries(snippet.nodes)) {
      const cloneID = idMap.get(nodeID);
      this.template.nodes[cloneID] = clone(node);
      const offset = snippet.layout?.[nodeID] || { x: 0, y: 0 };
      this.layout.nodes[cloneID] = { x: x + offset.x, y: y + offset.y };
    }
    this.edges.push(...edges);
    return idMap;
  }

  // graph projects the edit model onto the shared graph core's input shape.
  // The start pseudo edge has no source node to render; defensively skip any
  // edge referencing a missing node so a hostile stored template cannot take
  // the whole canvas down (the layout module throws on unknown refs).
  graph() {
    const nodes = Object.entries(this.template.nodes).map(([id, node]) => ({
      id,
      type: node.type || 'task',
      label: node.name || id,
      portAvailability: processNodePortAvailability(node),
      pinned: this.layout.nodes[id] ? { ...this.layout.nodes[id] } : undefined,
    }));
    const inCounts = new Map();
    for (const edge of this.edges) {
      if (edge.from === '') continue;
      inCounts.set(edge.to, (inCounts.get(edge.to) || 0) + 1);
    }
    const edges = [];
    for (const edge of this.edges) {
      if (edge.from === '') continue;
      if (!this.template.nodes[edge.from] || !this.template.nodes[edge.to]) continue;
      const join = this.template.nodes[edge.to]?.join;
      edges.push({
        id: graphEdgeID(edge.from, edge.outcome),
        from: edge.from,
        to: edge.to,
        outcome: edge.outcome,
        // Tri-state: undefined leaves the default label rule in charge.
        pinned: this.edgePinned(edge.from, edge.outcome),
        joinOnTarget: join && (inCounts.get(edge.to) || 0) > 1 ? join : undefined,
      });
    }
    return this.expandGraph({ nodes, edges });
  }

  // ---- inline expansion (E2) -------------------------------------------------
  //
  // An expanded ▣ node is drawn in place as a framed group: its body nodes
  // join this canvas under namespaced ids ("qa/unit", the same keys its body
  // layout is stored under), connectors into the ▣ land on the body's start,
  // and connectors out of it leave from the body end mapped onto that
  // outcome. Edits on a namespaced id are routed into the body (see the
  // NESTED_ROUTES wrapper below the class), so they are ordinary undoable
  // edits of this draft.

  canExpand(id) {
    return this.canEnterScope(id);
  }

  isExpanded(id) {
    return this.expansions.has(id) && this.canExpand(id);
  }

  // expandSubprocess centers the body on `origin` (the ▣ node's drawn
  // position; the editor passes it because an unpinned node has none here).
  expandSubprocess(id, origin = this.layout.nodes[id] || { x: 0, y: 0 }) {
    if (!this.canExpand(id)) throw new Error(`node ${id} has no inline subprocess body to expand`);
    // A wrapped region keeps its pins in this canvas's coordinates around
    // the ▣ node: show it exactly where it was drawn before wrapping.
    const prefix = id + SCOPE_SEPARATOR;
    const pins = Object.keys(this.template.nodes[id].subprocess.nodes || {})
      .map((inner) => this.layout.nodes[prefix + inner]).filter(Boolean);
    const near = 120;
    if (pins.length && Math.min(...pins.map((p) => p.x)) - near <= origin.x && origin.x <= Math.max(...pins.map((p) => p.x)) + near
        && Math.min(...pins.map((p) => p.y)) - near <= origin.y && origin.y <= Math.max(...pins.map((p) => p.y)) + near) {
      this.expansions.set(id, { dx: 0, dy: 0 });
      return true;
    }
    const positions = [...this.bodyPositions(id).values()];
    const left = Math.min(...positions.map((p) => p.x - p.width / 2));
    const right = Math.max(...positions.map((p) => p.x + p.width / 2));
    const top = Math.min(...positions.map((p) => p.y - p.height / 2));
    const bottom = Math.max(...positions.map((p) => p.y + p.height / 2));
    const cx = positions.length ? (left + right) / 2 : 0;
    const cy = positions.length ? (top + bottom) / 2 : 0;
    this.expansions.set(id, { dx: Math.round(origin.x - cx), dy: Math.round(origin.y - cy) });
    return true;
  }

  collapseSubprocess(id) {
    return this.expansions.delete(id);
  }

  // bodyPositions lays the body out in its own coordinates: stored body
  // pins where present, the shared layout engine for the rest.
  bodyPositions(id) {
    const body = this.template.nodes[id]?.subprocess || {};
    const prefix = id + SCOPE_SEPARATOR;
    const laid = layoutProcessGraph({
      nodes: Object.entries(body.nodes || {}).map(([inner, node]) => ({
        id: inner, type: node?.type || 'task', label: node?.name || inner,
        pinned: this.layout.nodes[prefix + inner] ? { ...this.layout.nodes[prefix + inner] } : undefined,
      })),
      edges: scopeEdges(body).filter((edge) => edge.from && body.nodes?.[edge.to])
        .map((edge) => ({ id: graphEdgeID(edge.from, edge.outcome), ...edge })),
    });
    return new Map(laid.nodes.map((node) => [node.id, node]));
  }

  expansionOffset(id) {
    return this.expansions.get(id) || { dx: 0, dy: 0 };
  }

  expandGraph(graph) {
    const groups = [];
    for (const id of [...this.expansions.keys()]) {
      if (!this.isExpanded(id)) continue;
      const node = this.template.nodes[id];
      const body = node.subprocess;
      const prefix = id + SCOPE_SEPARATOR;
      const { dx, dy } = this.expansionOffset(id);
      const positions = this.bodyPositions(id);
      const inner = Object.entries(body.nodes || {}).map(([innerID, child]) => ({
        id: prefix + innerID,
        type: child?.type || 'task',
        label: child?.name || innerID,
        portAvailability: processNodePortAvailability(child),
        pinned: { x: positions.get(innerID).x + dx, y: positions.get(innerID).y + dy },
        group: id,
      }));
      const bodyEdges = scopeEdges(body).filter((edge) => edge.from && body.nodes?.[edge.to]).map((edge) => ({
        id: graphEdgeID(prefix + edge.from, edge.outcome),
        from: prefix + edge.from, to: prefix + edge.to, outcome: edge.outcome,
        pinned: this.edgePinned(prefix + edge.from, edge.outcome),
      }));
      const ends = Object.keys(body.nodes || {}).filter((inner) => body.nodes[inner]?.type === 'end').sort();
      const outputs = body.outputs || {};
      const outcomes = graph.edges.filter((edge) => edge.from === id).length;
      const sourceEnd = (outcome) => ends.find((end) => outputs[end] === outcome)
        || (outcomes === 1 ? ends.find((end) => !outputs[end]) : undefined) || ends[0];
      const entry = body.start && body.nodes?.[body.start] ? prefix + body.start : '';
      graph.nodes = graph.nodes.filter((candidate) => candidate.id !== id).concat(inner);
      graph.edges = graph.edges.flatMap((edge) => {
        if (edge.to === id) return entry ? [{ ...edge, to: entry }] : [];
        if (edge.from !== id) return [edge];
        const end = sourceEnd(edge.outcome);
        // source keeps the connector's real identity for selection and edits.
        return end ? [{ ...edge, from: prefix + end, source: { from: id, outcome: edge.outcome } }] : [];
      }).concat(bodyEdges);
      groups.push({ id, label: String(node.name || id), nodes: inner.map((child) => child.id) });
    }
    if (groups.length) graph.groups = groups;
    return graph;
  }

  // commitScope replaces the current scope's content as ONE undo step: the
  // whole-graph rewrites of process-compose.js (splice, wrap, unwrap) build
  // the next state from clones and land it here.
  commitScope({ template, edges, layout }) {
    this.assertCanInsert();
    const id = this.template.id;
    this.begin();
    this.template = template;
    this.template.id = id;
    this.edges = edges;
    this.layout = layout;
    this.template.start = edges.find((edge) => edge.from === '' && edge.outcome === START_OUTCOME)?.to || '';
    for (const owner of [...this.expansions.keys()]) if (!this.canExpand(owner)) this.expansions.delete(owner);
    return true;
  }

  // nestedTarget splits a namespaced id ("qa/unit") into the ▣ node of this
  // scope whose inline body holds it and the body-local id.
  nestedTarget(id) {
    if (typeof id !== 'string') return null;
    const at = id.indexOf(SCOPE_SEPARATOR);
    if (at <= 0) return null;
    const owner = id.slice(0, at);
    const inner = id.slice(at + 1);
    if (!inner || inner.includes(SCOPE_SEPARATOR) || !this.canEnterScope(owner)) return null;
    return { owner, inner };
  }

  // withinBody runs fn inside a ▣ node's body and folds straight back; the
  // body's own mutators take the (root-snapshotting) undo step.
  withinBody(owner, fn) {
    this.enterScope(owner);
    try {
      return fn();
    } finally {
      this.exitScope();
    }
  }

  // saveBody is the POST payload. The server decodes with unknown fields
  // disallowed, so only the recognized edit-view fields go over the wire, and
  // template.layout stays empty — the top-level layout is authoritative.
  saveBody() {
    const { template, edges, layout } = this.rootState();
    delete template.layout;
    return { template, edges, layout, sourceHash: this.sourceHash };
  }

  // applyDraft replaces the root content with an edited view as ONE undoable
  // mutation, keeping the template id and the CAS base. The inline mini
  // editor (process-mini-edit.js) computes each structural edit as a whole
  // new view and lands it here, so undo, redo and dirty work as in the full
  // editor.
  applyDraft({ template, edges, layout } = {}) {
    this.exitToRoot();
    this.begin();
    const id = this.template.id;
    this.template = clone(template) || {};
    this.template.id = id;
    if (!this.template.nodes) this.template.nodes = {};
    if (!this.template.params) this.template.params = {};
    this.edges = clone(edges) || [];
    this.layout = clone(layout) || {};
    if (!this.layout.nodes) this.layout.nodes = {};
    this.pruneEdgePins();
  }

  // adoptReviewedGeneration applies a reviewed three-way merge
  // (process-draft-diff.js) as ONE undoable mutation and rebases the CAS base
  // onto the reviewed generation: its changes are now part of this draft,
  // whether accepted or deliberately rejected. clean marks the merge as
  // byte-for-byte that saved generation; otherwise the draft stays dirty and
  // a later Save writes the human's choices on top of it.
  adoptReviewedGeneration({ template, edges, layout } = {}, { ref, sourceHash, semanticHash } = {}, { clean = false } = {}) {
    this.exitToRoot();
    this.begin();
    const id = this.template.id;
    this.template = clone(template) || {};
    this.template.id = id;
    if (!this.template.nodes) this.template.nodes = {};
    if (!this.template.params) this.template.params = {};
    this.edges = clone(edges) || [];
    this.layout = clone(layout) || {};
    if (!this.layout.nodes) this.layout.nodes = {};
    this.sourceHash = sourceHash || '';
    this.currentRef = ref || '';
    this.semanticHash = semanticHash || '';
    // Not clean: the CAS base moved to the reviewed version, so no undo
    // snapshot (whose content predates it) may ever read as saved again.
    this.savedRev = clean ? this.rev : -1;
  }

  // markSaved baselines dirty at savedAtRev -- the rev captured when the save
  // payload was built. Edits made while the POST was in flight keep the model
  // dirty because their revs are minted after savedAtRev.
  markSaved({ ref, sourceHash, semanticHash, diagnostics } = {}, savedAtRev = this.rev) {
    if (sourceHash) {
      this.sourceHash = sourceHash;
      this.savedTemplateID = this.template.id || '';
    }
    if (semanticHash) this.semanticHash = semanticHash;
    if (ref) this.currentRef = ref;
    if (diagnostics) this.diagnostics = diagnostics;
    this.savedRev = savedAtRev;
  }
}

// Palette seed data. Primitives map 1:1 onto the v1 node types; snippets are
// preconfigured compounds seeded from the embedded pinned example templates
// (docs/examples/code-change-with-review.yaml), trimmed to the fields the
// graph editor can meaningfully own today.
// ---- edits on an inline-expanded body (E2) ---------------------------------
//
// The canvas addresses an expanded ▣ node's body nodes by namespaced id
// ("qa/unit"). These model entry points accept such ids and run inside the
// body (withinBody), so the body's own validation applies and the edit is
// one ordinary undo step. A connector cannot cross the frame: wiring into or
// out of a body goes through its ports.
const CROSS_BOUNDARY = 'Connectors cannot cross a subprocess frame; wire into the ▣ node and map its ports instead.';
const NESTED_ROUTES = {
  updateNode: [0], renameNode: [0], setJoin: [0], deleteNode: [0],
  addEdge: [0, 2], deleteEdge: [0], setEdgeOutcome: [0], setEdgeTarget: [0, 2], setEdgePinned: [0],
  findEdge: [0], incomingEdges: [0], outgoingEdges: [0],
};

function namespaceEdge(edge, prefix) {
  return edge && { ...edge, from: edge.from ? prefix + edge.from : '', to: prefix + edge.to };
}

function routeNested(model, args, positions) {
  const targets = positions.map((index) => [index, model.nestedTarget(args[index])]);
  if (!targets.some(([, target]) => target)) return null;
  const owners = new Set(targets.map(([, target]) => target?.owner || ''));
  if (owners.size !== 1 || owners.has('')) throw new Error(CROSS_BOUNDARY);
  const local = [...args];
  for (const [index, target] of targets) local[index] = target.inner;
  return { owner: [...owners][0], local };
}

for (const [name, positions] of Object.entries(NESTED_ROUTES)) {
  const original = ProcessEditModel.prototype[name];
  ProcessEditModel.prototype[name] = function nestedRoute(...args) {
    const route = routeNested(this, args, positions);
    if (!route) return original.apply(this, args);
    const prefix = route.owner + SCOPE_SEPARATOR;
    const result = this.withinBody(route.owner, () => original.apply(this, route.local));
    if (name === 'findEdge' || name === 'addEdge') return namespaceEdge(result, prefix);
    if (name === 'incomingEdges' || name === 'outgoingEdges') return result.map((edge) => namespaceEdge(edge, prefix));
    return result;
  };
}

{
  const proto = ProcessEditModel.prototype;
  const { node, edgePortRejection, addConnectedNode, moveNodes, deleteItems } = proto;

  // A body node reads straight from the ▣ node (no scope entry: the canvas
  // asks on every render). Its next map is topology, shown as edges.
  proto.node = function nestedNode(id) {
    const target = this.nestedTarget(id);
    if (!target) return node.call(this, id);
    const { next: _next, ...rest } = this.template.nodes[target.owner].subprocess.nodes?.[target.inner] || {};
    return this.template.nodes[target.owner].subprocess.nodes?.[target.inner] ? rest : undefined;
  };

  proto.edgePortRejection = function nestedPortRejection(from, to, nodes, context) {
    if (nodes !== undefined) return edgePortRejection.call(this, from, to, nodes, context);
    let route;
    try {
      route = routeNested(this, [from, to], [0, 1]);
    } catch (error) {
      return error.message;
    }
    if (!route) return edgePortRejection.call(this, from, to, undefined, context);
    return this.withinBody(route.owner, () => edgePortRejection.call(this, route.local[0], route.local[1], undefined, context));
  };

  proto.addConnectedNode = function nestedConnectedNode(type, options = {}) {
    const existing = options.connectFrom || options.connectTo;
    const target = this.nestedTarget(existing);
    if (!target) return addConnectedNode.call(this, type, options);
    const { dx, dy } = this.expansionOffset(target.owner);
    const local = {
      ...options,
      ...(options.connectFrom ? { connectFrom: target.inner } : { connectTo: target.inner }),
      ...(Number.isFinite(options.x) && Number.isFinite(options.y) ? { x: options.x - dx, y: options.y - dy } : {}),
    };
    const result = this.withinBody(target.owner, () => addConnectedNode.call(this, type, local));
    const prefix = target.owner + SCOPE_SEPARATOR;
    return { id: prefix + result.id, edge: namespaceEdge(result.edge, prefix) };
  };

  // Moving a body node stores its body-coordinate pin under the namespaced
  // key, which is exactly where a drill-in reads it from.
  proto.moveNodes = function nestedMoveNodes(moves) {
    const nested = (moves || []).map((move) => [move, this.nestedTarget(move.id)]);
    if (!nested.some(([, target]) => target)) return moveNodes.call(this, moves);
    const unique = new Map(nested.map(([move, target]) => [move.id, { move, target }]));
    for (const { move, target } of unique.values()) {
      if (!target && !this.template.nodes[move.id]) throw new Error(`unknown node ${move.id}`);
      if (target && !this.node(move.id)) throw new Error(`unknown node ${move.id}`);
      this.assertNodeEditable(target ? target.owner : move.id);
      if (!Number.isFinite(move.x) || !Number.isFinite(move.y)) throw new Error('moveNodes needs finite coordinates');
    }
    this.begin();
    for (const { move, target } of unique.values()) {
      const { dx, dy } = target ? this.expansionOffset(target.owner) : { dx: 0, dy: 0 };
      this.layout.nodes[move.id] = { x: move.x - dx, y: move.y - dy };
    }
    return true;
  };

  proto.deleteItems = function nestedDeleteItems(items, options) {
    const ids = (items || []).map((item) => (item.type === 'edge' ? item.from : item.id));
    const targets = ids.map((id) => this.nestedTarget(id));
    if (!targets.some(Boolean)) return deleteItems.call(this, items, options);
    const owners = new Set(targets.map((target) => target?.owner || ''));
    if (owners.size !== 1 || owners.has('')) throw new Error('Delete items inside one expanded subprocess at a time.');
    const owner = [...owners][0];
    const strip = (id) => this.nestedTarget(id)?.inner ?? id;
    const local = items.map((item) => (item.type === 'edge'
      ? { ...item, from: strip(item.from), ...(item.to ? { to: strip(item.to) } : {}) }
      : { ...item, id: strip(item.id) }));
    return this.withinBody(owner, () => deleteItems.call(this, local, options));
  };
}

export const PALETTE_SNIPPETS = [
  {
    key: 'code-change-with-review',
    label: 'Code change with review',
    hint: 'Implement, escalate on repeated failure, terminal success/cancel',
    nodes: {
      // implement must stay a COMPOUND task (plan/checks/review) whose fail
      // edge enters the escalate decision: that is the exact shape
      // validateAcyclic sanctions for the escalate -> implement retry loop.
      // A plain task here would make the snippet's own retry edge a
      // graph_cycle ERROR the moment it is dropped.
      implement: {
        type: 'task',
        name: 'Implement',
        performer: { kind: 'agent', profile: 'dev', prompt: 'Implement the change' },
        plan: {
          id: 'plan',
          performer: { kind: 'agent', profile: 'dev', prompt: 'Plan the implementation' },
        },
        checks: [
          {
            id: 'tests',
            performer: { kind: 'program', run: 'go', args: ['test', './...'] },
          },
        ],
        review: {
          id: 'merge-approval',
          performer: { kind: 'human', profile: 'operator', ask: 'Approve merge?' },
        },
        retry: { maxAttempts: 3, onFail: 'feedback-same-session' },
      },
      escalate: {
        type: 'decision',
        name: 'Escalate',
        performer: { kind: 'human', profile: 'operator', ask: 'Retries exhausted. Continue?' },
      },
      done: { type: 'end', name: 'Done', result: 'success' },
      canceled: { type: 'end', name: 'Canceled', result: 'canceled' },
    },
    edges: [
      { from: 'implement', outcome: 'pass', to: 'done' },
      { from: 'implement', outcome: 'fail', to: 'escalate' },
      { from: 'escalate', outcome: 'retry', to: 'implement' },
      { from: 'escalate', outcome: 'cancel', to: 'canceled' },
    ],
    layout: {
      implement: { x: 0, y: 0 },
      escalate: { x: 240, y: 130 },
      done: { x: -40, y: 260 },
      canceled: { x: 260, y: 300 },
    },
  },
  {
    key: 'human-approval-gate',
    label: 'Human approval gate',
    hint: 'A human decision with approve/reject branches',
    nodes: {
      approval: {
        type: 'decision',
        name: 'Approval',
        performer: { kind: 'human', profile: 'operator', ask: 'Approve?' },
      },
      approved: { type: 'end', name: 'Approved', result: 'success' },
      rejected: { type: 'end', name: 'Rejected', result: 'failed' },
    },
    edges: [
      { from: 'approval', outcome: 'approve', to: 'approved' },
      { from: 'approval', outcome: 'reject', to: 'rejected' },
    ],
    layout: {
      approval: { x: 0, y: 0 },
      approved: { x: -120, y: 210 },
      rejected: { x: 140, y: 210 },
    },
  },
];
