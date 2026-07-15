import test from 'node:test';
import assert from 'node:assert/strict';
import { rankCommands } from '../dashboard/js/palette-score.js';
import {
  buildProcessEditorCommands, buildProcessNodeTypeCommands,
} from '../dashboard/js/process-command-registry.js';
import {
  buildRegisteredCommands, registerCommandProvider,
} from '../dashboard/js/command-registry.js';

function context(overrides = {}) {
  return {
    hasGraph: true, hasSelection: true, hasGraphSelection: true,
    canCreate: true, createReason: '', canEdit: true, editReason: '',
    canDuplicate: true, duplicateReason: '', canDelete: true, deleteReason: '',
    canValidate: true, validateReason: '', issueCount: 2, hasCurrentIssue: true,
    canSave: true, saveReason: '', canInstantiate: true, instantiateReason: '',
    ...overrides,
  };
}

function editorFixture(overrides = {}) {
  const calls = [];
  const editor = { commandContext: () => context(), ...overrides };
  for (const method of [
    'addNodeType', 'editSelection', 'duplicateSelection', 'deleteSelection', 'selectAll',
    'clearSelection', 'fitGraph', 'centerSelection', 'zoomGraph', 'resetZoom',
    'validateNow', 'focusIssue', 'requestScribe', 'save', 'requestInstantiate',
  ]) editor[method] ||= (...args) => calls.push([method, ...args]);
  return { editor, calls };
}

test('process commands search in plain and wizard vocabulary without duplicate implementations', () => {
  const { editor } = editorFixture();
  const plain = buildProcessEditorCommands({ editor, actions: {}, wizard: false });
  const wizard = buildProcessEditorCommands({ editor, actions: {}, wizard: true });
  assert.equal(plain.length, wizard.length);
  assert.equal(new Set(plain.map((command) => command.id)).size, plain.length, 'one command implementation per id');
  assert.equal(rankCommands(plain, 'conjure decision')[0].id, 'process.create.decision');
  assert.equal(rankCommands(wizard, 'create decision')[0].id, 'process.create.decision');
  assert.equal(rankCommands(plain, 'omens')[0].id, 'process.validate');
  assert.equal(rankCommands(wizard, 'validate')[0].id, 'process.validate');
  assert.equal(rankCommands(plain, 'agent selection')[0].id, 'process.scribe-selection');
  assert.equal(rankCommands(wizard, 'fix issue')[0].id, 'process.scribe-diagnostic');
});

test('context keeps unavailable commands visible, disabled, and reasoned', () => {
  const { editor } = editorFixture({
    commandContext: () => context({
      canCreate: false, createReason: 'Read-only process view.',
      canDelete: false, deleteReason: 'Select graph items first.',
      issueCount: 0, hasCurrentIssue: false, hasGraphSelection: false,
      canSave: false, saveReason: 'There are no unsaved changes.',
    }),
  });
  const commands = buildProcessEditorCommands({ editor, actions: {}, wizard: false });
  const create = commands.find((command) => command.id === 'process.create.task');
  const remove = commands.find((command) => command.id === 'process.delete-selection');
  const next = commands.find((command) => command.id === 'process.next-issue');
  const save = commands.find((command) => command.id === 'process.save');
  const selection = commands.find((command) => command.id === 'process.scribe-selection');
  const diagnostic = commands.find((command) => command.id === 'process.scribe-diagnostic');
  assert.deepEqual([create.enabled, create.disabledReason], [false, 'Read-only process view.']);
  assert.deepEqual([remove.enabled, remove.disabledReason], [false, 'Select graph items first.']);
  assert.deepEqual([next.enabled, next.disabledReason], [false, 'No validation issues.']);
  assert.deepEqual([save.enabled, save.disabledReason], [false, 'There are no unsaved changes.']);
  assert.deepEqual([selection.enabled, selection.disabledReason], [false, 'Select a node or edge first.']);
  assert.deepEqual([diagnostic.enabled, diagnostic.disabledReason], [false, 'Focus a validation issue first.']);
});

test('commands delegate to the editor and process navigation handlers', () => {
  const { editor, calls } = editorFixture();
  const navigation = [];
  const commands = buildProcessEditorCommands({
    editor, actions: { activateSubtab: (name) => navigation.push(name) }, wizard: false,
  });
  for (const id of [
    'process.create.wait', 'process.edit-selection', 'process.duplicate-selection',
    'process.delete-selection', 'process.validate', 'process.next-issue',
    'process.scribe-selection', 'process.scribe-diagnostic', 'process.scribe-template',
    'process.save', 'process.instantiate', 'process.templates', 'process.runs',
  ]) commands.find((command) => command.id === id).run();
  assert.deepEqual(calls, [
    ['addNodeType', 'wait'], ['editSelection'], ['duplicateSelection'], ['deleteSelection'],
    ['validateNow'], ['focusIssue', 1], ['requestScribe', 'selection'],
    ['requestScribe', 'diagnostic'], ['requestScribe', 'template'], ['save'], ['requestInstantiate'],
  ]);
  assert.deepEqual(navigation, ['templates', 'runs']);
});

test('node-type chooser substrate is injectable for a future positioned chooser', () => {
  const created = [];
  const commands = buildProcessNodeTypeCommands({ onCreate: (type) => created.push(type), wizard: false });
  assert.deepEqual(commands.map((command) => command.group), Array(commands.length).fill('process-node-type'));
  commands.find((command) => command.id === 'process.create.end').run();
  assert.deepEqual(created, ['end']);
});

test('feature providers contribute live context and unregister cleanly', () => {
  let selected = 'a';
  const unregister = registerCommandProvider('test-process-provider', ({ snapshot }) => [{
    label: `${snapshot.prefix} ${selected}`,
  }]);
  assert.deepEqual(buildRegisteredCommands({ snapshot: { prefix: 'Edit' } }), [{ label: 'Edit a' }]);
  selected = 'b';
  assert.deepEqual(buildRegisteredCommands({ snapshot: { prefix: 'Edit' } }), [{ label: 'Edit b' }]);
  unregister();
  assert.deepEqual(buildRegisteredCommands({ snapshot: { prefix: 'Edit' } }), []);
});
