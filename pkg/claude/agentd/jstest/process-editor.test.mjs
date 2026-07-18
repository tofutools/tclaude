import test from 'node:test';
import assert from 'node:assert/strict';
import {
  PROCESS_PASTE_TARGET_EPSILON, ProcessTemplateEditor, hasNonCollapsedDOMSelection,
  isProcessEditorFormControl, resolveProcessPastePlacement,
} from '../dashboard/js/process-editor.js';
import { ProcessEditModel } from '../dashboard/js/process-edit-model.js';
import { buildProcessEditorCommands } from '../dashboard/js/process-command-registry.js';
import { parseProcessSelection, serializeProcessSelection } from '../dashboard/js/process-editor-clipboard.js';
import { diagnosticIdentity, LiveValidation } from '../dashboard/js/process-validation.js';

test('Delete dispatches against the current visible editor selection', () => {
  const selected = { type: 'node', id: 'highlighted' };
  let deleted = null;
  let prevented = false;
  const fake = {
    selection: selected,
    deleteSelection() { deleted = this.selection; },
  };
  ProcessTemplateEditor.prototype.onEditorKeyDown.call(fake, {
    key: 'Delete', target: { tagName: 'DIV' }, ctrlKey: false, metaKey: false,
    preventDefault() { prevented = true; },
  });
  assert.equal(prevented, true);
  assert.equal(deleted, selected, 'the handler reads the highlighted selection, not creation order');
});

test('Delete remains native while editing form fields', () => {
  assert.equal(isProcessEditorFormControl({ tagName: 'input' }), true);
  let deleted = false;
  ProcessTemplateEditor.prototype.onEditorKeyDown.call({
    selection: { type: 'node', id: 'a' }, deleteSelection() { deleted = true; },
  }, {
    key: 'Delete', target: { tagName: 'INPUT' }, ctrlKey: false, metaKey: false,
    preventDefault() { throw new Error('input delete must not be prevented'); },
  });
  assert.equal(deleted, false);
});

function clipboardText(id = 'copied') {
  return serializeProcessSelection({
    kind: 'tclaude/process-selection', version: 1,
    nodes: [{ id, node: { type: 'task', performer: { kind: 'agent', profile: 'implementer' } }, position: { x: 10, y: 20 } }],
    edges: [],
  });
}

test('paste placement absorbs subpixel transform noise but resets for a moved cursor', () => {
  const first = resolveProcessPastePlacement('same', { x: 100.125, y: -20.375 });
  assert.deepEqual(first, { center: { x: 100.125, y: -20.375 }, repeat: 0 });
  const subpixel = resolveProcessPastePlacement('same', {
    x: 100.125 + PROCESS_PASTE_TARGET_EPSILON / 2,
    y: -20.375 - PROCESS_PASTE_TARGET_EPSILON / 2,
  }, { fingerprint: 'same', anchor: first.center, repeat: first.repeat });
  assert.deepEqual(subpixel, { center: first.center, repeat: 1 },
    'subpixel SVGRect/view noise retains the exact first anchor');
  const moved = resolveProcessPastePlacement('same', { x: 102.5, y: -20.375 }, {
    fingerprint: 'same', anchor: first.center, repeat: subpixel.repeat,
  });
  assert.deepEqual(moved, { center: { x: 102.5, y: -20.375 }, repeat: 0 });
});

test('cursor paste target converts the live client point through current pan/zoom and falls back outside', () => {
  const fake = {
    canvasPointer: { clientX: 450.25, clientY: 280.5 },
    graph: {
      containsClientPoint: (x, y) => x >= 100.125 && x <= 900.125 && y >= 50.25 && y <= 650.25,
      clientToGraph: (x, y) => ({
        x: (x - 100.125 - 37.75) / 1.75,
        y: (y - 50.25 + 22.5) / 1.75,
      }),
    },
    canvasCenterPoint: () => ({ x: 999, y: 888 }),
  };
  assert.deepEqual(ProcessTemplateEditor.prototype.pasteTargetPoint.call(fake), {
    x: (450.25 - 100.125 - 37.75) / 1.75,
    y: (280.5 - 50.25 + 22.5) / 1.75,
  });
  fake.canvasPointer = { clientX: 901, clientY: 280.5 };
  assert.deepEqual(ProcessTemplateEditor.prototype.pasteTargetPoint.call(fake), { x: 999, y: 888 });
  assert.equal(fake.canvasPointer, null, 'live bounds rejection invalidates the stale client point');
});

test('copy claims native clipboard only after a selected node serializes successfully', () => {
  const model = new ProcessEditModel({
    template: { nodes: { build: { type: 'task', name: 'Build' } } },
    edges: [], layout: { nodes: { build: { x: 10, y: 20 } } },
  });
  let copied = '';
  let prevented = 0;
  const statuses = [];
  const fake = {
    model, selection: { type: 'node', id: 'build' }, modalDispose: null,
    graph: { layoutSnapshot: () => ({ nodes: [{ id: 'build', x: 12, y: 34 }] }) },
    pasteFingerprint: 'old', pasteRepeat: 4, pasteAnchor: { x: 1, y: 2 },
    status: (...args) => statuses.push(args),
  };
  const accepted = ProcessTemplateEditor.prototype.onEditorCopy.call(fake, {
    target: { tagName: 'DIV' },
    clipboardData: { setData(type, value) { assert.equal(type, 'text/plain'); copied = value; } },
    preventDefault() { prevented += 1; },
  });
  assert.equal(accepted, true);
  assert.equal(prevented, 1);
  const copiedPayload = parseProcessSelection(copied);
  assert.equal(copiedPayload.nodes[0].id, 'build');
  assert.deepEqual(copiedPayload.nodes[0].node, { type: 'task', name: 'Build' });
  assert.deepEqual(copiedPayload.nodes[0].position, { x: 12, y: 34 });
  assert.deepEqual(statuses.at(-1), ['Copied 1 node.']);
  assert.equal(fake.pasteFingerprint, '');
  assert.equal(fake.pasteRepeat, 0);
  assert.equal(fake.pasteAnchor, null);

  assert.equal(hasNonCollapsedDOMSelection({ view: { getSelection: () => ({ isCollapsed: false }) } }), true);
  assert.equal(ProcessTemplateEditor.prototype.onEditorCopy.call(fake, {
    target: { tagName: 'DIV' }, view: { getSelection: () => ({ isCollapsed: false }) },
    clipboardData: { setData() { assert.fail('highlighted DOM text keeps native copy ownership'); } },
    preventDefault() { assert.fail('highlighted DOM text keeps native copy ownership'); },
  }), false);

  fake.selection = { type: 'edge', from: 'build', outcome: 'pass' };
  assert.equal(ProcessTemplateEditor.prototype.onEditorCopy.call(fake, {
    target: { tagName: 'DIV' }, clipboardData: { setData() { assert.fail('no node means no write'); } },
    preventDefault() { assert.fail('native copy must remain unclaimed'); },
  }), false);
});

test('copy and paste remain native in text editors, embedded editors, and open modals', () => {
  assert.equal(isProcessEditorFormControl({ tagName: 'DIV', isContentEditable: true }), true);
  assert.equal(isProcessEditorFormControl({
    tagName: 'SPAN', closest: (selector) => selector.includes('.monaco-editor') ? {} : null,
  }), true);
  for (const target of [{ tagName: 'INPUT' }, { tagName: 'DIV', isContentEditable: true }]) {
    assert.equal(ProcessTemplateEditor.prototype.onEditorPaste.call({ modalDispose: null }, {
      target, clipboardData: { getData() { assert.fail('owned text controls are never inspected'); } },
      preventDefault() { assert.fail('owned text controls stay native'); },
    }), false);
  }
  assert.equal(ProcessTemplateEditor.prototype.onEditorPaste.call({ modalDispose: () => {} }, {
    target: { tagName: 'DIV' }, clipboardData: { getData() { assert.fail('open modal owns paste'); } },
    preventDefault() { assert.fail('open modal paste stays native'); },
  }), false);
  assert.equal(ProcessTemplateEditor.prototype.onEditorPaste.call({ modalDispose: null }, {
    isTrusted: false, target: { tagName: 'DIV' },
    clipboardData: { getData() { assert.fail('synthetic events never expose clipboard bytes'); } },
    preventDefault() { assert.fail('synthetic events remain unclaimed'); },
  }), false);
});

test('unrelated paste stays native and sentinel-invalid paste fails atomically with bounded status', () => {
  let prevented = 0;
  let status = null;
  const model = new ProcessEditModel({
    template: { nodes: { kept: { type: 'task' } } }, edges: [], layout: { nodes: {} },
  });
  const selection = { type: 'node', id: 'kept' };
  const fake = {
    model, selection, modalDispose: null,
    pasteFingerprint: 'prior', pasteRepeat: 3, pasteAnchor: { x: 7, y: 8 },
    status: (...args) => { status = args; },
  };
  assert.equal(ProcessTemplateEditor.prototype.onEditorPaste.call(fake, {
    target: { tagName: 'DIV' }, clipboardData: { getData: () => 'ordinary prose' },
    preventDefault() { prevented += 1; },
  }), false);
  assert.equal(prevented, 0);
  assert.equal(status, null);

  const before = model.saveBody();
  assert.equal(ProcessTemplateEditor.prototype.onEditorPaste.call(fake, {
    target: { tagName: 'DIV' },
    clipboardData: { getData: () => 'tclaude-process-selection:v1\n{"kind":"tclaude/process-selection","version":1,"nodes":[' },
    preventDefault() { prevented += 1; },
  }), true);
  assert.equal(prevented, 1);
  assert.match(status[0], /not valid JSON/);
  assert.equal(status[0].includes('{"kind"'), false, 'raw clipboard bytes are never surfaced');
  assert.deepEqual(model.saveBody(), before);
  assert.equal(model.canUndo, false);
  assert.equal(fake.selection, selection);
  assert.deepEqual([fake.pasteFingerprint, fake.pasteRepeat, fake.pasteAnchor],
    ['prior', 3, { x: 7, y: 8 }]);

  const malformed = `tclaude-process-selection:v1\n${JSON.stringify({
    kind: 'tclaude/process-selection', version: 1,
    nodes: [{ id: 'bad', node: { type: 'task', checks: {} }, position: { x: 0, y: 0 } }], edges: [],
  })}`;
  assert.equal(ProcessTemplateEditor.prototype.onEditorPaste.call(fake, {
    target: { tagName: 'DIV' }, clipboardData: { getData: () => malformed },
    preventDefault() { prevented += 1; },
  }), true);
  assert.equal(prevented, 2);
  assert.match(status[0], /incompatible process node data/);
  assert.deepEqual(model.saveBody(), before);
  assert.equal(model.canUndo, false);
  assert.equal(fake.selection, selection);
  assert.deepEqual([fake.pasteFingerprint, fake.pasteRepeat, fake.pasteAnchor],
    ['prior', 3, { x: 7, y: 8 }]);
});

test('valid repeated paste uses fresh ids, one history step each, cascading placement, and focus', async () => {
  const model = new ProcessEditModel({
    template: { nodes: { copied: { type: 'task', name: 'Existing' } } }, edges: [],
    layout: { nodes: { copied: { x: 0, y: 0 } } },
  });
  const focused = [];
  let refreshes = 0;
  const fake = {
    model, selection: null, modalDispose: null, pasteFingerprint: '', pasteRepeat: 0, pasteAnchor: null,
    externalDecisionPending: false, externalReloadPending: false,
    graph: { focusNode: (id) => focused.push(id) },
    pasteTargetPoint: () => ({ x: 100, y: 200 }),
    refresh: () => { refreshes += 1; },
    setSelection(value) { this.selection = value; },
    status() {},
  };
  const text = clipboardText();
  const paste = () => ProcessTemplateEditor.prototype.onEditorPaste.call(fake, {
    target: { tagName: 'DIV' }, clipboardData: { getData: () => text }, preventDefault() {},
  });
  assert.equal(paste(), true);
  assert.equal(paste(), true);
  await Promise.resolve();
  assert.ok(model.node('copied-2'));
  assert.ok(model.node('copied-3'));
  assert.deepEqual(model.layout.nodes['copied-2'], { x: 100, y: 200 });
  assert.deepEqual(model.layout.nodes['copied-3'], { x: 136, y: 236 });
  assert.deepEqual(fake.selection, { type: 'node', id: 'copied-3' });
  assert.deepEqual(focused, ['copied-2', 'copied-3']);
  assert.equal(model.undoStack.length, 2);
  assert.equal(refreshes, 2);
});

test('read-only editor claims a valid sentinel paste but never mutates history or selection', () => {
  const model = new ProcessEditModel({
    template: { nodes: { kept: { type: 'task' } } }, edges: [], layout: { nodes: {} },
  }, { canInsert: false });
  const selection = { type: 'node', id: 'kept' };
  let prevented = 0;
  let status = null;
  const fake = {
    model, selection, modalDispose: null,
    pasteFingerprint: 'prior', pasteRepeat: 2, pasteAnchor: { x: 4, y: 5 },
    status: (...args) => { status = args; },
  };
  assert.equal(ProcessTemplateEditor.prototype.onEditorPaste.call(fake, {
    target: { tagName: 'DIV' }, clipboardData: { getData: () => clipboardText() },
    preventDefault() { prevented += 1; },
  }), true);
  assert.equal(prevented, 1);
  assert.match(status[0], /read-only/);
  assert.equal(model.canUndo, false);
  assert.equal(fake.selection, selection);
  assert.equal(model.node('copied'), undefined);
  assert.deepEqual([fake.pasteFingerprint, fake.pasteRepeat, fake.pasteAnchor],
    ['prior', 2, { x: 4, y: 5 }]);
});

test('external, capacity, and coordinate failures never advance paste cascade state', () => {
  const text = clipboardText();
  const makeFake = () => {
    const model = new ProcessEditModel({ template: { nodes: {} }, edges: [], layout: { nodes: {} } });
    return {
      model, selection: null, modalDispose: null,
      pasteFingerprint: 'prior', pasteRepeat: 2, pasteAnchor: { x: 4, y: 5 },
      externalDecisionPending: false, externalReloadPending: false,
      pasteTargetPoint: () => ({ x: 10, y: 20 }),
      status() {},
    };
  };
  const paste = (fake) => ProcessTemplateEditor.prototype.onEditorPaste.call(fake, {
    target: { tagName: 'DIV' }, clipboardData: { getData: () => text }, preventDefault() {},
  });
  const unchanged = (fake) => assert.deepEqual(
    [fake.pasteFingerprint, fake.pasteRepeat, fake.pasteAnchor],
    ['prior', 2, { x: 4, y: 5 }],
  );

  const pending = makeFake();
  pending.externalReloadPending = true;
  pending.pasteTargetPoint = () => assert.fail('pending reload rejects before coordinate resolution');
  assert.equal(paste(pending), true);
  unchanged(pending);
  assert.equal(pending.model.canUndo, false);

  const capacity = makeFake();
  capacity.model.insertClipboardSelection = () => { throw new Error('destination capacity'); };
  assert.equal(paste(capacity), true);
  unchanged(capacity);
  assert.equal(capacity.model.canUndo, false);

  const coordinate = makeFake();
  coordinate.pasteTargetPoint = () => { throw new Error('coordinate resolution'); };
  assert.equal(paste(coordinate), true);
  unchanged(coordinate);
  assert.equal(coordinate.model.canUndo, false);
});

test('palette drop and contextual create command share addNodeType', () => {
  const calls = [];
  const fake = {
    paletteDragPayload: null,
    addNodeType: (...args) => calls.push(args),
  };
  ProcessTemplateEditor.prototype.onCanvasDrop.call(fake, {
    point: { x: 12, y: 34 },
    event: { dataTransfer: { getData: () => JSON.stringify({ kind: 'primitive', type: 'decision' }) } },
  });
  assert.deepEqual(calls, [['decision', { x: 12, y: 34 }]]);
});

test('only a valid empty-canvas port release opens the connected chooser', () => {
  const opened = [];
  const fake = {
    band: null,
    model: new ProcessEditModel({ template: { nodes: { a: { type: 'task' } } }, edges: [], layout: { nodes: {} } }),
    removeBand() {},
    status() {},
    openConnectedNodeChooser: (...args) => opened.push(args),
  };
  const point = { x: 12.5, y: -8 };
  const event = { clientX: 440, clientY: 220 };
  ProcessTemplateEditor.prototype.onPortDragEnd.call(fake, {
    nodeId: 'a', port: 'out', point, targetNodeId: null, targetPort: null, emptyCanvas: true, event,
  });
  assert.deepEqual(opened, [[{ nodeId: 'a', port: 'out' }, point, event]]);

  ProcessTemplateEditor.prototype.onPortDragEnd.call(fake, {
    nodeId: 'a', port: 'out', point, targetNodeId: null, emptyCanvas: true, cancelled: true, event,
  });
  assert.equal(opened.length, 1, 'pointer cancellation never opens a chooser');

  ProcessTemplateEditor.prototype.onPortDragEnd.call(fake, {
    nodeId: 'a', port: 'out', point, targetNodeId: null, emptyCanvas: false, event,
  });
  assert.equal(opened.length, 1, 'release over an edge, control, or outside the SVG is not empty canvas');
});

test('drop commit reuses connection feedback preflight for direction and invalid reasons', () => {
  const statuses = [];
  const model = new ProcessEditModel({
    template: { nodes: { build: { type: 'task', name: 'Build' }, review: { type: 'decision', name: 'Review' }, ship: { type: 'end', name: 'Ship' } } },
    edges: [], layout: { nodes: {} },
  });
  const fake = {
    model, band: null, removeBand() {},
    mutate(operation) { try { return operation(); } catch (error) { statuses.push([error.message, true]); return undefined; } },
    status: (...args) => statuses.push(args),
    setSelection() {}, openInlineOutcomeEdit() {},
  };
  ProcessTemplateEditor.prototype.onPortDragEnd.call(fake, {
    nodeId: 'build', port: 'in', point: {}, targetNodeId: 'review', targetPort: 'out',
  });
  assert.ok(model.edges.some((edge) => edge.from === 'review' && edge.to === 'build'),
    'the resolver-provided reverse direction is the committed edge');

  const before = structuredClone(model.edges);
  ProcessTemplateEditor.prototype.onPortDragEnd.call(fake, {
    nodeId: 'build', port: 'in', point: {}, targetNodeId: 'review', targetPort: 'in',
  });
  assert.deepEqual(model.edges, before);
  assert.deepEqual(statuses.at(-1), ['Connect this input to an output port or another node body.', true]);

  ProcessTemplateEditor.prototype.onPortDragEnd.call(fake, {
    nodeId: 'ship', port: 'out', point: {}, targetNodeId: 'review', targetPort: 'in',
  });
  assert.deepEqual(model.edges, before);
  assert.deepEqual(statuses.at(-1), ['End nodes cannot have outgoing connections.', true]);
});

test('missing keyboard source cancellation clears editor gesture state without commit or status mutation', () => {
  const model = new ProcessEditModel({
    template: { nodes: { build: { type: 'task' } } }, edges: [], layout: { nodes: {} },
  });
  const before = model.saveBody();
  let removed = 0;
  const fake = {
    model, band: { source: { nodeId: 'build', port: 'out' } },
    removeBand() { removed += 1; this.band = null; },
    mutate() { assert.fail('cancelled removal must not enter a model mutation'); },
    status() { assert.fail('cancelled removal must not publish a status'); },
  };
  ProcessTemplateEditor.prototype.onPortDragEnd.call(fake, {
    nodeId: 'build', port: 'out', point: { x: 1, y: 2 },
    targetNodeId: null, targetPort: null, keyboard: true, cancelled: true,
    cancellation: 'source-removed',
  });
  assert.equal(removed, 1);
  assert.equal(fake.band, null);
  assert.deepEqual(model.saveBody(), before);
});

test('connected-node selection preserves direction and opens required configuration', async () => {
  const calls = [];
  const fake = {
    model: {
      addConnectedNode(type, options) {
        calls.push(['add', type, options]);
        const from = options.connectFrom || type;
        const to = options.connectTo || type;
        return { id: `${type}-id`, edge: { from, outcome: 'pass', to } };
      },
    },
    mutate(operation) { return operation(); },
    setSelection(value) { calls.push(['selection', value]); },
    status(value) { calls.push(['status', value]); },
    openNodeSettings(id) { calls.push(['settings', id]); return true; },
    graph: { root: { focus() {} }, focusNode(id) { calls.push(['focus', id]); } },
  };

  assert.equal(ProcessTemplateEditor.prototype.addConnectedNodeType.call(
    fake, 'task', { nodeId: 'origin', port: 'out' }, { x: 40, y: 55 },
  ), 'task-id');
  assert.deepEqual(calls[0], ['add', 'task', { x: 40, y: 55, connectFrom: 'origin' }]);
  assert.ok(calls.some((entry) => entry[0] === 'settings' && entry[1] === 'task-id'));
  assert.ok(calls.some((entry) => entry[0] === 'selection' && entry[1].id === 'task-id'));

  calls.length = 0;
  ProcessTemplateEditor.prototype.addConnectedNodeType.call(
    fake, 'start', { nodeId: 'existing', port: 'in' }, { x: -3, y: 7 },
  );
  await Promise.resolve();
  assert.deepEqual(calls[0], ['add', 'start', { x: -3, y: 7, connectTo: 'existing' }]);
  assert.ok(calls.some((entry) => entry[0] === 'focus' && entry[1] === 'start-id'));
  assert.equal(calls.some((entry) => entry[0] === 'settings'), false);
});

test('invalid end-node origin is rejected before the chooser opens', () => {
  const statuses = [];
  const fake = {
    model: { node: () => ({ type: 'end' }), config: { canInsert: true } },
    status: (...args) => statuses.push(args),
  };
  assert.equal(ProcessTemplateEditor.prototype.openConnectedNodeChooser.call(
    fake, { nodeId: 'done', port: 'out' }, { x: 1, y: 2 }, {},
  ), false);
  assert.deepEqual(statuses, [['End nodes cannot have outgoing connections.', true]]);
});

test('command context reflects selection, editability, dirty state, and validation issues', () => {
  const edge = { from: 'a', outcome: 'pass', to: 'b' };
  const fake = {
    selection: { type: 'node', id: 'a' },
    externalDecisionPending: false, externalReloadPending: false, savePending: false, blank: false,
    options: { onInstantiate() {} },
    validation: { mapped: { entries: [{ code: 'issue' }] } },
    model: {
      template: { id: 'flow', nodes: { a: { type: 'task' }, b: { type: 'end' } } },
      edges: [edge], dirty: true,
      config: { canInsert: true, nodeEditable: (id) => id !== 'b', edgeEditable: () => true },
      node(id) { return this.template.nodes[id]; },
      findEdge(from, outcome) { return this.edges.find((item) => item.from === from && item.outcome === outcome); },
    },
  };
  const current = ProcessTemplateEditor.prototype.commandContext.call(fake);
  assert.equal(current.canCreate, true);
  assert.equal(current.canEdit, true);
  assert.equal(current.canDuplicate, true);
  assert.equal(current.canDelete, true);
  assert.equal(current.canSave, true);
  assert.equal(current.canInstantiate, true);
  assert.equal(current.issueCount, 1);

  fake.selection = { type: 'node', id: 'b' };
  fake.model.dirty = false;
  const locked = ProcessTemplateEditor.prototype.commandContext.call(fake);
  assert.equal(locked.canEdit, false);
  assert.equal(locked.canDelete, false);
  assert.match(locked.editReason, /read-only/);
  assert.equal(locked.canSave, false);
  assert.match(locked.saveReason, /no unsaved changes/i);
});

test('validate now delegates without leaving a persistent progress status', () => {
  let accepted = true;
  const statuses = [];
  const fake = {
    validation: { validateNow: () => accepted },
    status: (message) => statuses.push(message),
  };
  assert.equal(ProcessTemplateEditor.prototype.validateNow.call(fake), true);
  accepted = false;
  assert.equal(ProcessTemplateEditor.prototype.validateNow.call(fake), false);
  assert.deepEqual(statuses, [], 'the issues panel owns completion and failure feedback');
});

test('process scribe handoff anchors a clean saved editor exactly', async () => {
  const emitted = [];
  const fake = {
    blank: false, dirty: false, savePending: false,
    selection: null, validation: null,
    model: { template: { id: 'release-flow', nodes: { ship: { type: 'task' } } }, edges: [], currentRef: `release-flow@sha256:${'a'.repeat(64)}`, sourceHash: 'b'.repeat(64) },
    scribePreviewModal: async (preview) => { assert.match(preview.context, /"kind": "whole-template"/); return 'Refactor safely.'; },
    options: { onScribe: async (...args) => { emitted.push(args); return { conv_id: 'scribe' }; } },
    abort: { signal: { aborted: false } },
  };
  assert.equal(await ProcessTemplateEditor.prototype.requestScribe.call(fake), true);
  assert.deepEqual(emitted[0][0], {
    kind: 'template', id: 'release-flow', currentRef: `release-flow@sha256:${'a'.repeat(64)}`,
    sourceHash: 'b'.repeat(64), isNew: false,
  });
  assert.equal(emitted[0][1].prompt, 'Refactor safely.');
  assert.equal(emitted[0][1].context.template.sourceHash, 'b'.repeat(64));
});

test('selection and diagnostic scribe handoffs fail visibly without current context', async () => {
  const statuses = [];
  const fake = {
    blank: false, dirty: false, savePending: false, selection: null,
    model: { template: { id: 'release-flow', nodes: {} }, edges: [], currentRef: `release-flow@sha256:${'a'.repeat(64)}`, sourceHash: 'b'.repeat(64) },
    validation: { currentIssue: () => null }, abort: { signal: { aborted: false } },
    options: { onScribe: async () => { throw new Error('must not send'); } },
    status: (...args) => statuses.push(args),
  };
  assert.equal(await ProcessTemplateEditor.prototype.requestScribe.call(fake, 'selection'), false);
  assert.match(statuses.at(-1)[0], /Select one or more graph items/);
  assert.equal(await ProcessTemplateEditor.prototype.requestScribe.call(fake, 'diagnostic'), false);
  assert.match(statuses.at(-1)[0], /Focus a validation issue/);
});

test('diagnostic scribe handoff fails closed for production-shaped duplicate identities', async () => {
  const entries = [
    { code: 'undeclared_param_ref', scope: 'node', targetId: 'work', node: 'work', message: 'param foo is undeclared' },
    { code: 'undeclared_param_ref', scope: 'node', targetId: 'work', node: 'work', message: 'param bar is undeclared' },
  ];
  const validation = {
    mapped: { entries }, issueCursor: -1, focusedIssueIdentity: '', focusedIssueAmbiguous: false,
    panel: { open: false }, list: { querySelector: () => null }, focusEntry() {},
    currentIssue: LiveValidation.prototype.currentIssue,
  };
  assert.equal(LiveValidation.prototype.focusIssueAt.call(validation, 0, { focusButton: false }), true);
  assert.equal(validation.focusedIssueAmbiguous, true, 'the two distinct messages share one stable identity');

  const statuses = [];
  const fake = {
    blank: false, dirty: false, savePending: false, selection: null, validation,
    model: {
      template: { id: 'release-flow', nodes: { work: { type: 'task' } } }, edges: [],
      currentRef: `release-flow@sha256:${'a'.repeat(64)}`, sourceHash: 'b'.repeat(64),
    },
    abort: { signal: { aborted: false } },
    scribePreviewModal: async () => { throw new Error('ambiguous diagnostic must not reach preview'); },
    options: { onScribe: async () => { throw new Error('ambiguous diagnostic must not be sent'); } },
    status: (...args) => statuses.push(args),
  };
  assert.equal(await ProcessTemplateEditor.prototype.requestScribe.call(fake, 'diagnostic'), false);
  assert.match(statuses.at(-1)[0], /Focus a validation issue/);
});

test('diagnostic send rechecks exact unique focus after the human preview', async () => {
  const selected = {
    code: 'missing_performer', scope: 'node', targetId: 'build', node: 'build',
    severity: 'error', message: 'build needs a performer',
  };
  const other = {
    code: 'missing_next', scope: 'node', targetId: 'ship', node: 'ship',
    severity: 'warning', message: 'ship needs a next edge',
  };
  const cases = [
    {
      name: 'removed', mutate(validation) {
        validation.mapped.entries = [];
        validation.issueCursor = -1;
        validation.focusedIssueIdentity = '';
      }, sends: 0,
    },
    {
      name: 'ambiguous', mutate(validation) {
        validation.mapped.entries.push({ ...selected, message: 'a second issue with the same stable identity' });
        validation.focusedIssueAmbiguous = true;
      }, sends: 0,
    },
    {
      name: 'changed identity', mutate(validation) {
        validation.mapped.entries = [other];
        validation.issueCursor = 0;
        validation.focusedIssueIdentity = diagnosticIdentity(other);
      }, sends: 0,
    },
    {
      name: 'harmless reorder', mutate(validation) {
        validation.mapped.entries = [other, selected];
        validation.issueCursor = 1;
      }, sends: 1,
    },
  ];

  for (const scenario of cases) {
    const validation = {
      mapped: { entries: [selected, other] }, issueCursor: 0,
      focusedIssueIdentity: diagnosticIdentity(selected), focusedIssueAmbiguous: false,
      currentIssue: LiveValidation.prototype.currentIssue,
    };
    const emitted = [];
    const statuses = [];
    let focused = 0;
    const fake = {
      blank: false, dirty: false, savePending: false, selection: null, validation,
      model: {
        template: { id: 'release-flow', nodes: { build: { type: 'task' }, ship: { type: 'task' } } }, edges: [],
        currentRef: `release-flow@sha256:${'a'.repeat(64)}`, sourceHash: 'b'.repeat(64),
      },
      abort: { signal: { aborted: false } },
      graph: { focus: () => { focused += 1; } },
      scribePreviewModal: async (preview) => {
        assert.match(preview.context, /missing_performer/, `${scenario.name}: preview keeps the approved snapshot`);
        scenario.mutate(validation);
        return 'Fix this issue.';
      },
      options: { onScribe: async (...args) => {
        assert.equal(args[1].freshnessGuard(), true, `${scenario.name}: action-boundary guard accepts only fresh context`);
        emitted.push(args); return {};
      } },
      status: (...args) => statuses.push(args),
    };
    assert.equal(await ProcessTemplateEditor.prototype.requestScribe.call(fake, 'diagnostic'), scenario.sends === 1,
      scenario.name);
    assert.equal(emitted.length, scenario.sends, `${scenario.name}: stale or ambiguous context is never sent`);
    if (scenario.sends) {
      assert.deepEqual(emitted[0][1].context.diagnostic.identity,
        { code: 'missing_performer', scope: 'node', targetId: 'build' });
      assert.equal(focused, 0, `${scenario.name}: accepted send keeps focus behavior unchanged`);
    } else {
      assert.match(statuses.at(-1)[0], /editor context changed while the request was open/);
      assert.equal(focused, 1, `${scenario.name}: cancelled send returns focus to the graph`);
    }
  }
});

test('scribe action freshness guard covers model lifecycle, selection, and diagnostic focus', async () => {
  const hash = 'a'.repeat(64);
  const diagnostic = { code: 'missing_performer', scope: 'node', targetId: 'build', node: 'build', message: 'missing' };
  const otherDiagnostic = { code: 'missing_next', scope: 'node', targetId: 'ship', node: 'ship', message: 'changed' };
  const cases = [
    { name: 'model revision', kind: 'template', mutate(fake) { fake.model.rev += 1; } },
    { name: 'model replacement', kind: 'template', mutate(fake) { fake.model = { ...fake.model }; } },
    { name: 'selection identity', kind: 'selection', mutate(fake) { fake.selection = { type: 'node', id: 'ship' }; } },
    { name: 'diagnostic identity', kind: 'diagnostic', mutate(fake) {
      fake.validation.mapped.entries = [otherDiagnostic];
      fake.validation.issueCursor = 0;
      fake.validation.focusedIssueIdentity = diagnosticIdentity(otherDiagnostic);
    } },
    { name: 'diagnostic message', kind: 'diagnostic', mutate(fake) {
      fake.validation.mapped.entries = [{ ...diagnostic, message: 'performer requirement changed' }];
    } },
    { name: 'diagnostic severity', kind: 'diagnostic', mutate(fake) {
      fake.validation.mapped.entries = [{ ...diagnostic, severity: 'error' }];
    } },
  ];

  for (const scenario of cases) {
    const validation = {
      mapped: { entries: [diagnostic] }, issueCursor: 0,
      focusedIssueIdentity: diagnosticIdentity(diagnostic), focusedIssueAmbiguous: false,
      currentIssue: LiveValidation.prototype.currentIssue,
    };
    const fake = {
      blank: false, dirty: false, savePending: false,
      externalDecisionPending: false, externalReloadPending: false,
      selection: scenario.kind === 'selection' ? { type: 'node', id: 'build' } : null,
      validation,
      model: {
        rev: 7, template: { id: 'release-flow', nodes: { build: { type: 'task' }, ship: { type: 'task' } } }, edges: [],
        currentRef: `release-flow@sha256:${hash}`, sourceHash: 'b'.repeat(64),
      },
      abort: { signal: { aborted: false } }, graph: { focus() {} }, status() {},
      scribePreviewModal: async () => 'Proceed.',
      options: { onScribe: async (_anchor, options) => {
        scenario.mutate(fake);
        assert.equal(options.freshnessGuard(), false, `${scenario.name} must invalidate the final action boundary`);
        return null;
      } },
    };
    assert.equal(await ProcessTemplateEditor.prototype.requestScribe.call(fake, scenario.kind), false, scenario.name);
  }
});

test('cancelling the scribe preview leaves focus restoration to the shared overlay', async () => {
  let focused = 0;
  const fake = {
    blank: false, dirty: false, savePending: false, selection: null,
    model: { template: { id: 'release-flow', nodes: {} }, edges: [], currentRef: `release-flow@sha256:${'a'.repeat(64)}`, sourceHash: 'b'.repeat(64) },
    validation: null, abort: { signal: { aborted: false } },
    graph: { focus: () => { focused += 1; } },
    scribePreviewModal: async () => null,
    options: { onScribe: async () => { throw new Error('cancel must not send'); } },
  };
  assert.equal(await ProcessTemplateEditor.prototype.requestScribe.call(fake), false);
  assert.equal(focused, 0, 'the controller does not override the component-owned invoker restoration');
});

test('dirty process scribe handoff requires an explicit successful resolution', async () => {
  const emitted = [];
  const base = () => ({
    blank: false, dirty: true, savePending: false,
    selection: null, validation: null,
    model: { template: { id: 'release-flow', nodes: {} }, edges: [], currentRef: 'old-ref', sourceHash: 'old-hash' },
    scribePreviewModal: async () => 'Please help.', abort: { signal: { aborted: false } },
    options: { onScribe: async (anchor) => { emitted.push(anchor); return {}; } },
  });
  const cancelled = { ...base(), choiceModal: async () => null };
  assert.equal(await ProcessTemplateEditor.prototype.requestScribe.call(cancelled), false);
  assert.deepEqual(emitted, []);

  const saved = { ...base(), choiceModal: async (copy) => {
    assert.match(copy.title, /Resolve unsaved edits/);
    assert.deepEqual(copy.choices.map(choice => choice.key), ['discard', 'save']);
    return 'save';
  } };
  saved.save = async () => {
    saved.dirty = false;
    saved.model.currentRef = `release-flow@sha256:${'c'.repeat(64)}`;
    saved.model.sourceHash = 'd'.repeat(64);
    return true;
  };
  assert.equal(await ProcessTemplateEditor.prototype.requestScribe.call(saved), true);
  assert.equal(emitted.length, 1);
  assert.equal(emitted[0].currentRef, `release-flow@sha256:${'c'.repeat(64)}`, 'handoff uses the generation produced by Save first');

  const discarded = {
    blank: true, dirty: true, savePending: false,
    model: { template: { id: 'new-process' }, sourceHash: '', config: {} },
    options: { onScribe: async (anchor) => { emitted.push(anchor); return {}; } },
    choiceModal: async () => 'discard', validation: null, refresh() {}, status() {}, selection: null,
    scribePreviewModal: async () => 'Build it.', abort: { signal: { aborted: false } },
  };
  assert.equal(await ProcessTemplateEditor.prototype.requestScribe.call(discarded), true);
  assert.deepEqual(emitted.at(-1), {
    kind: 'template', id: 'new-process', currentRef: '', sourceHash: '', isNew: true,
  }, 'explicit discard resets a new draft before handing off its creation identity');
});

test('existing-template scribe discard fails closed when the editor changes during reload', async () => {
  const previousFetch = globalThis.fetch;
  const reload = deferred(); const emitted = [];
  globalThis.fetch = async () => reload.promise;
  try {
    const editor = externalReloadEditor({ dirty: true });
    editor.choiceModal = async () => 'discard';
    editor.options.onScribe = async (anchor) => { emitted.push(anchor); return {}; };
    const original = editor.model;
    const pending = ProcessTemplateEditor.prototype.requestScribe.call(editor);
    await Promise.resolve();
    assert.equal(editor.externalReloadPending, true, 'editor interactions are locked while canonical state loads');
    original.setTemplateMeta({ description: 'new edit while reload was pending' });
    reload.resolve({
      ok: true, status: 200, statusText: 'OK',
      json: async () => ({
        template: { id: 'alpha', name: 'Canonical', start: 'a', nodes: { a: { type: 'start' } } },
        edges: [], layout: {}, sourceHash: 'source-new', semanticHash: 'semantic-new', currentRef: 'alpha@sha256:new',
      }),
    });
    assert.equal(await pending, false);
    assert.equal(editor.model, original, 'revision advance preserves the newer local draft');
    assert.deepEqual(emitted, [], 'no external scribe starts from a stale discard decision');
    assert.match(editor.lastStatus.message, /cancelled because the editor changed/);
  } finally {
    if (previousFetch === undefined) delete globalThis.fetch;
    else globalThis.fetch = previousFetch;
  }
});

test('destroy during existing-template scribe discard prevents model swap and handoff', async () => {
  const previousFetch = globalThis.fetch;
  const reload = deferred(); const emitted = [];
  globalThis.fetch = async () => reload.promise;
  try {
    const editor = externalReloadEditor({ dirty: true });
    editor.choiceModal = async () => 'discard';
    editor.options.onScribe = async (anchor) => { emitted.push(anchor); return {}; };
    editor.saveSeq = 0;
    editor.graph = { dispose() {} };
    editor.closeInline = () => {};
    editor.mount = { __processEditor: editor, classList: { remove() {} }, replaceChildren() {} };
    const original = editor.model;
    const pending = ProcessTemplateEditor.prototype.requestScribe.call(editor);
    ProcessTemplateEditor.prototype.destroy.call(editor);
    reload.resolve({
      ok: true, status: 200, statusText: 'OK',
      json: async () => ({
        template: { id: 'alpha', name: 'Canonical', start: 'a', nodes: { a: { type: 'start' } } },
        edges: [], layout: {}, sourceHash: 'source-new', semanticHash: 'semantic-new', currentRef: 'alpha@sha256:new',
      }),
    });
    assert.equal(await pending, false);
    assert.equal(editor.model, original);
    assert.deepEqual(emitted, []);
  } finally {
    if (previousFetch === undefined) delete globalThis.fetch;
    else globalThis.fetch = previousFetch;
  }
});

test('template settings selection stays editor-owned and clears the graph adapter', () => {
  let graphSelection = 'not-cleared';
  let publishes = 0;
  const fake = {
    selection: null,
    graph: { setSelection(value) { graphSelection = value; } },
    publish() { publishes += 1; },
  };

  ProcessTemplateEditor.prototype.setSelection.call(fake, { type: 'template' });
  assert.deepEqual(fake.selection, { type: 'template' });
  assert.equal(graphSelection, null, 'template chrome never becomes a graph highlight');
  assert.equal(publishes, 1);

  // refresh() replays setSelection(this.selection), so the editor-only state
  // must survive the same round trip without graph normalization dropping it.
  ProcessTemplateEditor.prototype.setSelection.call(fake, fake.selection);
  assert.deepEqual(fake.selection, { type: 'template' });
});

test('graph multi-selection remains normalized and replaces template settings', () => {
  let graphSelection = null;
  let renders = 0;
  const fake = {
    selection: { type: 'template' },
    graph: { setSelection(value) { graphSelection = value; }, layoutSnapshot: () => ({ edges: [] }) },
    publish() { renders += 1; },
    laidEdge: ProcessTemplateEditor.prototype.laidEdge,
  };
  const multi = { type: 'multi', items: [{ type: 'node', id: 'a' }, { type: 'node', id: 'b' }] };
  ProcessTemplateEditor.prototype.setSelection.call(fake, multi);
  assert.deepEqual(fake.selection, multi);
  assert.deepEqual(graphSelection, multi);
  assert.equal(renders, 1);
});

test('undo and redo preserve template settings selection', () => {
  for (const direction of ['undo', 'redo']) {
    let refreshed = 0;
    const fake = {
      selection: { type: 'template' },
      model: {
        undo() { return true; },
        redo() { return true; },
        node() { throw new Error('template selection must not enter graph liveness filtering'); },
        findEdge() { throw new Error('template selection must not enter graph liveness filtering'); },
      },
      refresh() { refreshed += 1; },
    };
    ProcessTemplateEditor.prototype.applyHistory.call(fake, direction);
    assert.deepEqual(fake.selection, { type: 'template' }, `${direction} keeps the metadata editor active`);
    assert.equal(refreshed, 1);
  }
});

test('history still drops graph selections removed by restored topology', () => {
  let refreshed = 0;
  const fake = {
    selection: { type: 'multi', items: [{ type: 'node', id: 'gone' }, { type: 'node', id: 'kept' }] },
    model: {
      undo() { return true; }, redo() { return true; },
      node(id) { return id === 'kept' ? { type: 'task' } : undefined; },
      findEdge() { return undefined; },
    },
    refresh() { refreshed += 1; },
  };
  ProcessTemplateEditor.prototype.applyHistory.call(fake, 'undo');
  assert.deepEqual(fake.selection, { type: 'node', id: 'kept' });
  assert.equal(refreshed, 1);
});

function deferred() {
  let resolve;
  const promise = new Promise(done => { resolve = done; });
  return { promise, resolve };
}

function saveEditor(id = 'alpha') {
  const editor = {
    blank: true, savePending: false, saveSeq: 0,
    model: {
      template: { id }, sourceHash: '', semanticHash: '', rev: 0,
      dirty: true, canUndo: false, canRedo: false,
      saveBody() { return { template: { ...this.template }, sourceHash: this.sourceHash }; },
      markSaved(body) { this.sourceHash = body.sourceHash; this.semanticHash = body.semanticHash; this.dirty = false; },
    },
    titleLabel: {}, idInput: { value: id },
    identity: { replaceChildren(child) { this.child = child; } },
    versionBadge: {}, dirtyBadge: {}, undoButton: {}, redoButton: {}, saveButton: {},
    renderInspector() {}, status(message, isError) { this.lastStatus = { message, isError }; },
    validation: null, options: {}, abort: { abort() {} },
    graph: { dispose() {} }, modalDispose: null,
    publish() {},
    mount: { classList: { remove() {} }, replaceChildren() {} },
    closeInline() {},
  };
  editor.updateChrome = () => ProcessTemplateEditor.prototype.updateChrome.call(editor);
  editor.saveRequest = requestSeq => ProcessTemplateEditor.prototype.saveRequest.call(editor, requestSeq);
  editor.resolveConflict = (conflict, requestSeq) => ProcessTemplateEditor.prototype.resolveConflict.call(editor, conflict, requestSeq);
  return editor;
}

test('pending first save stays single-flight and refresh cannot re-enable identity/save', async () => {
  const previousFetch = globalThis.fetch;
  const response = deferred();
  let fetches = 0;
  globalThis.fetch = () => { fetches += 1; return response.promise; };
  try {
    const editor = saveEditor('alpha');
    const first = ProcessTemplateEditor.prototype.save.call(editor);
    assert.equal(editor.savePending, true);

    // An allowed canvas edit refreshes chrome while the POST is delayed.
    editor.model.rev += 1;
    editor.model.dirty = true;
    editor.updateChrome();
    assert.equal(editor.savePending, true, 'refresh keeps the in-flight transaction locked');
    assert.equal(await ProcessTemplateEditor.prototype.save.call(editor), false);
    assert.equal(fetches, 1, 'duplicate click does not issue a second POST');

    response.resolve({
      ok: true, status: 201, statusText: 'Created',
      json: async () => ({ sourceHash: 'source-alpha', semanticHash: 'semantic-alpha', diagnostics: [] }),
    });
    assert.equal(await first, true);
    assert.equal(editor.model.template.id, 'alpha');
    assert.equal(editor.model.sourceHash, 'source-alpha');
    assert.equal(editor.savePending, false);
  } finally {
    if (previousFetch === undefined) delete globalThis.fetch;
    else globalThis.fetch = previousFetch;
  }
});

test('stale save completion cannot overwrite a newer request generation', async () => {
  const previousFetch = globalThis.fetch;
  const alpha = deferred();
  const beta = deferred();
  globalThis.fetch = url => String(url).endsWith('/alpha') ? alpha.promise : beta.promise;
  try {
    const editor = saveEditor('alpha');
    const first = ProcessTemplateEditor.prototype.save.call(editor);

    // Simulate a newer editor lifecycle generation taking ownership. Public
    // duplicate saves cannot do this (covered above), but stale completions
    // must still be harmless if the generation changes.
    editor.savePending = false;
    editor.model.template.id = 'beta';
    const second = ProcessTemplateEditor.prototype.save.call(editor);
    beta.resolve({
      ok: true, status: 201, statusText: 'Created',
      json: async () => ({ sourceHash: 'source-beta', semanticHash: 'semantic-beta', diagnostics: [] }),
    });
    assert.equal(await second, true);
    alpha.resolve({
      ok: true, status: 201, statusText: 'Created',
      json: async () => ({ sourceHash: 'source-alpha', semanticHash: 'semantic-alpha', diagnostics: [] }),
    });
    await first;
    assert.equal(editor.model.template.id, 'beta');
    assert.equal(editor.model.sourceHash, 'source-beta');
  } finally {
    if (previousFetch === undefined) delete globalThis.fetch;
    else globalThis.fetch = previousFetch;
  }
});

test('destroy invalidates a delayed save completion and callbacks', async () => {
  const previousFetch = globalThis.fetch;
  const response = deferred();
  globalThis.fetch = () => response.promise;
  try {
    let savedCallbacks = 0;
    const editor = saveEditor('alpha');
    editor.options.onSaved = () => { savedCallbacks += 1; };
    const pending = ProcessTemplateEditor.prototype.save.call(editor);
    ProcessTemplateEditor.prototype.destroy.call(editor);
    assert.equal(editor.savePending, false);

    response.resolve({
      ok: true, status: 201, statusText: 'Created',
      json: async () => ({ sourceHash: 'stale-source', semanticHash: 'stale-semantic', diagnostics: [] }),
    });
    await pending;
    assert.equal(editor.model.sourceHash, '', 'destroyed editor ignores delayed response state');
    assert.equal(savedCallbacks, 0, 'destroyed editor emits no saved callback');
    assert.equal(editor.lastStatus, undefined, 'destroyed editor emits no completion status');
  } finally {
    if (previousFetch === undefined) delete globalThis.fetch;
    else globalThis.fetch = previousFetch;
  }
});

function conflictResponse() {
  return {
    ok: false, status: 409, statusText: 'Conflict',
    json: async () => ({
      code: 'process_template_conflict', error: 'head moved',
      currentSourceHash: 'existing-source', currentRef: 'alpha@sha256:existing',
    }),
  };
}

test('destroy while conflict choice is pending prevents force retry', async () => {
  const previousFetch = globalThis.fetch;
  let fetches = 0;
  globalThis.fetch = async () => { fetches += 1; return conflictResponse(); };
  try {
    const choice = deferred();
    const choiceStarted = deferred();
    const editor = saveEditor('alpha');
    editor.choiceModal = () => { choiceStarted.resolve(); return choice.promise; };
    const pending = ProcessTemplateEditor.prototype.save.call(editor);
    await choiceStarted.promise;
    ProcessTemplateEditor.prototype.destroy.call(editor);
    choice.resolve('force');
    await pending;
    assert.equal(fetches, 1, 'stale force choice cannot start a retry POST');
    assert.equal(editor.model.sourceHash, '', 'stale force choice cannot adopt a CAS head');
  } finally {
    if (previousFetch === undefined) delete globalThis.fetch;
    else globalThis.fetch = previousFetch;
  }
});

test('failed force retry keeps an untouched blank editor retryable', async () => {
  const previousFetch = globalThis.fetch;
  let fetches = 0;
  globalThis.fetch = async () => { fetches += 1; return conflictResponse(); };
  try {
    const choices = ['force', null];
    const editor = saveEditor('alpha');
    editor.model.dirty = false;
    editor.choiceModal = async () => choices.shift();

    assert.equal(await ProcessTemplateEditor.prototype.save.call(editor), true);
    assert.equal(fetches, 2, 'force retries once against the adopted CAS head');
    assert.equal(editor.blank, true, 'a failed retry does not pretend the draft was saved');
    assert.equal(editor.model.sourceHash, 'existing-source', 'the adopted CAS head stays pinned');
    assert.equal(editor.model.sourceHash, 'existing-source', 'adopting a CAS head keeps identity locked');
    assert.equal(editor.savePending, false, 'cancelled re-conflict leaves Save available');
  } finally {
    if (previousFetch === undefined) delete globalThis.fetch;
    else globalThis.fetch = previousFetch;
  }
});

test('destroy while conflict reload is pending prevents model swap and refresh', async () => {
  const previousFetch = globalThis.fetch;
  const reload = deferred();
  const reloadStarted = deferred();
  let fetches = 0;
  globalThis.fetch = async (_url, options) => {
    fetches += 1;
    if (options?.method === 'POST') return conflictResponse();
    reloadStarted.resolve();
    return reload.promise;
  };
  try {
    const originalModel = saveEditor('alpha').model;
    const editor = saveEditor('alpha');
    editor.model = originalModel;
    editor.choiceModal = async () => 'reload';
    let refreshes = 0;
    editor.refresh = () => { refreshes += 1; };
    const pending = ProcessTemplateEditor.prototype.save.call(editor);
    await reloadStarted.promise;
    ProcessTemplateEditor.prototype.destroy.call(editor);
    reload.resolve({
      ok: true, status: 200, statusText: 'OK',
      json: async () => ({
        template: { id: 'alpha', name: 'Their head', nodes: {} }, edges: [], layout: {},
        sourceHash: 'reloaded-source', semanticHash: 'reloaded-semantic',
      }),
    });
    await pending;
    assert.equal(fetches, 2);
    assert.equal(editor.model, originalModel, 'stale reload cannot replace the destroyed editor model');
    assert.equal(refreshes, 0, 'stale reload cannot refresh destroyed editor DOM');
    assert.equal(editor.lastStatus, undefined, 'stale reload emits no completion status');
  } finally {
    if (previousFetch === undefined) delete globalThis.fetch;
    else globalThis.fetch = previousFetch;
  }
});

function externalReloadEditor({ dirty = false, confirmDiscard = async () => true } = {}) {
  const model = new ProcessEditModel({
    template: { id: 'alpha', name: 'Loaded', start: 'a', nodes: { a: { type: 'start' }, gone: { type: 'task' } } },
    edges: [{ from: '', outcome: 'start', to: 'a' }], layout: {},
    sourceHash: 'source-old', semanticHash: 'semantic-old', currentRef: 'alpha@sha256:old',
  });
  if (dirty) model.setTemplateMeta({ name: 'Local draft' });
  const editor = {
    model, blank: false, selection: { type: 'node', id: 'gone' },
    externalChange: { kind: dirty ? 'dirty' : 'clean', ref: 'alpha@sha256:new', sourceHash: 'source-new' },
    externalDecisionPending: false, externalReloadPending: false, externalReloadSeq: 0, savePending: false,
    externalReviewSeq: 0, externalReviewPending: false, externalReviewRequest: null,
    abort: new AbortController(), options: { confirmDiscard }, modalDispose: null, inlineCommit: null,
    validation: null,
    renderExternalChange() {},
    updateChrome() {},
    retainLiveSelection() { return ProcessTemplateEditor.prototype.retainLiveSelection.call(this); },
    refresh(options) { this.refreshOptions = options; },
    status(message, isError) { this.lastStatus = { message, isError }; },
  };
  Object.defineProperty(editor, 'dirty', { get() { return this.model.dirty || !!this.modalDispose?.isDirty?.(); } });
  return editor;
}

function externalReviewResponse(ref, sourceHash, name = 'External') {
  return {
    ok: true, status: 200, statusText: 'OK',
    json: async () => ({
      template: { id: 'alpha', name, start: 'a', nodes: { a: { type: 'start' } } },
      edges: [{ from: '', outcome: 'start', to: 'a' }], layout: {},
      currentRef: ref, sourceHash, semanticHash: ref.split(':').at(-1), source: `id: alpha\nname: ${name}\n`,
    }),
  };
}

test('repeated head polls single-flight a hung external review and a newer head aborts it', async () => {
  const previousFetch = globalThis.fetch;
  const requests = [];
  globalThis.fetch = (_path, options) => {
    const response = deferred();
    requests.push({ response, signal: options.signal });
    return response.promise;
  };
  try {
    const editor = externalReloadEditor();
    editor.loadedView = { template: structuredClone(editor.model.template), edges: structuredClone(editor.model.edges), source: 'id: alpha\n' };
    editor.externalChange = { kind: 'none', ref: '' };
    editor.loadExternalReview = () => ProcessTemplateEditor.prototype.loadExternalReview.call(editor);

    const oldHead = { ref: 'alpha@sha256:first', sourceHash: 'source-first' };
    for (let i = 0; i < 25; i += 1) ProcessTemplateEditor.prototype.observeExternalHead.call(editor, oldHead);
    assert.equal(requests.length, 1, 'one exact generation owns at most one in-flight review');
    const stale = editor.externalReviewRequest.promise;

    ProcessTemplateEditor.prototype.observeExternalHead.call(editor, {
      ref: 'alpha@sha256:second', sourceHash: 'source-second',
    });
    assert.equal(requests.length, 2, 'the newer exact generation starts its own review');
    assert.equal(requests[0].signal.aborted, true, 'the superseded generation is actively cancelled');
    const current = editor.externalReviewRequest.promise;

    requests[0].response.resolve(externalReviewResponse(oldHead.ref, oldHead.sourceHash, 'Stale'));
    assert.equal(await stale, false);
    assert.equal(editor.externalChange.ref, 'alpha@sha256:second');
    assert.equal(editor.externalChange.review, undefined, 'an abort-ignoring stale completion cannot install review state');

    requests[1].response.resolve(externalReviewResponse('alpha@sha256:second', 'source-second', 'Current'));
    assert.equal(await current, true);
    assert.ok(editor.externalChange.review);
    assert.equal(editor.externalReviewPending, false);
  } finally {
    if (previousFetch === undefined) delete globalThis.fetch;
    else globalThis.fetch = previousFetch;
  }
});

test('editor teardown aborts an in-flight external review', async () => {
  const previousFetch = globalThis.fetch;
  let requestSignal;
  globalThis.fetch = (_path, options) => new Promise((_resolve, reject) => {
    requestSignal = options.signal;
    requestSignal.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')), { once: true });
  });
  try {
    const editor = externalReloadEditor();
    editor.loadedView = { template: structuredClone(editor.model.template), edges: [], source: 'id: alpha\n' };
    editor.externalChange = { kind: 'clean', ref: 'alpha@sha256:new', sourceHash: 'source-new' };
    editor.nodeChooserDispose = null; editor.closeInline = () => {}; editor.validation = null;
    editor.graph = { dispose() {} }; editor.modalDispose = null;
    editor.mount = { classList: { remove() {} }, replaceChildren() {}, __processEditor: editor };
    const pending = ProcessTemplateEditor.prototype.loadExternalReview.call(editor);
    ProcessTemplateEditor.prototype.destroy.call(editor);
    assert.equal(requestSignal.aborted, true);
    assert.equal(await pending, false);
    assert.equal(editor.externalReviewPending, false);
  } finally {
    if (previousFetch === undefined) delete globalThis.fetch;
    else globalThis.fetch = previousFetch;
  }
});

test('a hung external review is cancelled at the bounded timeout', async () => {
  const previousFetch = globalThis.fetch;
  let requestSignal;
  globalThis.fetch = (_path, options) => new Promise((_resolve, reject) => {
    requestSignal = options.signal;
    requestSignal.addEventListener('abort', () => reject(new DOMException('timed out', 'AbortError')), { once: true });
  });
  try {
    const editor = externalReloadEditor();
    editor.options.externalReviewTimeoutMs = 5;
    editor.loadedView = { template: structuredClone(editor.model.template), edges: [], source: 'id: alpha\n' };
    assert.equal(await ProcessTemplateEditor.prototype.loadExternalReview.call(editor), false);
    assert.equal(requestSignal.aborted, true);
    assert.equal(editor.externalReviewPending, false);
    assert.equal(editor.lastStatus, undefined, 'expected cancellation is not reported as a failed review');
  } finally {
    if (previousFetch === undefined) delete globalThis.fetch;
    else globalThis.fetch = previousFetch;
  }
});

test('a stale review GET cannot clear or reorder the polled external generation', async () => {
  const previousFetch = globalThis.fetch;
  let requested = '';
  globalThis.fetch = async (path) => {
    requested = path;
    return ({
    ok: true, status: 200, statusText: 'OK',
    json: async () => ({
      template: { id: 'alpha', nodes: { a: { type: 'start' } } }, edges: [], layout: {},
      currentRef: 'alpha@sha256:old', sourceHash: 'source-old', semanticHash: 'old', source: 'id: alpha\n',
    }),
    });
  };
  try {
    const editor = externalReloadEditor();
    editor.loadedView = { template: structuredClone(editor.model.template), edges: [], source: 'id: alpha\n' };
    editor.externalReviewSeq = 0; editor.externalReviewPending = false;
    assert.equal(await ProcessTemplateEditor.prototype.loadExternalReview.call(editor), false);
    assert.deepEqual(editor.externalChange, { kind: 'clean', ref: 'alpha@sha256:new', sourceHash: 'source-new' });
    assert.equal(editor.externalReviewPending, false);
    assert.match(requested, /[?&]authorship=omit(?:&|$)/, 'automatic review never requests append-only history');
  } finally {
    if (previousFetch === undefined) delete globalThis.fetch;
    else globalThis.fetch = previousFetch;
  }
});

test('a changed head automatically loads bounded review with exact head attribution', async () => {
  const previousFetch = globalThis.fetch;
  let requested = '';
  globalThis.fetch = async (path) => {
    requested = path;
    return {
      ok: true, status: 200, statusText: 'OK',
      json: async () => ({
        template: { id: 'alpha', name: 'External', start: 'a', nodes: { a: { type: 'start' } } },
        edges: [{ from: '', outcome: 'start', to: 'a' }], layout: { nodes: { a: { x: 40, y: 50 } } },
        currentRef: 'alpha@sha256:new', sourceHash: 'source-new', semanticHash: 'semantic-new',
        source: 'id: alpha\nname: External\n', actor: 'agent:agt_exact', authoredAt: '2026-07-15T08:00:00Z',
      }),
    };
  };
  try {
    const editor = externalReloadEditor();
    editor.loadedView = { template: structuredClone(editor.model.template), edges: structuredClone(editor.model.edges), source: 'id: alpha\n' };
    editor.externalChange = { kind: 'none', ref: '' };
    editor.externalReviewSeq = 0; editor.externalReviewPending = false;
    let reviewPromise;
    editor.loadExternalReview = () => {
      reviewPromise = ProcessTemplateEditor.prototype.loadExternalReview.call(editor);
      return reviewPromise;
    };

    ProcessTemplateEditor.prototype.observeExternalHead.call(editor, {
      ref: 'alpha@sha256:new', sourceHash: 'source-new', actor: 'agent:agt_polled', authoredAt: '2026-07-15T07:59:00Z',
    });
    assert.ok(reviewPromise, 'a changed polled head starts automatic review');
    assert.equal(await reviewPromise, true);
    assert.match(requested, /[?&]authorship=omit(?:&|$)/);
    assert.equal(editor.externalChange.actor, 'agent:agt_exact');
    assert.equal(editor.externalChange.authoredAt, '2026-07-15T08:00:00Z');
    assert.ok(editor.externalChange.review, 'bounded response still exposes exact external semantics');
  } finally {
    if (previousFetch === undefined) delete globalThis.fetch;
    else globalThis.fetch = previousFetch;
  }
});

test('dirty external Reload never fetches or replaces the model when discard is denied', async () => {
  const previousFetch = globalThis.fetch;
  let fetches = 0; let prompts = 0;
  globalThis.fetch = async () => { fetches += 1; throw new Error('must not fetch'); };
  try {
    const editor = externalReloadEditor({ dirty: true, confirmDiscard: async () => { prompts += 1; return false; } });
    const original = editor.model;
    assert.equal(await ProcessTemplateEditor.prototype.reloadExternalChange.call(editor), false);
    assert.equal(prompts, 1);
    assert.equal(fetches, 0);
    assert.equal(editor.model, original);
    assert.deepEqual(editor.externalChange, { kind: 'dirty', ref: 'alpha@sha256:new', sourceHash: 'source-new' });
  } finally {
    if (previousFetch === undefined) delete globalThis.fetch;
    else globalThis.fetch = previousFetch;
  }
});

test('a model mutation while discard confirmation is pending cancels external Reload', async () => {
  const previousFetch = globalThis.fetch;
  const confirmation = deferred(); let fetches = 0;
  globalThis.fetch = async () => { fetches += 1; throw new Error('stale confirmation must not fetch'); };
  try {
    const editor = externalReloadEditor({ dirty: true, confirmDiscard: () => confirmation.promise });
    const original = editor.model;
    const pending = ProcessTemplateEditor.prototype.reloadExternalChange.call(editor);
    assert.equal(editor.externalDecisionPending, true);
    assert.equal(ProcessTemplateEditor.prototype.mutate.call(editor, () => original.setTemplateMeta({ doc: 'blocked' })), undefined,
      'normal editor mutations are locked while the decision is pending');
    original.setTemplateMeta({ description: 'out-of-band local mutation' });
    confirmation.resolve(true);
    assert.equal(await pending, false);
    assert.equal(fetches, 0);
    assert.equal(editor.model, original);
    assert.equal(editor.model.template.description, 'out-of-band local mutation');
    assert.equal(editor.externalDecisionPending, false);
  } finally {
    if (previousFetch === undefined) delete globalThis.fetch;
    else globalThis.fetch = previousFetch;
  }
});

test('a model swap or completed save while discard confirmation is pending cancels external Reload', async () => {
  const previousFetch = globalThis.fetch;
  let fetches = 0;
  globalThis.fetch = async () => { fetches += 1; throw new Error('stale confirmation must not fetch'); };
  try {
    for (const scenario of ['model swap', 'completed save']) {
      const confirmation = deferred();
      const editor = externalReloadEditor({ dirty: true, confirmDiscard: () => confirmation.promise });
      const original = editor.model;
      const pending = ProcessTemplateEditor.prototype.reloadExternalChange.call(editor);
      if (scenario === 'model swap') {
        editor.model = externalReloadEditor().model;
      } else {
        original.markSaved({ ref: original.currentRef, sourceHash: 'source-saved', semanticHash: original.semanticHash });
      }
      confirmation.resolve(true);
      assert.equal(await pending, false, scenario);
      assert.equal(editor.externalDecisionPending, false, scenario);
    }
    assert.equal(fetches, 0);
  } finally {
    if (previousFetch === undefined) delete globalThis.fetch;
    else globalThis.fetch = previousFetch;
  }
});

test('a replacement modal while discard confirmation is pending is never closed', async () => {
  const previousFetch = globalThis.fetch;
  const confirmation = deferred(); let fetches = 0; let originalClosed = 0; let replacementClosed = 0;
  globalThis.fetch = async () => { fetches += 1; throw new Error('stale confirmation must not fetch'); };
  try {
    const editor = externalReloadEditor({ dirty: true, confirmDiscard: () => confirmation.promise });
    const originalModal = () => { originalClosed += 1; };
    originalModal.isDirty = () => true;
    editor.modalDispose = originalModal;
    const pending = ProcessTemplateEditor.prototype.reloadExternalChange.call(editor);
    const replacement = () => { replacementClosed += 1; };
    replacement.isDirty = () => true;
    editor.modalDispose = replacement;
    confirmation.resolve(true);
    assert.equal(await pending, false);
    assert.equal(fetches, 0);
    assert.equal(originalClosed, 0);
    assert.equal(replacementClosed, 0);
    assert.equal(editor.modalDispose, replacement);
  } finally {
    if (previousFetch === undefined) delete globalThis.fetch;
    else globalThis.fetch = previousFetch;
  }
});

test('a newer target head while discard confirmation is pending cancels the old Reload', async () => {
  const previousFetch = globalThis.fetch;
  const confirmation = deferred(); let fetches = 0;
  globalThis.fetch = async () => { fetches += 1; throw new Error('stale confirmation must not fetch'); };
  try {
    const editor = externalReloadEditor({ dirty: true, confirmDiscard: () => confirmation.promise });
    const pending = ProcessTemplateEditor.prototype.reloadExternalChange.call(editor);
    editor.externalChange = { kind: 'dirty', ref: 'alpha@sha256:newer', sourceHash: 'source-newer' };
    confirmation.resolve(true);
    assert.equal(await pending, false);
    assert.equal(fetches, 0);
    assert.deepEqual(editor.externalChange, { kind: 'dirty', ref: 'alpha@sha256:newer', sourceHash: 'source-newer' });
  } finally {
    if (previousFetch === undefined) delete globalThis.fetch;
    else globalThis.fetch = previousFetch;
  }
});

test('confirmed external Reload swaps in the new head without fitting the viewport', async () => {
  const previousFetch = globalThis.fetch;
  let prompts = 0; let modalClosed = 0;
  globalThis.fetch = async () => ({
    ok: true, status: 200, statusText: 'OK',
    json: async () => ({
      template: { id: 'alpha', name: 'External', start: 'a', nodes: { a: { type: 'start' } } },
      edges: [{ from: '', outcome: 'start', to: 'a' }], layout: {},
      sourceHash: 'source-new', semanticHash: 'semantic-new', currentRef: 'alpha@sha256:new',
    }),
  });
  try {
    const editor = externalReloadEditor({ dirty: true, confirmDiscard: async () => { prompts += 1; return true; } });
    let layeringResets = 0;
    editor.graph = { resetInteractionLayering() { layeringResets += 1; } };
    editor.selection = { type: 'multi', items: [{ type: 'node', id: 'a' }, { type: 'node', id: 'gone' }] };
    const dispose = () => { modalClosed += 1; editor.modalDispose = null; };
    dispose.isDirty = () => false;
    editor.modalDispose = dispose;
    assert.equal(await ProcessTemplateEditor.prototype.reloadExternalChange.call(editor), true);
    assert.equal(prompts, 1);
    assert.equal(modalClosed, 1);
    assert.equal(editor.model.template.name, 'External');
    assert.equal(editor.model.currentRef, 'alpha@sha256:new');
    assert.deepEqual(editor.selection, { type: 'node', id: 'a' }, 'surviving selected ids remain selected while removed ids drop');
    assert.equal(editor.refreshOptions, undefined, 'no fit request preserves the graph pan/zoom');
    assert.equal(editor.loadedView.currentRef, 'alpha@sha256:new', 'applied canonical view becomes the next review baseline');
    assert.equal(editor.externalChange.kind, 'none');
    assert.equal(layeringResets, 1,
      'whole-model replacement explicitly drops presentation layering before reused IDs render');
    assert.match(editor.lastStatus.message, /Reloaded external version/);
  } finally {
    if (previousFetch === undefined) delete globalThis.fetch;
    else globalThis.fetch = previousFetch;
  }
});

test('external Reload and Save are mutually exclusive while the GET is pending', async () => {
  const previousFetch = globalThis.fetch;
  const reload = deferred(); let fetches = 0;
  globalThis.fetch = async () => { fetches += 1; return reload.promise; };
  try {
    const editor = externalReloadEditor();
    const pending = ProcessTemplateEditor.prototype.reloadExternalChange.call(editor);
    assert.equal(editor.externalReloadPending, true);
    assert.equal(await ProcessTemplateEditor.prototype.save.call(editor), false);
    assert.equal(fetches, 1, 'Save cannot issue a POST behind the pending Reload GET');
    reload.resolve({
      ok: true, status: 200, statusText: 'OK',
      json: async () => ({
        template: { id: 'alpha', name: 'External', start: 'a', nodes: { a: { type: 'start' } } },
        edges: [], layout: {}, sourceHash: 'source-new', semanticHash: 'semantic-new', currentRef: 'alpha@sha256:new',
      }),
    });
    assert.equal(await pending, true);
  } finally {
    if (previousFetch === undefined) delete globalThis.fetch;
    else globalThis.fetch = previousFetch;
  }
});

test('an edit made during external Reload cancels the swap and preserves the draft/banner', async () => {
  const previousFetch = globalThis.fetch;
  const reload = deferred(); const started = deferred();
  globalThis.fetch = async () => { started.resolve(); return reload.promise; };
  try {
    const editor = externalReloadEditor({ dirty: true });
    const original = editor.model;
    const pending = ProcessTemplateEditor.prototype.reloadExternalChange.call(editor);
    await started.promise;
    original.setTemplateMeta({ description: 'new local edit during reload' });
    reload.resolve({
      ok: true, status: 200, statusText: 'OK',
      json: async () => ({
        template: { id: 'alpha', name: 'External', start: 'a', nodes: { a: { type: 'start' } } },
        edges: [], layout: {}, sourceHash: 'source-new', semanticHash: 'semantic-new', currentRef: 'alpha@sha256:new',
      }),
    });
    assert.equal(await pending, false);
    assert.equal(editor.model, original, 'revision advance fails closed instead of swapping the model');
    assert.equal(editor.model.template.description, 'new local edit during reload');
    assert.deepEqual(editor.externalChange, { kind: 'dirty', ref: 'alpha@sha256:new', sourceHash: 'source-new' });
    assert.match(editor.lastStatus.message, /Reload cancelled/);
  } finally {
    if (previousFetch === undefined) delete globalThis.fetch;
    else globalThis.fetch = previousFetch;
  }
});

test('a node drag begun during external Reload cancels the stale model swap', async () => {
  const previousFetch = globalThis.fetch;
  const reload = deferred(); const started = deferred();
  globalThis.fetch = async () => { started.resolve(); return reload.promise; };
  try {
    const editor = externalReloadEditor();
    const original = editor.model;
    let generation = 0;
    let active = false;
    editor.graph = {
      interactionSnapshot: () => ({ generation, active }),
    };
    const pending = ProcessTemplateEditor.prototype.reloadExternalChange.call(editor);
    await started.promise;
    generation += 1;
    active = true; // the adapter observed a node pointer-down while GET was pending
    generation += 1;
    active = false; // even a completed drag invalidates the request generation
    reload.resolve({
      ok: true, status: 200, statusText: 'OK',
      json: async () => ({
        template: { id: 'alpha', name: 'External', start: 'a', nodes: { a: { type: 'start' } } },
        edges: [], layout: {}, sourceHash: 'source-new', semanticHash: 'semantic-new', currentRef: 'alpha@sha256:new',
      }),
    });
    assert.equal(await pending, false);
    assert.equal(editor.model, original, 'a response captured before the drag cannot replace its graph model');
    assert.match(editor.lastStatus.message, /Reload cancelled/);
  } finally {
    if (previousFetch === undefined) delete globalThis.fetch;
    else globalThis.fetch = previousFetch;
  }
});

test('a newer polled head during external Reload cancels the older response', async () => {
  const previousFetch = globalThis.fetch;
  const reload = deferred();
  globalThis.fetch = async () => reload.promise;
  try {
    const editor = externalReloadEditor();
    const original = editor.model;
    const pending = ProcessTemplateEditor.prototype.reloadExternalChange.call(editor);
    editor.externalChange = { kind: 'clean', ref: 'alpha@sha256:newer', sourceHash: 'source-newer' };
    reload.resolve({
      ok: true, status: 200, statusText: 'OK',
      json: async () => ({
        template: { id: 'alpha', name: 'Old response', start: 'a', nodes: { a: { type: 'start' } } },
        edges: [], layout: {}, sourceHash: 'source-new', semanticHash: 'semantic-new', currentRef: 'alpha@sha256:new',
      }),
    });
    assert.equal(await pending, false);
    assert.equal(editor.model, original, 'the older exact response cannot replace the editor model');
    assert.deepEqual(editor.externalChange, { kind: 'clean', ref: 'alpha@sha256:newer', sourceHash: 'source-newer' });
    assert.match(editor.lastStatus.message, /newer external version/);
  } finally {
    if (previousFetch === undefined) delete globalThis.fetch;
    else globalThis.fetch = previousFetch;
  }
});

test('a new dialog draft during external Reload cancels the swap', async () => {
  const previousFetch = globalThis.fetch;
  const reload = deferred();
  globalThis.fetch = async () => reload.promise;
  try {
    const editor = externalReloadEditor();
    const original = editor.model;
    const pending = ProcessTemplateEditor.prototype.reloadExternalChange.call(editor);
    const draft = () => { editor.modalDispose = null; };
    draft.isDirty = () => true;
    editor.modalDispose = draft;
    reload.resolve({
      ok: true, status: 200, statusText: 'OK',
      json: async () => ({
        template: { id: 'alpha', name: 'External', start: 'a', nodes: { a: { type: 'start' } } },
        edges: [], layout: {}, sourceHash: 'source-new', semanticHash: 'semantic-new', currentRef: 'alpha@sha256:new',
      }),
    });
    assert.equal(await pending, false);
    assert.equal(editor.model, original);
    assert.equal(editor.modalDispose, draft, 'the new dialog draft remains owned by the old model');
    assert.deepEqual(editor.externalChange, { kind: 'clean', ref: 'alpha@sha256:new', sourceHash: 'source-new' });
  } finally {
    if (previousFetch === undefined) delete globalThis.fetch;
    else globalThis.fetch = previousFetch;
  }
});

// ---- TCL-530: controller command hardening after destroy ---------------------

// A real-prototype editor with just enough state for destroy() and every
// public command method to run. Object.create keeps the class getters
// (dirty) and lets each test call methods directly instead of through
// ProcessTemplateEditor.prototype.<name>.call.
function destroyableEditor() {
  const editor = Object.create(ProcessTemplateEditor.prototype);
  Object.assign(editor, {
    destroyed: false,
    model: new ProcessEditModel({
      template: { id: 'alpha', name: 'Loaded', start: 'a', nodes: { a: { type: 'start' }, b: { type: 'task' } } },
      edges: [{ from: '', outcome: 'start', to: 'a' }, { from: 'a', outcome: 'pass', to: 'b' }],
      layout: { nodes: { a: { x: 0, y: 0 }, b: { x: 0, y: 80 } } },
      sourceHash: 'source-old', semanticHash: 'semantic-old', currentRef: 'alpha@sha256:old',
    }),
    loadedView: null, blank: false,
    selection: { type: 'node', id: 'b' },
    band: null, nodeChooserDispose: null,
    savePending: false, saveSeq: 0,
    externalReloadPending: false, externalDecisionPending: false, externalReloadSeq: 0,
    externalReviewSeq: 0, externalReviewPending: false, externalReviewRequest: null,
    externalChange: { kind: 'clean', ref: 'alpha@sha256:new', sourceHash: 'source-new' },
    externalReviewOpen: false,
    paletteHidden: false,
    customSnippets: [{ id: 'snip', name: 'Snip', available: true, payload: { nodes: [] } }],
    snippetLibrary: { loading: false, error: '', generation: 1, pendingID: '', creating: false },
    snippetLoadSeq: 0,
    statusState: { message: '', error: false },
    inlineState: { open: false, token: 0, left: 0, top: 0, value: '' },
    inlineCommit: null, inspectorFocusRequest: 0,
    modalState: null, modalGeneration: 0, modalHandle: null, modalDispose: null,
    pasteFingerprint: '', pasteRepeat: 0, pasteAnchor: null, canvasPointer: null,
    abort: new AbortController(),
    graph: { dispose() {} },
    validation: null,
    snapshotSignal: { value: 'initial' },
    uiCleanup: null,
    options: {},
    mount: { classList: { add() {}, remove() {} } },
  });
  editor.mount.__processEditor = editor;
  return editor;
}

function failingFetch(name) {
  return () => { throw new Error(`${name} must not start network work after destroy`); };
}

test('synchronous commands are inert and cannot mutate state after repeated destroy', () => {
  const previousFetch = globalThis.fetch;
  globalThis.fetch = failingFetch('sync command');
  try {
    const editor = destroyableEditor();
    editor.destroy();
    editor.destroy(); // repeated destroy stays a safe no-op
    assert.equal(editor.destroyed, true);
    assert.equal(editor.graph, null);

    const before = editor.model.saveBody();
    const revBefore = editor.model.rev;
    const selectionBefore = editor.selection;
    const externalBefore = editor.externalChange;

    assert.equal(editor.addNodeType('task'), false, 'default placement must survive the disposed graph');
    assert.equal(editor.editSelection(), false);
    assert.equal(editor.duplicateSelection(), false);
    assert.equal(editor.selectAll(), false);
    assert.equal(editor.clearSelection(), false);
    assert.equal(editor.fitGraph(), false);
    assert.equal(editor.centerSelection(), false);
    assert.equal(editor.zoomGraph(1.2), false);
    assert.equal(editor.resetZoom(), false);
    assert.equal(editor.validateNow(), false);
    assert.equal(editor.focusIssue(1), false);
    assert.equal(editor.setTemplateID('renamed'), false);
    assert.equal(editor.model.template.id, 'alpha');
    assert.equal(editor.setTemplateMeta({ name: 'renamed' }), undefined);
    assert.equal(editor.renameNode('b', 'renamed'), undefined);
    assert.equal(editor.mutate(() => editor.model.setTemplateMeta({ doc: 'late' })), undefined);
    assert.equal(editor.applyHistory('undo'), false);
    assert.equal(editor.insertPaletteItem({ kind: 'primitive', type: 'task' }, { x: 1, y: 2 }), false);
    assert.equal(editor.insertCustomSnippet('snip'), false);
    assert.equal(editor.toggleExternalReview(), false);
    assert.equal(editor.keepExternalChange(), false);
    assert.equal(editor.observeExternalHead({ ref: 'alpha@sha256:next', sourceHash: 'source-next' }), externalBefore,
      'a stale head poll cannot restart review work on a destroyed editor');
    assert.equal(editor.externalChange, externalBefore);
    assert.equal(editor.openInline(5, 6, 'label', () => assert.fail('inline commit must not attach')), false);
    assert.equal(editor.inlineState.open, false);
    editor.togglePalette();
    assert.equal(editor.paletteHidden, false);
    editor.openCommands(); // must not reach the global command palette
    editor.openExternalActor();
    editor.status('late status', true);
    assert.deepEqual(editor.statusState, { message: '', error: false });
    editor.setSelection({ type: 'node', id: 'a' });
    assert.equal(editor.selection, selectionBefore);
    editor.attachGraphHost({});
    assert.equal(editor.graph, null, 'a destroyed editor cannot mint a new graph adapter');
    editor.publish();
    assert.equal(editor.snapshotSignal.value, 'initial', 'destroyed editor publishes no snapshots');
    editor.refresh();

    const context = editor.commandContext();
    for (const flag of ['hasGraph', 'hasSelection', 'hasGraphSelection', 'canCreate', 'canEdit',
      'canDuplicate', 'canDelete', 'canValidate', 'canSave', 'canInstantiate', 'hasCurrentIssue']) {
      assert.equal(context[flag], false, `destroyed context must disable ${flag}`);
    }
    assert.equal(context.issueCount, 0);

    assert.deepEqual(editor.model.saveBody(), before, 'no command may mutate the model after destroy');
    assert.equal(editor.model.rev, revBefore);
    assert.equal(editor.model.canUndo, false);
  } finally {
    if (previousFetch === undefined) delete globalThis.fetch;
    else globalThis.fetch = previousFetch;
  }
});

test('asynchronous commands cannot start network, modal, or callback work after destroy', async () => {
  const previousFetch = globalThis.fetch;
  let fetches = 0;
  globalThis.fetch = () => { fetches += 1; throw new Error('must not fetch'); };
  try {
    const editor = destroyableEditor();
    let outward = 0;
    editor.options.onScribe = async () => { outward += 1; return {}; };
    editor.options.onInstantiate = () => { outward += 1; };
    editor.options.confirmDiscard = async () => { outward += 1; return true; };
    editor.destroy();
    editor.destroy();
    const before = editor.model.saveBody();

    assert.equal(await editor.save(), false);
    assert.equal(editor.savePending, false);
    assert.equal(await editor.requestScribe('template'), false);
    assert.equal(await editor.requestInstantiate(), false,
      'a clean saved model must not reach onInstantiate after destroy');
    assert.equal(await editor.deleteSelection(), false);
    assert.equal(await editor.openNodeSettings('b'), false);
    assert.equal(await editor.openParamsSettings(), false);
    assert.equal(await editor.saveSelectionAsSnippet(), false);
    assert.equal(await editor.renameCustomSnippet('snip'), false);
    assert.equal(await editor.deleteCustomSnippet('snip'), false);
    assert.equal(await editor.reloadExternalChange(), false);
    assert.equal(editor.externalDecisionPending, false);
    assert.equal(editor.externalReloadPending, false);

    // Modal requests resolve immediately as cancelled instead of hanging on
    // UI that can no longer render, and never touch modal state.
    let resolved = 'unresolved';
    const dispose = editor.openModal({ kind: 'choice' }, (value) => { resolved = value; });
    assert.equal(resolved, null);
    assert.equal(await dispose.requestClose(), true);
    assert.equal(dispose.isDirty(), false);
    assert.equal(await editor.choiceModal({ title: 't', body: 'b', choices: [{ key: 'k', label: 'l' }] }), null);
    assert.equal(await editor.nameSnippetModal({ title: 't', submitLabel: 's' }), null);
    assert.equal(await editor.scribePreviewModal({ kind: 'template', prompt: '', context: '' }), null);
    assert.equal(editor.modalState, null);
    assert.equal(editor.modalGeneration, 0);
    assert.equal(editor.modalDispose, null);

    assert.equal(fetches, 0, 'no async command may start a request after destroy');
    assert.equal(outward, 0, 'no async command may invoke outward callbacks after destroy');
    assert.deepEqual(editor.model.saveBody(), before);
  } finally {
    if (previousFetch === undefined) delete globalThis.fetch;
    else globalThis.fetch = previousFetch;
  }
});

test('every palette command built from a destroyed editor is disabled and inert to run', async () => {
  const previousFetch = globalThis.fetch;
  globalThis.fetch = failingFetch('registry command');
  try {
    const editor = destroyableEditor();
    editor.options.onScribe = async () => assert.fail('scribe handoff must not start');
    editor.options.onInstantiate = () => assert.fail('instantiation must not start');
    editor.destroy();
    const before = editor.model.saveBody();

    const commands = buildProcessEditorCommands({ editor, wizard: false });
    assert.ok(commands.length > 0);
    const byID = Object.fromEntries(commands.map((command) => [command.id, command]));
    for (const id of ['process.create.task', 'process.edit-selection', 'process.duplicate-selection',
      'process.delete-selection', 'process.select-all', 'process.clear-selection', 'process.fit',
      'process.center', 'process.validate', 'process.next-issue', 'process.previous-issue',
      'process.scribe-selection', 'process.scribe-diagnostic', 'process.save', 'process.instantiate']) {
      assert.equal(byID[id].enabled, false, `${id} must be disabled on a destroyed editor`);
    }
    // Even the always-enabled commands (zoom, whole-template scribe) and any
    // stale run handler a client may still hold must be harmless.
    for (const command of commands) await command.run();
    assert.deepEqual(editor.model.saveBody(), before);
    assert.equal(editor.modalState, null);
    assert.equal(editor.graph, null);
  } finally {
    if (previousFetch === undefined) delete globalThis.fetch;
    else globalThis.fetch = previousFetch;
  }
});
