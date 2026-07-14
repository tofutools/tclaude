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

async function settle() {
  await Promise.resolve();
  await Promise.resolve();
}

test('node marker detail is visible on hover/focus and part of the node accessible name', async (t) => {
  const harness = await createPreactHarness(t);
  const { ProcessGraph } = await harness.importDashboardModule('js/process-graph.js');
  const host = harness.document.body.appendChild(harness.document.createElement('div'));
  const issue = 'E_PERFORMER: Work performer is required';
  const graph = new ProcessGraph(host, {
    nodes: [{ id: 'work', type: 'task', overlay: { glyph: '!', severity: 'error', issues: [issue] } }],
    edges: [],
  }, { fitOnRender: false });
  const node = host.querySelector('.process-node');
  assert.match(node.getAttribute('aria-label'), /E_PERFORMER: Work performer is required/);
  assert.equal(node.getAttribute('tabindex'), '0', 'keyboard focus reaches the described node');
  assert.equal(host.querySelector('.process-overlay-anchor title').textContent, issue, 'native hover fallback has full detail');
  assert.match(host.querySelector('.process-overlay-tooltip text').textContent, /Work performer is required/);
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
  harness.fireEvent(overlay.querySelector('.process-node-save'), 'keydown', { key: 'Enter', ctrlKey: true });
  assert.equal(model.node('work').name, 'Original', 'save shortcuts cannot commit behind a pending confirmation');
  assert.ok(harness.document.querySelector('.process-node-modal'));
  decisions.shift()(false);
  await settle();
  assert.ok(harness.document.querySelector('.process-node-modal'), 'reject leaves the dialog and its draft open');
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
