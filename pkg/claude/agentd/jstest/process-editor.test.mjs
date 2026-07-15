import test from 'node:test';
import assert from 'node:assert/strict';
import { ProcessTemplateEditor, isProcessEditorFormControl } from '../dashboard/js/process-editor.js';
import { ProcessEditModel } from '../dashboard/js/process-edit-model.js';
import { LiveValidation } from '../dashboard/js/process-validation.js';

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
    removeBand() {},
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
  assert.deepEqual(statuses, [['end node must not have outgoing edges', true]]);
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

test('cancelling the scribe preview restores predictable editor focus', async () => {
  let focused = 0;
  const fake = {
    blank: false, dirty: false, savePending: false, selection: null,
    model: { template: { id: 'release-flow', nodes: {} }, edges: [], currentRef: `release-flow@sha256:${'a'.repeat(64)}`, sourceHash: 'b'.repeat(64) },
    validation: null, abort: { signal: { aborted: false } },
    graph: { root: { focus: () => { focused += 1; } } },
    scribePreviewModal: async () => null,
    options: { onScribe: async () => { throw new Error('cancel must not send'); } },
  };
  assert.equal(await ProcessTemplateEditor.prototype.requestScribe.call(fake), false);
  assert.equal(focused, 1);
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
    editor.graph = { destroy() {} };
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

function withFakeDocument(run) {
  const previous = globalThis.document;
  globalThis.document = {
    createElement(tag) {
      return {
        tagName: String(tag).toUpperCase(), attributes: {}, children: [],
        setAttribute(key, value) { this.attributes[key] = String(value); },
        addEventListener() {},
        append(...children) { this.children.push(...children); },
      };
    },
  };
  try {
    return run();
  } finally {
    if (previous === undefined) delete globalThis.document;
    else globalThis.document = previous;
  }
}

test('template settings selection stays editor-owned and renders the display name', () => {
  withFakeDocument(() => {
    let graphSelection = 'not-cleared';
    let rendered = [];
    const fake = {
      selection: null,
      graph: { select(value) { graphSelection = value; } },
      model: { template: { id: 'release', name: 'Release train', description: 'Ship safely' } },
      inspector: { replaceChildren(...children) { rendered = children; } },
      renderInspector: ProcessTemplateEditor.prototype.renderInspector,
    };

    ProcessTemplateEditor.prototype.setSelection.call(fake, { type: 'template' });
    assert.deepEqual(fake.selection, { type: 'template' });
    assert.equal(graphSelection, null, 'template chrome never becomes a graph highlight');
    const name = rendered.find(element => element.attributes?.['aria-label'] === 'Template display name');
    assert.ok(name, 'settings button selection renders the display-name control');
    assert.equal(name.value, 'Release train');

    // refresh() replays setSelection(this.selection), so the editor-only state
    // must survive the same round trip without graph normalization dropping it.
    ProcessTemplateEditor.prototype.setSelection.call(fake, fake.selection);
    assert.deepEqual(fake.selection, { type: 'template' });
  });
});

test('graph multi-selection remains normalized and replaces template settings', () => {
  let graphSelection = null;
  let renders = 0;
  const fake = {
    selection: { type: 'template' },
    graph: { select(value) { graphSelection = value; }, layout: { edges: [] } },
    renderInspector() { renders += 1; },
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
    graph: { destroy() {} }, modalDispose: null,
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
    assert.equal(editor.idInput.disabled, true);
    assert.equal(editor.saveButton.disabled, true);

    // An allowed canvas edit refreshes chrome while the POST is delayed.
    editor.model.rev += 1;
    editor.model.dirty = true;
    editor.updateChrome();
    assert.equal(editor.idInput.disabled, true, 'refresh keeps the creation identity locked');
    assert.equal(editor.saveButton.disabled, true, 'refresh cannot arm a duplicate save');
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
    assert.equal(editor.idInput.disabled, true, 'adopting a CAS head keeps identity locked');
    assert.equal(editor.saveButton.disabled, false, 'cancelled re-conflict leaves Save available');
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

test('a stale review GET cannot clear or reorder the polled external generation', async () => {
  const previousFetch = globalThis.fetch;
  globalThis.fetch = async () => ({
    ok: true, status: 200, statusText: 'OK',
    json: async () => ({
      template: { id: 'alpha', nodes: { a: { type: 'start' } } }, edges: [], layout: {},
      currentRef: 'alpha@sha256:old', sourceHash: 'source-old', semanticHash: 'old', source: 'id: alpha\n',
    }),
  });
  try {
    const editor = externalReloadEditor();
    editor.loadedView = { template: structuredClone(editor.model.template), edges: [], source: 'id: alpha\n' };
    editor.externalReviewSeq = 0; editor.externalReviewPending = false;
    assert.equal(await ProcessTemplateEditor.prototype.loadExternalReview.call(editor), false);
    assert.deepEqual(editor.externalChange, { kind: 'clean', ref: 'alpha@sha256:new', sourceHash: 'source-new' });
    assert.equal(editor.externalReviewPending, false);
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
