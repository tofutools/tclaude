import test from 'node:test';
import assert from 'node:assert/strict';
import { createPreactHarness } from './preact-harness.mjs';

function view() {
  return {
    template: {
      id: 'dialog', start: 'work',
      nodes: {
        work: { type: 'start', name: 'Original' },
        done: { type: 'end', result: 'success' },
      },
    },
    edges: [{ from: 'work', outcome: 'pass', to: 'done' }],
    layout: { nodes: {} }, sourceHash: 'source', semanticHash: 'semantic',
  };
}

function taskView() {
  const performer = () => ({ kind: 'agent', profile: 'dev' });
  return {
    template: {
      id: 'checks', start: 'work',
      nodes: {
        work: {
          type: 'task', name: 'Work', performer: performer(),
          checks: [
            { id: 'lint', performer: performer() },
            { id: 'test', performer: performer() },
          ],
        },
        done: { type: 'end', result: 'success' },
      },
    },
    edges: [{ from: 'work', outcome: 'pass', to: 'done' }],
    layout: { nodes: {} }, sourceHash: 'source', semanticHash: 'semantic',
  };
}

function deletionView() {
  return {
    template: {
      id: 'deletion', start: 'begin',
      nodes: {
        begin: { type: 'start' },
        work: { type: 'task' },
        done: { type: 'end', result: 'success' },
      },
    },
    edges: [
      { from: '', outcome: 'start', to: 'begin' },
      { from: 'begin', outcome: 'pass', to: 'work' },
      { from: 'work', outcome: 'pass', to: 'done' },
    ],
    layout: { nodes: {} }, sourceHash: 'source', semanticHash: 'semantic',
  };
}

function deletionEditor(ProcessTemplateEditor, ProcessEditModel, selection) {
  const editor = {
    model: new ProcessEditModel(deletionView()), selection,
    modalDispose: null, abort: new AbortController(),
    externalDecisionPending: false, externalReloadPending: false,
    refresh() {}, status() {},
    setSelection(value) { this.selection = value; },
    choiceModal(options) {
      return ProcessTemplateEditor.prototype.choiceModal.call(this, options);
    },
    mutate(operation) {
      return ProcessTemplateEditor.prototype.mutate.call(this, operation);
    },
    deleteSelection() {
      this.pendingDeletion = ProcessTemplateEditor.prototype.deleteSelection.call(this);
      return this.pendingDeletion;
    },
  };
  return editor;
}

function pressDelete(harness, ProcessTemplateEditor, editor) {
  const event = harness.fireEvent(harness.document.body, 'keydown', { key: 'Delete' });
  ProcessTemplateEditor.prototype.onEditorKeyDown.call(editor, event);
  assert.equal(event.defaultPrevented, true);
}

function pressFocusedEnter(harness) {
  const focused = harness.document.activeElement;
  const keydown = harness.fireEvent(focused, 'keydown', { key: 'Enter' });
  // Browsers synthesize a click for Enter on a focused button; LinkeDOM does
  // not emulate that default action, so complete it explicitly after proving
  // which real DOM button owns focus.
  if (!keydown.defaultPrevented) harness.fireEvent(focused, 'click');
  harness.fireEvent(focused, 'keyup', { key: 'Enter' });
}

async function settle() {
  await Promise.resolve();
  await Promise.resolve();
}

async function waitForSelector(document, selector) {
  for (let attempt = 0; attempt < 50; attempt += 1) {
    const match = document.querySelector(selector);
    if (match) return match;
    await new Promise((resolve) => setTimeout(resolve, 0));
  }
  assert.fail(`timed out waiting for ${selector}`);
}

test('Delete then Enter confirms a simple selection deletion from the focused destructive action', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ ProcessEditModel }, { ProcessTemplateEditor }] = await Promise.all([
    harness.importDashboardModule('js/process-edit-model.js'),
    harness.importDashboardModule('js/process-editor.js'),
  ]);
  const editor = deletionEditor(ProcessTemplateEditor, ProcessEditModel, { type: 'node', id: 'done' });

  pressDelete(harness, ProcessTemplateEditor, editor);
  const overlay = await waitForSelector(harness.document, '.process-editor-modal');
  const destructive = harness.getByRole(overlay, 'button', { name: 'Delete selection' });
  assert.equal(harness.document.activeElement, destructive);
  assert.notEqual(harness.document.activeElement.textContent, 'Cancel');

  pressFocusedEnter(harness);
  await editor.pendingDeletion;
  assert.equal(editor.model.node('done'), undefined);
  assert.equal(editor.selection, null);
  assert.equal(harness.document.querySelector('.process-editor-modal'), null);
});

test('Delete then Enter keeps the primary rewire choice for a mid-graph node', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ ProcessEditModel }, { ProcessTemplateEditor }] = await Promise.all([
    harness.importDashboardModule('js/process-edit-model.js'),
    harness.importDashboardModule('js/process-editor.js'),
  ]);
  const editor = deletionEditor(ProcessTemplateEditor, ProcessEditModel, { type: 'node', id: 'work' });

  pressDelete(harness, ProcessTemplateEditor, editor);
  const overlay = await waitForSelector(harness.document, '.process-editor-modal');
  const rewire = harness.getByRole(overlay, 'button', { name: 'Delete + rewire through' });
  assert.equal(harness.document.activeElement, rewire);
  assert.match(rewire.className, /\bprimary\b/);

  pressFocusedEnter(harness);
  await editor.pendingDeletion;
  assert.equal(editor.model.node('work'), undefined);
  assert.deepEqual(editor.model.findEdge('begin', 'pass'), { from: 'begin', outcome: 'pass', to: 'done' });
  assert.equal(editor.selection, null);
});

test('selection deletion Escape, Cancel, and backdrop remain non-destructive', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ ProcessEditModel }, { ProcessTemplateEditor }] = await Promise.all([
    harness.importDashboardModule('js/process-edit-model.js'),
    harness.importDashboardModule('js/process-editor.js'),
  ]);
  for (const gesture of ['Escape', 'Cancel', 'backdrop']) {
    const selection = { type: 'node', id: 'done' };
    const editor = deletionEditor(ProcessTemplateEditor, ProcessEditModel, selection);
    pressDelete(harness, ProcessTemplateEditor, editor);
    const overlay = await waitForSelector(harness.document, '.process-editor-modal');
    if (gesture === 'Escape') harness.fireEvent(harness.document.activeElement, 'keydown', { key: 'Escape' });
    else if (gesture === 'Cancel') harness.fireEvent(harness.getByRole(overlay, 'button', { name: 'Cancel' }), 'click');
    else harness.fireEvent(overlay, 'mousedown');
    await editor.pendingDeletion;
    assert.ok(editor.model.node('done'), `${gesture} keeps the selected node`);
    assert.equal(editor.model.dirty, false, `${gesture} does not create an edit`);
    assert.deepEqual(editor.selection, selection, `${gesture} keeps the selection`);
  }
});

test('choice dialogs without an explicit or primary focus keep the existing Cancel default', async (t) => {
  const harness = await createPreactHarness(t);
  const { ProcessTemplateEditor } = await harness.importDashboardModule('js/process-editor.js');
  const editor = { modalDispose: null, abort: new AbortController() };
  const pending = ProcessTemplateEditor.prototype.choiceModal.call(editor, {
    title: 'Unrelated choice', body: 'No action is designated as the default.',
    choices: [{ key: 'continue', label: 'Continue' }],
  });
  const overlay = await waitForSelector(harness.document, '.process-editor-modal');
  assert.equal(harness.document.activeElement, harness.getByRole(overlay, 'button', { name: 'Cancel' }));
  harness.fireEvent(harness.document.activeElement, 'click');
  assert.equal(await pending, null);
});

test('diagnostic markers use one custom tooltip and keep accessible node and edge detail', async (t) => {
  const harness = await createPreactHarness(t);
  const { ProcessGraph } = await harness.importDashboardModule('js/process-graph.js');
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const nodeIssue = 'E_PERFORMER: Work performer is required';
  const edgeIssues = ['E_OUTCOME: pass is not declared', 'E_TARGET: done is unreachable'];
  const graph = new ProcessGraph(host, {
    nodes: [
      { id: 'work', type: 'task', overlay: { glyph: '!', severity: 'error', issues: [nodeIssue] } },
      { id: 'done', type: 'end' },
    ],
    edges: [{
      id: 'work:pass', from: 'work', to: 'done', outcome: 'pass',
      badge: '!', badgeSeverity: 'error', issues: edgeIssues,
    }],
  }, { fitOnRender: false });
  const node = host.querySelector('.process-node[data-node-id="work"]');
  assert.match(node.getAttribute('aria-label'), /E_PERFORMER: Work performer is required/);
  assert.equal(node.getAttribute('tabindex'), '0', 'keyboard focus reaches the described node');
  const nodeMarker = node.querySelector('.process-overlay-anchor');
  assert.equal(nodeMarker.hasAttribute('title'), false, 'the custom node target has no title attribute');
  assert.equal(nodeMarker.querySelector('title'), null, 'the custom node tooltip has no duplicate SVG title');
  assert.match(nodeMarker.querySelector('.process-overlay-tooltip text').textContent, /Work performer is required/);

  const edge = host.querySelector('.process-edge');
  assert.match(edge.getAttribute('aria-label'), /E_OUTCOME: pass is not declared/);
  assert.equal(edge.getAttribute('tabindex'), '0', 'keyboard focus reaches the described edge');
  const edgeMarker = edge.querySelector('.process-edge-issue-marker');
  assert.equal(edge.hasAttribute('title'), false, 'the focusable edge target has no title attribute');
  assert.equal(edgeMarker.hasAttribute('title'), false, 'the custom edge target has no title attribute');
  assert.equal(edgeMarker.querySelector('title'), null, 'the custom edge tooltip has no duplicate SVG title');
  assert.match(edgeMarker.querySelector('.process-overlay-tooltip text').textContent, /pass is not declared/);
  assert.match(edgeMarker.querySelector('.process-overlay-tooltip text').textContent, /done is unreachable/,
    'the custom edge tooltip includes every diagnostic for its target');
  graph.destroy();
});

test('node dialog Save is one undoable edit and Cmd/Ctrl+Enter uses the same transaction', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ ProcessEditModel }, { openNodeDialog }] = await Promise.all([
    harness.importDashboardModule('js/process-edit-model.js'),
    harness.importDashboardModule('js/process-node-dialog.js'),
  ]);
  for (const modifier of ['button', 'ctrlKey', 'metaKey']) {
    const model = new ProcessEditModel(view());
    const dispose = openNodeDialog({ model, nodeId: 'work', confirmDiscard: async () => true });
    const overlay = harness.document.querySelector('.process-node-modal');
    const input = overlay.querySelector('.process-node-input');
    input.focus();
    input.value = `Changed by ${modifier}`;
    assert.equal(model.node('work').name, 'Original', 'dialog edits remain private before Save');
    assert.equal(model.undoStack.length, 0);
    assert.equal(overlay.querySelector('.process-node-save').disabled, false,
      'Save stays available before a text control has blurred');
    if (modifier === 'button') harness.fireEvent(overlay.querySelector('.process-node-save'), 'click');
    else harness.fireEvent(overlay.querySelector('.process-node-input'), 'keydown', { key: 'Enter', [modifier]: true });
    assert.equal(model.node('work').name, `Changed by ${modifier}`);
    assert.equal(model.undoStack.length, 1, 'the complete dialog transaction occupies one history slot');
    assert.equal(harness.document.querySelector('.process-node-modal'), null);
    assert.equal(model.undo(), true);
    assert.equal(model.node('work').name, 'Original');
  }
});

test('dirty Escape awaits discard confirmation: reject keeps the draft, accept closes with no edit', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ ProcessEditModel }, { openNodeDialog }] = await Promise.all([
    harness.importDashboardModule('js/process-edit-model.js'),
    harness.importDashboardModule('js/process-node-dialog.js'),
  ]);
  const decisions = [];
  const model = new ProcessEditModel(view());
  const dispose = openNodeDialog({
    model, nodeId: 'work',
    confirmDiscard: () => new Promise((resolve) => decisions.push(resolve)),
  });
  const overlay = harness.document.querySelector('.process-node-modal');
  const input = overlay.querySelector('.process-node-input');
  input.value = 'Unsaved';
  harness.fireEvent(input, 'change');
  harness.fireEvent(overlay.querySelector('.process-node-input'), 'keydown', { key: 'Escape' });
  await settle();
  assert.equal(decisions.length, 1, 'Escape requests the shared asynchronous discard decision');
  assert.equal(overlay.inert, true, 'the underlying node dialog is inert while confirmation owns focus');
  assert.equal(overlay.getAttribute('aria-hidden'), 'true');
  assert.equal(overlay.querySelector('[role="dialog"]').getAttribute('aria-modal'), 'false');
  harness.fireEvent(overlay.querySelector('.process-node-save'), 'keydown', { key: 'Enter', ctrlKey: true });
  assert.equal(model.node('work').name, 'Original', 'save shortcuts cannot commit behind a pending confirmation');
  assert.ok(harness.document.querySelector('.process-node-modal'));
  decisions.shift()(false);
  await settle();
  assert.ok(harness.document.querySelector('.process-node-modal'), 'reject leaves the dialog and its draft open');
  assert.equal(overlay.inert, false);
  assert.equal(overlay.hasAttribute('aria-hidden'), false);
  assert.equal(overlay.querySelector('[role="dialog"]').getAttribute('aria-modal'), 'true');
  assert.equal(dispose.isDirty(), true);

  harness.fireEvent(harness.document.querySelector('.process-node-save'), 'keydown', { key: 'Escape' });
  await settle();
  assert.equal(decisions.length, 1);
  decisions.shift()(true);
  await settle();
  assert.equal(harness.document.querySelector('.process-node-modal'), null);
  assert.equal(model.node('work').name, 'Original');
  assert.equal(model.undoStack.length, 0, 'discard never creates a history entry');
  assert.equal(model.dirty, false);
});

test('Escape and backdrop flush an active raw node field before deciding whether to discard', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ ProcessEditModel }, { openNodeDialog }] = await Promise.all([
    harness.importDashboardModule('js/process-edit-model.js'),
    harness.importDashboardModule('js/process-node-dialog.js'),
  ]);
  for (const gesture of ['Escape', 'backdrop']) {
    let confirmations = 0;
    const model = new ProcessEditModel(view());
    openNodeDialog({
      model, nodeId: 'work',
      confirmDiscard: async () => { confirmations += 1; return true; },
    });
    const overlay = harness.document.querySelector('.process-node-modal');
    const input = overlay.querySelector('.process-node-input');
    input.focus();
    input.value = `Unblurred ${gesture}`;
    harness.fireEvent(input, 'input');
    if (gesture === 'Escape') harness.fireEvent(input, 'keydown', { key: 'Escape' });
    else harness.fireEvent(overlay, 'mousedown');
    await settle();
    assert.equal(confirmations, 1, `${gesture} flushes the raw field before the dirty check`);
    assert.equal(harness.document.querySelector('.process-node-modal'), null);
    assert.equal(model.node('work').name, 'Original', `${gesture} discards the staged edit atomically`);
    assert.equal(model.undoStack.length, 0);
  }
});

test('discard confirmation rejection is contained for gesture and programmatic node dismissal', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ ProcessEditModel }, { openNodeDialog }] = await Promise.all([
    harness.importDashboardModule('js/process-edit-model.js'),
    harness.importDashboardModule('js/process-node-dialog.js'),
  ]);
  let attempts = 0;
  const model = new ProcessEditModel(view());
  const dispose = openNodeDialog({
    model, nodeId: 'work',
    confirmDiscard: async () => { attempts += 1; throw new Error('confirmation service unavailable'); },
  });
  const overlay = harness.document.querySelector('.process-node-modal');
  const input = overlay.querySelector('.process-node-input');
  input.value = 'Unsaved';
  harness.fireEvent(input, 'change');

  harness.fireEvent(input, 'keydown', { key: 'Escape' });
  await settle();
  assert.equal(attempts, 1);
  assert.equal(harness.document.querySelector('.process-node-modal'), overlay,
    'a rejected gesture confirmation keeps the dialog mounted');
  assert.match(overlay.querySelector('.process-node-status').textContent,
    /Discard confirmation failed: confirmation service unavailable/);
  assert.equal(overlay.inert, false);
  assert.equal(await dispose.requestClose(), false, 'programmatic replacement receives a contained false result');
  await settle();
  assert.equal(attempts, 2);
  assert.equal(harness.document.querySelector('.process-node-modal'), overlay);
  assert.equal(model.node('work').name, 'Original');
  dispose(null);
});

test('rejected raw input stays dirty: Save remains open and Cancel confirms', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ ProcessEditModel }, { openNodeDialog }] = await Promise.all([
    harness.importDashboardModule('js/process-edit-model.js'),
    harness.importDashboardModule('js/process-node-dialog.js'),
  ]);
  let confirmations = 0;
  const model = new ProcessEditModel(taskView());
  const dispose = openNodeDialog({
    model, nodeId: 'work',
    confirmDiscard: async () => { confirmations += 1; return false; },
  });
  const overlay = harness.document.querySelector('.process-node-modal');
  const checkRows = overlay.querySelectorAll('.process-node-check');
  const secondID = checkRows[1].querySelector('.process-node-check-head .process-node-input');
  secondID.focus();
  secondID.value = 'lint';
  harness.fireEvent(overlay.querySelector('.process-node-save'), 'click');
  await settle();
  assert.ok(harness.document.querySelector('.process-node-modal'), 'Save cannot close over a rejected DOM value');
  assert.equal(model.node('work').checks[1].id, 'test', 'the previous staged draft is not committed');
  assert.equal(model.undoStack.length, 0);
  assert.equal(secondID.getAttribute('aria-invalid'), 'true');
  assert.match(overlay.querySelector('.process-node-status').textContent, /duplicate check id/);
  assert.equal(dispose.isDirty(), true, 'a rejected raw value still gates dismissal');

  const label = overlay.querySelector('.process-node-detail > .process-node-section .process-node-input');
  label.focus();
  label.value = 'Renamed';
  harness.fireEvent(label, 'change');
  await settle();
  assert.equal(label.value, 'Work', 'a second edit is reverted immediately instead of disappearing on a later render');
  assert.equal(label.hasAttribute('aria-invalid'), false, 'the unchanged field is not misrepresented as invalid');
  assert.match(overlay.querySelector('.process-node-status').textContent, /change was not applied/);

  harness.fireEvent(overlay.querySelector('.process-node-cancel'), 'click');
  await settle();
  assert.equal(confirmations, 1);
  assert.ok(harness.document.querySelector('.process-node-modal'), 'rejected discard keeps the invalid value visible');

  secondID.focus();
  secondID.value = 'verify';
  harness.fireEvent(secondID, 'change');
  const correctedLabel = overlay.querySelector('.process-node-detail > .process-node-section .process-node-input');
  assert.equal(correctedLabel.value, 'Work', 'correcting the invalid field rerenders only committed values');
  correctedLabel.focus();
  correctedLabel.value = 'Renamed';
  harness.fireEvent(correctedLabel, 'change');
  harness.fireEvent(harness.document.querySelector('.process-node-save'), 'click');
  assert.equal(model.node('work').checks[1].id, 'verify');
  assert.equal(model.node('work').name, 'Renamed');
  assert.equal(model.undoStack.length, 1);
  assert.equal(harness.document.querySelector('.process-node-modal'), null);
});

test('node dialog traps Tab and restores its invoker on forced teardown', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ ProcessEditModel }, { openNodeDialog }] = await Promise.all([
    harness.importDashboardModule('js/process-edit-model.js'),
    harness.importDashboardModule('js/process-node-dialog.js'),
  ]);
  const invoker = harness.document.body.appendChild(harness.document.createElement('button'));
  invoker.focus();
  const model = new ProcessEditModel(view());
  const dispose = openNodeDialog({ model, nodeId: 'work' });
  await settle();
  const overlay = harness.document.querySelector('.process-node-modal');
  const save = overlay.querySelector('.process-node-save');
  const first = overlay.querySelector('.process-node-close');
  save.focus();
  const tab = harness.fireEvent(save, 'keydown', { key: 'Tab' });
  assert.equal(tab.defaultPrevented, true);
  assert.equal(harness.document.activeElement, first, 'Tab wraps to the first dialog control');
  dispose(null);
  assert.equal(harness.document.activeElement, invoker, 'forced parent teardown restores the invoker');
});

test('node dialog opens with the label field focused, falling back when read-only', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ ProcessEditModel }, { openNodeDialog }] = await Promise.all([
    harness.importDashboardModule('js/process-edit-model.js'),
    harness.importDashboardModule('js/process-node-dialog.js'),
  ]);
  const model = new ProcessEditModel(taskView());
  const dispose = openNodeDialog({ model, nodeId: 'work' });
  await settle();
  const overlay = harness.document.querySelector('.process-node-modal');
  const label = overlay.querySelector('.process-node-detail > .process-node-section .process-node-input');
  assert.equal(harness.document.activeElement, label, 'editing a node lands the caret in the label field');
  dispose(null);

  const viewDispose = openNodeDialog({ model, nodeId: 'work', mode: 'view' });
  await settle();
  const readOnly = harness.document.querySelector('.process-node-modal');
  assert.equal(
    harness.document.activeElement,
    readOnly.querySelector('.process-node-close'),
    'a disabled label field yields to the first focusable control',
  );
  viewDispose(null);
});

test('node dialog restores and persists its own resize without bypassing dirty focus lifecycle', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ ProcessEditModel }, { openNodeDialog }, { dashPrefs }] = await Promise.all([
    harness.importDashboardModule('js/process-edit-model.js'),
    harness.importDashboardModule('js/process-node-dialog.js'),
    harness.importDashboardModule('js/prefs.js'),
  ]);
  const key = 'tclaude.dash.modalSize.process-node-editor';
  dashPrefs.setItem(key, JSON.stringify({ w: 920, h: 640 }));

  const invoker = harness.document.body.appendChild(harness.document.createElement('button'));
  invoker.focus();
  const decisions = [];
  const dispose = openNodeDialog({
    model: new ProcessEditModel(taskView()), nodeId: 'work',
    confirmDiscard: () => new Promise((resolve) => decisions.push(resolve)),
  });
  await settle();
  const overlay = harness.document.querySelector('.process-node-modal');
  const dialog = overlay.querySelector('.process-node-dialog');
  assert.equal(dialog.style.width, '920px', 'the node-dialog-specific width is restored');
  assert.equal(dialog.style.height, '640px', 'the node-dialog-specific height is restored');

  const planToggle = dialog.querySelector('.process-node-toggle');
  planToggle.checked = true;
  harness.fireEvent(planToggle, 'change');
  await settle();
  assert.equal(overlay.querySelector('.process-node-dialog'), dialog,
    'dynamic stage fields rerender inside the same resizable card');
  assert.equal(dialog.style.width, '920px');
  assert.equal(dialog.style.height, '640px');
  assert.ok([...dialog.querySelectorAll('.process-node-field-label')].some((label) => label.textContent === 'approval'),
    'the resized form exposes the newly enabled stage fields');

  let measuredWidth = 920;
  let measuredHeight = 640;
  Object.defineProperties(dialog, {
    offsetWidth: { configurable: true, get: () => measuredWidth },
    offsetHeight: { configurable: true, get: () => measuredHeight },
  });
  const textarea = dialog.querySelector('.process-node-textarea');
  harness.fireEvent(textarea, 'pointerdown');
  measuredWidth = 940;
  measuredHeight = 660;
  harness.fireEvent(textarea, 'pointerup');
  assert.deepEqual(JSON.parse(dashPrefs.getItem(key)), { w: 920, h: 640 },
    'a descendant textarea resize is not persisted as a dialog resize');

  measuredWidth = 920;
  measuredHeight = 640;
  harness.fireEvent(dialog, 'pointerdown');
  measuredWidth = 1000;
  measuredHeight = 720;
  harness.fireEvent(dialog, 'pointerup');
  assert.deepEqual(JSON.parse(dashPrefs.getItem(key)), { w: 1000, h: 720 },
    'a genuine pointer resize persists through dashPrefs');

  const label = overlay.querySelector('.process-node-input');
  label.value = 'Still guarded after resize';
  harness.fireEvent(label, 'change');
  harness.fireEvent(harness.document.activeElement, 'keydown', { key: 'Escape' });
  await settle();
  assert.equal(decisions.length, 1, 'resize wiring leaves dirty Escape behind confirmation');
  decisions.shift()(false);
  await settle();
  assert.equal(harness.document.querySelector('.process-node-modal'), overlay,
    'rejecting discard keeps the resized draft open');

  harness.fireEvent(overlay.querySelector('.process-node-cancel'), 'click');
  await settle();
  decisions.shift()(true);
  await settle();
  assert.equal(harness.document.querySelector('.process-node-modal'), null);
  assert.equal(harness.document.activeElement, invoker, 'confirmed close still restores the invoker');

  // The helper cleanup is part of dialog disposal: detached pointer events
  // cannot mutate the preference after ownership ends.
  harness.fireEvent(dialog, 'pointerdown');
  measuredWidth = 1010;
  measuredHeight = 730;
  harness.fireEvent(dialog, 'pointerup');
  assert.deepEqual(JSON.parse(dashPrefs.getItem(key)), { w: 1000, h: 720 });
  dashPrefs.removeItem(key);
});

test('opening another node settings dialog cannot replace a rejected dirty draft', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ ProcessEditModel }, { ProcessTemplateEditor }] = await Promise.all([
    harness.importDashboardModule('js/process-edit-model.js'),
    harness.importDashboardModule('js/process-editor.js'),
  ]);
  let confirmations = 0;
  const editor = {
    model: new ProcessEditModel(view()), modalDispose: null,
    options: { confirmDiscard: async () => { confirmations += 1; return false; } },
    abort: new AbortController(), refresh() {},
  };
  assert.equal(await ProcessTemplateEditor.prototype.openNodeSettings.call(editor, 'work'), true);
  const overlay = harness.document.querySelector('.process-node-modal');
  const input = overlay.querySelector('.process-node-input');
  input.value = 'Dirty';
  harness.fireEvent(input, 'change');
  assert.equal(await ProcessTemplateEditor.prototype.openNodeSettings.call(editor, 'done'), false);
  assert.equal(confirmations, 1);
  assert.equal(harness.document.querySelector('.process-node-modal'), overlay);
  assert.equal(overlay.querySelector('[role="dialog"]').getAttribute('aria-label'), 'Node work');
  editor.modalDispose(null);
});

test('programmatic node-dialog replacement contains a rejected discard promise', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ ProcessEditModel }, { ProcessTemplateEditor }] = await Promise.all([
    harness.importDashboardModule('js/process-edit-model.js'),
    harness.importDashboardModule('js/process-editor.js'),
  ]);
  const editor = {
    model: new ProcessEditModel(view()), modalDispose: null,
    options: { confirmDiscard: async () => { throw new Error('confirmation service unavailable'); } },
    abort: new AbortController(), refresh() {},
  };
  assert.equal(await ProcessTemplateEditor.prototype.openNodeSettings.call(editor, 'work'), true);
  const overlay = harness.document.querySelector('.process-node-modal');
  const input = overlay.querySelector('.process-node-input');
  input.value = 'Dirty';
  harness.fireEvent(input, 'change');
  assert.equal(await ProcessTemplateEditor.prototype.openNodeSettings.call(editor, 'done'), false);
  await settle();
  assert.equal(harness.document.querySelector('.process-node-modal'), overlay);
  assert.equal(overlay.querySelector('[role="dialog"]').getAttribute('aria-label'), 'Node work');
  assert.match(overlay.querySelector('.process-node-status').textContent,
    /Discard confirmation failed: confirmation service unavailable/);
  editor.modalDispose(null);
});

test('params dialog edits identifiers, typed defaults, descriptions, and required state atomically', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ ProcessEditModel }, { openProcessParamsDialog }] = await Promise.all([
    harness.importDashboardModule('js/process-edit-model.js'),
    harness.importDashboardModule('js/process-params-dialog.js'),
  ]);
  const loaded = view();
  loaded.template.params = {
    issue: { type: 'string', description: 'Issue id', required: true },
    retries: { type: 'number', default: 2 },
    legacy: { type: 'custom-kind', default: { keep: true }, doc: 'preserved', required: false },
  };
  const model = new ProcessEditModel(loaded);
  let mutations = 0;
  openProcessParamsDialog({ model, onMutated: () => { mutations += 1; } });
  const overlay = harness.document.querySelector('.process-param-modal');
  assert.equal(overlay.querySelectorAll('.process-param-row').length, 3);
  const required = overlay.querySelector('[data-process-param="issue"] .process-param-required');
  assert.equal(typeof required.checked === 'boolean' ? required.checked : required.hasAttribute('checked'), true);
  assert.equal(overlay.querySelector('[data-process-param="retries"] .process-param-default').value, '2');
  assert.equal(overlay.querySelector('[data-process-param="legacy"] .process-param-type').value, 'custom-kind', 'unknown types remain editable');

  const issue = overlay.querySelector('[data-process-param="issue"]');
  const name = issue.querySelector('.process-param-name');
  name.value = 'ticket'; harness.fireEvent(name, 'input');
  const description = issue.querySelector('.process-param-description');
  description.value = 'Tracker ticket'; harness.fireEvent(description, 'input');
  const retries = overlay.querySelector('[data-process-param="retries"]');
  const defaultValue = retries.querySelector('.process-param-default');
  defaultValue.value = '3'; harness.fireEvent(defaultValue, 'input');
  harness.fireEvent(overlay.querySelector('.modal-buttons .primary'), 'click');

  assert.equal(mutations, 1);
  assert.equal(harness.document.querySelector('.process-param-modal'), null);
  assert.equal(model.template.params.ticket.description, 'Tracker ticket');
  assert.equal(model.template.params.ticket.required, true);
  assert.equal(model.template.params.retries.default, 3, 'number defaults retain their declared type');
  assert.deepEqual(model.template.params.legacy.default, { keep: true }, 'untouched free-form defaults are lossless');
  assert.equal(model.template.params.legacy.doc, 'preserved');
  assert.equal(model.template.params.legacy.required, false, 'untouched explicit false metadata is preserved');
  assert.equal(model.undoStack.length, 1, 'the complete param edit is one transaction');
  assert.equal(model.undo(), true);
  assert.ok(model.template.params.issue);
  assert.equal(model.template.params.ticket, undefined);
});

test('params dialog rejects invalid edited number and boolean defaults without applying', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ ProcessEditModel }, { openProcessParamsDialog }] = await Promise.all([
    harness.importDashboardModule('js/process-edit-model.js'),
    harness.importDashboardModule('js/process-params-dialog.js'),
  ]);
  const cases = [
    ['empty number', 'retries', '', /finite number/],
    ['non-finite number', 'retries', 'Infinity', /finite number/],
    ['invalid boolean', 'enabled', 'TRUE', /exactly "true" or "false"/],
  ];
  for (const [label, paramName, value, expectedError] of cases) {
    const loaded = view();
    loaded.template.params = {
      retries: { type: 'number', default: 2 },
      enabled: { type: 'boolean', default: true },
    };
    const original = structuredClone(loaded.template.params);
    const model = new ProcessEditModel(loaded);
    let mutations = 0;
    const dispose = openProcessParamsDialog({ model, onMutated: () => { mutations += 1; } });
    const overlay = harness.document.querySelector('.process-param-modal');
    const input = overlay.querySelector(`[data-process-param="${paramName}"] .process-param-default`);
    input.value = value;
    harness.fireEvent(input, 'input');
    harness.fireEvent(overlay.querySelector('.modal-buttons .primary'), 'click');
    await settle();

    assert.equal(harness.document.querySelector('.process-param-modal'), overlay, `${label}: dialog remains open`);
    const alert = overlay.querySelector('[role="alert"]');
    assert.match(alert.textContent, expectedError, `${label}: accessible validation feedback`);
    assert.deepEqual(model.template.params, original, `${label}: model is unchanged`);
    assert.equal(model.undoStack.length, 0, `${label}: no undo entry is created`);
    assert.equal(model.dirty, false, `${label}: editor remains clean`);
    assert.equal(mutations, 0, `${label}: mutation callback is not emitted`);
    dispose(null);
  }
});

test('params dialog default presence round-trip preserves an untouched custom object losslessly', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ ProcessEditModel }, { openProcessParamsDialog }] = await Promise.all([
    harness.importDashboardModule('js/process-edit-model.js'),
    harness.importDashboardModule('js/process-params-dialog.js'),
  ]);
  const loaded = view();
  loaded.template.params = {
    config: { type: 'custom-kind', default: { nested: { enabled: true }, retries: 2 } },
  };
  const original = structuredClone(loaded.template.params.config.default);
  const model = new ProcessEditModel(loaded);
  const dispose = openProcessParamsDialog({ model });
  const overlay = harness.document.querySelector('.process-param-modal');
  const enabled = overlay.querySelector('.process-param-default-enabled');
  enabled.checked = false;
  harness.fireEvent(enabled, 'change');
  enabled.checked = true;
  harness.fireEvent(enabled, 'change');

  assert.equal(dispose.isDirty(), false, 'presence off then on restores the original draft state');
  harness.fireEvent(overlay.querySelector('.modal-buttons .primary'), 'click');
  assert.deepEqual(model.template.params.config.default, original);
  assert.equal(model.undoStack.length, 0, 'lossless presence round-trip creates no model edit');
  assert.equal(model.dirty, false);
});

test('params dialog restores focus near removed rows and to Add after the final row', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ ProcessEditModel }, { openProcessParamsDialog }] = await Promise.all([
    harness.importDashboardModule('js/process-edit-model.js'),
    harness.importDashboardModule('js/process-params-dialog.js'),
  ]);
  const cases = [
    ['middle', ['alpha', 'beta', 'gamma'], 'beta', 'gamma'],
    ['last', ['alpha', 'beta', 'gamma'], 'gamma', 'beta'],
    ['only', ['alpha'], 'alpha', null],
  ];
  for (const [label, names, removed, focusedName] of cases) {
    const loaded = view();
    loaded.template.params = Object.fromEntries(names.map((name) => [name, { type: 'string' }]));
    const model = new ProcessEditModel(loaded);
    const dispose = openProcessParamsDialog({ model });
    const overlay = harness.document.querySelector('.process-param-modal');
    const remove = overlay.querySelector(`[data-process-param="${removed}"] .process-action-danger`);
    remove.focus();
    harness.fireEvent(remove, 'click');
    await settle();

    const expected = focusedName
      ? overlay.querySelector(`[data-process-param="${focusedName}"] .process-param-name`)
      : overlay.querySelector('.process-param-toolbar button');
    assert.equal(harness.document.activeElement, expected, `${label}: focus moves to the predictable nearby control`);
    assert.equal(overlay.querySelector('[role="dialog"]').contains(harness.document.activeElement), true,
      `${label}: focus remains inside the dialog`);
    dispose(null);
  }
});

test('params dialog reports every raw staged field and structural edit as dirty', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ ProcessEditModel }, { openProcessParamsDialog }] = await Promise.all([
    harness.importDashboardModule('js/process-edit-model.js'),
    harness.importDashboardModule('js/process-params-dialog.js'),
  ]);
  const cases = [
    ['add', (overlay) => harness.fireEvent(overlay.querySelector('.process-param-toolbar button'), 'click')],
    ['remove', (overlay) => harness.fireEvent(overlay.querySelector('[data-process-param="issue"] .process-action-danger'), 'click')],
    ['raw name', (overlay) => {
      const control = overlay.querySelector('[data-process-param="issue"] .process-param-name');
      control.value = 'issue ';
      harness.fireEvent(control, 'input');
    }],
    ['raw type', (overlay) => {
      const control = overlay.querySelector('[data-process-param="issue"] .process-param-type');
      control.value = 'string ';
      harness.fireEvent(control, 'input');
    }],
    ['description', (overlay) => {
      const control = overlay.querySelector('[data-process-param="issue"] .process-param-description');
      control.value = 'Changed';
      harness.fireEvent(control, 'input');
    }],
    ['default enabled', (overlay) => {
      const control = overlay.querySelector('[data-process-param="issue"] .process-param-default-enabled');
      control.checked = true;
      harness.fireEvent(control, 'change');
    }],
    ['default value', (overlay) => {
      const control = overlay.querySelector('[data-process-param="retries"] .process-param-default');
      control.value = '2 ';
      harness.fireEvent(control, 'input');
    }],
    ['required', (overlay) => {
      const control = overlay.querySelector('[data-process-param="issue"] .process-param-required');
      control.checked = false;
      harness.fireEvent(control, 'change');
    }],
  ];
  for (const [label, mutate] of cases) {
    const loaded = view();
    loaded.template.params = {
      issue: { type: 'string', description: 'Issue id', required: true },
      retries: { type: 'number', default: 2 },
    };
    const model = new ProcessEditModel(loaded);
    const dispose = openProcessParamsDialog({ model });
    assert.equal(dispose.isDirty(), false, `${label} begins clean`);
    mutate(harness.document.querySelector('.process-param-modal'));
    assert.equal(dispose.isDirty(), true, `${label} dirties the staged dialog`);
    assert.equal(model.dirty, false, `${label} remains atomic before Apply`);
    dispose(null);
  }
});

test('params dialog contains rejected discard confirmation and reports programmatic close failure', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ ProcessEditModel }, { openProcessParamsDialog }] = await Promise.all([
    harness.importDashboardModule('js/process-edit-model.js'),
    harness.importDashboardModule('js/process-params-dialog.js'),
  ]);
  const loaded = view();
  loaded.template.params = { issue: { type: 'string' } };
  const model = new ProcessEditModel(loaded);
  const dispose = openProcessParamsDialog({
    model,
    confirmDiscard: async () => { throw new Error('confirmation service unavailable'); },
  });
  const overlay = harness.document.querySelector('.process-param-modal');
  harness.fireEvent(overlay.querySelector('.process-param-toolbar button'), 'click');
  assert.equal(await dispose.requestClose(), false);
  await settle();
  assert.equal(harness.document.querySelector('.process-param-modal'), overlay);
  assert.match(overlay.querySelector('[role="alert"]').textContent,
    /Discard confirmation failed: confirmation service unavailable/);
  assert.equal(overlay.inert, false);
  assert.deepEqual(model.template.params, { issue: { type: 'string' } });
  dispose(null);
});

test('dirty params participate in navigation and rejected modal replacement guards', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ ProcessEditModel }, { ProcessTemplateEditor }, { createProcessesActions }] = await Promise.all([
    harness.importDashboardModule('js/process-edit-model.js'),
    harness.importDashboardModule('js/process-editor.js'),
    harness.importDashboardModule('js/processes-actions.js'),
  ]);
  let confirmations = 0;
  const editor = {
    model: new ProcessEditModel(view()), modalDispose: null,
    options: { confirmDiscard: async () => { confirmations += 1; return false; } },
    abort: new AbortController(), refresh() {}, savePending: false,
  };
  Object.defineProperty(editor, 'dirty', {
    get: () => Object.getOwnPropertyDescriptor(ProcessTemplateEditor.prototype, 'dirty').get.call(editor),
  });
  assert.equal(await ProcessTemplateEditor.prototype.openParamsSettings.call(editor), true);
  const overlay = harness.document.querySelector('.process-param-modal');
  harness.fireEvent(overlay.querySelector('.process-param-toolbar button'), 'click');
  assert.equal(editor.dirty, true, 'the editor exposes its staged params as dirty');

  const actions = createProcessesActions({
    state: { currentEditor: () => editor },
    confirmDiscard: editor.options.confirmDiscard,
  });
  assert.equal(await actions.closeCanvas(), false, 'outer navigation is rejected');
  assert.equal(confirmations, 1);
  assert.equal(harness.document.querySelector('.process-param-modal'), overlay, 'navigation rejection keeps the draft open');

  assert.equal(await ProcessTemplateEditor.prototype.openNodeSettings.call(editor, 'work'), false,
    'another editor modal cannot replace the rejected params draft');
  assert.equal(confirmations, 2);
  assert.equal(harness.document.querySelector('.process-param-modal'), overlay);
  assert.equal(harness.document.querySelector('.process-node-modal'), null);
  editor.modalDispose(null);
});

test('params dialog traps Tab and restores focus on every close path without prompting clean close', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ ProcessEditModel }, { openProcessParamsDialog }] = await Promise.all([
    harness.importDashboardModule('js/process-edit-model.js'),
    harness.importDashboardModule('js/process-params-dialog.js'),
  ]);

  let prompts = 0;
  const cleanInvoker = harness.document.body.appendChild(harness.document.createElement('button'));
  cleanInvoker.focus();
  openProcessParamsDialog({
    model: new ProcessEditModel(view()),
    confirmDiscard: async () => { prompts += 1; return true; },
  });
  await settle();
  harness.fireEvent(harness.document.querySelector('.process-param-modal .modal-buttons button'), 'click');
  await settle();
  assert.equal(prompts, 0, 'clean Cancel closes without confirmation');
  assert.equal(harness.document.activeElement, cleanInvoker);

  for (const gesture of ['cancel', 'escape', 'backdrop', 'forced']) {
    const invoker = harness.document.body.appendChild(harness.document.createElement('button'));
    invoker.focus();
    let confirmations = 0;
    const dispose = openProcessParamsDialog({
      model: new ProcessEditModel(view()),
      confirmDiscard: async () => { confirmations += 1; return true; },
    });
    await settle();
    const overlay = harness.document.querySelector('.process-param-modal');
    const add = overlay.querySelector('.process-param-toolbar button');
    harness.fireEvent(add, 'click');
    await settle();
    const first = overlay.querySelector('.process-param-name');
    const apply = overlay.querySelector('.modal-buttons .primary');
    apply.focus();
    const tab = harness.fireEvent(apply, 'keydown', { key: 'Tab' });
    assert.equal(tab.defaultPrevented, true, `${gesture}: Tab is contained`);
    assert.equal(harness.document.activeElement, first, `${gesture}: Tab wraps to the first control`);

    if (gesture === 'cancel') harness.fireEvent(overlay.querySelector('.modal-buttons button'), 'click');
    else if (gesture === 'escape') harness.fireEvent(first, 'keydown', { key: 'Escape' });
    else if (gesture === 'backdrop') harness.fireEvent(overlay, 'mousedown');
    else dispose(null);
    await settle();
    assert.equal(confirmations, gesture === 'forced' ? 0 : 1, `${gesture}: confirmation count`);
    assert.equal(harness.document.querySelector('.process-param-modal'), null, `${gesture}: dialog closes`);
    assert.equal(harness.document.activeElement, invoker, `${gesture}: invoker focus is restored`);
  }
});

test('dirty editor instantiate requires a successful clean save before emitting an exact ref', async (t) => {
  const harness = await createPreactHarness(t);
  const { ProcessTemplateEditor } = await harness.importDashboardModule('js/process-editor.js');
  const emitted = [];
  const base = () => ({
    blank: false, dirty: true, savePending: false,
    abort: new AbortController(),
    model: { dirty: true, currentRef: 'release@sha256:old', template: { id: 'release' } },
    options: { onInstantiate: (value) => emitted.push(value) },
    choiceModal: async (copy) => { assert.match(copy.title, /Save before instantiating/); return 'save'; },
    status() {},
  });

  const clean = base();
  clean.save = async () => { clean.dirty = false; clean.model.dirty = false; clean.model.currentRef = 'release@sha256:saved'; return true; };
  assert.equal(await ProcessTemplateEditor.prototype.requestInstantiate.call(clean), true);
  assert.equal(emitted[0].ref, 'release@sha256:saved');

  const changedInFlight = base();
  let status = '';
  changedInFlight.save = async () => true;
  changedInFlight.status = (message) => { status = message; };
  assert.equal(await ProcessTemplateEditor.prototype.requestInstantiate.call(changedInFlight), false);
  assert.match(status, /changed while saving/);
  assert.equal(emitted.length, 1, 'dirty state is never instantiated');
});

test('Cancel, backdrop, and close affordance discard only after confirmation', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ ProcessEditModel }, { openNodeDialog }] = await Promise.all([
    harness.importDashboardModule('js/process-edit-model.js'),
    harness.importDashboardModule('js/process-node-dialog.js'),
  ]);
  for (const gesture of ['cancel', 'backdrop', 'close']) {
    let confirmations = 0;
    const model = new ProcessEditModel(view());
    openNodeDialog({
      model, nodeId: 'work',
      confirmDiscard: async () => { confirmations += 1; return true; },
    });
    const overlay = harness.document.querySelector('.process-node-modal');
    const input = overlay.querySelector('.process-node-input');
    input.value = `Unsaved ${gesture}`;
    harness.fireEvent(input, 'change');
    const target = gesture === 'cancel' ? overlay.querySelector('.process-node-cancel')
      : gesture === 'close' ? overlay.querySelector('.process-node-close') : overlay;
    harness.fireEvent(target, gesture === 'backdrop' ? 'mousedown' : 'click');
    await settle();
    assert.equal(confirmations, 1, `${gesture} confirms a dirty discard`);
    assert.equal(harness.document.querySelector('.process-node-modal'), null);
    assert.equal(model.node('work').name, 'Original');
    assert.equal(model.undoStack.length, 0);
  }
});
