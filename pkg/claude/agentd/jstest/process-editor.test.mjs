import test from 'node:test';
import assert from 'node:assert/strict';
import { ProcessTemplateEditor, isProcessEditorFormControl } from '../dashboard/js/process-editor.js';

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
