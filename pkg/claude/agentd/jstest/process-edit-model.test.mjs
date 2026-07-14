// Unit tests for the process editor's pure edit model
// (dashboard/js/process-edit-model.js), run with Node's built-in test runner.
// The Go wrapper TestPaletteScore_JS globs jstest/*.test.mjs, so this runs
// under `go test ./...` and skips when node is absent. No DOM needed: the
// module is deliberately pure so the exact file shipped to the browser is
// exercised here — undo/redo bounds, edge invariants, delete-with-rewire,
// snippet id remapping, and the save payload shape.

import test from 'node:test';
import assert from 'node:assert/strict';
import {
  ProcessEditModel, blankEditView, graphEdgeID, MAX_UNDO,
  PALETTE_PRIMITIVES, PALETTE_SNIPPETS, templateIDEditable,
} from '../dashboard/js/process-edit-model.js';

function view() {
  return {
    template: {
      apiVersion: 'tclaude.dev/v1alpha1',
      kind: 'ProcessTemplate',
      id: 'release',
      name: 'Release train',
      start: 'begin',
      nodes: {
        begin: { type: 'start' },
        build: { type: 'task', name: 'Build' },
        ship: { type: 'end', result: 'success' },
      },
    },
    edges: [
      { from: '', outcome: 'start', to: 'begin' },
      { from: 'begin', outcome: 'pass', to: 'build' },
      { from: 'build', outcome: 'pass', to: 'ship' },
    ],
    layout: { nodes: { begin: { x: 100, y: 40 } } },
    sourceHash: 'hash-source-1',
    semanticHash: 'hash-semantic-1',
    currentRef: 'release@sha256:source-1',
  };
}

test('model construction clones the view and starts clean', () => {
  const input = view();
  const model = new ProcessEditModel(input);
  assert.equal(model.dirty, false);
  assert.equal(model.canUndo, false);
  model.template.nodes.begin.type = 'task';
  assert.equal(input.template.nodes.begin.type, 'start', 'input view must not be aliased');
});

test('addNode uniquifies ids, pins the drop position, and is undoable', () => {
  const model = new ProcessEditModel(view());
  const first = model.addNode('task', { x: 10, y: 20 });
  const second = model.addNode('task', { x: 30, y: 40 });
  assert.equal(first, 'task');
  assert.equal(second, 'task-2');
  assert.deepEqual(model.layout.nodes['task-2'], { x: 30, y: 40 });
  assert.equal(model.dirty, true);
  assert.ok(model.undo());
  assert.equal(model.template.nodes['task-2'], undefined);
  assert.ok(model.undo());
  assert.equal(model.template.nodes.task, undefined);
  assert.equal(model.dirty, false, 'undo back to the load point reads clean');
  assert.ok(model.redo());
  assert.equal(model.template.nodes.task.type, 'task');
});

test('unknown node types degrade to task', () => {
  const model = new ProcessEditModel(view());
  const id = model.addNode('gateway', {});
  assert.equal(model.template.nodes[id].type, 'task');
});

test('addEdge enforces the unique (from, outcome) invariant', () => {
  const model = new ProcessEditModel(view());
  model.addEdge('begin', 'fail', 'ship');
  assert.throws(() => model.addEdge('begin', 'fail', 'build'), /duplicate edge/);
  assert.throws(() => model.addEdge('begin', '', 'build'), /outcome is required/);
  assert.throws(() => model.addEdge('nope', 'x', 'ship'), /unknown node/);
});

test('setEdgeOutcome renames and blocks collisions', () => {
  const model = new ProcessEditModel(view());
  model.setEdgeOutcome('build', 'pass', 'ok');
  assert.ok(model.findEdge('build', 'ok'));
  assert.equal(model.findEdge('build', 'pass'), undefined);
  model.addEdge('build', 'fail', 'ship');
  assert.throws(() => model.setEdgeOutcome('build', 'fail', 'ok'), /duplicate edge/);
});

test('deleteNode drops touching edges; rewire redirects incoming to the primary successor', () => {
  const dropped = new ProcessEditModel(view());
  dropped.deleteNode('build');
  assert.equal(dropped.template.nodes.build, undefined);
  assert.equal(dropped.edges.some(e => e.from === 'build' || e.to === 'build'), false);

  const rewired = new ProcessEditModel(view());
  rewired.deleteNode('build', { rewire: true });
  const bridge = rewired.findEdge('begin', 'pass');
  assert.ok(bridge, 'incoming edge keeps its (from, outcome) identity');
  assert.equal(bridge.to, 'ship', 'incoming edge redirected to the deleted node\'s successor');
  assert.ok(rewired.undo());
  assert.equal(rewired.findEdge('begin', 'pass').to, 'build', 'rewire is one undo step');
});

test('moveNode pins layout and undo restores the previous pin state', () => {
  const model = new ProcessEditModel(view());
  model.moveNode('build', 250, 260);
  assert.deepEqual(model.layout.nodes.build, { x: 250, y: 260 });
  assert.ok(model.undo());
  assert.equal(model.layout.nodes.build, undefined);
  assert.throws(() => model.moveNode('build', NaN, 1), /finite/);
});

test('multi-node move is one atomic undo step', () => {
  const model = new ProcessEditModel(view());
  model.moveNodes([
    { id: 'begin', x: 20, y: 30 },
    { id: 'build', x: 120, y: 130 },
  ]);
  assert.deepEqual(model.layout.nodes.begin, { x: 20, y: 30 });
  assert.deepEqual(model.layout.nodes.build, { x: 120, y: 130 });
  assert.equal(model.undoStack.length, 1);
  assert.ok(model.undo());
  assert.deepEqual(model.layout.nodes.begin, { x: 100, y: 40 });
  assert.equal(model.layout.nodes.build, undefined);
});

test('multi-selection delete rewires across selected nodes atomically', () => {
  const seeded = view();
  seeded.template.nodes.review = { type: 'task' };
  seeded.edges = [
    { from: '', outcome: 'start', to: 'begin' },
    { from: 'begin', outcome: 'pass', to: 'build' },
    { from: 'build', outcome: 'pass', to: 'review' },
    { from: 'review', outcome: 'pass', to: 'ship' },
  ];
  const model = new ProcessEditModel(seeded);
  model.deleteItems([
    { type: 'node', id: 'build' },
    { type: 'node', id: 'review' },
  ], { rewire: true });
  assert.equal(model.node('build'), undefined);
  assert.equal(model.node('review'), undefined);
  assert.equal(model.findEdge('begin', 'pass').to, 'ship');
  assert.equal(model.undoStack.length, 1);
  assert.ok(model.undo());
  assert.equal(model.findEdge('begin', 'pass').to, 'build');
});

test('setJoin stores fan-in semantics as node metadata and clears cleanly', () => {
  const model = new ProcessEditModel(view());
  model.setJoin('ship', 'all');
  assert.equal(model.template.nodes.ship.metadata.join, 'all');
  model.setJoin('ship', null);
  assert.equal(model.template.nodes.ship.metadata, undefined);
  assert.throws(() => model.setJoin('ship', 'most'), /invalid join/);
});

test('undo stack is bounded and drops the oldest snapshots', () => {
  const model = new ProcessEditModel(view(), { maxUndo: 5 });
  for (let i = 0; i < 12; i += 1) model.moveNode('build', i, i);
  assert.equal(model.undoStack.length, 5);
  let undos = 0;
  while (model.undo()) undos += 1;
  assert.equal(undos, 5);
  // The oldest reachable state is move #7, not the pristine load state.
  assert.deepEqual(model.layout.nodes.build, { x: 6, y: 6 });
  assert.equal(model.dirty, true, 'a truncated stack cannot reach the clean rev');
});

test('default undo bound satisfies the >=20 steps requirement', () => {
  assert.ok(MAX_UNDO >= 20);
  const model = new ProcessEditModel(view());
  for (let i = 0; i < MAX_UNDO + 10; i += 1) model.moveNode('build', i, i);
  assert.equal(model.undoStack.length, MAX_UNDO);
});

test('a new mutation invalidates the redo branch', () => {
  const model = new ProcessEditModel(view());
  model.moveNode('build', 1, 1);
  model.moveNode('build', 2, 2);
  model.undo();
  assert.equal(model.canRedo, true);
  model.moveNode('build', 9, 9);
  assert.equal(model.canRedo, false);
});

test('markSaved re-baselines dirty across undo/redo', () => {
  const model = new ProcessEditModel(view());
  model.moveNode('build', 5, 5);
  model.markSaved({ sourceHash: 'hash-source-2', semanticHash: 'hash-semantic-2' });
  assert.equal(model.dirty, false);
  assert.equal(model.sourceHash, 'hash-source-2');
  model.undo();
  assert.equal(model.dirty, true, 'undoing past the save point is dirty again');
  model.redo();
  assert.equal(model.dirty, false);
});

test('markSaved adopts the saved immutable ref for read-time head comparison', () => {
  const model = new ProcessEditModel(view());
  assert.equal(model.currentRef, 'release@sha256:source-1');
  model.markSaved({
    ref: 'release@sha256:source-2', sourceHash: 'source-2', semanticHash: 'semantic-2',
  });
  assert.equal(model.currentRef, 'release@sha256:source-2');
});

test('regression: save -> undo -> divergent edit stays dirty (rev serials are never reused)', () => {
  const model = new ProcessEditModel(view());
  model.moveNode('build', 5, 5);
  model.markSaved({ sourceHash: 'hash-source-2' });
  assert.equal(model.dirty, false);
  model.undo();
  model.moveNode('begin', 7, 7); // different content than the saved state
  assert.equal(model.dirty, true, 'a post-undo edit must never collide with savedRev');
});

test('regression: no-op rename and no-op join neither dirty nor burn an undo slot', () => {
  const model = new ProcessEditModel(view());
  model.renameNode('build', 'Build'); // same name as loaded
  model.renameNode('begin', '');      // unnamed stays unnamed
  assert.equal(model.dirty, false);
  assert.equal(model.canUndo, false);
  model.setJoin('ship', null); // join already unset
  assert.equal(model.dirty, false);
  assert.equal(model.canUndo, false);
});

test('regression: no-op setTemplateMeta neither dirties nor burns an undo slot', () => {
  const model = new ProcessEditModel(view());
  model.setTemplateMeta({ name: 'Release train' });  // unchanged name
  model.setTemplateMeta({ description: '' });        // absent stays absent
  assert.equal(model.dirty, false);
  assert.equal(model.canUndo, false);
  model.setTemplateMeta({ name: 'Freight train' });  // a real change still lands
  assert.equal(model.template.name, 'Freight train');
  assert.equal(model.dirty, true);
});

test('template metadata edits are undoable and preserve the immutable id', () => {
  const model = new ProcessEditModel(view());
  model.setTemplateMeta({
    name: 'Freight train',
    description: 'Move releases safely',
    doc: 'Operator-facing release procedure\nKeep the run history intact.',
  });
  assert.equal(model.template.id, 'release');
  assert.equal(model.template.name, 'Freight train');
  assert.equal(model.template.description, 'Move releases safely');
  assert.equal(model.template.doc, 'Operator-facing release procedure\nKeep the run history intact.');
  assert.equal(model.dirty, true);
  assert.equal(model.undo(), true);
  assert.equal(model.template.name, 'Release train');
  assert.equal(model.template.description, undefined);
  assert.equal(model.template.doc, undefined);
});

test('new-template id edits are dirty outside graph history until save pins the identity', () => {
  const model = new ProcessEditModel(blankEditView('new-process'));
  assert.equal(model.setTemplateID('release'), true);
  assert.equal(model.dirty, true, 'navigation sees the id edit and prompts before discard');
  assert.equal(model.canUndo, false, 'identity is not graph/content undo history');

  model.markSaved({ sourceHash: 'saved-source' });
  assert.equal(model.dirty, false);
  assert.equal(model.undo(), false, 'no identity-only snapshot can produce false dirty after save');
  assert.equal(model.dirty, false);

  model.addNode('task', { id: 'draft' }); // history snapshot carries the saved id
  assert.equal(model.undo(), true);
  assert.equal(model.template.id, 'release', 'post-save graph history preserves the pinned store key');
  assert.equal(model.dirty, false);
  assert.equal(model.undo(), false, 'a second undo cannot reach unchanged-but-dirty identity history');
  assert.equal(model.dirty, false);
  assert.equal(model.setTemplateID('copy'), false);
  assert.equal(model.template.id, 'release');
});

test('saving a draft id at the payload revision preserves in-flight edit dirtiness', () => {
  const model = new ProcessEditModel(blankEditView('new-process'));
  model.setTemplateID('release');
  const savedAtRev = model.rev;
  model.addNode('task', { id: 'in-flight' });
  model.markSaved({ sourceHash: 'saved-source' }, savedAtRev);
  assert.equal(model.template.id, 'release');
  assert.equal(model.dirty, true, 'the topology edit is newer than the saved payload');
  assert.equal(model.undo(), true);
  assert.equal(model.dirty, false);
});

test('failed first-save force retry keeps the adopted identity locked and consistent', () => {
  const model = new ProcessEditModel(blankEditView('collision'));
  assert.equal(templateIDEditable(true, model.sourceHash), true);

  // resolveConflict(force) adopts the existing head before recursively saving.
  // If that retry fails or re-conflicts, blank remains true but the CAS base
  // now fixes which store identity the editor owns.
  model.sourceHash = 'existing-head-source';
  assert.equal(templateIDEditable(true, model.sourceHash), false);
  assert.equal(model.setTemplateID('misleading-copy'), false, 'rejected without throwing from the change event');
  assert.equal(model.template.id, 'collision', 'visible title and model identity stay on the adopted head');
});

test('regression: markSaved at the payload rev keeps in-flight edits dirty', () => {
  const model = new ProcessEditModel(view());
  model.moveNode('build', 5, 5);
  const savedAtRev = model.rev;      // save button clicked: payload built here
  model.moveNode('begin', 9, 9);     // user keeps editing while POST in flight
  model.markSaved({ sourceHash: 'hash-source-2' }, savedAtRev);
  assert.equal(model.dirty, true, 'the in-flight edit is not in the saved payload');
  model.undo();
  assert.equal(model.dirty, false, 'undoing the in-flight edit lands exactly on the saved state');
});

test('regression: self-loop edges are refused and mutate nothing', () => {
  const model = new ProcessEditModel(view());
  const before = model.edges.length;
  assert.throws(() => model.addEdge('build', 'loop', 'build'), /self-loop/);
  assert.equal(model.edges.length, before);
  assert.equal(model.dirty, false);
  assert.equal(model.canUndo, false);
});

test('regression: run-mode seam — canInsert:false refuses adds/snippets, locked edges refuse setStart', () => {
  const locked = new ProcessEditModel(view(), {
    mode: 'run',
    canInsert: false,
    edgeEditable: () => false,
  });
  assert.throws(() => locked.addNode('task', { x: 1, y: 1 }), /not allowed/);
  assert.throws(() => locked.insertSnippet(PALETTE_SNIPPETS[0], { x: 0, y: 0 }), /not allowed/);
  assert.throws(() => locked.setStart('build'), /read-only/, 'the start pseudo edge is an edge mutation');
  assert.equal(locked.dirty, false);
  assert.equal(locked.canUndo, false);
});

test('regression: a locked existing node does not block snippet insertion when inserting is allowed', () => {
  const seeded = view();
  seeded.template.nodes.implement = { type: 'task' }; // collides with the snippet seed id
  const model = new ProcessEditModel(seeded, {
    mode: 'run',
    nodeEditable: id => id !== 'implement', // the EXISTING implement is locked
  });
  const snippet = PALETTE_SNIPPETS.find(s => s.key === 'code-change-with-review');
  const idMap = model.insertSnippet(snippet, { x: 10, y: 10 });
  assert.equal(idMap.get('implement'), 'implement-2', 'new material never collides with the locked node');
  assert.ok(model.template.nodes['implement-2']);
});

test('snippet retry loops keep the engine-sanctioned shape (compound target, fail re-entry, human decision)', () => {
  for (const snippet of PALETTE_SNIPPETS) {
    for (const edge of snippet.edges) {
      if (edge.outcome !== 'retry') continue;
      const decision = snippet.nodes[edge.from];
      const target = snippet.nodes[edge.to];
      assert.equal(decision.type, 'decision', `${snippet.key}: retry source must be a decision`);
      assert.equal(decision.performer?.kind, 'human', `${snippet.key}: retry decision must be human`);
      const compound = target.type === 'task' && !!(target.plan || (target.checks || []).length || target.review);
      assert.ok(compound, `${snippet.key}: retry target ${edge.to} must be a compound task`);
      const failEdge = snippet.edges.find(e => e.from === edge.to && e.outcome === 'fail');
      assert.equal(failEdge?.to, edge.from, `${snippet.key}: ${edge.to} must fail into ${edge.from}`);
    }
  }
});

test('graphEdgeID cannot collide across the separator', () => {
  assert.notEqual(graphEdgeID('a:b', 'c'), graphEdgeID('a', 'b:c'));
  assert.notEqual(graphEdgeID('a--b', 'c'), graphEdgeID('a', 'b--c'));
});

test('graph projection hides the start pseudo edge and surfaces join on fan-in', () => {
  const model = new ProcessEditModel(view());
  model.addEdge('begin', 'skip', 'ship');
  model.setJoin('ship', 'any');
  const graph = model.graph();
  assert.equal(graph.nodes.length, 3);
  assert.equal(graph.edges.some(edge => edge.from === ''), false, 'start pseudo edge never renders');
  const intoShip = graph.edges.filter(edge => edge.to === 'ship');
  assert.equal(intoShip.length, 2);
  for (const edge of intoShip) assert.equal(edge.joinOnTarget, 'any');
  const single = graph.edges.find(edge => edge.to === 'build');
  assert.equal(single.joinOnTarget, undefined, 'join renders only with fan-in >= 2');
  assert.equal(graph.edges[0].id, graphEdgeID(graph.edges[0].from, graph.edges[0].outcome));
  const begin = graph.nodes.find(node => node.id === 'begin');
  assert.deepEqual(begin.pinned, { x: 100, y: 40 });
});

test('graph projection skips edges referencing missing nodes instead of throwing', () => {
  const model = new ProcessEditModel(view());
  model.edges.push({ from: 'ghost', outcome: 'x', to: 'ship' });
  const graph = model.graph();
  assert.equal(graph.edges.some(edge => edge.from === 'ghost'), false);
});

test('saveBody carries only recognized edit-view fields and no nested layout', () => {
  const model = new ProcessEditModel(view());
  model.template.layout = { nodes: { begin: { x: 1, y: 1 } } }; // hostile aliasing
  const body = model.saveBody();
  assert.deepEqual(Object.keys(body).sort(), ['edges', 'layout', 'sourceHash', 'template']);
  assert.equal(body.template.layout, undefined, 'top-level layout is authoritative');
  assert.equal(body.sourceHash, 'hash-source-1');
  assert.ok(body.edges.some(edge => edge.from === '' && edge.outcome === 'start'));
});

test('snippet insert remaps colliding ids and internal edges together', () => {
  const model = new ProcessEditModel(view());
  const snippet = PALETTE_SNIPPETS.find(s => s.key === 'code-change-with-review');
  model.addNode('task', { id: 'implement', x: 0, y: 0 }); // force a collision
  const idMap = model.insertSnippet(snippet, { x: 500, y: 300 });
  assert.equal(idMap.get('implement'), 'implement-2');
  const cloneEdges = model.edges.filter(edge => edge.from === 'implement-2');
  assert.equal(cloneEdges.length, 2);
  assert.ok(cloneEdges.every(edge => edge.to === idMap.get('done') || edge.to === idMap.get('escalate')));
  const pin = model.layout.nodes[idMap.get('escalate')];
  assert.deepEqual(pin, { x: 500 + snippet.layout.escalate.x, y: 300 + snippet.layout.escalate.y });
  // One undo step removes the whole compound.
  assert.ok(model.undo());
  assert.equal(model.template.nodes['implement-2'], undefined);
  assert.equal(model.edges.some(edge => edge.from === 'implement-2'), false);
});

test('palette data is well-formed: known primitive types, internally consistent snippets', () => {
  const types = new Set(['task', 'decision', 'wait', 'start', 'end']);
  for (const primitive of PALETTE_PRIMITIVES) assert.ok(types.has(primitive.type), primitive.type);
  for (const snippet of PALETTE_SNIPPETS) {
    const seen = new Map();
    for (const edge of snippet.edges) {
      assert.ok(snippet.nodes[edge.from], `${snippet.key}: edge from ${edge.from}`);
      assert.ok(snippet.nodes[edge.to], `${snippet.key}: edge to ${edge.to}`);
      const key = `${edge.from} ${edge.outcome}`;
      assert.ok(!seen.has(key), `${snippet.key}: duplicate (from, outcome) ${key}`);
      seen.set(key, true);
    }
    for (const id of Object.keys(snippet.nodes)) assert.ok(snippet.layout[id], `${snippet.key}: ${id} has a layout offset`);
  }
});

test('editability config guards mutations for the future run-editing surface', () => {
  const model = new ProcessEditModel(view(), {
    mode: 'run',
    nodeEditable: id => id !== 'begin',
    edgeEditable: edge => edge.from !== 'begin',
  });
  assert.throws(() => model.renameNode('begin', 'x'), /read-only/);
  assert.throws(() => model.moveNode('begin', 1, 2), /read-only/);
  assert.throws(() => model.deleteNode('build'), /read-only/, 'touching a locked edge is blocked');
  assert.throws(() => model.setEdgeOutcome('begin', 'pass', 'ok'), /read-only/);
  model.renameNode('build', 'Rebuild'); // unlocked nodes stay editable
  assert.equal(model.template.nodes.build.name, 'Rebuild');
});

test('blankEditView scaffolds a minimal valid editor document', () => {
  const blank = blankEditView('fresh');
  const model = new ProcessEditModel(blank, {});
  assert.equal(model.template.id, 'fresh');
  assert.ok(model.template.nodes.start);
  assert.ok(model.edges.some(edge => edge.from === '' && edge.outcome === 'start'));
  assert.equal(model.sourceHash, '', 'first save must CAS against an empty head');
  const graph = model.graph();
  assert.equal(graph.nodes.length, 2);
  assert.equal(graph.edges.length, 1);
});

test('setStart repoints the start pseudo edge exactly once', () => {
  const model = new ProcessEditModel(view());
  model.setStart('build');
  const starts = model.edges.filter(edge => edge.from === '' && edge.outcome === 'start');
  assert.equal(starts.length, 1);
  assert.equal(starts[0].to, 'build');
  assert.equal(model.template.start, 'build');
});
