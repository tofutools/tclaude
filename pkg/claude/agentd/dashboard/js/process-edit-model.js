// process-edit-model.js -- pure edit-model state for the process template
// editor (TCL-296). No DOM, no fetch: Node's test runner exercises the exact
// file shipped to the browser (jstest/process-edit-model.test.mjs).
//
// The model mirrors the REST edit view from GET /v1/process/templates/{id}:
//   { template, edges[], layout, sourceHash, semanticHash }
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

export const MAX_UNDO = 50;

// The start-of-process marker is a pseudo edge with an empty `from` and the
// reserved outcome "start" — exactly what model.NormalizeEdges emits and what
// assembleProcessEditModel reads back into template.start on save.
export const START_OUTCOME = 'start';

const NODE_TYPES = new Set(['task', 'decision', 'wait', 'start', 'end']);

function clone(value) {
  return value === undefined ? undefined : structuredClone(value);
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
      start: 'start',
      nodes: {
        start: { type: 'start' },
        end: { type: 'end', result: 'success' },
      },
    },
    edges: [
      { from: '', outcome: START_OUTCOME, to: 'start' },
      { from: 'start', outcome: 'pass', to: 'end' },
    ],
    layout: { nodes: { start: { x: 120, y: 90 }, end: { x: 120, y: 320 } } },
    sourceHash: '',
    semanticHash: '',
  };
}

export class ProcessEditModel {
  constructor(view, config = {}) {
    const v = view || blankEditView();
    this.template = clone(v.template) || {};
    if (!this.template.nodes) this.template.nodes = {};
    this.edges = clone(v.edges) || [];
    this.layout = clone(v.layout) || {};
    if (!this.layout.nodes) this.layout.nodes = {};
    this.sourceHash = v.sourceHash || '';
    this.semanticHash = v.semanticHash || '';
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
    return this.rev !== this.savedRev;
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
    this.template = snapshot.template;
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
    this.begin();
    const successor = rewire
      ? [...outgoing].sort((a, b) => String(a.outcome).localeCompare(String(b.outcome), 'en'))[0]?.to
      : undefined;
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

  // setJoin records fan-in semantics on the TARGET node (design §8a). The v1
  // engine treats joins as deferred, so this persists as advisory authoring
  // intent in the node's freeform metadata.
  setJoin(id, join) {
    const node = this.template.nodes[id];
    if (!node) throw new Error(`unknown node ${id}`);
    this.assertNodeEditable(id);
    if (join && join !== 'all' && join !== 'any') throw new Error(`invalid join ${join}`);
    if ((node.metadata?.join || null) === (join || null)) return;
    this.begin();
    if (join) {
      node.metadata = { ...(node.metadata || {}), join };
    } else if (node.metadata) {
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
    if (this.findEdge(from, outcome)) throw new Error(`duplicate edge: ${from} already has outcome ${outcome}`);
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
    if (this.findEdge(from, newOutcome)) throw new Error(`duplicate edge: ${from} already has outcome ${newOutcome}`);
    this.assertEdgeEditable(edge);
    this.begin();
    edge.outcome = newOutcome;
  }

  setEdgeTarget(from, outcome, to) {
    const edge = this.findEdge(from, outcome);
    if (!edge) throw new Error(`unknown edge ${from} (${outcome})`);
    if (!this.template.nodes[to]) throw new Error(`unknown node ${to}`);
    this.assertEdgeEditable(edge);
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

  setTemplateMeta({ id, name, description } = {}) {
    // Same no-op discipline as renameNode/setJoin/setEdgeOutcome: a change
    // event that commits the current value must not dirty the model.
    const changed = (id !== undefined && id !== this.template.id)
      || (name !== undefined && (name || undefined) !== this.template.name)
      || (description !== undefined && (description || undefined) !== this.template.description);
    if (!changed) return;
    this.begin();
    if (id !== undefined) this.template.id = id;
    if (name !== undefined) {
      if (name) this.template.name = name;
      else delete this.template.name;
    }
    if (description !== undefined) {
      if (description) this.template.description = description;
      else delete this.template.description;
    }
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
    this.begin();
    for (const [nodeID, node] of Object.entries(snippet.nodes)) {
      // uniqueNodeID sees nodes inserted earlier in this loop, so intra-snippet
      // uniqueness holds even when every id collides with the template.
      const cloneID = this.uniqueNodeID(nodeID);
      idMap.set(nodeID, cloneID);
      this.template.nodes[cloneID] = clone(node);
      const offset = snippet.layout?.[nodeID] || { x: 0, y: 0 };
      this.layout.nodes[cloneID] = { x: x + offset.x, y: y + offset.y };
    }
    for (const edge of snippet.edges || []) {
      const from = idMap.get(edge.from);
      const to = idMap.get(edge.to);
      if (!from || !to) continue;
      this.edges.push({ from, outcome: edge.outcome, to });
    }
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
      const join = this.template.nodes[edge.to]?.metadata?.join;
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
  markSaved({ sourceHash, semanticHash, diagnostics } = {}, savedAtRev = this.rev) {
    if (sourceHash) this.sourceHash = sourceHash;
    if (semanticHash) this.semanticHash = semanticHash;
    if (diagnostics) this.diagnostics = diagnostics;
    this.savedRev = savedAtRev;
  }
}

// Palette seed data. Primitives map 1:1 onto the v1 node types; snippets are
// preconfigured compounds seeded from the embedded pinned example templates
// (docs/examples/code-change-with-review.yaml), trimmed to the fields the
// graph editor can meaningfully own today.
export const PALETTE_PRIMITIVES = [
  { type: 'task', label: 'Task', hint: 'A unit of work with a performer' },
  { type: 'decision', label: 'Decision', hint: 'Branch on an explicit outcome' },
  { type: 'wait', label: 'Wait / timer', hint: 'Pause for a duration or signal' },
  { type: 'start', label: 'Start', hint: 'Entry marker' },
  { type: 'end', label: 'End', hint: 'Terminal node with a result' },
];

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
