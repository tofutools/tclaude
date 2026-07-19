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

const NODE_TYPES = new Set(['task', 'decision', 'parallel', 'wait', 'start', 'end']);

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

export function blankEditView(id) {
  return {
    template: {
      apiVersion: 'tclaude.dev/v1alpha1',
      kind: 'ProcessTemplate',
      id: id || 'new-process',
      name: '',
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

// A blank shell stops owning its id as soon as conflict resolution adopts an
// existing head as the CAS base, even if the retry has not saved successfully.
// Keeping this predicate pure lets the model and rendered control agree.
export function templateIDEditable(blank, sourceHash) {
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

  snapshotState() {
    return {
      template: clone(this.template),
      edges: clone(this.edges),
      layout: clone(this.layout),
      rev: this.rev,
    };
  }

  restoreState(snapshot) {
    // Identity lives outside graph/content undo. This keeps a draft id stable
    // while topology history moves, and prevents pre-save snapshots from
    // resurrecting another store key after the identity becomes pinned.
    const id = this.template.id;
    this.template = snapshot.template;
    this.template.id = id;
    this.edges = snapshot.edges;
    this.layout = snapshot.layout;
    this.rev = snapshot.rev;
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
  freeOutcome(from, base = 'pass') {
    const taken = new Set(this.outgoingEdges(from).map((edge) => edge.outcome));
    let candidate = base;
    for (let n = 2; taken.has(candidate); n += 1) candidate = `${base}-${n}`;
    return candidate;
  }

  addNode(type, { x, y, id, name } = {}) {
    this.assertCanInsert();
    const nodeType = NODE_TYPES.has(type) ? type : 'task';
    const nodeID = this.uniqueNodeID(id || nodeType);
    this.begin();
    const node = { type: nodeType };
    if (name) node.name = name;
    if (nodeType === 'end') node.result = 'success';
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
    const node = { type: nodeType };
    if (nodeType === 'end') node.result = 'success';
    this.template.nodes[nodeID] = node;
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
    this.edges = this.edges.filter((edge) => edge.from !== id && edge.to !== id);
    if (rewire && successor && successor !== id) {
      for (const edge of incoming) {
        this.edges.push({ from: edge.from, outcome: edge.outcome, to: successor });
      }
    }
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
    }
    this.edges = this.edges.filter((edge) => !selectedEdge(edge) && !nodes.has(edge.from) && !nodes.has(edge.to));
    this.edges.push(...bridges);
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
  }

  setEdgeOutcome(from, oldOutcome, newOutcome) {
    const edge = this.findEdge(from, oldOutcome);
    if (!edge) throw new Error(`unknown edge ${from} (${oldOutcome})`);
    if (!newOutcome) throw new Error('edge outcome is required');
    if (newOutcome === oldOutcome) return;
    if (this.findEdge(from, newOutcome)) throw new Error(`${from} already has a connector labelled "${newOutcome}". Outcome labels are the keys that pick which connector a run takes, so they must be unique per node.`);
    this.assertEdgeEditable(edge);
    this.begin();
    edge.outcome = newOutcome;
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

  setTemplateID(id) {
    if (this.sourceHash) return false;
    if (id === this.template.id) return true;
    this.template.id = id;
    return true;
  }

  setTemplateMeta({ name, description, doc } = {}) {
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
        joinOnTarget: join && (inCounts.get(edge.to) || 0) > 1 ? join : undefined,
      });
    }
    return { nodes, edges };
  }

  // saveBody is the POST payload. The server decodes with unknown fields
  // disallowed, so only the recognized edit-view fields go over the wire, and
  // template.layout stays empty — the top-level layout is authoritative.
  saveBody() {
    const template = clone(this.template);
    delete template.layout;
    return {
      template,
      edges: clone(this.edges),
      layout: clone(this.layout),
      sourceHash: this.sourceHash,
    };
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
