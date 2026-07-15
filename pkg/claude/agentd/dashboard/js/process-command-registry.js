import { PROCESS_NODE_TYPES } from './process-node-types.js';
import { isWizardActive } from './slop.js';

function presented(plain, wizard, active) {
  return active ? wizard : plain;
}

function available(command, enabled, disabledReason) {
  return {
    ...command,
    enabled: enabled !== false,
    disabledReason: enabled === false ? disabledReason : '',
  };
}

// Reusable node-type command/chooser substrate. `onCreate` owns placement and
// mutation; this layer owns only searchable presentation and availability.
export function buildProcessNodeTypeCommands({
  onCreate,
  canCreate = true,
  disabledReason = 'Adding nodes is not available in this view.',
  wizard = false,
} = {}) {
  if (typeof onCreate !== 'function') throw new TypeError('node-type commands require onCreate');
  return PROCESS_NODE_TYPES.map((node) => available({
    id: `process.create.${node.type}`,
    group: 'process-node-type',
    icon: node.type === 'decision' ? '◇' : node.type === 'wait' ? '◷' : node.type === 'end' ? '⏹' : '＋',
    label: presented(`Create ${node.label.toLowerCase()} node`, `Conjure ${node.wizardLabel.toLowerCase()}`, wizard),
    hint: node.hint,
    keywords: `process graph node add insert create ${node.type} ${node.label} conjure rune sigil`,
    run: () => onCreate(node.type),
  }, canCreate, disabledReason));
}

export function buildProcessEditorCommands({ editor, actions, wizard = isWizardActive() } = {}) {
  if (!editor) return [];
  const context = editor.commandContext();
  const command = (id, icon, plain, arcane, hint, keywords, enabled, reason, run) => available({
    id: `process.${id}`,
    scope: 'process-editor',
    icon,
    label: presented(plain, arcane, wizard),
    hint,
    keywords: `process editor graph ${keywords}`,
    run,
  }, enabled, reason);
  const commands = buildProcessNodeTypeCommands({
    onCreate: (type) => editor.addNodeType(type),
    canCreate: context.canCreate,
    disabledReason: context.createReason,
    wizard,
  });
  commands.push(
    command('edit-selection', '✎', 'Edit selection', 'Enchant selection', 'Open the selected item for editing',
      'edit modify selection enchant', context.canEdit, context.editReason, () => editor.editSelection()),
    command('duplicate-selection', '⧉', 'Duplicate selection', 'Echo selection', 'Copy the selected nodes and their internal connections',
      'duplicate copy clone selection echo mirror', context.canDuplicate, context.duplicateReason, () => editor.duplicateSelection()),
    command('delete-selection', '⌫', 'Delete selection…', 'Banish selection…', 'Use the editor delete confirmation and rewire choices',
      'delete remove selection banish dispel', context.canDelete, context.deleteReason, () => editor.deleteSelection()),
    command('select-all', '☷', 'Select all graph items', 'Bind the whole graph', 'Select every node and edge',
      'select all everything bind whole', context.hasGraph, 'The graph is empty.', () => editor.selectAll()),
    command('clear-selection', '○', 'Clear selection', 'Release selection', 'Leave every graph item unselected',
      'clear deselect unselect release', context.hasSelection, 'Nothing is selected.', () => editor.clearSelection()),
    command('fit', '⊡', 'Fit graph to view', 'Survey the whole graph', 'Show the complete process graph',
      'fit view frame all survey', context.hasGraph, 'The graph is empty.', () => editor.fitGraph()),
    command('center', '◎', 'Center selection', 'Center the chosen runes', 'Pan the selected graph items into view',
      'center focus selection chosen runes', context.hasGraphSelection, 'Select a node or edge first.', () => editor.centerSelection()),
    command('zoom-in', '＋', 'Zoom in', 'Magnify the graph', 'Increase canvas magnification',
      'zoom in magnify closer', true, '', () => editor.zoomGraph(1.2)),
    command('zoom-out', '−', 'Zoom out', 'Diminish the graph', 'Decrease canvas magnification',
      'zoom out diminish farther', true, '', () => editor.zoomGraph(1 / 1.2)),
    command('zoom-reset', '1:1', 'Reset zoom', 'Restore true sight', 'Return canvas magnification to 100%',
      'zoom reset actual size 100 restore sight', true, '', () => editor.resetZoom()),
    command('validate', '✓', 'Validate process now', 'Read the process omens', 'Run validation immediately against the current draft',
      'validate lint check issues diagnostics omens', context.canValidate, context.validateReason, () => editor.validateNow()),
    command('next-issue', '↓', 'Focus next issue', 'Seek the next omen', `${context.issueCount} current validation issue${context.issueCount === 1 ? '' : 's'}`,
      'next issue error warning diagnostic omen', context.issueCount > 0, 'No validation issues.', () => editor.focusIssue(1)),
    command('previous-issue', '↑', 'Focus previous issue', 'Seek the previous omen', `${context.issueCount} current validation issue${context.issueCount === 1 ? '' : 's'}`,
      'previous prior issue error warning diagnostic omen', context.issueCount > 0, 'No validation issues.', () => editor.focusIssue(-1)),
    command('scribe-selection', '🤖', 'Ask agent about selection', 'Ask a scribe about chosen runes', 'Preview selected stable graph identities, then reuse or summon the scoped scribe',
      'ask agent scribe selection context chosen runes', context.hasGraphSelection, 'Select a node or edge first.', () => editor.requestScribe('selection')),
    command('scribe-diagnostic', '🩹', 'Ask agent to fix this issue', 'Ask a scribe to mend this omen', 'Preview the focused diagnostic code and target, then reuse or summon the scoped scribe',
      'ask agent scribe fix issue diagnostic error warning omen mend', context.hasCurrentIssue, 'Focus a validation issue first.', () => editor.requestScribe('diagnostic')),
    command('scribe-template', '🤖', 'Edit / refactor with agent', 'Rewrite the process with a scribe', 'Preview bounded whole-template context, then reuse or summon the scoped scribe',
      'edit refactor agent scribe whole template rewrite process', true, '', () => editor.requestScribe('template')),
    command('save', '💾', 'Save template', 'Seal the template', 'Use validation, versioning, and CAS conflict handling',
      'save template version persist seal scroll', context.canSave, context.saveReason, () => editor.save()),
    command('instantiate', '▶', 'Instantiate / run template…', 'Awaken this process…', 'Create a run from an exact saved version',
      'instantiate run start launch execute awaken cast', context.canInstantiate, context.instantiateReason, () => editor.requestInstantiate()),
    command('templates', '▤', 'Focus process templates', 'Scry the process templates', 'Leave the editor for the templates list (dirty guard applies)',
      'focus go navigate templates list scry scrolls', true, '', () => actions?.activateSubtab?.('templates')),
    command('runs', '▶', 'Focus process runs', 'Scry the process runs', 'Leave the editor for the runs list (dirty guard applies)',
      'focus go navigate runs list scry quests', true, '', () => actions?.activateSubtab?.('runs')),
  );
  return commands;
}
