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
