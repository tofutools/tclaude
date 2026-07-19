// Unit tests for the process editor's pure edit model
// (dashboard/js/process-edit-model.js), run with Node's built-in test runner.
// The Go wrapper TestDashboardJS globs the package's *.test.mjs, so this runs
// under `go test ./...` and skips when node is absent. No DOM needed: the
// module is deliberately pure so the exact file shipped to the browser is
// exercised here — undo/redo bounds, edge invariants, delete-with-rewire,
// snippet id remapping, and the save payload shape.

import test from 'node:test';
import assert from 'node:assert/strict';
import {
  ProcessEditModel, blankEditView, graphEdgeID, MAX_UNDO,
  PALETTE_PRIMITIVES, PALETTE_SNIPPETS, processSelectionRenderedCenter,
  templateIDEditable,
} from '../dashboard/js/process-edit-model.js';
import { layoutProcessGraph } from '../dashboard/js/process-layout.js';
import { ProcessClipboardError } from '../dashboard/js/process-editor-clipboard.js';

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

test('connector-drop node and edge are one atomic positioned undo step in either direction', () => {
  const after = new ProcessEditModel(view());
  const created = after.addConnectedNode('decision', {
    x: 321.5, y: -47.25, connectFrom: 'begin',
  });
  assert.equal(created.id, 'decision');
  assert.deepEqual(after.layout.nodes.decision, { x: 321.5, y: -47.25 });
  assert.deepEqual(created.edge, { from: 'begin', outcome: 'pass-2', to: 'decision' });
  assert.equal(after.undoStack.length, 1);
  assert.equal(after.undo(), true);
  assert.equal(after.node('decision'), undefined);
  assert.equal(after.findEdge('begin', 'pass-2'), undefined);

  const before = new ProcessEditModel(view());
  const prepended = before.addConnectedNode('task', { x: 9, y: 11, connectTo: 'build' });
  assert.deepEqual(prepended.edge, { from: 'task', outcome: 'pass', to: 'build' });
  assert.equal(before.undoStack.length, 1);
});

test('invalid connector-drop origins and locked edges reject before any partial mutation', () => {
  for (const [model, operation, message] of [
    [new ProcessEditModel(view()), (candidate) => candidate.addConnectedNode('task', { connectFrom: 'ship' }), /End nodes cannot have outgoing connections/],
    [new ProcessEditModel(view()), (candidate) => candidate.addConnectedNode('end', { connectTo: 'build' }), /End nodes cannot have outgoing connections/],
    [new ProcessEditModel(view()), (candidate) => candidate.addConnectedNode('start', { connectFrom: 'build' }), /Start nodes cannot have incoming connections/],
    [new ProcessEditModel(view(), { edgeEditable: () => false }), (candidate) => candidate.addConnectedNode('task', { connectFrom: 'build' }), /read-only/],
  ]) {
    const nodes = Object.keys(model.template.nodes);
    const edges = structuredClone(model.edges);
    assert.throws(() => operation(model), message);
    assert.deepEqual(Object.keys(model.template.nodes), nodes);
    assert.deepEqual(model.edges, edges);
    assert.equal(model.dirty, false);
    assert.equal(model.canUndo, false);
  }
});

test('addEdge enforces the unique (from, outcome) invariant', () => {
  const model = new ProcessEditModel(view());
  model.addEdge('begin', 'fail', 'ship');
  assert.throws(() => model.addEdge('begin', 'fail', 'build'), /already has a connector labelled "fail"/);
  assert.throws(() => model.addEdge('begin', '', 'build'), /outcome is required/);
  assert.throws(() => model.addEdge('nope', 'x', 'ship'), /unknown node/);
});

test('all topology creation paths reject unavailable Start/End sides transactionally', () => {
  const assertRejected = (model, operation, message) => {
    const before = model.saveBody();
    const history = { rev: model.rev, undo: model.undoStack.length, redo: model.redoStack.length };
    assert.throws(operation, message);
    assert.deepEqual(model.saveBody(), before);
    assert.deepEqual({ rev: model.rev, undo: model.undoStack.length, redo: model.redoStack.length }, history);
  };

  let model = new ProcessEditModel(view());
  assertRejected(model, () => model.addEdge('build', 'back', 'begin'), /Start nodes cannot have incoming/);
  assertRejected(model, () => model.addEdge('ship', 'after', 'build'), /End nodes cannot have outgoing/);
  assertRejected(model, () => model.setEdgeTarget('build', 'pass', 'begin'), /Start nodes cannot have incoming/);

  model = new ProcessEditModel({
    template: { nodes: { source: { type: 'task' }, middle: { type: 'task' }, start: { type: 'start' } } },
    edges: [
      { from: 'source', outcome: 'pass', to: 'middle' },
      { from: 'middle', outcome: 'pass', to: 'start' },
    ], layout: { nodes: {} },
  });
  assertRejected(model, () => model.deleteNode('middle', { rewire: true }), /Start nodes cannot have incoming/);
  assertRejected(model, () => model.deleteItems([{ type: 'node', id: 'middle' }], { rewire: true }), /Start nodes cannot have incoming/);

  const invalidSnippet = {
    nodes: { ordinary: { type: 'task' }, start: { type: 'start' } },
    edges: [{ from: 'ordinary', outcome: 'pass', to: 'start' }], layout: {},
  };
  model = new ProcessEditModel(view());
  assertRejected(model, () => model.insertSnippet(invalidSnippet), /Start nodes cannot have incoming/);
  assertRejected(model, () => model.insertClipboardSelection({
    kind: 'tclaude/process-selection', version: 1,
    nodes: [
      { id: 'ordinary', node: { type: 'task' }, position: { x: 0, y: 0 } },
      { id: 'start', node: { type: 'start' }, position: { x: 0, y: 100 } },
    ],
    edges: [{ from: 'ordinary', outcome: 'pass', to: 'start' }],
  }), /Start nodes cannot have incoming/);

  const legacy = new ProcessEditModel({
    template: { nodes: { end: { type: 'end' }, ordinary: { type: 'task' } } },
    edges: [{ from: 'end', outcome: 'after', to: 'ordinary' }], layout: { nodes: {} },
  });
  assertRejected(legacy, () => legacy.duplicateNodes(['end', 'ordinary']), /End nodes cannot have outgoing/);
});

test('duplicate, paste, and delete-with-rewire name the offending legacy edge and a recovery path', () => {
  const legacyView = () => ({
    template: {
      id: 'legacy', start: 'start', params: {}, nodes: {
        start: { type: 'start' }, ordinary: { type: 'task' }, end: { type: 'end' },
      },
    },
    edges: [
      { from: '', outcome: 'start', to: 'start' },
      { from: 'end', outcome: 'legacy-out', to: 'ordinary' },
    ],
    layout: { nodes: { start: { x: 0, y: 0 }, ordinary: { x: 0, y: 100 }, end: { x: 0, y: 200 } } },
  });
  const rejects = (model, operation, expectations) => {
    const before = model.saveBody();
    const history = { rev: model.rev, undo: model.undoStack.length, redo: model.redoStack.length };
    assert.throws(operation, (error) => {
      for (const expectation of expectations) assert.match(error.message, expectation);
      return true;
    });
    assert.deepEqual(model.saveBody(), before, 'rejection mutates nothing');
    assert.deepEqual({ rev: model.rev, undo: model.undoStack.length, redo: model.redoStack.length },
      history, 'rejection leaves undo/redo history untouched');
  };

  // Duplicate blames the source edge ids, not the clone ids the operator has
  // never seen and cannot select.
  const duplicate = new ProcessEditModel(legacyView());
  rejects(duplicate, () => duplicate.duplicateNodes(['end', 'ordinary']), [
    /Duplicate cannot copy the edge end -> ordinary \(outcome "legacy-out"\)/,
    /End nodes cannot have outgoing connections\./,
    /predates the current Start\/End port rules/,
    /Deselect one of its endpoint nodes, or delete the edge first\./,
  ]);
  assert.doesNotMatch(
    (() => { try { duplicate.duplicateNodes(['end', 'ordinary']); return ''; } catch (e) { return e.message; } })(),
    /end-2|ordinary-2/, 'clone ids never leak into the recovery guidance',
  );

  // Paste stays inside the clipboard error type so the paste handler surfaces
  // it instead of collapsing it to the generic clipboard failure.
  const paste = new ProcessEditModel(legacyView());
  rejects(paste, () => paste.insertClipboardSelection({
    kind: 'tclaude/process-selection', version: 1,
    nodes: [
      { id: 'end', node: { type: 'end', result: 'success' }, position: { x: 0, y: 0 } },
      { id: 'ordinary', node: { type: 'task' }, position: { x: 0, y: 100 } },
    ],
    edges: [{ from: 'end', outcome: 'legacy-out', to: 'ordinary' }],
  }), [
    /Paste cannot re-create the edge end -> ordinary \(outcome "legacy-out"\)/,
    /End nodes cannot have outgoing connections\./,
    /Delete that edge in the source template and copy again, or copy without selecting both of its endpoints\./,
  ]);
  try {
    paste.insertClipboardSelection({
      kind: 'tclaude/process-selection', version: 1,
      nodes: [
        { id: 'end', node: { type: 'end', result: 'success' }, position: { x: 0, y: 0 } },
        { id: 'ordinary', node: { type: 'task' }, position: { x: 0, y: 100 } },
      ],
      edges: [{ from: 'end', outcome: 'legacy-out', to: 'ordinary' }],
    });
    assert.fail('paste of a legacy illegal-side edge must be rejected');
  } catch (error) {
    assert.equal(error instanceof ProcessClipboardError, true, 'paste rejections are clipboard errors');
    assert.equal(error.code, 'port');
  }

  // The custom-snippet palette shares this transaction with paste, so it must
  // not tell an operator who never copied anything to "copy the selection
  // again". Same rejection, surface-appropriate recovery.
  const snippet = new ProcessEditModel(legacyView());
  const snippetPayload = {
    kind: 'tclaude/process-selection', version: 1,
    nodes: [
      { id: 'end', node: { type: 'end', result: 'success' }, position: { x: 0, y: 0 } },
      { id: 'ordinary', node: { type: 'task' }, position: { x: 0, y: 100 } },
    ],
    edges: [{ from: 'end', outcome: 'legacy-out', to: 'ordinary' }],
  };
  rejects(snippet, () => snippet.insertClipboardSelection(snippetPayload, { operation: 'snippet' }), [
    /This snippet cannot be inserted because of the edge end -> ordinary/,
    /End nodes cannot have outgoing connections\./,
    /Save a replacement snippet from a corrected selection, then delete or rename the old one\./,
  ]);
  assert.doesNotMatch(
    (() => {
      try { snippet.insertClipboardSelection(snippetPayload, { operation: 'snippet' }); return ''; } catch (e) { return e.message; }
    })(),
    /Paste|Copy the selection again/, 'snippet insertion is never described as a paste',
  );

  // Delete-with-rewire describes the bridge it would have to build, and must
  // not call that synthesized edge legacy. Plain delete stays the way out.
  const rewireView = {
    template: { nodes: { source: { type: 'task' }, middle: { type: 'task' }, start: { type: 'start' } } },
    edges: [
      { from: 'source', outcome: 'pass', to: 'middle' },
      { from: 'middle', outcome: 'pass', to: 'start' },
    ],
    layout: { nodes: {} },
  };
  const rewire = new ProcessEditModel(structuredClone(rewireView));
  rejects(rewire, () => rewire.deleteItems([{ type: 'node', id: 'middle' }], { rewire: true }), [
    /Delete with rewire cannot re-create the edge source -> start \(outcome "pass"\)/,
    /Start nodes cannot have incoming connections\./,
    /Rewiring has to build that connection anew/,
    /Choose "Delete \+ drop edges" instead/,
  ]);
  rejects(rewire, () => rewire.deleteNode('middle', { rewire: true }), [
    /Delete with rewire cannot re-create the edge source -> start/,
    /Choose "Delete \+ drop edges" instead/,
  ]);
  assert.doesNotMatch(
    (() => { try { rewire.deleteNode('middle', { rewire: true }); return ''; } catch (e) { return e.message; } })(),
    /predates/, 'a synthesized bridge is never described as legacy topology',
  );
  rewire.deleteItems([{ type: 'node', id: 'middle' }], { rewire: false });
  assert.equal(rewire.template.nodes.middle, undefined, 'plain delete is the working recovery path');
  assert.deepEqual(rewire.edges, [], 'plain delete drops the touching edges instead of bridging them');

  // Paths that are not copying preserved topology keep the bare sentence.
  const plain = new ProcessEditModel(legacyView());
  assert.throws(() => plain.addEdge('end', 'again', 'ordinary'),
    (error) => error.message === 'End nodes cannot have outgoing connections.');
});

test('legacy ordinary illegal-side edges and the Start pseudo-edge load, render, delete, undo, and round-trip unchanged', () => {
  const loaded = {
    template: {
      id: 'legacy', start: 'start', params: {}, nodes: {
        start: { type: 'start' }, ordinary: { type: 'task' }, end: { type: 'end' },
      },
    },
    edges: [
      { from: '', outcome: 'start', to: 'start' },
      { from: 'ordinary', outcome: 'legacy-in', to: 'start' },
      { from: 'end', outcome: 'legacy-out', to: 'ordinary' },
    ],
    layout: { nodes: { start: { x: 10, y: 20 }, ordinary: { x: 20, y: 120 }, end: { x: 30, y: 220 } } },
  };
  const model = new ProcessEditModel(loaded);
  assert.deepEqual(model.saveBody(), { ...structuredClone(loaded), sourceHash: '' });
  assert.deepEqual(model.graph().edges.map(({ from, outcome, to }) => ({ from, outcome, to })), [
    { from: 'ordinary', outcome: 'legacy-in', to: 'start' },
    { from: 'end', outcome: 'legacy-out', to: 'ordinary' },
  ]);
  assert.equal(model.graph().edges.some((edge) => edge.from === ''), false, 'Start pseudo-edge is never an ordinary rendered edge');
  const editorGraph = model.graph();
  const viewerDefaultGraph = structuredClone(editorGraph);
  viewerDefaultGraph.nodes.forEach((node) => { delete node.portAvailability; });
  const editorLayout = layoutProcessGraph(editorGraph);
  const defaultLayout = layoutProcessGraph(viewerDefaultGraph);
  assert.deepEqual(
    editorLayout.edges.map(({ id, path, label }) => ({ id, path, label })),
    defaultLayout.edges.map(({ id, path, label }) => ({ id, path, label })),
    'port presence metadata changes neither legal nor legacy edge endpoint/routing geometry',
  );
  model.deleteEdge('ordinary', 'legacy-in');
  model.deleteEdge('end', 'legacy-out');
  assert.equal(model.edges.length, 1);
  assert.equal(model.undo(), true);
  assert.equal(model.undo(), true);
  assert.deepEqual(model.saveBody(), { ...structuredClone(loaded), sourceHash: '' });
});

test('setEdgeOutcome renames and blocks collisions', () => {
  const model = new ProcessEditModel(view());
  model.setEdgeOutcome('build', 'pass', 'ok');
  assert.ok(model.findEdge('build', 'ok'));
  assert.equal(model.findEdge('build', 'pass'), undefined);
  model.addEdge('build', 'fail', 'ship');
  assert.throws(() => model.setEdgeOutcome('build', 'fail', 'ok'), /already has a connector labelled "ok"/);
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

test('multi-selection delete preserves the Template.Start pseudo-edge rewire contract', () => {
  const model = new ProcessEditModel(view());
  const before = model.saveBody();
  model.deleteItems([{ type: 'node', id: 'begin' }], { rewire: true });
  assert.equal(model.node('begin'), undefined);
  assert.deepEqual(model.edges.find((edge) => edge.from === '' && edge.outcome === 'start'),
    { from: '', outcome: 'start', to: 'build' });
  assert.equal(model.template.start, 'build');
  assert.equal(model.undoStack.length, 1);
  assert.equal(model.undo(), true);
  assert.deepEqual(model.saveBody(), before);
});

test('setJoin stores typed fan-in semantics, removes legacy metadata, and clears cleanly', () => {
  const model = new ProcessEditModel(view());
  model.template.nodes.ship.metadata = { join: 'any', owner: 'routing-team' };
  model.setJoin('ship', 'all');
  assert.equal(model.template.nodes.ship.join, 'all');
  assert.deepEqual(model.template.nodes.ship.metadata, { owner: 'routing-team' });
  model.setJoin('ship', null);
  assert.equal(model.template.nodes.ship.join, undefined);
  assert.deepEqual(model.template.nodes.ship.metadata, { owner: 'routing-team' });
  assert.throws(() => model.setJoin('ship', 'most'), /invalid join/);
});

test('parallel is a first-class performer-free primitive and survives save projection', () => {
  const model = new ProcessEditModel(view());
  const id = model.addNode('parallel', { x: 12, y: 34 });
  assert.equal(id, 'parallel');
  assert.deepEqual(model.node(id), { type: 'parallel' });
  assert.deepEqual(model.layout.nodes[id], { x: 12, y: 34 });
  assert.deepEqual(model.saveBody().template.nodes[id], { type: 'parallel' });
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

test('template params are one undoable mutation and no-op apply stays clean', () => {
  const model = new ProcessEditModel(view());
  assert.deepEqual(model.template.params, {}, 'older templates normalize to an empty param map');
  assert.equal(model.setParams({ issue: { type: 'string', description: 'Issue id', required: true } }), true);
  assert.equal(model.template.params.issue.required, true);
  assert.equal(model.dirty, true);
  assert.equal(model.undoStack.length, 1);
  assert.equal(model.undo(), true);
  assert.deepEqual(model.template.params, {});
  assert.equal(model.dirty, false);
  assert.equal(model.setParams({}), false);
  assert.equal(model.canUndo, false);
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

test('duplicateNodes copies semantics, internal edges, placement, and one undo step', () => {
  const model = new ProcessEditModel(view());
  const idMap = model.duplicateNodes(['begin', 'build'], {
    positions: { begin: { x: 10, y: 20 }, build: { x: 50, y: 90 } },
    offset: { x: 7, y: 11 },
  });
  assert.equal(idMap.get('begin'), 'begin-2');
  assert.equal(idMap.get('build'), 'build-2');
  assert.deepEqual(model.template.nodes['build-2'], model.template.nodes.build);
  assert.deepEqual(model.layout.nodes['begin-2'], { x: 17, y: 31 });
  assert.ok(model.edges.some((edge) => edge.from === 'begin-2' && edge.to === 'build-2' && edge.outcome === 'pass'));
  assert.equal(model.edges.some((edge) => edge.from === 'build-2' && edge.to === 'ship'), false,
    'edges crossing out of the selection are not duplicated');
  assert.ok(model.undo());
  assert.equal(model.template.nodes['begin-2'], undefined);
  assert.equal(model.edges.some((edge) => edge.from === 'begin-2'), false);
});

test('clipboard insertion remaps ids and references atomically around the requested center', () => {
  const model = new ProcessEditModel(view());
  const payload = {
    kind: 'tclaude/process-selection', version: 1,
    nodes: [
      { id: 'build', node: { type: 'task', name: 'Imported build', performer: { kind: 'agent', profile: 'implementer' } }, position: { x: 100, y: 200 } },
      { id: 'review', node: { type: 'decision', performer: { kind: 'human', ask: 'Accept?' } }, position: { x: 300, y: 400 } },
    ],
    edges: [{ from: 'build', outcome: 'pass', to: 'review' }],
  };
  const before = model.saveBody();
  assert.deepEqual(processSelectionRenderedCenter(payload), { x: 185, y: 310 },
    'source bounds use the task and decision rectangles, not only their origins');
  const idMap = model.insertClipboardSelection(payload, { center: { x: 500, y: 600 } });
  assert.deepEqual([...idMap], [['build', 'build-2'], ['review', 'review']]);
  assert.deepEqual(model.node('build-2'), payload.nodes[0].node, 'the complete node definition survives import');
  assert.deepEqual(model.layout.nodes['build-2'], { x: 415, y: 490 });
  assert.deepEqual(model.layout.nodes.review, { x: 615, y: 690 });
  assert.equal((415 - 168 / 2 + 615 + 108 / 2) / 2, 500,
    'combined rendered-node horizontal bounds center at the requested target');
  assert.equal((490 - 68 / 2 + 690 + 108 / 2) / 2, 600,
    'combined rendered-node vertical bounds center at the requested target');
  assert.ok(model.edges.some((edge) => edge.from === 'build-2' && edge.outcome === 'pass' && edge.to === 'review'));
  assert.equal(model.undoStack.length, 1);
  assert.equal(model.undo(), true);
  assert.deepEqual(model.saveBody(), before, 'one undo removes the whole pasted subgraph');
});

test('repeated clipboard insertion deterministically advances ids and placement', () => {
  const model = new ProcessEditModel(view());
  const payload = {
    kind: 'tclaude/process-selection', version: 1,
    nodes: [{ id: 'build', node: { type: 'task', name: 'Imported' }, position: { x: 10, y: 20 } }],
    edges: [],
  };
  const first = model.insertClipboardSelection(payload, { center: { x: 50, y: 60 } });
  const second = model.insertClipboardSelection(payload, { center: { x: 50, y: 60 }, offset: { x: 36, y: 36 } });
  assert.equal(first.get('build'), 'build-2');
  assert.equal(second.get('build'), 'build-3');
  assert.deepEqual(model.layout.nodes['build-2'], { x: 50, y: 60 });
  assert.deepEqual(model.layout.nodes['build-3'], { x: 86, y: 96 });

  const longID = 'n'.repeat(128);
  model.template.nodes[longID] = { type: 'task' };
  const longMap = model.insertClipboardSelection({
    kind: 'tclaude/process-selection', version: 1,
    nodes: [{ id: longID, node: { type: 'task' }, position: { x: 0, y: 0 } }], edges: [],
  });
  assert.equal(longMap.get(longID).length, 128, 'collision suffix stays within the clipboard id bound');
  assert.match(longMap.get(longID), /-2$/);
});

test('clipboard insertion revalidates hostile payloads before history or state changes', () => {
  const model = new ProcessEditModel(view());
  const payload = {
    kind: 'tclaude/process-selection', version: 1,
    nodes: [{ id: 'copy', node: { type: 'task' }, position: { x: 0, y: 0 } }],
    edges: [{ from: 'copy', outcome: 'pass', to: 'missing' }],
  };
  const before = model.saveBody();
  assert.throws(() => model.insertClipboardSelection(payload), /missing endpoint/);
  assert.deepEqual(model.saveBody(), before);
  assert.equal(model.canUndo, false);

  payload.edges = [];
  payload.nodes[0].node.checks = {};
  assert.throws(() => model.insertClipboardSelection(payload), /incompatible process node data/);
  assert.deepEqual(model.saveBody(), before);
  assert.equal(model.canUndo, false);

  delete payload.nodes[0].node.checks;
  assert.throws(() => model.insertClipboardSelection(payload, {
    center: { x: Number.MAX_VALUE, y: 0 },
  }), /coordinate limits/);
  assert.deepEqual(model.saveBody(), before);
  assert.equal(model.canUndo, false);
});

test('palette data is well-formed: known primitive types, internally consistent snippets', () => {
  const types = new Set(['task', 'decision', 'parallel', 'wait', 'start', 'end']);
  for (const primitive of PALETTE_PRIMITIVES) assert.ok(types.has(primitive.type), primitive.type);
  assert.ok(PALETTE_PRIMITIVES.some(primitive => primitive.type === 'parallel'));
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

test('blankEditView scaffolds only the start node and preserves blank-editor state', () => {
  const blank = blankEditView('fresh');
  const model = new ProcessEditModel(blank, {});
  assert.equal(model.template.id, 'fresh');
  assert.deepEqual(model.template.nodes, { start: { type: 'start' } });
  assert.deepEqual(model.edges, [{ from: '', outcome: 'start', to: 'start' }]);
  assert.deepEqual(model.layout, { nodes: { start: { x: 120, y: 90 } } });
  assert.equal(model.sourceHash, '', 'first save must CAS against an empty head');
  assert.equal(model.dirty, false);
  assert.equal(model.canUndo, false);
  assert.equal(model.canRedo, false);
  const graph = model.graph();
  assert.deepEqual(graph.nodes.map(({ id, type }) => ({ id, type })), [{ id: 'start', type: 'start' }]);
  assert.deepEqual(graph.edges, []);
  assert.deepEqual(model.saveBody(), {
    template: blank.template,
    edges: blank.edges,
    layout: blank.layout,
    sourceHash: '',
  });
});

test('setStart repoints the start pseudo edge exactly once', () => {
  const model = new ProcessEditModel(view());
  model.setStart('build');
  const starts = model.edges.filter(edge => edge.from === '' && edge.outcome === 'start');
  assert.equal(starts.length, 1);
  assert.equal(starts[0].to, 'build');
  assert.equal(model.template.start, 'build');
});

test('a lone connector keeps the precedence-winning pass name', () => {
  // Hiding the label must not change which edge the runtime picks. 'pass' is
  // the FIRST entry in model/next.go's passOutcomeLabels, so a sibling added
  // later can never outrank it and steal the pass routing. Minting 'next' here
  // -- the LAST alias -- would do exactly that.
  const model = new ProcessEditModel(view());
  model.addNode('wait', { id: 'hold' });
  const first = model.freeOutcome('hold', 'pass');
  assert.equal(first, 'pass');
  model.addEdge('hold', first, 'ship');

  const second = model.freeOutcome('hold', 'pass');
  model.addEdge('hold', second, 'build');
  const passVocabulary = ['pass', 'done', 'success', 'next'];
  const winner = passVocabulary.find((label) => [first, second].includes(label));
  assert.equal(winner, first, 'the connector drawn first must keep the pass routing');
});

test('outcomes stay real non-empty keys', () => {
  const model = new ProcessEditModel(view());
  model.addNode('task', { id: 'fresh' });
  const outcome = model.freeOutcome('fresh', 'pass');
  assert.notEqual(outcome, '');
  model.addEdge('fresh', outcome, 'ship');
  assert.equal(model.findEdge('fresh', outcome).to, 'ship');
  assert.throws(() => model.addEdge('fresh', '', 'build'), /outcome is required/);
});

test('pin state lives in layout and survives save', () => {
  const model = new ProcessEditModel(view());
  assert.equal(model.edgePinned('build', 'pass'), undefined, 'no stored opinion by default');

  model.setEdgePinned('build', 'pass', false);
  assert.equal(model.edgePinned('build', 'pass'), false);
  assert.equal(model.layout.edges.build.pass.pinned, false);
  assert.equal(JSON.parse(JSON.stringify(model.saveBody())).layout.edges.build.pass.pinned, false);

  // Clearing returns the edge to the default rule and prunes the containers, so
  // an untouched template never grows an empty layout.edges block.
  model.setEdgePinned('build', 'pass', undefined);
  assert.equal(model.edgePinned('build', 'pass'), undefined);
  assert.equal(model.layout.edges, undefined);
});

test('pinning is one undoable edit', () => {
  const model = new ProcessEditModel(view());
  const before = model.undoStack.length;
  model.setEdgePinned('build', 'pass', false);
  assert.equal(model.undoStack.length, before + 1);
  assert.equal(model.undo(), true);
  assert.equal(model.edgePinned('build', 'pass'), undefined);
  // Setting what is already stored is a no-op, not a wasted undo step.
  assert.equal(model.setEdgePinned('build', 'pass', undefined), false);
  assert.equal(model.undoStack.length, before);
});

test('pin state follows a renamed connector instead of its old name', () => {
  const model = new ProcessEditModel(view());
  model.setEdgePinned('build', 'pass', true);
  model.setEdgeOutcome('build', 'pass', 'shipped');
  assert.equal(model.edgePinned('build', 'shipped'), true, 'the opinion belongs to the connector');
  assert.equal(model.edgePinned('build', 'pass'), undefined, 'and must not be left on the old name');
});

test('a deleted connector does not bequeath its pin to the next one', () => {
  // freeOutcome reuses a name as soon as it is free, so stale pin state would
  // silently apply this edge's opinion to an unrelated new connector.
  const model = new ProcessEditModel(view());
  model.setEdgePinned('build', 'pass', false);
  model.deleteEdge('build', 'pass');
  model.addEdge('build', 'pass', 'ship');
  assert.equal(model.edgePinned('build', 'pass'), undefined);
});

test('deleting a node takes its outgoing pins with it', () => {
  const model = new ProcessEditModel(view());
  model.setEdgePinned('build', 'pass', true);
  model.deleteNode('build');
  assert.equal(model.layout.edges?.build, undefined);
});
