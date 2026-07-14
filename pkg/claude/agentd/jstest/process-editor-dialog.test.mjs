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

test('Delete then Enter confirms a simple selection deletion from the focused destructive action', async (t) => {
  const harness = await createPreactHarness(t);
  const [{ ProcessEditModel }, { ProcessTemplateEditor }] = await Promise.all([
    harness.importDashboardModule('js/process-edit-model.js'),
    harness.importDashboardModule('js/process-editor.js'),
  ]);
  const editor = deletionEditor(ProcessTemplateEditor, ProcessEditModel, { type: 'node', id: 'done' });

  pressDelete(harness, ProcessTemplateEditor, editor);
  const overlay = harness.document.querySelector('.process-editor-modal');
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
  const overlay = harness.document.querySelector('.process-editor-modal');
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
    const overlay = harness.document.querySelector('.process-editor-modal');
    if (gesture === 'Escape') harness.fireEvent(harness.document.activeElement, 'keydown', { key: 'Escape' });
    else if (gesture === 'Cancel') harness.fireEvent(harness.getByRole(overlay, 'button', { name: 'Cancel' }), 'click');
    else harness.fireEvent(overlay, 'click');
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
  const overlay = harness.document.querySelector('.process-editor-modal');
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
  assert.equal(decisions.length, 1);
  decisions.shift()(true);
  await settle();
  assert.equal(harness.document.querySelector('.process-node-modal'), null);
  assert.equal(model.node('work').name, 'Original');
  assert.equal(model.undoStack.length, 0, 'discard never creates a history entry');
  assert.equal(model.dirty, false);
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
  assert.equal(overlay.querySelector('[data-process-param="issue"] .process-param-required').checked, true);
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
    harness.fireEvent(target, 'click');
    await settle();
    assert.equal(confirmations, 1, `${gesture} confirms a dirty discard`);
    assert.equal(harness.document.querySelector('.process-node-modal'), null);
    assert.equal(model.node('work').name, 'Original');
    assert.equal(model.undoStack.length, 0);
  }
});
