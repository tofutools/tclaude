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
